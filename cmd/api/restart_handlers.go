package main

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// ============================================================================
// CONSTANTS
// ============================================================================

const (
	minRestartInterval     = 30 * time.Second
	maxRestartRate         = 5  // max restarts per hour
	restartCooldownPeriod  = 60 * time.Second
	defaultRestartTimeout  = 5 * time.Minute
)

// ============================================================================
// TYPES
// ============================================================================

type RestartRequest struct {
	Reason       string `json:"reason,omitempty"`
	Force        bool   `json:"force,omitempty"`
	WaitForReady bool   `json:"waitForReady,omitempty"`
}

type RestartResponse struct {
	Message       string        `json:"message"`
	DeploymentID  string        `json:"deploymentId"`
	JobID         string        `json:"jobId"`
	Status        string        `json:"status"`
	RequestedAt   time.Time     `json:"requestedAt"`
	EstimatedWait time.Duration `json:"estimatedWait,omitempty"`
}

type RestartCooldown struct {
	LastRestartAt time.Time     `json:"lastRestartAt"`
	NextAllowedAt time.Time     `json:"nextAllowedAt"`
	Remaining     time.Duration `json:"remaining"`
	Allowed       bool          `json:"allowed"`
}

// ============================================================================
// RESTART HANDLER
// ============================================================================

// restartAppHandler re-runs a deployment so env edits take effect. Env vars are
// injected at container creation, so applying them requires recreating the
// container. We route through the deployment worker (same path as deploy)
// rather than calling RestartContainer directly: the deploy flow never persists
// image_name, so a direct recreate would hit the "imageName is required" guard.
// The worker rebuilds from source when needed, stops the old container, and
// starts a fresh one with the current environment.
func restartAppHandler(c *gin.Context) {
	user, deployment, ok := currentUserDeployment(c)
	if !ok {
		return
	}

	// Parse request
	var req RestartRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		// Allow empty body (defaults)
		req = RestartRequest{}
	}

	// Validate deployment status
	if err := validateDeploymentForRestart(deployment); err != nil {
		if req.Force {
			// Log warning but proceed with force
			fmt.Printf("[WARN] Force restart requested despite validation: %v\n", err)
		} else {
			c.JSON(http.StatusBadRequest, gin.H{
				"error":   "invalid_deployment_state",
				"details": err.Error(),
			})
			return
		}
	}

	// Check restart cooldown
	cooldown, err := checkRestartCooldown(c.Request.Context(), deployment.DeploymentID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":   "failed_to_check_cooldown",
			"details": err.Error(),
		})
		return
	}

	if !cooldown.Allowed && !req.Force {
		c.JSON(http.StatusTooManyRequests, gin.H{
			"error":        "restart_rate_limit_exceeded",
			"message":      "Please wait before requesting another restart",
			"cooldown":     cooldown,
			"retry_after":  int(cooldown.Remaining.Seconds()),
		})
		return
	}

	// Check if there's already a pending restart
	pending, err := hasPendingRestart(c.Request.Context(), deployment.DeploymentID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":   "failed_to_check_pending_restart",
			"details": err.Error(),
		})
		return
	}
	if pending && !req.Force {
		c.JSON(http.StatusConflict, gin.H{
			"error":   "restart_already_pending",
			"message": "A restart is already in progress for this deployment",
		})
		return
	}

	// Check resource availability
	if err := checkResourceAvailability(c.Request.Context(), user.ID, deployment); err != nil {
		if req.Force {
			fmt.Printf("[WARN] Force restart requested despite resource check: %v\n", err)
		} else {
			c.JSON(http.StatusUnprocessableEntity, gin.H{
				"error":   "insufficient_resources",
				"details": err.Error(),
			})
			return
		}
	}

	// Record restart reason
	reason := req.Reason
	if reason == "" {
		reason = "manual restart via API"
	}

	// Enqueue restart job
	jobID, err := deploymentJobs.EnqueueDeployment(
		c.Request.Context(),
		user.ID,
		deployment.DeploymentID,
		false, // RebuildImage = false (reuse existing image)
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":   "failed_to_restart_service",
			"details": err.Error(),
		})
		return
	}

	// Record audit log (with reason)
	if err := deploymentStore.RecordDeploymentRestartAudit(
		c.Request.Context(),
		deployment.DeploymentID,
		"manual_restart",
		"queued",
		fmt.Sprintf("%s (reason: %s)", "manual restart via api", reason),
		deployment.ContainerID,
		"",
	); err != nil {
		fmt.Printf("[WARN] Failed to record restart audit: %v\n", err)
	}

	// Update last restart time
	if err := updateLastRestartTime(c.Request.Context(), deployment.DeploymentID); err != nil {
		fmt.Printf("[WARN] Failed to update restart time: %v\n", err)
	}

	// Send WebSocket notification
	go notifyRestartRequested(deployment.DeploymentID, user.ID, jobID, reason)

	// Prepare response
	response := RestartResponse{
		Message:      "service restart queued",
		DeploymentID: deployment.DeploymentID,
		JobID:        jobID,
		Status:       "queued",
		RequestedAt:  time.Now(),
	}

	// Estimate wait time
	queueLength, err := getQueueLength()
	if err == nil {
		response.EstimatedWait = time.Duration(queueLength*30) * time.Second
	}

	c.JSON(http.StatusAccepted, response)
}

// ============================================================================
// VALIDATION FUNCTIONS
// ============================================================================

func validateDeploymentForRestart(deployment DeploymentRecord) error {
	// Check if deployment exists and has valid status
	switch deployment.Status {
	case string(DeploymentStatusBuilding):
		return fmt.Errorf("deployment is currently building, cannot restart")
	case string(DeploymentStatusFailed):
		if deployment.ContainerID == "" {
			return fmt.Errorf("deployment failed and has no running container")
		}
		// Can restart failed deployments if they have a container
		return nil
	case string(DeploymentStatusRunning):
		if deployment.ContainerID == "" {
			return fmt.Errorf("deployment is running but has no container ID")
		}
		return nil
	case string(DeploymentStatusDeployed):
		if deployment.ContainerID == "" {
			return fmt.Errorf("deployment is deployed but has no container ID")
		}
		return nil
	case string(DeploymentStatusStopped):
		return fmt.Errorf("deployment is stopped, use deploy instead")
	case string(DeploymentStatusPaused):
		return fmt.Errorf("deployment is paused, resume before restart")
	default:
		return fmt.Errorf("deployment in unknown state: %s", deployment.Status)
	}
}

func checkRestartCooldown(ctx context.Context, deploymentID string) (RestartCooldown, error) {
	// Get last restart time from Redis or database
	var lastRestart time.Time

	// Check if we have a last restart record
	if deploymentStore != nil {
		// Query last restart audit
		history, err := deploymentStore.GetRestartHistory(ctx, deploymentID, 1)
		if err == nil && len(history) > 0 {
			lastRestart = history[0].CreatedAt
		}
	}

	cooldown := RestartCooldown{
		LastRestartAt: lastRestart,
		NextAllowedAt: lastRestart.Add(restartCooldownPeriod),
		Allowed:       true,
	}

	if !lastRestart.IsZero() {
		cooldown.Remaining = cooldown.NextAllowedAt.Sub(time.Now())
		cooldown.Allowed = time.Now().After(cooldown.NextAllowedAt)
	}

	// Also check rate limit (max restarts per hour)
	if cooldown.Allowed {
		restartsLastHour, err := deploymentStore.CountRestartsInWindow(ctx, deploymentID, time.Hour)
		if err == nil && restartsLastHour >= maxRestartRate {
			cooldown.Allowed = false
			cooldown.Remaining = time.Hour - time.Since(lastRestart)
		}
	}

	return cooldown, nil
}

func hasPendingRestart(ctx context.Context, deploymentID string) (bool, error) {
	// Check if there's a pending job for this deployment
	if deploymentJobs == nil {
		return false, nil
	}

	// Check job status for pending jobs
	// This would query Redis for active jobs
	pendingJobs, err := deploymentJobs.ListPendingJobs(ctx, deploymentID)
	if err != nil {
		return false, err
	}

	return len(pendingJobs) > 0, nil
}

func checkResourceAvailability(ctx context.Context, userID string, deployment DeploymentRecord) error {
	// Check if there are enough resources to restart
	if deploymentStore == nil {
		return nil
	}

	// Get quota summary
	summary, err := deploymentStore.GetQuotaSummary(ctx, userID)
	if err != nil {
		return err
	}

	// Check if we have enough resources
	neededCPU := defaultDeployCPU
	neededMemory := defaultDeployMemoryMB

	if summary.Available.MaxCPU < neededCPU {
		return fmt.Errorf("insufficient CPU: available %.2f, needed %.2f", summary.Available.MaxCPU, neededCPU)
	}
	if summary.Available.MaxMemoryMB < int64(neededMemory) {
		return fmt.Errorf("insufficient memory: available %d MB, needed %d MB", summary.Available.MaxMemoryMB, neededMemory)
	}

	return nil
}

// ============================================================================
// DATABASE OPERATIONS
// ============================================================================

func updateLastRestartTime(ctx context.Context, deploymentID string) error {
	if deploymentStore == nil {
		return nil
	}

	// Store last restart time in deployment record or separate table
	// This is a simplified example - adjust based on your schema
	_, err := deploymentStore.UpdateDeploymentLastRestart(ctx, deploymentID)
	return err
}

func getQueueLength() (int, error) {
	if deploymentJobs == nil {
		return 0, nil
	}
	return deploymentJobs.GetQueueLength(context.Background())
}

// ============================================================================
// NOTIFICATIONS
// ============================================================================

func notifyRestartRequested(deploymentID string, userID string, jobID string, reason string) {
	// Send WebSocket notification
	event := map[string]interface{}{
		"type":         "restart_requested",
		"deploymentId": deploymentID,
		"userId":       userID,
		"jobId":        jobID,
		"reason":       reason,
		"timestamp":    time.Now().UTC(),
	}

	// Publish to Redis channel
	if deploymentHealthManager != nil && deploymentHealthManager.redisClient != nil {
		channel := deploymentHealthChannel(deploymentID)
		_ = deploymentHealthManager.redisClient.Publish(context.Background(), channel, event)
	}
}

// ============================================================================
// STORE EXTENSIONS
// ============================================================================

// Add these methods to your DeploymentStore

func (s *DeploymentStore) GetRestartHistory(ctx context.Context, deploymentID string, limit int) ([]DeploymentRestartAuditRecord, error) {
	if s == nil || s.pool == nil {
		return nil, fmt.Errorf("deployment store is not initialized")
	}

	rows, err := s.pool.Query(ctx, `
SELECT id::text, deployment_id::text, action, outcome, reason, previous_container_id, new_container_id, created_at
FROM deployment_restart_audits
WHERE deployment_id = $1 AND action = 'manual_restart'
ORDER BY created_at DESC
LIMIT $2
`, deploymentID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var history []DeploymentRestartAuditRecord
	for rows.Next() {
		var record DeploymentRestartAuditRecord
		if err := rows.Scan(
			&record.ID,
			&record.DeploymentID,
			&record.Action,
			&record.Outcome,
			&record.Reason,
			&record.PreviousContainerID,
			&record.NewContainerID,
			&record.CreatedAt,
		); err != nil {
			return nil, err
		}
		history = append(history, record)
	}
	return history, nil
}

func (s *DeploymentStore) CountRestartsInWindow(ctx context.Context, deploymentID string, window time.Duration) (int, error) {
	if s == nil || s.pool == nil {
		return 0, fmt.Errorf("deployment store is not initialized")
	}

	var count int
	err := s.pool.QueryRow(ctx, `
SELECT COUNT(*)
FROM deployment_restart_audits
WHERE deployment_id = $1
  AND action = 'manual_restart'
  AND created_at >= now() - $2
`, deploymentID, window).Scan(&count)
	return count, err
}

func (s *DeploymentStore) UpdateDeploymentLastRestart(ctx context.Context, deploymentID string) (bool, error) {
	if s == nil || s.pool == nil {
		return false, fmt.Errorf("deployment store is not initialized")
	}

	result, err := s.pool.Exec(ctx, `
UPDATE deployments
SET last_restart_at = now()
WHERE id = $1
`, deploymentID)
	if err != nil {
		return false, err
	}
	return result.RowsAffected() > 0, nil
}

// ============================================================================
// DEPLOYMENT JOB MANAGER EXTENSIONS
// ============================================================================

func (m *DeploymentJobManager) ListPendingJobs(ctx context.Context, deploymentID string) ([]string, error) {
	if m == nil || m.redisClient == nil {
		return nil, nil
	}

	// Get pending jobs from Redis
	// This is a simplified implementation
	var jobs []string
	
	// Scan for jobs with this deployment ID
	pattern := fmt.Sprintf("deployment:job:*")
	iter := m.redisClient.Scan(ctx, 0, pattern, 0).Iterator()
	
	for iter.Next(ctx) {
		_ = iter.Val()
		// Check if this job is for this deployment
		// This would need to parse the job status
		// Simplified for example
	}
	
	return jobs, iter.Err()
}

func (m *DeploymentJobManager) GetQueueLength(ctx context.Context) (int, error) {
	if m == nil || m.redisClient == nil {
		return 0, nil
	}
	
	// Get queue length from Redis
	length, err := m.redisClient.LLen(ctx, "deployments").Result()
	return int(length), err
}

// ============================================================================
// ROUTE SETUP
// ============================================================================

// Add to your existing route setup:
// api.POST("/apps/:id/restart", AuthMiddleware(false), restartAppHandler)

// ============================================================================
// USAGE EXAMPLES
// ============================================================================

/*
EXAMPLE USAGE:

1. Basic restart:
   POST /api/apps/550e8400/restart
   Authorization: Bearer <token>
   {}

2. Restart with reason:
   POST /api/apps/550e8400/restart
   Authorization: Bearer <token>
   {
     "reason": "configuration update applied"
   }

3. Force restart (bypass cooldown):
   POST /api/apps/550e8400/restart
   Authorization: Bearer <token>
   {
     "force": true,
     "reason": "emergency reset"
   }

4. Response:
   {
     "message": "service restart queued",
     "deploymentId": "550e8400",
     "jobId": "job-123",
     "status": "queued",
     "requestedAt": "2024-01-15T10:00:00Z",
     "estimatedWait": "30s"
   }

5. Rate limit response:
   {
     "error": "restart_rate_limit_exceeded",
     "message": "Please wait before requesting another restart",
     "cooldown": {
       "lastRestartAt": "2024-01-15T09:59:00Z",
       "nextAllowedAt": "2024-01-15T10:01:00Z",
       "remaining": "60s",
       "allowed": false
     },
     "retry_after": 60
   }
*/