package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// ============================================================================
// CONSTANTS
// ============================================================================

const (
	defaultMaxCPU         = 2.00
	defaultMaxMemoryMB    = 1024
	defaultMaxApps        = 3
	defaultMaxStorageMB   = 2048
	defaultDeployCPU      = 0.50
	defaultDeployMemoryMB = 512
	defaultDeployApps     = 1
	
	quotaCheckTimeout = 10 * time.Second
	quotaAlertThreshold = 0.8 // 80% usage triggers alert
)

// ============================================================================
// TYPES
// ============================================================================

type QuotaRecord struct {
	UserID       string    `json:"userId"`
	MaxCPU       float64   `json:"maxCpu"`
	MaxMemoryMB  int64     `json:"maxMemoryMb"`
	MaxApps      int64     `json:"maxApps"`
	MaxStorageMB int64     `json:"maxStorageMb"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

type ResourceUsageRecord struct {
	UserID           string    `json:"userId"`
	CurrentCPU       float64   `json:"currentCpu"`
	CurrentMemoryMB  int64     `json:"currentMemoryMb"`
	CurrentApps      int64     `json:"currentApps"`
	CurrentStorageMB int64     `json:"currentStorageMb"`
	UpdatedAt        time.Time `json:"updatedAt"`
}

type QuotaSummary struct {
	Quota     QuotaRecord         `json:"quota"`
	Usage     ResourceUsageRecord `json:"usage"`
	Available QuotaRecord         `json:"available"`
	UsagePercent struct {
		CPU     float64 `json:"cpu"`
		Memory  float64 `json:"memory"`
		Apps    float64 `json:"apps"`
		Storage float64 `json:"storage"`
	} `json:"usagePercent"`
	Alert bool `json:"alert"`
}

type QuotaHistory struct {
	ID          string    `json:"id"`
	UserID      string    `json:"userId"`
	Field       string    `json:"field"` // "max_cpu", "max_memory_mb", etc.
	OldValue    string    `json:"oldValue"`
	NewValue    string    `json:"newValue"`
	ChangedBy   string    `json:"changedBy"`
	CreatedAt   time.Time `json:"createdAt"`
}

type QuotaAlert struct {
	UserID      string    `json:"userId"`
	Resource    string    `json:"resource"` // "cpu", "memory", "apps", "storage"
	Current     float64   `json:"current"`
	Limit       float64   `json:"limit"`
	Percentage  float64   `json:"percentage"`
	CreatedAt   time.Time `json:"createdAt"`
}

type ResourceUpdate struct {
	CPU       float64
	MemoryMB  int64
	Apps      int64
	StorageMB int64
}

// ============================================================================
// QUOTA MANAGEMENT
// ============================================================================

func (s *DeploymentStore) UpdateQuota(
	ctx context.Context,
	userID string,
	maxCPU *float64,
	maxMemoryMB *int64,
	maxApps *int64,
	maxStorageMB *int64,
	changedBy string,
) (QuotaRecord, error) {
	if s == nil || s.pool == nil {
		return QuotaRecord{}, fmt.Errorf("deployment store is not initialized")
	}

	userID = strings.TrimSpace(userID)
	if userID == "" {
		return QuotaRecord{}, fmt.Errorf("userID is required")
	}

	ctx, cancel := context.WithTimeout(ctx, quotaCheckTimeout)
	defer cancel()

	// Get current quota
	current, err := s.GetQuota(ctx, userID)
	if err != nil {
		return QuotaRecord{}, err
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return QuotaRecord{}, fmt.Errorf("begin quota update transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Build update query
	updates := []string{}
	args := []interface{}{userID}
	argIndex := 2

	if maxCPU != nil && *maxCPU >= 0 {
		updates = append(updates, fmt.Sprintf("max_cpu = $%d", argIndex))
		args = append(args, *maxCPU)
		argIndex++
	}
	if maxMemoryMB != nil && *maxMemoryMB >= 0 {
		updates = append(updates, fmt.Sprintf("max_memory_mb = $%d", argIndex))
		args = append(args, *maxMemoryMB)
		argIndex++
	}
	if maxApps != nil && *maxApps >= 0 {
		updates = append(updates, fmt.Sprintf("max_apps = $%d", argIndex))
		args = append(args, *maxApps)
		argIndex++
	}
	if maxStorageMB != nil && *maxStorageMB >= 0 {
		updates = append(updates, fmt.Sprintf("max_storage_mb = $%d", argIndex))
		args = append(args, *maxStorageMB)
		argIndex++
	}

	if len(updates) == 0 {
		return QuotaRecord{}, fmt.Errorf("no fields to update")
	}

	updates = append(updates, "updated_at = now()")
	query := fmt.Sprintf(`
UPDATE quotas
SET %s
WHERE user_id = $1
RETURNING user_id::text, max_cpu, max_memory_mb, max_apps, max_storage_mb, created_at, updated_at
`, strings.Join(updates, ", "))

	var record QuotaRecord
	if err := tx.QueryRow(ctx, query, args...).Scan(
		&record.UserID,
		&record.MaxCPU,
		&record.MaxMemoryMB,
		&record.MaxApps,
		&record.MaxStorageMB,
		&record.CreatedAt,
		&record.UpdatedAt,
	); err != nil {
		return QuotaRecord{}, fmt.Errorf("update quota: %w", err)
	}

	// Record history
	if err := s.recordQuotaHistory(ctx, tx, userID, current, record, changedBy); err != nil {
		return QuotaRecord{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return QuotaRecord{}, fmt.Errorf("commit quota update: %w", err)
	}

	return record, nil
}

func (s *DeploymentStore) GetQuota(ctx context.Context, userID string) (QuotaRecord, error) {
	if s == nil || s.pool == nil {
		return QuotaRecord{}, fmt.Errorf("deployment store is not initialized")
	}

	if err := s.EnsureResourceAccounting(ctx, userID); err != nil {
		return QuotaRecord{}, err
	}

	var record QuotaRecord
	err := s.pool.QueryRow(ctx, `
SELECT user_id::text, max_cpu, max_memory_mb, max_apps, max_storage_mb, created_at, updated_at
FROM quotas
WHERE user_id = $1
`, userID).Scan(
		&record.UserID,
		&record.MaxCPU,
		&record.MaxMemoryMB,
		&record.MaxApps,
		&record.MaxStorageMB,
		&record.CreatedAt,
		&record.UpdatedAt,
	)
	if err != nil {
		return QuotaRecord{}, fmt.Errorf("get quota: %w", err)
	}
	return record, nil
}

// ============================================================================
// QUOTA HISTORY
// ============================================================================

func (s *DeploymentStore) recordQuotaHistory(
	ctx context.Context,
	tx pgx.Tx,
	userID string,
	old QuotaRecord,
	new QuotaRecord,
	changedBy string,
) error {
	changes := []struct {
		field    string
		oldValue string
		newValue string
	}{
		{"max_cpu", fmt.Sprintf("%f", old.MaxCPU), fmt.Sprintf("%f", new.MaxCPU)},
		{"max_memory_mb", fmt.Sprintf("%d", old.MaxMemoryMB), fmt.Sprintf("%d", new.MaxMemoryMB)},
		{"max_apps", fmt.Sprintf("%d", old.MaxApps), fmt.Sprintf("%d", new.MaxApps)},
		{"max_storage_mb", fmt.Sprintf("%d", old.MaxStorageMB), fmt.Sprintf("%d", new.MaxStorageMB)},
	}

	for _, change := range changes {
		if change.oldValue != change.newValue {
			_, err := tx.Exec(ctx, `
INSERT INTO quota_history (user_id, field, old_value, new_value, changed_by)
VALUES ($1, $2, $3, $4, $5)
`, userID, change.field, change.oldValue, change.newValue, changedBy)
			if err != nil {
				return fmt.Errorf("record quota history: %w", err)
			}
		}
	}

	return nil
}

func (s *DeploymentStore) GetQuotaHistory(
	ctx context.Context,
	userID string,
	limit int,
) ([]QuotaHistory, error) {
	if s == nil || s.pool == nil {
		return nil, fmt.Errorf("deployment store is not initialized")
	}

	if limit <= 0 || limit > 100 {
		limit = 50
	}

	rows, err := s.pool.Query(ctx, `
SELECT id::text, user_id::text, field, old_value, new_value, changed_by::text, created_at
FROM quota_history
WHERE user_id = $1
ORDER BY created_at DESC
LIMIT $2
`, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("get quota history: %w", err)
	}
	defer rows.Close()

	var history []QuotaHistory
	for rows.Next() {
		var record QuotaHistory
		if err := rows.Scan(
			&record.ID,
			&record.UserID,
			&record.Field,
			&record.OldValue,
			&record.NewValue,
			&record.ChangedBy,
			&record.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan quota history: %w", err)
		}
		history = append(history, record)
	}
	return history, nil
}

// ============================================================================
// QUOTA SUMMARY
// ============================================================================

func (s *DeploymentStore) GetQuotaSummary(ctx context.Context, userID string) (QuotaSummary, error) {
	if err := s.EnsureResourceAccounting(ctx, userID); err != nil {
		return QuotaSummary{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, quotaCheckTimeout)
	defer cancel()

	var summary QuotaSummary
	if err := s.pool.QueryRow(ctx, `
SELECT
	q.user_id::text,
	q.max_cpu,
	q.max_memory_mb,
	q.max_apps,
	q.max_storage_mb,
	ru.current_cpu,
	ru.current_memory_mb,
	ru.current_apps,
	ru.current_storage_mb
FROM quotas q
JOIN resource_usage ru ON ru.user_id = q.user_id
WHERE q.user_id = $1
`, userID).Scan(
		&summary.Quota.UserID,
		&summary.Quota.MaxCPU,
		&summary.Quota.MaxMemoryMB,
		&summary.Quota.MaxApps,
		&summary.Quota.MaxStorageMB,
		&summary.Usage.CurrentCPU,
		&summary.Usage.CurrentMemoryMB,
		&summary.Usage.CurrentApps,
		&summary.Usage.CurrentStorageMB,
	); err != nil {
		return QuotaSummary{}, fmt.Errorf("get quota summary: %w", err)
	}

	// Set timestamps
	summary.Quota.CreatedAt = time.Now()
	summary.Quota.UpdatedAt = time.Now()
	summary.Usage.UpdatedAt = time.Now()

	summary.Usage.UserID = userID
	summary.Quota.UserID = userID

	// Calculate available resources
	summary.Available = QuotaRecord{
		UserID:       userID,
		MaxCPU:       summary.Quota.MaxCPU - summary.Usage.CurrentCPU,
		MaxMemoryMB:  summary.Quota.MaxMemoryMB - summary.Usage.CurrentMemoryMB,
		MaxApps:      summary.Quota.MaxApps - summary.Usage.CurrentApps,
		MaxStorageMB: summary.Quota.MaxStorageMB - summary.Usage.CurrentStorageMB,
	}

	// Ensure non-negative
	if summary.Available.MaxCPU < 0 {
		summary.Available.MaxCPU = 0
	}
	if summary.Available.MaxMemoryMB < 0 {
		summary.Available.MaxMemoryMB = 0
	}
	if summary.Available.MaxApps < 0 {
		summary.Available.MaxApps = 0
	}
	if summary.Available.MaxStorageMB < 0 {
		summary.Available.MaxStorageMB = 0
	}

	// Calculate usage percentages
	summary.UsagePercent.CPU = 0
	if summary.Quota.MaxCPU > 0 {
		summary.UsagePercent.CPU = (summary.Usage.CurrentCPU / summary.Quota.MaxCPU) * 100
	}
	summary.UsagePercent.Memory = 0
	if summary.Quota.MaxMemoryMB > 0 {
		summary.UsagePercent.Memory = (float64(summary.Usage.CurrentMemoryMB) / float64(summary.Quota.MaxMemoryMB)) * 100
	}
	summary.UsagePercent.Apps = 0
	if summary.Quota.MaxApps > 0 {
		summary.UsagePercent.Apps = (float64(summary.Usage.CurrentApps) / float64(summary.Quota.MaxApps)) * 100
	}
	summary.UsagePercent.Storage = 0
	if summary.Quota.MaxStorageMB > 0 {
		summary.UsagePercent.Storage = (float64(summary.Usage.CurrentStorageMB) / float64(summary.Quota.MaxStorageMB)) * 100
	}

	// Check if any resource exceeds threshold
	summary.Alert = summary.UsagePercent.CPU > quotaAlertThreshold*100 ||
		summary.UsagePercent.Memory > quotaAlertThreshold*100 ||
		summary.UsagePercent.Apps > quotaAlertThreshold*100 ||
		summary.UsagePercent.Storage > quotaAlertThreshold*100

	// Record alert if needed
	if summary.Alert {
		s.recordQuotaAlert(ctx, userID, summary)
	}

	return summary, nil
}

// ============================================================================
// QUOTA ALERTS
// ============================================================================

func (s *DeploymentStore) recordQuotaAlert(ctx context.Context, userID string, summary QuotaSummary) {
	alerts := []struct {
		resource    string
		current     float64
		limit       float64
		percentage  float64
	}{
		{"cpu", summary.Usage.CurrentCPU, summary.Quota.MaxCPU, summary.UsagePercent.CPU},
		{"memory", float64(summary.Usage.CurrentMemoryMB), float64(summary.Quota.MaxMemoryMB), summary.UsagePercent.Memory},
		{"apps", float64(summary.Usage.CurrentApps), float64(summary.Quota.MaxApps), summary.UsagePercent.Apps},
		{"storage", float64(summary.Usage.CurrentStorageMB), float64(summary.Quota.MaxStorageMB), summary.UsagePercent.Storage},
	}

	for _, alert := range alerts {
		if alert.percentage > quotaAlertThreshold*100 {
			_, _ = s.pool.Exec(ctx, `
INSERT INTO quota_alerts (user_id, resource, current, limit, percentage)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (user_id, resource) DO UPDATE SET
	current = EXCLUDED.current,
	percentage = EXCLUDED.percentage,
	created_at = now()
`, userID, alert.resource, alert.current, alert.limit, alert.percentage)
		}
	}
}

func (s *DeploymentStore) GetQuotaAlerts(ctx context.Context, userID string) ([]QuotaAlert, error) {
	if s == nil || s.pool == nil {
		return nil, fmt.Errorf("deployment store is not initialized")
	}

	rows, err := s.pool.Query(ctx, `
SELECT user_id::text, resource, current, "limit", percentage, created_at
FROM quota_alerts
WHERE user_id = $1
`, userID)
	if err != nil {
		return nil, fmt.Errorf("get quota alerts: %w", err)
	}
	defer rows.Close()

	var alerts []QuotaAlert
	for rows.Next() {
		var alert QuotaAlert
		if err := rows.Scan(
			&alert.UserID,
			&alert.Resource,
			&alert.Current,
			&alert.Limit,
			&alert.Percentage,
			&alert.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan quota alert: %w", err)
		}
		alerts = append(alerts, alert)
	}
	return alerts, nil
}

// ============================================================================
// RESOURCE RESERVATION
// ============================================================================

func (s *DeploymentStore) ReserveDeploymentResources(
	ctx context.Context,
	userID string,
	cpu float64,
	memoryMB int64,
	apps int64,
	storageMB int64,
) error {
	if s == nil || s.pool == nil {
		return fmt.Errorf("deployment store is not initialized")
	}

	// Validate resources
	if err := validateResources(cpu, memoryMB, apps, storageMB); err != nil {
		return err
	}

	if err := s.EnsureResourceAccounting(ctx, userID); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, quotaCheckTimeout)
	defer cancel()

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin resource reservation transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Lock rows
	if _, err := tx.Exec(ctx, `SELECT 1 FROM quotas WHERE user_id = $1 FOR UPDATE`, userID); err != nil {
		return fmt.Errorf("lock quota row: %w", err)
	}
	if _, err := tx.Exec(ctx, `SELECT 1 FROM resource_usage WHERE user_id = $1 FOR UPDATE`, userID); err != nil {
		return fmt.Errorf("lock usage row: %w", err)
	}

	var maxCPU float64
	var maxMemoryMB int64
	var maxApps int64
	var maxStorageMB int64
	var currentCPU float64
	var currentMemoryMB int64
	var currentApps int64
	var currentStorageMB int64
	if err := tx.QueryRow(ctx, `
SELECT q.max_cpu, q.max_memory_mb, q.max_apps, q.max_storage_mb,
	ru.current_cpu, ru.current_memory_mb, ru.current_apps, ru.current_storage_mb
FROM quotas q
JOIN resource_usage ru ON ru.user_id = q.user_id
WHERE q.user_id = $1
`, userID).Scan(&maxCPU, &maxMemoryMB, &maxApps, &maxStorageMB, &currentCPU, &currentMemoryMB, &currentApps, &currentStorageMB); err != nil {
		return fmt.Errorf("load quota summary for reservation: %w", err)
	}

	// Check quotas
	if currentCPU+cpu > maxCPU {
		return fmt.Errorf("quota exceeded: CPU (current: %f, requested: %f, max: %f)", currentCPU, cpu, maxCPU)
	}
	if currentMemoryMB+memoryMB > maxMemoryMB {
		return fmt.Errorf("quota exceeded: Memory (current: %d, requested: %d, max: %d)", currentMemoryMB, memoryMB, maxMemoryMB)
	}
	if currentApps+apps > maxApps {
		return fmt.Errorf("quota exceeded: Apps (current: %d, requested: %d, max: %d)", currentApps, apps, maxApps)
	}
	if currentStorageMB+storageMB > maxStorageMB {
		return fmt.Errorf("quota exceeded: Storage (current: %d, requested: %d, max: %d)", currentStorageMB, storageMB, maxStorageMB)
	}

	if _, err := tx.Exec(ctx, `
UPDATE resource_usage
SET current_cpu = current_cpu + $2,
	current_memory_mb = current_memory_mb + $3,
	current_apps = current_apps + $4,
	current_storage_mb = current_storage_mb + $5,
	updated_at = now()
WHERE user_id = $1
`, userID, cpu, memoryMB, apps, storageMB); err != nil {
		return fmt.Errorf("reserve resource usage: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit resource reservation: %w", err)
	}

	return nil
}

// ============================================================================
// RESOURCE RELEASE
// ============================================================================

func (s *DeploymentStore) ReleaseDeploymentResources(
	ctx context.Context,
	userID string,
	cpu float64,
	memoryMB int64,
	apps int64,
	storageMB int64,
) error {
	if s == nil || s.pool == nil {
		return fmt.Errorf("deployment store is not initialized")
	}

	if err := validateResources(cpu, memoryMB, apps, storageMB); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, quotaCheckTimeout)
	defer cancel()

	result, err := s.pool.Exec(ctx, `
UPDATE resource_usage
SET current_cpu = GREATEST(current_cpu - $2, 0),
	current_memory_mb = GREATEST(current_memory_mb - $3, 0),
	current_apps = GREATEST(current_apps - $4, 0),
	current_storage_mb = GREATEST(current_storage_mb - $5, 0),
	updated_at = now()
WHERE user_id = $1
`, userID, cpu, memoryMB, apps, storageMB)
	if err != nil {
		return fmt.Errorf("release resource usage: %w", err)
	}

	if result.RowsAffected() == 0 {
		return fmt.Errorf("user not found: %s", userID)
	}

	return nil
}

// ============================================================================
// BULK OPERATIONS
// ============================================================================

func (s *DeploymentStore) BulkReserveResources(
	ctx context.Context,
	userID string,
	updates []ResourceUpdate,
) error {
	if s == nil || s.pool == nil {
		return fmt.Errorf("deployment store is not initialized")
	}

	if err := s.EnsureResourceAccounting(ctx, userID); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(ctx, quotaCheckTimeout)
	defer cancel()

	total := ResourceUpdate{}
	for _, u := range updates {
		total.CPU += u.CPU
		total.MemoryMB += u.MemoryMB
		total.Apps += u.Apps
		total.StorageMB += u.StorageMB
	}

	return s.ReserveDeploymentResources(ctx, userID, total.CPU, total.MemoryMB, total.Apps, total.StorageMB)
}

func (s *DeploymentStore) BulkReleaseResources(
	ctx context.Context,
	userID string,
	updates []ResourceUpdate,
) error {
	if s == nil || s.pool == nil {
		return fmt.Errorf("deployment store is not initialized")
	}

	total := ResourceUpdate{}
	for _, u := range updates {
		total.CPU += u.CPU
		total.MemoryMB += u.MemoryMB
		total.Apps += u.Apps
		total.StorageMB += u.StorageMB
	}

	return s.ReleaseDeploymentResources(ctx, userID, total.CPU, total.MemoryMB, total.Apps, total.StorageMB)
}

// ============================================================================
// QUOTA RESET
// ============================================================================

func (s *DeploymentStore) ResetQuota(ctx context.Context, userID string, changedBy string) error {
	if s == nil || s.pool == nil {
		return fmt.Errorf("deployment store is not initialized")
	}

	ctx, cancel := context.WithTimeout(ctx, quotaCheckTimeout)
	defer cancel()

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin quota reset transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Reset resource usage to zero
	if _, err := tx.Exec(ctx, `
UPDATE resource_usage
SET current_cpu = 0,
	current_memory_mb = 0,
	current_apps = 0,
	current_storage_mb = 0,
	updated_at = now()
WHERE user_id = $1
`, userID); err != nil {
		return fmt.Errorf("reset resource usage: %w", err)
	}

	// Record reset in history
	_, err = tx.Exec(ctx, `
INSERT INTO quota_history (user_id, field, old_value, new_value, changed_by)
VALUES ($1, 'resource_reset', 'used', 'reset', $2)
`, userID, changedBy)
	if err != nil {
		return fmt.Errorf("record quota reset: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit quota reset: %w", err)
	}

	return nil
}

// ============================================================================
// VALIDATION HELPERS
// ============================================================================

func validateResources(cpu float64, memoryMB int64, apps int64, storageMB int64) error {
	if cpu < 0 {
		return fmt.Errorf("CPU cannot be negative: %f", cpu)
	}
	if memoryMB < 0 {
		return fmt.Errorf("memoryMB cannot be negative: %d", memoryMB)
	}
	if apps < 0 {
		return fmt.Errorf("apps cannot be negative: %d", apps)
	}
	if storageMB < 0 {
		return fmt.Errorf("storageMB cannot be negative: %d", storageMB)
	}
	return nil
}

// ============================================================================
// STORAGE ESTIMATION
// ============================================================================

func estimateStorageUsageMB(path string) (int64, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return 0, fmt.Errorf("path is required")
	}

	if isRemoteRepoSource(path) {
		// Remote URLs are cloned before quota checks; assume a modest checkout size.
		return 256, nil
	}

	absPath, err := filepath.Abs(path)
	if err != nil {
		return 0, fmt.Errorf("resolve path: %w", err)
	}

	var totalBytes int64
	err = filepath.Walk(absPath, func(_ string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			return nil
		}
		totalBytes += info.Size()
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("walk path size: %w", err)
	}

	if totalBytes == 0 {
		return 1, nil
	}

	const bytesPerMB = 1024 * 1024
	return (totalBytes + bytesPerMB - 1) / bytesPerMB, nil
}

// ============================================================================
// DATABASE SCHEMA
// ============================================================================

func (s *DeploymentStore) CreateQuotaTablesIfNotExists(ctx context.Context) error {
	if s == nil || s.pool == nil {
		return fmt.Errorf("deployment store is not initialized")
	}

	// Create quotas table
	_, err := s.pool.Exec(ctx, `
CREATE TABLE IF NOT EXISTS quotas (
	user_id UUID PRIMARY KEY,
	max_cpu FLOAT NOT NULL DEFAULT 2.0,
	max_memory_mb BIGINT NOT NULL DEFAULT 1024,
	max_apps BIGINT NOT NULL DEFAULT 3,
	max_storage_mb BIGINT NOT NULL DEFAULT 2048,
	created_at TIMESTAMP WITH TIME ZONE DEFAULT now(),
	updated_at TIMESTAMP WITH TIME ZONE DEFAULT now()
)`)
	if err != nil {
		return err
	}

	// Create resource_usage table
	_, err = s.pool.Exec(ctx, `
CREATE TABLE IF NOT EXISTS resource_usage (
	user_id UUID PRIMARY KEY,
	current_cpu FLOAT NOT NULL DEFAULT 0,
	current_memory_mb BIGINT NOT NULL DEFAULT 0,
	current_apps BIGINT NOT NULL DEFAULT 0,
	current_storage_mb BIGINT NOT NULL DEFAULT 0,
	created_at TIMESTAMP WITH TIME ZONE DEFAULT now(),
	updated_at TIMESTAMP WITH TIME ZONE DEFAULT now()
)`)
	if err != nil {
		return err
	}

	// Create quota_history table
	_, err = s.pool.Exec(ctx, `
CREATE TABLE IF NOT EXISTS quota_history (
	id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	user_id UUID NOT NULL REFERENCES quotas(user_id) ON DELETE CASCADE,
	field TEXT NOT NULL,
	old_value TEXT,
	new_value TEXT,
	changed_by UUID NOT NULL REFERENCES users(id),
	created_at TIMESTAMP WITH TIME ZONE DEFAULT now()
)`)
	if err != nil {
		return err
	}

	// Create quota_alerts table
	_, err = s.pool.Exec(ctx, `
CREATE TABLE IF NOT EXISTS quota_alerts (
	id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	user_id UUID NOT NULL REFERENCES quotas(user_id) ON DELETE CASCADE,
	resource TEXT NOT NULL,
	current FLOAT NOT NULL,
	"limit" FLOAT NOT NULL,
	percentage FLOAT NOT NULL,
	created_at TIMESTAMP WITH TIME ZONE DEFAULT now(),
	UNIQUE(user_id, resource)
)`)
	if err != nil {
		return err
	}

	// Create indexes
	_, err = s.pool.Exec(ctx, `
CREATE INDEX IF NOT EXISTS idx_quota_history_user_id ON quota_history(user_id);
CREATE INDEX IF NOT EXISTS idx_quota_history_created_at ON quota_history(created_at);
CREATE INDEX IF NOT EXISTS idx_quota_alerts_user_id ON quota_alerts(user_id);
CREATE INDEX IF NOT EXISTS idx_quota_alerts_created_at ON quota_alerts(created_at);
`)
	if err != nil {
		return err
	}

	return nil
}

// ============================================================================
// USAGE EXAMPLES
// ============================================================================

/*
EXAMPLE USAGE:

1. Ensure resource accounting:
   err := store.EnsureResourceAccounting(ctx, "user-123")

2. Get quota summary:
   summary, err := store.GetQuotaSummary(ctx, "user-123")
   fmt.Printf("CPU Usage: %.2f%%\n", summary.UsagePercent.CPU)
   fmt.Printf("Alert: %v\n", summary.Alert)

3. Update quota:
   newCPU := 4.0
   record, err := store.UpdateQuota(ctx, "user-123", &newCPU, nil, nil, nil, "admin-456")
   // Record has max_cpu = 4.0

4. Get quota history:
   history, err := store.GetQuotaHistory(ctx, "user-123", 20)

5. Reserve resources:
   err := store.ReserveDeploymentResources(ctx, "user-123", 0.5, 512, 1, 256)

6. Release resources:
   err := store.ReleaseDeploymentResources(ctx, "user-123", 0.5, 512, 1, 256)

7. Bulk operations:
   updates := []ResourceUpdate{
	   {CPU: 0.5, MemoryMB: 512, Apps: 1, StorageMB: 256},
	   {CPU: 1.0, MemoryMB: 1024, Apps: 1, StorageMB: 512},
   }
   err := store.BulkReserveResources(ctx, "user-123", updates)

8. Reset quota:
   err := store.ResetQuota(ctx, "user-123", "admin-456")

9. Get quota alerts:
   alerts, err := store.GetQuotaAlerts(ctx, "user-123")
   for _, alert := range alerts {
	   fmt.Printf("%s usage: %.2f%%\n", alert.Resource, alert.Percentage)
   }
*/