package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jackc/pgx/v5"
)

const (
	defaultMaxCPU         = 2.00
	defaultMaxMemoryMB    = 1024
	defaultMaxApps        = 3
	defaultMaxStorageMB   = 2048
	defaultDeployCPU      = 0.50
	defaultDeployMemoryMB = 512
	defaultDeployApps     = 1
)

type QuotaRecord struct {
	UserID       string  `json:"userId"`
	MaxCPU       float64 `json:"maxCpu"`
	MaxMemoryMB  int64   `json:"maxMemoryMb"`
	MaxApps      int64   `json:"maxApps"`
	MaxStorageMB int64   `json:"maxStorageMb"`
}

type ResourceUsageRecord struct {
	UserID           string  `json:"userId"`
	CurrentCPU       float64 `json:"currentCpu"`
	CurrentMemoryMB  int64   `json:"currentMemoryMb"`
	CurrentApps      int64   `json:"currentApps"`
	CurrentStorageMB int64   `json:"currentStorageMb"`
}

type QuotaSummary struct {
	Quota     QuotaRecord         `json:"quota"`
	Usage     ResourceUsageRecord `json:"usage"`
	Available QuotaRecord         `json:"available"`
}

func (s *DeploymentStore) EnsureResourceAccounting(ctx context.Context, userID string) error {
	if s == nil || s.pool == nil {
		return fmt.Errorf("deployment store is not initialized")
	}

	userID = strings.TrimSpace(userID)
	if userID == "" {
		return fmt.Errorf("userID is required")
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin resource accounting transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `
INSERT INTO quotas (user_id, max_cpu, max_memory_mb, max_apps, max_storage_mb)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (user_id) DO NOTHING
`, userID, defaultMaxCPU, defaultMaxMemoryMB, defaultMaxApps, defaultMaxStorageMB); err != nil {
		return fmt.Errorf("ensure quotas row: %w", err)
	}
	if _, err := tx.Exec(ctx, `
INSERT INTO resource_usage (user_id, current_cpu, current_memory_mb, current_apps, current_storage_mb)
VALUES ($1, 0, 0, 0, 0)
ON CONFLICT (user_id) DO NOTHING
`, userID); err != nil {
		return fmt.Errorf("ensure resource usage row: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit resource accounting transaction: %w", err)
	}

	return nil
}

func (s *DeploymentStore) GetQuotaSummary(ctx context.Context, userID string) (QuotaSummary, error) {
	if err := s.EnsureResourceAccounting(ctx, userID); err != nil {
		return QuotaSummary{}, err
	}

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

	summary.Usage.UserID = userID
	summary.Quota.UserID = userID
	summary.Available = QuotaRecord{
		UserID:       userID,
		MaxCPU:       summary.Quota.MaxCPU - summary.Usage.CurrentCPU,
		MaxMemoryMB:  summary.Quota.MaxMemoryMB - summary.Usage.CurrentMemoryMB,
		MaxApps:      summary.Quota.MaxApps - summary.Usage.CurrentApps,
		MaxStorageMB: summary.Quota.MaxStorageMB - summary.Usage.CurrentStorageMB,
	}
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

	return summary, nil
}

func (s *DeploymentStore) ReserveDeploymentResources(ctx context.Context, userID string, cpu float64, memoryMB int64, apps int64, storageMB int64) error {
	if s == nil || s.pool == nil {
		return fmt.Errorf("deployment store is not initialized")
	}
	if err := s.EnsureResourceAccounting(ctx, userID); err != nil {
		return err
	}

	userID = strings.TrimSpace(userID)
	if userID == "" {
		return fmt.Errorf("userID is required")
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return fmt.Errorf("begin resource reservation transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

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

	if currentCPU+cpu > maxCPU || currentMemoryMB+memoryMB > maxMemoryMB || currentApps+apps > maxApps || currentStorageMB+storageMB > maxStorageMB {
		return fmt.Errorf("quota exceeded")
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

func (s *DeploymentStore) ReleaseDeploymentResources(ctx context.Context, userID string, cpu float64, memoryMB int64, apps int64, storageMB int64) error {
	if s == nil || s.pool == nil {
		return fmt.Errorf("deployment store is not initialized")
	}

	userID = strings.TrimSpace(userID)
	if userID == "" {
		return fmt.Errorf("userID is required")
	}

	_, err := s.pool.Exec(ctx, `
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

	return nil
}

func estimateStorageUsageMB(path string) (int64, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return 0, fmt.Errorf("path is required")
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
