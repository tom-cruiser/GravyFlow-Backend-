package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// ============================================================================
// TYPES
// ============================================================================

// AuditLogRecord mirrors the audit_logs table (migration 14 in db.go), which
// is made immutable at the database level via a BEFORE UPDATE/DELETE trigger
// — there is deliberately no update/delete method or handler for this table.
type AuditLogRecord struct {
	ID          string          `json:"id"`
	ActorUserID *string         `json:"actorUserId"`
	ActorEmail  string          `json:"actorEmail"`
	Action      string          `json:"action"`
	TargetType  string          `json:"targetType"`
	TargetID    string          `json:"targetId,omitempty"`
	Details     json.RawMessage `json:"details,omitempty"`
	IPAddress   string          `json:"ipAddress,omitempty"`
	CreatedAt   time.Time       `json:"createdAt"`
}

type AuditLogFilter struct {
	ActorEmail string
	TargetType string
	TargetID   string
	Action     string
}

type PaginatedAuditLogs struct {
	Items      []AuditLogRecord `json:"items"`
	TotalCount int              `json:"totalCount"`
	Page       int              `json:"page"`
	PerPage    int              `json:"perPage"`
	TotalPages int              `json:"totalPages"`
}

// ============================================================================
// WRITE PATH (insert-only)
// ============================================================================

// RecordAuditLog is the single insertion point for admin_audit_logs. Every
// admin-facing mutation in this file, admin.go, admin_infra.go, and
// admin_billing.go must call this so Module D's log table has full coverage
// ("Capture Timestamp, Admin Email, Target User/Project ID, Action Taken, and
// IP Address"). actorUserID may be nil (e.g. a system-triggered action).
func RecordAuditLog(ctx context.Context, actorUserID *string, actorEmail string, action string, targetType string, targetID string, details map[string]any, ipAddress string) error {
	if deploymentStore == nil || deploymentStore.pool == nil {
		return &StoreError{Type: ErrDatabase, Message: "deployment store is not initialized"}
	}

	var detailsJSON []byte
	if details != nil {
		encoded, err := json.Marshal(details)
		if err != nil {
			return err
		}
		detailsJSON = encoded
	}

	var targetIDParam any
	if strings.TrimSpace(targetID) != "" {
		targetIDParam = targetID
	}

	_, err := deploymentStore.pool.Exec(ctx, `
INSERT INTO audit_logs (actor_user_id, actor_email, action, target_type, target_id, details, ip_address)
VALUES ($1, $2, $3, $4, $5, $6, $7)
`, actorUserID, actorEmail, action, targetType, targetIDParam, detailsJSON, ipAddress)
	return err
}

// auditActorFromContext reads the admin performing the current request, for
// handlers to pass into RecordAuditLog without repeating the boilerplate.
func auditActorFromContext(c *gin.Context) (userID *string, email string) {
	user, ok := currentAuthUser(c)
	if !ok {
		return nil, "unknown"
	}
	id := user.ID
	return &id, user.Email
}

// ============================================================================
// READ PATH
// ============================================================================

func (s *DeploymentStore) AdminListAuditLogs(ctx context.Context, filter AuditLogFilter, pagination Pagination) (PaginatedAuditLogs, error) {
	if s == nil || s.pool == nil {
		return PaginatedAuditLogs{}, &StoreError{Type: ErrDatabase, Message: "deployment store is not initialized"}
	}

	if pagination.Limit <= 0 || pagination.Limit > 200 {
		pagination.Limit = 50
	}
	if pagination.Offset < 0 {
		pagination.Offset = 0
	}

	var conditions []string
	var args []any
	argIndex := 1

	if v := strings.TrimSpace(filter.ActorEmail); v != "" {
		conditions = append(conditions, "actor_email ILIKE $"+strconv.Itoa(argIndex))
		args = append(args, "%"+v+"%")
		argIndex++
	}
	if v := strings.TrimSpace(filter.TargetType); v != "" {
		conditions = append(conditions, "target_type = $"+strconv.Itoa(argIndex))
		args = append(args, v)
		argIndex++
	}
	if v := strings.TrimSpace(filter.TargetID); v != "" {
		conditions = append(conditions, "target_id = $"+strconv.Itoa(argIndex))
		args = append(args, v)
		argIndex++
	}
	if v := strings.TrimSpace(filter.Action); v != "" {
		conditions = append(conditions, "action = $"+strconv.Itoa(argIndex))
		args = append(args, v)
		argIndex++
	}

	whereClause := ""
	if len(conditions) > 0 {
		whereClause = "WHERE " + strings.Join(conditions, " AND ")
	}

	var totalCount int
	countQuery := "SELECT COUNT(*) FROM audit_logs " + whereClause
	if err := s.pool.QueryRow(ctx, countQuery, args...).Scan(&totalCount); err != nil {
		return PaginatedAuditLogs{}, &StoreError{Type: ErrDatabase, Message: "failed to count audit logs", Err: err}
	}

	listArgs := append(append([]any{}, args...), pagination.Limit, pagination.Offset)
	listQuery := `
SELECT id::text, actor_user_id::text, actor_email, action, target_type, COALESCE(target_id, ''), details, COALESCE(ip_address::text, ''), created_at
FROM audit_logs
` + whereClause + `
ORDER BY created_at DESC
LIMIT $` + strconv.Itoa(argIndex) + ` OFFSET $` + strconv.Itoa(argIndex+1)

	rows, err := s.pool.Query(ctx, listQuery, listArgs...)
	if err != nil {
		return PaginatedAuditLogs{}, &StoreError{Type: ErrDatabase, Message: "failed to list audit logs", Err: err}
	}
	defer rows.Close()

	items := make([]AuditLogRecord, 0)
	for rows.Next() {
		var record AuditLogRecord
		var actorUserID *string
		if err := rows.Scan(&record.ID, &actorUserID, &record.ActorEmail, &record.Action, &record.TargetType, &record.TargetID, &record.Details, &record.IPAddress, &record.CreatedAt); err != nil {
			return PaginatedAuditLogs{}, &StoreError{Type: ErrDatabase, Message: "failed to scan audit log", Err: err}
		}
		record.ActorUserID = actorUserID
		items = append(items, record)
	}
	if err := rows.Err(); err != nil {
		return PaginatedAuditLogs{}, &StoreError{Type: ErrDatabase, Message: "failed to iterate audit logs", Err: err}
	}

	page := (pagination.Offset / pagination.Limit) + 1
	totalPages := (totalCount + pagination.Limit - 1) / pagination.Limit
	if totalPages < 1 {
		totalPages = 1
	}

	return PaginatedAuditLogs{
		Items:      items,
		TotalCount: totalCount,
		Page:       page,
		PerPage:    pagination.Limit,
		TotalPages: totalPages,
	}, nil
}

// ============================================================================
// HANDLERS
// ============================================================================

func adminListAuditLogsHandler(c *gin.Context) {
	filter := AuditLogFilter{
		ActorEmail: c.Query("actorEmail"),
		TargetType: c.Query("targetType"),
		TargetID:   c.Query("targetId"),
		Action:     c.Query("action"),
	}

	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	if page < 1 {
		page = 1
	}
	perPage, _ := strconv.Atoi(c.DefaultQuery("perPage", "50"))
	if perPage <= 0 || perPage > 200 {
		perPage = 50
	}

	result, err := deploymentStore.AdminListAuditLogs(c.Request.Context(), filter, Pagination{
		Limit:  perPage,
		Offset: (page - 1) * perPage,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":      "failed_to_list_audit_logs",
			"details":    err.Error(),
			"request_id": c.GetString("requestID"),
		})
		return
	}

	c.JSON(http.StatusOK, result)
}
