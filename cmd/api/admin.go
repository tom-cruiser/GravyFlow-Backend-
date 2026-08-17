package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// BootstrapAdminUsers grants is_admin to every already-registered user whose
// email appears in the comma-separated ADMIN_BOOTSTRAP_EMAILS env var. There
// is no signup flow for admins — someone has to be first — so this runs once
// at startup after migrations, the same way seed/ops tasks are handled
// elsewhere in this codebase (e.g. EnsureResourceAccounting on user create).
// It only ever adds the admin flag; it never removes it, so operators can't
// accidentally de-admin someone by editing the env var.
func (s *DeploymentStore) BootstrapAdminUsers(ctx context.Context) error {
	raw := envOrDefault("ADMIN_BOOTSTRAP_EMAILS", "")
	if raw == "" {
		return nil
	}

	for _, email := range strings.Split(raw, ",") {
		email = strings.ToLower(strings.TrimSpace(email))
		if email == "" {
			continue
		}
		tag, err := s.pool.Exec(ctx, `UPDATE users SET is_admin = TRUE WHERE email = $1 AND is_admin = FALSE`, email)
		if err != nil {
			return fmt.Errorf("bootstrap admin %q: %w", email, err)
		}
		if tag.RowsAffected() > 0 {
			log.Printf("[admin] granted admin access to %s via ADMIN_BOOTSTRAP_EMAILS", email)
		}
	}
	return nil
}

// ============================================================================
// Module A: User & Team Administration
// ============================================================================

// AdminUserSummary is one row of the admin user search/listing table.
type AdminUserSummary struct {
	ID              string     `json:"id"`
	Email           string     `json:"email"`
	DisplayName     string     `json:"displayName"`
	GitHubHandle    string     `json:"githubHandle"`
	IsAdmin         bool       `json:"isAdmin"`
	Status          string     `json:"status"`
	DeletedAt       *time.Time `json:"deletedAt"`
	LastLoginAt     *time.Time `json:"lastLoginAt"`
	CreatedAt       time.Time  `json:"createdAt"`
	DeploymentCount int        `json:"deploymentCount"`
}

type AdminUserFilter struct {
	Email         string // partial, case-insensitive
	UserID        string // exact
	WorkspaceName string // partial match against team name, per "workspace name" in the spec
	GitHubHandle  string // partial, case-insensitive
}

type PaginatedAdminUsers struct {
	Items      []AdminUserSummary `json:"items"`
	TotalCount int                `json:"totalCount"`
	Page       int                `json:"page"`
	PerPage    int                `json:"perPage"`
	TotalPages int                `json:"totalPages"`
}

// AdminUserDetail backs the user detail view: identity, quota, usage,
// deployments, and team memberships in one call.
type AdminUserDetail struct {
	User        AdminUserSummary   `json:"user"`
	Teams       []string           `json:"teams"`
	Quota       QuotaRecord        `json:"quota"`
	Usage       ResourceUsageRecord `json:"usage"`
	Deployments []DeploymentRecord `json:"deployments"`
	CreditBalanceCents int64       `json:"creditBalanceCents"`
}

// ============================================================================
// STORE METHODS — search & detail
// ============================================================================

func (s *DeploymentStore) AdminListUsers(ctx context.Context, filter AdminUserFilter, pagination Pagination) (PaginatedAdminUsers, error) {
	if s == nil || s.pool == nil {
		return PaginatedAdminUsers{}, &StoreError{Type: ErrDatabase, Message: "deployment store is not initialized"}
	}

	if pagination.Limit <= 0 || pagination.Limit > 200 {
		pagination.Limit = 25
	}
	if pagination.Offset < 0 {
		pagination.Offset = 0
	}

	var conditions []string
	var args []any
	argIndex := 1

	if v := strings.TrimSpace(filter.Email); v != "" {
		conditions = append(conditions, "u.email ILIKE $"+strconv.Itoa(argIndex))
		args = append(args, "%"+v+"%")
		argIndex++
	}
	if v := strings.TrimSpace(filter.UserID); v != "" {
		conditions = append(conditions, "u.id::text = $"+strconv.Itoa(argIndex))
		args = append(args, v)
		argIndex++
	}
	if v := strings.TrimSpace(filter.WorkspaceName); v != "" {
		conditions = append(conditions, "t.name ILIKE $"+strconv.Itoa(argIndex))
		args = append(args, "%"+v+"%")
		argIndex++
	}
	if v := strings.TrimSpace(filter.GitHubHandle); v != "" {
		conditions = append(conditions, "u.github_handle ILIKE $"+strconv.Itoa(argIndex))
		args = append(args, "%"+v+"%")
		argIndex++
	}

	whereClause := ""
	if len(conditions) > 0 {
		whereClause = "WHERE " + strings.Join(conditions, " AND ")
	}

	var totalCount int
	countQuery := `
SELECT COUNT(DISTINCT u.id)
FROM users u
LEFT JOIN team_members tm ON tm.user_id = u.id
LEFT JOIN teams t ON t.id = tm.team_id
` + whereClause
	if err := s.pool.QueryRow(ctx, countQuery, args...).Scan(&totalCount); err != nil {
		return PaginatedAdminUsers{}, &StoreError{Type: ErrDatabase, Message: "failed to count users", Err: err}
	}

	listArgs := append(append([]any{}, args...), pagination.Limit, pagination.Offset)
	listQuery := `
SELECT DISTINCT u.id::text, u.email, u.display_name, COALESCE(u.github_handle, ''), u.is_admin, u.status, u.deleted_at, u.last_login_at, u.created_at,
	(SELECT COUNT(*) FROM deployments d WHERE d.owner_user_id = u.id AND d.deleted_at IS NULL) AS deployment_count
FROM users u
LEFT JOIN team_members tm ON tm.user_id = u.id
LEFT JOIN teams t ON t.id = tm.team_id
` + whereClause + `
ORDER BY u.created_at DESC
LIMIT $` + strconv.Itoa(argIndex) + ` OFFSET $` + strconv.Itoa(argIndex+1)

	rows, err := s.pool.Query(ctx, listQuery, listArgs...)
	if err != nil {
		return PaginatedAdminUsers{}, &StoreError{Type: ErrDatabase, Message: "failed to list users", Err: err}
	}
	defer rows.Close()

	items := make([]AdminUserSummary, 0)
	for rows.Next() {
		var u AdminUserSummary
		if err := rows.Scan(&u.ID, &u.Email, &u.DisplayName, &u.GitHubHandle, &u.IsAdmin, &u.Status, &u.DeletedAt, &u.LastLoginAt, &u.CreatedAt, &u.DeploymentCount); err != nil {
			return PaginatedAdminUsers{}, &StoreError{Type: ErrDatabase, Message: "failed to scan user", Err: err}
		}
		items = append(items, u)
	}
	if err := rows.Err(); err != nil {
		return PaginatedAdminUsers{}, &StoreError{Type: ErrDatabase, Message: "failed to iterate users", Err: err}
	}

	page := (pagination.Offset / pagination.Limit) + 1
	totalPages := (totalCount + pagination.Limit - 1) / pagination.Limit
	if totalPages < 1 {
		totalPages = 1
	}

	return PaginatedAdminUsers{
		Items:      items,
		TotalCount: totalCount,
		Page:       page,
		PerPage:    pagination.Limit,
		TotalPages: totalPages,
	}, nil
}

func (s *DeploymentStore) AdminGetUserDetail(ctx context.Context, userID string) (AdminUserDetail, error) {
	if s == nil || s.pool == nil {
		return AdminUserDetail{}, &StoreError{Type: ErrDatabase, Message: "deployment store is not initialized"}
	}

	var u AdminUserSummary
	err := s.pool.QueryRow(ctx, `
SELECT u.id::text, u.email, u.display_name, COALESCE(u.github_handle, ''), u.is_admin, u.status, u.deleted_at, u.last_login_at, u.created_at,
	(SELECT COUNT(*) FROM deployments d WHERE d.owner_user_id = u.id AND d.deleted_at IS NULL) AS deployment_count
FROM users u
WHERE u.id = $1
`, userID).Scan(&u.ID, &u.Email, &u.DisplayName, &u.GitHubHandle, &u.IsAdmin, &u.Status, &u.DeletedAt, &u.LastLoginAt, &u.CreatedAt, &u.DeploymentCount)
	if err != nil {
		return AdminUserDetail{}, &StoreError{Type: ErrNotFound, Message: "user not found", Err: err}
	}

	teams := make([]string, 0)
	rows, err := s.pool.Query(ctx, `
SELECT t.name FROM teams t
JOIN team_members tm ON tm.team_id = t.id
WHERE tm.user_id = $1
ORDER BY t.name
`, userID)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err == nil {
				teams = append(teams, name)
			}
		}
	}

	quota, err := s.GetQuota(ctx, userID)
	if err != nil {
		quota = QuotaRecord{UserID: userID}
	}

	usage, err := s.getResourceUsageOrDefault(ctx, userID)
	if err != nil {
		usage = ResourceUsageRecord{UserID: userID}
	}

	deployments, err := s.ListDeploymentsForUser(ctx, userID)
	if err != nil {
		deployments = []DeploymentRecord{}
	}

	balance, err := s.GetCreditBalance(ctx, userID)
	if err != nil {
		balance = 0
	}

	return AdminUserDetail{
		User:               u,
		Teams:              teams,
		Quota:              quota,
		Usage:              usage,
		Deployments:        deployments,
		CreditBalanceCents: balance,
	}, nil
}

// getResourceUsageOrDefault reads resource_usage directly; GetQuotaSummary
// pulls in alert side-effects we don't want for a plain detail read.
func (s *DeploymentStore) getResourceUsageOrDefault(ctx context.Context, userID string) (ResourceUsageRecord, error) {
	var usage ResourceUsageRecord
	usage.UserID = userID
	err := s.pool.QueryRow(ctx, `
SELECT current_cpu, current_memory_mb, current_apps, current_storage_mb, updated_at
FROM resource_usage WHERE user_id = $1
`, userID).Scan(&usage.CurrentCPU, &usage.CurrentMemoryMB, &usage.CurrentApps, &usage.CurrentStorageMB, &usage.UpdatedAt)
	return usage, err
}

// ============================================================================
// STORE METHODS — status, deletion
// ============================================================================

// AdminSetUserStatus implements Account Status Management. Moving a user out
// of "active" also revokes their refresh tokens so any existing session dies
// on next token refresh, and (for suspend/flag/delete) can't be used to mint
// a new access token via /auth/login since loginHandler checks status too.
func (s *DeploymentStore) AdminSetUserStatus(ctx context.Context, targetUserID string, status string) error {
	if s == nil || s.pool == nil {
		return &StoreError{Type: ErrDatabase, Message: "deployment store is not initialized"}
	}
	if !isValidUserStatus(status) {
		return &StoreError{Type: ErrInvalidInput, Message: "invalid status"}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return &StoreError{Type: ErrDatabase, Message: "failed to begin transaction", Err: err}
	}
	defer tx.Rollback(ctx)

	tag, err := tx.Exec(ctx, `UPDATE users SET status = $1, updated_at = now() WHERE id = $2`, status, targetUserID)
	if err != nil {
		return &StoreError{Type: ErrDatabase, Message: "failed to update status", Err: err}
	}
	if tag.RowsAffected() == 0 {
		return &StoreError{Type: ErrNotFound, Message: "user not found"}
	}

	if status != UserStatusActive {
		if _, err := tx.Exec(ctx, `UPDATE refresh_tokens SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`, targetUserID); err != nil {
			return &StoreError{Type: ErrDatabase, Message: "failed to revoke sessions", Err: err}
		}
	}

	return tx.Commit(ctx)
}

// AdminSoftDeleteUser retains the row (and everything referencing it) for a
// 30-day grace period while immediately revoking access, per the two-step
// deletion workflow's first option.
func (s *DeploymentStore) AdminSoftDeleteUser(ctx context.Context, targetUserID string, reason string) error {
	if s == nil || s.pool == nil {
		return &StoreError{Type: ErrDatabase, Message: "deployment store is not initialized"}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return &StoreError{Type: ErrDatabase, Message: "failed to begin transaction", Err: err}
	}
	defer tx.Rollback(ctx)

	tag, err := tx.Exec(ctx, `
UPDATE users
SET status = $1, deleted_at = now(), deleted_reason = $2, updated_at = now()
WHERE id = $3
`, UserStatusDeleted, reason, targetUserID)
	if err != nil {
		return &StoreError{Type: ErrDatabase, Message: "failed to soft-delete user", Err: err}
	}
	if tag.RowsAffected() == 0 {
		return &StoreError{Type: ErrNotFound, Message: "user not found"}
	}

	if _, err := tx.Exec(ctx, `UPDATE refresh_tokens SET revoked_at = now() WHERE user_id = $1 AND revoked_at IS NULL`, targetUserID); err != nil {
		return &StoreError{Type: ErrDatabase, Message: "failed to revoke sessions", Err: err}
	}

	return tx.Commit(ctx)
}

// SoftDeletedPastRetention lists users whose 30-day soft-delete grace period
// has elapsed, i.e. candidates an operator should review for hard deletion.
// There is no automatic cron sweep in this codebase; this is surfaced in the
// admin UI for a human to act on deliberately.
func (s *DeploymentStore) SoftDeletedPastRetention(ctx context.Context, retention time.Duration) ([]AdminUserSummary, error) {
	if s == nil || s.pool == nil {
		return nil, &StoreError{Type: ErrDatabase, Message: "deployment store is not initialized"}
	}

	rows, err := s.pool.Query(ctx, `
SELECT id::text, email, display_name, is_admin, status, deleted_at, last_login_at, created_at, 0
FROM users
WHERE status = $1 AND deleted_at IS NOT NULL AND deleted_at < now() - $2::interval
ORDER BY deleted_at ASC
`, UserStatusDeleted, fmt.Sprintf("%d seconds", int(retention.Seconds())))
	if err != nil {
		return nil, &StoreError{Type: ErrDatabase, Message: "failed to list retained users", Err: err}
	}
	defer rows.Close()

	items := make([]AdminUserSummary, 0)
	for rows.Next() {
		var u AdminUserSummary
		if err := rows.Scan(&u.ID, &u.Email, &u.DisplayName, &u.IsAdmin, &u.Status, &u.DeletedAt, &u.LastLoginAt, &u.CreatedAt, &u.DeploymentCount); err != nil {
			return nil, &StoreError{Type: ErrDatabase, Message: "failed to scan user", Err: err}
		}
		items = append(items, u)
	}
	return items, rows.Err()
}

// AdminHardDeleteUser purges the database records, stops and removes every
// running container the user owns, and only then deletes the user row (which
// cascades to projects/deployments/api_keys/refresh_tokens/etc. via the
// existing ON DELETE CASCADE foreign keys in db/schema.sql). Containers are
// torn down before the DB delete so a mid-failure never leaves an orphaned
// container with no deployment record pointing back to it.
func (s *DeploymentStore) AdminHardDeleteUser(ctx context.Context, targetUserID string) error {
	if s == nil || s.pool == nil {
		return &StoreError{Type: ErrDatabase, Message: "deployment store is not initialized"}
	}

	deployments, err := s.ListDeploymentsForUser(ctx, targetUserID)
	if err != nil {
		return err
	}
	for _, d := range deployments {
		if d.ContainerID == "" {
			continue
		}
		// removeVolumes=true: the spec's hard-delete workflow explicitly calls
		// for purging storage volumes along with the container, unlike the
		// other three StopAndRemoveContainer call sites (force-stop, isolate,
		// restart) which only ever tear down the container itself.
		if err := StopAndRemoveContainer(d.ContainerID, true); err != nil {
			// Best-effort: log and continue so a stale/already-gone container
			// doesn't block account deletion the operator explicitly requested.
			fmt.Printf("[WARN] hard delete: failed to remove container %q for deployment %q: %v\n", d.ContainerID, d.DeploymentID, err)
		}
	}

	tag, err := s.pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, targetUserID)
	if err != nil {
		return &StoreError{Type: ErrDatabase, Message: "failed to hard-delete user", Err: err}
	}
	if tag.RowsAffected() == 0 {
		return &StoreError{Type: ErrNotFound, Message: "user not found"}
	}
	return nil
}

// ============================================================================
// STORE METHODS — impersonation
// ============================================================================

func (s *DeploymentStore) RecordImpersonationGrant(ctx context.Context, adminID string, targetUserID string, reason string, expiresAt time.Time) error {
	if s == nil || s.pool == nil {
		return &StoreError{Type: ErrDatabase, Message: "deployment store is not initialized"}
	}
	_, err := s.pool.Exec(ctx, `
INSERT INTO impersonation_grants (admin_user_id, target_user_id, reason, expires_at)
VALUES ($1, $2, $3, $4)
`, adminID, targetUserID, reason, expiresAt)
	return err
}

// ============================================================================
// HANDLERS
// ============================================================================

func adminListUsersHandler(c *gin.Context) {
	filter := AdminUserFilter{
		Email:         c.Query("email"),
		UserID:        c.Query("userId"),
		WorkspaceName: c.Query("workspace"),
		GitHubHandle:  c.Query("githubHandle"),
	}

	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	if page < 1 {
		page = 1
	}
	perPage, _ := strconv.Atoi(c.DefaultQuery("perPage", "25"))
	if perPage <= 0 || perPage > 200 {
		perPage = 25
	}

	result, err := deploymentStore.AdminListUsers(c.Request.Context(), filter, Pagination{
		Limit:  perPage,
		Offset: (page - 1) * perPage,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_list_users", "details": err.Error(), "request_id": c.GetString("requestID")})
		return
	}
	c.JSON(http.StatusOK, result)
}

func adminGetUserHandler(c *gin.Context) {
	targetUserID := strings.TrimSpace(c.Param("id"))
	if targetUserID == "" {
		sendBadRequest(c, "user id is required", nil)
		return
	}

	detail, err := deploymentStore.AdminGetUserDetail(c.Request.Context(), targetUserID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "user_not_found", "details": err.Error()})
		return
	}
	c.JSON(http.StatusOK, detail)
}

type AdminSetUserStatusRequest struct {
	Status string `json:"status" binding:"required"`
	Reason string `json:"reason"`
}

func adminSetUserStatusHandler(c *gin.Context) {
	admin, ok := currentAuthUser(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	targetUserID := strings.TrimSpace(c.Param("id"))
	if targetUserID == "" {
		sendBadRequest(c, "user id is required", nil)
		return
	}
	if targetUserID == admin.ID {
		c.JSON(http.StatusConflict, gin.H{"error": "cannot_modify_self", "details": "admins cannot change their own account status"})
		return
	}

	var req AdminSetUserStatusRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		sendBadRequest(c, "invalid JSON body", err)
		return
	}
	status := strings.ToLower(strings.TrimSpace(req.Status))
	if !isValidUserStatus(status) {
		sendBadRequest(c, "status must be one of: active, suspended, flagged, deleted", nil)
		return
	}

	if err := deploymentStore.AdminSetUserStatus(c.Request.Context(), targetUserID, status); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_update_status", "details": err.Error()})
		return
	}

	actorID, actorEmail := auditActorFromContext(c)
	if err := RecordAuditLog(c.Request.Context(), actorID, actorEmail, "user.status.set."+status, "user", targetUserID, map[string]any{"reason": req.Reason}, c.ClientIP()); err != nil {
		fmt.Printf("[WARN] failed to record audit log for user.status.set.%s on %q: %v\n", status, targetUserID, err)
	}

	c.JSON(http.StatusOK, gin.H{"message": "status updated", "status": status})
}

type AdminDeleteUserRequest struct {
	Mode   string `json:"mode" binding:"required"` // "soft" or "hard"
	Reason string `json:"reason"`
}

// adminDeleteUserHandler is the two-step deletion workflow's execution step —
// the frontend modal collects confirmation and mode before calling this.
func adminDeleteUserHandler(c *gin.Context) {
	admin, ok := currentAuthUser(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	targetUserID := strings.TrimSpace(c.Param("id"))
	if targetUserID == "" {
		sendBadRequest(c, "user id is required", nil)
		return
	}
	if targetUserID == admin.ID {
		c.JSON(http.StatusConflict, gin.H{"error": "cannot_delete_self"})
		return
	}

	var req AdminDeleteUserRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		sendBadRequest(c, "invalid JSON body", err)
		return
	}

	mode := strings.ToLower(strings.TrimSpace(req.Mode))
	actorID, actorEmail := auditActorFromContext(c)

	switch mode {
	case "soft":
		if err := deploymentStore.AdminSoftDeleteUser(c.Request.Context(), targetUserID, req.Reason); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_soft_delete", "details": err.Error()})
			return
		}
		if err := RecordAuditLog(c.Request.Context(), actorID, actorEmail, "user.delete.soft", "user", targetUserID, map[string]any{"reason": req.Reason, "retentionDays": 30}, c.ClientIP()); err != nil {
			fmt.Printf("[WARN] failed to record audit log for user.delete.soft on %q: %v\n", targetUserID, err)
		}
		c.JSON(http.StatusOK, gin.H{"message": "user soft-deleted; data retained for 30 days", "mode": "soft"})
	case "hard":
		if err := deploymentStore.AdminHardDeleteUser(c.Request.Context(), targetUserID); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_hard_delete", "details": err.Error()})
			return
		}
		if err := RecordAuditLog(c.Request.Context(), actorID, actorEmail, "user.delete.hard", "user", targetUserID, map[string]any{"reason": req.Reason}, c.ClientIP()); err != nil {
			fmt.Printf("[WARN] failed to record audit log for user.delete.hard on %q: %v\n", targetUserID, err)
		}
		c.JSON(http.StatusOK, gin.H{"message": "user and all associated resources permanently deleted", "mode": "hard"})
	default:
		sendBadRequest(c, `mode must be "soft" or "hard"`, nil)
	}
}

type AdminImpersonateRequest struct {
	Reason string `json:"reason"`
}

// adminImpersonateHandler issues a short-lived, read-only-enforced access
// token scoped to the target user, per "Impersonation Mode" in the spec.
func adminImpersonateHandler(c *gin.Context) {
	admin, ok := currentAuthUser(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	targetUserID := strings.TrimSpace(c.Param("id"))
	if targetUserID == "" {
		sendBadRequest(c, "user id is required", nil)
		return
	}

	target, err := deploymentStore.GetUserByID(c.Request.Context(), targetUserID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "user_not_found", "details": err.Error()})
		return
	}

	var req AdminImpersonateRequest
	_ = c.ShouldBindJSON(&req)

	token, expiresAt, err := issueImpersonationToken(admin, target)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_issue_impersonation_token", "details": err.Error()})
		return
	}

	if err := deploymentStore.RecordImpersonationGrant(c.Request.Context(), admin.ID, target.ID, req.Reason, expiresAt); err != nil {
		fmt.Printf("[WARN] failed to record impersonation grant: %v\n", err)
	}

	actorID, actorEmail := auditActorFromContext(c)
	if err := RecordAuditLog(c.Request.Context(), actorID, actorEmail, "user.impersonate", "user", targetUserID, map[string]any{"reason": req.Reason}, c.ClientIP()); err != nil {
		fmt.Printf("[WARN] failed to record audit log for user.impersonate on %q: %v\n", targetUserID, err)
	}

	c.JSON(http.StatusOK, gin.H{
		"accessToken": token,
		"tokenType":   tokenTypeAccess,
		"expiresIn":   int64(time.Until(expiresAt).Seconds()),
		"readOnly":    true,
		"user": AuthUserResponse{
			ID:          target.ID,
			Email:       target.Email,
			DisplayName: target.DisplayName,
			IsAdmin:     target.IsAdmin,
			MFAEnabled:  target.MFAEnabled,
		},
	})
}
