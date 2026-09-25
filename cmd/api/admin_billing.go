package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
)

// ============================================================================
// Module C: Billing, Quotas & Abuse Control
// ============================================================================

const (
	// creditEntryIssue / creditEntryRevoke are the only two credit_ledger
	// entry_type values; the ledger is append-only, so a revoke is a new
	// negative-amount row rather than an edit of a prior issuance.
	creditEntryIssue  = "issue"
	creditEntryRevoke = "revoke"

	// riskScoreHighCPU flags a deployment whose live CPU usage looks like
	// sustained crypto-mining load rather than normal request traffic.
	riskScoreHighCPU     = 75
	riskCPUCoreThreshold = 1.8 // cores; compared against ContainerStats.CPUUsage
	// riskSustainedSamples consecutive sweeps above the threshold before an
	// alert opens (~3 minutes at the default one-minute interval).
	riskSustainedSamples     = 3
	defaultRiskSweepInterval = time.Minute
)

// ============================================================================
// CREDIT LEDGER
// ============================================================================

type CreditLedgerEntry struct {
	ID          string    `json:"id"`
	UserID      string    `json:"userId"`
	AmountCents int64     `json:"amountCents"`
	EntryType   string    `json:"entryType"`
	Reason      string    `json:"reason"`
	IssuedBy    string    `json:"issuedBy"`
	CreatedAt   time.Time `json:"createdAt"`
}

func (s *DeploymentStore) GetCreditBalance(ctx context.Context, userID string) (int64, error) {
	if s == nil || s.pool == nil {
		return 0, &StoreError{Type: ErrDatabase, Message: "deployment store is not initialized"}
	}
	var balance int64
	err := s.pool.QueryRow(ctx, `SELECT COALESCE(SUM(amount_cents), 0) FROM credit_ledger WHERE user_id = $1`, userID).Scan(&balance)
	return balance, err
}

func (s *DeploymentStore) ListCreditHistory(ctx context.Context, userID string, limit int) ([]CreditLedgerEntry, error) {
	if s == nil || s.pool == nil {
		return nil, &StoreError{Type: ErrDatabase, Message: "deployment store is not initialized"}
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.pool.Query(ctx, `
SELECT id::text, user_id::text, amount_cents, entry_type, reason, issued_by::text, created_at
FROM credit_ledger WHERE user_id = $1 ORDER BY created_at DESC LIMIT $2
`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := make([]CreditLedgerEntry, 0)
	for rows.Next() {
		var e CreditLedgerEntry
		if err := rows.Scan(&e.ID, &e.UserID, &e.AmountCents, &e.EntryType, &e.Reason, &e.IssuedBy, &e.CreatedAt); err != nil {
			return nil, err
		}
		items = append(items, e)
	}
	return items, rows.Err()
}

// applyCreditEntry inserts one append-only ledger row. amountCents is signed:
// positive for an issuance, negative for a revocation.
func (s *DeploymentStore) applyCreditEntry(ctx context.Context, userID string, amountCents int64, entryType string, reason string, issuedBy string) (CreditLedgerEntry, error) {
	if s == nil || s.pool == nil {
		return CreditLedgerEntry{}, &StoreError{Type: ErrDatabase, Message: "deployment store is not initialized"}
	}
	var e CreditLedgerEntry
	err := s.pool.QueryRow(ctx, `
INSERT INTO credit_ledger (user_id, amount_cents, entry_type, reason, issued_by)
VALUES ($1, $2, $3, $4, $5)
RETURNING id::text, user_id::text, amount_cents, entry_type, reason, issued_by::text, created_at
`, userID, amountCents, entryType, reason, issuedBy).Scan(&e.ID, &e.UserID, &e.AmountCents, &e.EntryType, &e.Reason, &e.IssuedBy, &e.CreatedAt)
	return e, err
}

// ============================================================================
// FRAUD & ABUSE
// ============================================================================

type RiskAlert struct {
	ID           string     `json:"id"`
	UserID       string     `json:"userId"`
	UserEmail    string     `json:"userEmail"`
	DeploymentID *string    `json:"deploymentId"`
	AppName      string     `json:"appName,omitempty"`
	RiskScore    int        `json:"riskScore"`
	Reason       string     `json:"reason"`
	Status       string     `json:"status"`
	CreatedAt    time.Time  `json:"createdAt"`
	ResolvedAt   *time.Time `json:"resolvedAt"`
}

// riskSweeper samples live CPU usage (via GetContainerStats, same real Docker
// stats path as the cluster overview) across every running deployment on a
// fixed interval, and opens a risk_alerts row only once a deployment has
// stayed above riskCPUCoreThreshold for riskSustainedSamples consecutive
// samples — a single busy moment (a build step, a traffic spike) never alerts.
type riskSweeper struct {
	store    *DeploymentStore
	required int
	hot      map[string]int // deploymentID -> consecutive high-CPU samples
}

func newRiskSweeper(store *DeploymentStore) *riskSweeper {
	return &riskSweeper{store: store, required: riskSustainedSamples, hot: make(map[string]int)}
}

// observe records one sample round — the IDs currently above the threshold —
// and returns those that have now been high for the required run. Anything
// not in this round resets. Returning the same ID on later rounds is fine:
// openRiskAlert dedupes against the deployment's open alert, and re-alerts
// if an admin resolved it but the load came back.
func (r *riskSweeper) observe(highIDs []string) []string {
	next := make(map[string]int, len(highIDs))
	sustained := make([]string, 0)
	for _, id := range highIDs {
		next[id] = r.hot[id] + 1
		if next[id] >= r.required {
			sustained = append(sustained, id)
		}
	}
	r.hot = next
	return sustained
}

// sweep runs one sample round and opens alerts for sustained load. It returns
// the alerts newly opened.
func (r *riskSweeper) sweep(ctx context.Context) ([]RiskAlert, error) {
	running, err := r.store.adminListRunningContainers(ctx)
	if err != nil {
		return nil, err
	}

	byID := make(map[string]AdminDeploymentSummary, len(running))
	cpuByID := make(map[string]float64, len(running))
	highIDs := make([]string, 0)
	for _, d := range running {
		// Per-container timeout: one wedged container mustn't starve the
		// rest of the round.
		statsCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		stats, err := GetContainerStats(statsCtx, d.ContainerID)
		cancel()
		if err != nil || stats == nil || stats.CPUUsage < riskCPUCoreThreshold {
			continue
		}
		byID[d.DeploymentID] = d
		cpuByID[d.DeploymentID] = stats.CPUUsage
		highIDs = append(highIDs, d.DeploymentID)
	}

	created := make([]RiskAlert, 0)
	for _, id := range r.observe(highIDs) {
		d := byID[id]
		reason := fmt.Sprintf("sustained high CPU usage consistent with crypto-mining (%.1f cores over %d consecutive samples)", cpuByID[id], r.hot[id])
		alert, opened, err := r.store.openRiskAlert(ctx, d, riskScoreHighCPU, reason)
		if err != nil {
			return created, err
		}
		if opened {
			created = append(created, alert)
		}
	}
	return created, nil
}

// openRiskAlert inserts an open alert unless the deployment already has one.
func (s *DeploymentStore) openRiskAlert(ctx context.Context, d AdminDeploymentSummary, score int, reason string) (RiskAlert, bool, error) {
	var alert RiskAlert
	err := s.pool.QueryRow(ctx, `
INSERT INTO risk_alerts (user_id, deployment_id, risk_score, reason, status)
SELECT $1, $2, $3, $4, 'open'
WHERE NOT EXISTS (
	SELECT 1 FROM risk_alerts WHERE deployment_id = $2 AND status = 'open'
)
RETURNING id::text, user_id::text, deployment_id::text, risk_score, reason, status, created_at
`, d.OwnerUserID, d.DeploymentID, score, reason).Scan(
		&alert.ID, &alert.UserID, &alert.DeploymentID, &alert.RiskScore, &alert.Reason, &alert.Status, &alert.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return RiskAlert{}, false, nil // an open alert already exists
	}
	if err != nil {
		return RiskAlert{}, false, err
	}
	alert.UserEmail = d.OwnerEmail
	alert.AppName = d.AppName
	return alert, true, nil
}

// RunRiskAlertSweeper runs the abuse heuristic every interval until ctx is
// done, so a miner is flagged whether or not an admin has the Abuse panel
// open. GRAVYFLOW_RISK_SWEEP_INTERVAL sets the interval; 0 disables it.
func RunRiskAlertSweeper(ctx context.Context, store *DeploymentStore, interval time.Duration) {
	if store == nil || interval <= 0 {
		log.Printf("[INFO] risk alert sweeper disabled")
		return
	}
	sweeper := newRiskSweeper(store)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		created, err := sweeper.sweep(ctx)
		if err != nil {
			log.Printf("[WARN] risk alert sweeper: %v", err)
		}
		for _, a := range created {
			log.Printf("[WARN] risk alert opened: %s (owner %s): %s", a.AppName, a.UserEmail, a.Reason)
		}
	}
}

func (s *DeploymentStore) ListRiskAlerts(ctx context.Context, status string) ([]RiskAlert, error) {
	if s == nil || s.pool == nil {
		return nil, &StoreError{Type: ErrDatabase, Message: "deployment store is not initialized"}
	}

	query := `
SELECT r.id::text, r.user_id::text, u.email, r.deployment_id::text, COALESCE(d.app_name, ''), r.risk_score, r.reason, r.status, r.created_at, r.resolved_at
FROM risk_alerts r
JOIN users u ON u.id = r.user_id
LEFT JOIN deployments d ON d.id = r.deployment_id
`
	var args []any
	if strings.TrimSpace(status) != "" {
		query += " WHERE r.status = $1"
		args = append(args, status)
	}
	query += " ORDER BY r.created_at DESC"

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := make([]RiskAlert, 0)
	for rows.Next() {
		var a RiskAlert
		if err := rows.Scan(&a.ID, &a.UserID, &a.UserEmail, &a.DeploymentID, &a.AppName, &a.RiskScore, &a.Reason, &a.Status, &a.CreatedAt, &a.ResolvedAt); err != nil {
			return nil, err
		}
		items = append(items, a)
	}
	return items, rows.Err()
}

func (s *DeploymentStore) ResolveRiskAlert(ctx context.Context, alertID string, resolvedBy string) error {
	if s == nil || s.pool == nil {
		return &StoreError{Type: ErrDatabase, Message: "deployment store is not initialized"}
	}
	tag, err := s.pool.Exec(ctx, `UPDATE risk_alerts SET status = 'resolved', resolved_by = $1, resolved_at = now() WHERE id = $2 AND status = 'open'`, resolvedBy, alertID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return &StoreError{Type: ErrNotFound, Message: "open risk alert not found"}
	}
	return nil
}

// ============================================================================
// HANDLERS — Quota & tier overrides (wraps resources.go's existing engine)
// ============================================================================

type AdminUpdateQuotaRequest struct {
	MaxCPU         *float64 `json:"maxCpu"`
	MaxMemoryMB    *int64   `json:"maxMemoryMb"`
	MaxApps        *int64   `json:"maxApps"`
	MaxStorageMB   *int64   `json:"maxStorageMb"`
	MaxBandwidthGB *int64   `json:"maxBandwidthGb"`
}

func adminUpdateQuotaHandler(c *gin.Context) {
	targetUserID := strings.TrimSpace(c.Param("id"))
	if targetUserID == "" {
		sendBadRequest(c, "user id is required", nil)
		return
	}

	var req AdminUpdateQuotaRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		sendBadRequest(c, "invalid JSON body", err)
		return
	}

	admin, _ := currentAuthUser(c)
	record, err := deploymentStore.UpdateQuota(c.Request.Context(), targetUserID, req.MaxCPU, req.MaxMemoryMB, req.MaxApps, req.MaxStorageMB, req.MaxBandwidthGB, admin.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_update_quota", "details": err.Error()})
		return
	}

	actorID, actorEmail := auditActorFromContext(c)
	if err := RecordAuditLog(c.Request.Context(), actorID, actorEmail, "quota.update", "user", targetUserID, map[string]any{"maxCpu": req.MaxCPU, "maxMemoryMb": req.MaxMemoryMB, "maxApps": req.MaxApps, "maxStorageMb": req.MaxStorageMB, "maxBandwidthGb": req.MaxBandwidthGB}, c.ClientIP()); err != nil {
		fmt.Printf("[WARN] failed to record audit log for quota.update on %q: %v\n", targetUserID, err)
	}

	c.JSON(http.StatusOK, record)
}

// adminResetQuotaHandler zeroes out tracked USAGE (current_cpu, current_apps,
// etc. in resource_usage) — not the quota LIMITS. resources.go's ResetQuota
// is misleadingly named for what it does; this handler's wording and route
// are deliberately about "usage" to avoid repeating that confusion in the
// admin UI. To restore quota LIMITS to plan defaults, see
// adminRestoreDefaultQuotaHandler below.
func adminResetQuotaHandler(c *gin.Context) {
	targetUserID := strings.TrimSpace(c.Param("id"))
	admin, _ := currentAuthUser(c)

	if err := deploymentStore.ResetQuota(c.Request.Context(), targetUserID, admin.ID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_reset_usage", "details": err.Error()})
		return
	}

	actorID, actorEmail := auditActorFromContext(c)
	if err := RecordAuditLog(c.Request.Context(), actorID, actorEmail, "quota.usage_reset", "user", targetUserID, nil, c.ClientIP()); err != nil {
		fmt.Printf("[WARN] failed to record audit log for quota.usage_reset on %q: %v\n", targetUserID, err)
	}

	c.JSON(http.StatusOK, gin.H{"message": "tracked usage counters reset to zero"})
}

// adminRestoreDefaultQuotaHandler restores quota LIMITS (max_cpu,
// max_memory_mb, max_apps, max_storage_mb) to the platform defaults, going
// through the same UpdateQuota path (and quota_history trail) as a manual
// override.
func adminRestoreDefaultQuotaHandler(c *gin.Context) {
	targetUserID := strings.TrimSpace(c.Param("id"))
	admin, _ := currentAuthUser(c)

	defaultCPU := defaultMaxCPU
	defaultMemory := int64(defaultMaxMemoryMB)
	defaultApps := int64(defaultMaxApps)
	defaultStorage := int64(defaultMaxStorageMB)
	defaultBandwidth := int64(defaultMaxBandwidthGB)

	record, err := deploymentStore.UpdateQuota(c.Request.Context(), targetUserID, &defaultCPU, &defaultMemory, &defaultApps, &defaultStorage, &defaultBandwidth, admin.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_restore_default_quota", "details": err.Error()})
		return
	}

	actorID, actorEmail := auditActorFromContext(c)
	if err := RecordAuditLog(c.Request.Context(), actorID, actorEmail, "quota.restore_defaults", "user", targetUserID, nil, c.ClientIP()); err != nil {
		fmt.Printf("[WARN] failed to record audit log for quota.restore_defaults on %q: %v\n", targetUserID, err)
	}

	c.JSON(http.StatusOK, record)
}

func adminGetQuotaHistoryHandler(c *gin.Context) {
	targetUserID := strings.TrimSpace(c.Param("id"))
	history, err := deploymentStore.GetQuotaHistory(c.Request.Context(), targetUserID, 100)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_load_quota_history", "details": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"history": history, "count": len(history)})
}

// ============================================================================
// HANDLERS — Credits
// ============================================================================

type AdminCreditRequest struct {
	AmountCents int64  `json:"amountCents" binding:"required"`
	Reason      string `json:"reason" binding:"required"`
}

func adminIssueCreditHandler(c *gin.Context) {
	targetUserID := strings.TrimSpace(c.Param("id"))
	var req AdminCreditRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		sendBadRequest(c, "invalid JSON body", err)
		return
	}
	if req.AmountCents <= 0 {
		sendBadRequest(c, "amountCents must be positive; use the revoke endpoint to remove credit", nil)
		return
	}

	admin, _ := currentAuthUser(c)
	entry, err := deploymentStore.applyCreditEntry(c.Request.Context(), targetUserID, req.AmountCents, creditEntryIssue, req.Reason, admin.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_issue_credit", "details": err.Error()})
		return
	}

	actorID, actorEmail := auditActorFromContext(c)
	if err := RecordAuditLog(c.Request.Context(), actorID, actorEmail, "credit.issue", "user", targetUserID, map[string]any{"amountCents": req.AmountCents, "reason": req.Reason}, c.ClientIP()); err != nil {
		fmt.Printf("[WARN] failed to record audit log for credit.issue on %q: %v\n", targetUserID, err)
	}

	c.JSON(http.StatusCreated, entry)
}

func adminRevokeCreditHandler(c *gin.Context) {
	targetUserID := strings.TrimSpace(c.Param("id"))
	var req AdminCreditRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		sendBadRequest(c, "invalid JSON body", err)
		return
	}
	if req.AmountCents <= 0 {
		sendBadRequest(c, "amountCents must be a positive amount to revoke", nil)
		return
	}

	admin, _ := currentAuthUser(c)
	entry, err := deploymentStore.applyCreditEntry(c.Request.Context(), targetUserID, -req.AmountCents, creditEntryRevoke, req.Reason, admin.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_revoke_credit", "details": err.Error()})
		return
	}

	actorID, actorEmail := auditActorFromContext(c)
	if err := RecordAuditLog(c.Request.Context(), actorID, actorEmail, "credit.revoke", "user", targetUserID, map[string]any{"amountCents": req.AmountCents, "reason": req.Reason}, c.ClientIP()); err != nil {
		fmt.Printf("[WARN] failed to record audit log for credit.revoke on %q: %v\n", targetUserID, err)
	}

	c.JSON(http.StatusCreated, entry)
}

func adminGetCreditBalanceHandler(c *gin.Context) {
	targetUserID := strings.TrimSpace(c.Param("id"))
	balance, err := deploymentStore.GetCreditBalance(c.Request.Context(), targetUserID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_load_balance", "details": err.Error()})
		return
	}
	history, err := deploymentStore.ListCreditHistory(c.Request.Context(), targetUserID, 50)
	if err != nil {
		history = []CreditLedgerEntry{}
	}
	c.JSON(http.StatusOK, gin.H{"balanceCents": balance, "history": history})
}

// ============================================================================
// HANDLERS — Fraud & Abuse
// ============================================================================

func adminListRiskAlertsHandler(c *gin.Context) {
	// Alerts are opened by RunRiskAlertSweeper in the background; listing
	// never samples Docker, so the panel loads fast however many apps run.
	alerts, err := deploymentStore.ListRiskAlerts(c.Request.Context(), c.Query("status"))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_load_risk_alerts", "details": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"alerts": alerts, "count": len(alerts)})
}

// adminIsolateServiceHandler is the "instant Isolate Service kill-switch":
// force-stop the container and resolve the alert in one action.
func adminIsolateServiceHandler(c *gin.Context) {
	alertID := strings.TrimSpace(c.Param("id"))

	admin, _ := currentAuthUser(c)
	if err := deploymentStore.ResolveRiskAlert(c.Request.Context(), alertID, admin.ID); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "risk_alert_not_found", "details": err.Error()})
		return
	}

	deploymentID := strings.TrimSpace(c.Query("deploymentId"))
	if deploymentID != "" {
		if deployment, err := deploymentStore.AdminGetDeploymentByID(c.Request.Context(), deploymentID); err == nil && deployment.ContainerID != "" {
			if err := StopAndRemoveContainer(deployment.ContainerID, false); err != nil {
				fmt.Printf("[WARN] risk alert isolate: failed to stop container %q for deployment %q: %v\n", deployment.ContainerID, deploymentID, err)
			}
			if err := deploymentStore.UpdateDeploymentStatus(c.Request.Context(), deploymentID, DeploymentStatusStopped, "isolated by admin: risk alert "+alertID, "", "", "", ""); err != nil {
				fmt.Printf("[WARN] risk alert isolate: failed to update deployment status for %q: %v\n", deploymentID, err)
			}
		}
	}

	actorID, actorEmail := auditActorFromContext(c)
	if err := RecordAuditLog(c.Request.Context(), actorID, actorEmail, "risk_alert.isolate", "deployment", deploymentID, map[string]any{"alertId": alertID}, c.ClientIP()); err != nil {
		fmt.Printf("[WARN] failed to record audit log for risk_alert.isolate on %q: %v\n", deploymentID, err)
	}

	c.JSON(http.StatusOK, gin.H{"message": "service isolated and alert resolved"})
}
