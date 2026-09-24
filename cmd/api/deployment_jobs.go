package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
)

// ============================================================================
// CONSTANTS - REMOVED (now in resources.go)
// ============================================================================

// Note: defaultDeployCPU, defaultDeployMemoryMB, defaultDeployApps
// are now in resources.go - DO NOT redeclare here

// ============================================================================
// TYPES
// ============================================================================

type JobPriority int

const (
	PriorityLow    JobPriority = 10
	PriorityNormal JobPriority = 50
	PriorityHigh   JobPriority = 100
	PriorityUrgent JobPriority = 200
)

type JobStatus string

const (
	JobStatusQueued     JobStatus = "queued"
	JobStatusActive     JobStatus = "active"
	JobStatusCompleted  JobStatus = "completed"
	JobStatusFailed     JobStatus = "failed"
	JobStatusCancelled  JobStatus = "cancelled"
	JobStatusPaused     JobStatus = "paused"
	JobStatusScheduled  JobStatus = "scheduled"
)

type DeploymentJobPayload struct {
	DeploymentID   string            `json:"deploymentId"`
	UserID         string            `json:"userId"`
	RebuildImage   bool              `json:"rebuildImage"`
	Priority       JobPriority       `json:"priority,omitempty"`
	Timeout        time.Duration     `json:"timeout,omitempty"`
	EnvOverrides   map[string]string `json:"envOverrides,omitempty"`
	Labels         map[string]string `json:"labels,omitempty"`
}

type DeploymentJobStatus struct {
	JobID        string      `json:"jobId"`
	DeploymentID string      `json:"deploymentId"`
	UserID       string      `json:"userId"`
	Status       JobStatus   `json:"status"`
	Stage        string      `json:"stage"`
	Message      string      `json:"message"`
	Progress     int         `json:"progress"`
	Error        string      `json:"error,omitempty"`
	CreatedAt    time.Time   `json:"createdAt"`
	UpdatedAt    time.Time   `json:"updatedAt"`
	StartedAt    *time.Time  `json:"startedAt,omitempty"`
	CompletedAt  *time.Time  `json:"completedAt,omitempty"`
	Priority     JobPriority `json:"priority,omitempty"`
	Duration     string      `json:"duration,omitempty"`
}

type JobConfig struct {
	Timeout          time.Duration
	MaxRetries       int
	RetryDelay       time.Duration
	GracefulShutdown time.Duration
	Priority         JobPriority
}

type JobHistory struct {
	ID           string    `json:"id"`
	JobID        string    `json:"jobId"`
	DeploymentID string    `json:"deploymentId"`
	UserID       string    `json:"userId"`
	Event        string    `json:"event"`
	Stage        string    `json:"stage"`
	Message      string    `json:"message"`
	CreatedAt    time.Time `json:"createdAt"`
}

type JobMetrics struct {
	TotalJobs       int64         `json:"totalJobs"`
	QueuedJobs      int64         `json:"queuedJobs"`
	ActiveJobs      int64         `json:"activeJobs"`
	CompletedJobs   int64         `json:"completedJobs"`
	FailedJobs      int64         `json:"failedJobs"`
	CancelledJobs   int64         `json:"cancelledJobs"`
	AverageDuration time.Duration `json:"averageDuration"`
	SuccessRate     float64       `json:"successRate"`
}

type SystemHealth struct {
	Redis       bool   `json:"redis"`
	Asynq       bool   `json:"asynq"`
	Database    bool   `json:"database"`
	Docker      bool   `json:"docker"`
	Workers     int    `json:"workers"`
	QueueLength int    `json:"queueLength"`
	Status      string `json:"status"`
}

type BatchJob struct {
	ID          string   `json:"id"`
	Deployments []string `json:"deployments"`
	Status      string   `json:"status"`
	Progress    int      `json:"progress"`
	Total       int      `json:"total"`
	Completed   int      `json:"completed"`
	Failed      int      `json:"failed"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

type NotificationType string

const (
	NotificationEmail   NotificationType = "email"
	NotificationSlack   NotificationType = "slack"
	NotificationWebhook NotificationType = "webhook"
)

type JobNotification struct {
	Type     NotificationType        `json:"type"`
	Target   string                  `json:"target"`
	Template string                  `json:"template"`
	Data     map[string]interface{}  `json:"data"`
}

// ============================================================================
// DEPLOYMENT JOB MANAGER
// ============================================================================

type DeploymentJobManager struct {
	redisClient *redis.Client
	redisOpt    asynq.RedisClientOpt
	asynqClient *asynq.Client
	asynqServer *asynq.Server
	config      JobConfig
	metrics     JobMetrics
	mu          sync.RWMutex
}

const (
	deploymentJobQueueName  = "deployment-jobs"
	deploymentJobTaskType   = "deployment:job"
	deploymentJobStatusKey  = "deployment:job:%s:status"
	deploymentJobChannelKey = "deployment:job:%s:channel"
	deploymentLastJobKey    = "deployment:%s:last-job"
)

// Note: deploymentJobs is now in globals.go
// var deploymentJobs *DeploymentJobManager

// Note: logsWebsocketUpgrader is now in logs.go - DO NOT redeclare here

// ============================================================================
// INITIALIZATION
// ============================================================================

func NewDeploymentJobManager(config JobConfig) (*DeploymentJobManager, error) {
	redisOpt, err := buildAsynqRedisClientOpt()
	if err != nil {
		return nil, err
	}

	redisClient := redis.NewClient(&redis.Options{
		Addr:     redisOpt.Addr,
		Username: redisOpt.Username,
		Password: redisOpt.Password,
		DB:       redisOpt.DB,
	})

	asynqClient := asynq.NewClient(redisOpt)

	// Deployments the worker runs at once. 1 serialises every clone/build on
	// the platform, so a single slow clone blocks all other deploys.
	concurrency := intFromEnvOrDefault("ASYNQ_CONCURRENCY", 1)
	log.Printf("deployment worker concurrency=%d (set ASYNQ_CONCURRENCY to change)", concurrency)
	asynqServer := asynq.NewServer(redisOpt, asynq.Config{
		Concurrency: concurrency,
		Queues: map[string]int{
			deploymentJobQueueName: 1,
		},
		// Add retry and timeout config
		RetryDelayFunc: func(n int, err error, task *asynq.Task) time.Duration {
			return time.Duration(n) * 5 * time.Second
		},
	})

	// Verify Redis connection
	pingCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := redisClient.Ping(pingCtx).Err(); err != nil {
		asynqClient.Close()
		_ = redisClient.Close()
		return nil, fmt.Errorf("ping redis: %w", err)
	}

	return &DeploymentJobManager{
		redisClient: redisClient,
		redisOpt:    redisOpt,
		asynqClient: asynqClient,
		asynqServer: asynqServer,
		config:      config,
	}, nil
}

func newDeploymentJobManager() (*DeploymentJobManager, error) {
	config := JobConfig{
		Timeout:          30 * time.Minute,
		MaxRetries:       0,
		RetryDelay:       5 * time.Second,
		GracefulShutdown: 10 * time.Second,
		Priority:         PriorityNormal,
	}
	return NewDeploymentJobManager(config)
}

func (m *DeploymentJobManager) Close() {
	if m == nil {
		return
	}
	if m.asynqClient != nil {
		m.asynqClient.Close()
	}
	if m.redisClient != nil {
		_ = m.redisClient.Close()
	}
}

func (m *DeploymentJobManager) ServeMux() *asynq.ServeMux {
	mux := asynq.NewServeMux()
	mux.HandleFunc(deploymentJobTaskType, m.handleDeploymentJobTask)
	return mux
}

// ============================================================================
// JOB ENQUEUEING
// ============================================================================

func (m *DeploymentJobManager) EnqueueDeployment(
	ctx context.Context,
	userID string,
	deploymentID string,
	rebuildImage bool,
) (string, error) {
	return m.EnqueueDeploymentWithConfig(ctx, userID, deploymentID, rebuildImage, m.config)
}

func (m *DeploymentJobManager) EnqueueDeploymentWithConfig(
	ctx context.Context,
	userID string,
	deploymentID string,
	rebuildImage bool,
	config JobConfig,
) (string, error) {
	if m == nil || m.asynqClient == nil || m.redisClient == nil {
		return "", fmt.Errorf("deployment job manager is not initialized")
	}

	userID = strings.TrimSpace(userID)
	deploymentID = strings.TrimSpace(deploymentID)
	if userID == "" || deploymentID == "" {
		return "", fmt.Errorf("userID and deploymentID are required")
	}

	// generateRandomToken is now in helpers.go
	jobID, err := generateRandomToken(16)
	if err != nil {
		return "", fmt.Errorf("generate job id: %w", err)
	}

	status := DeploymentJobStatus{
		JobID:        jobID,
		DeploymentID: deploymentID,
		UserID:       userID,
		Status:       JobStatusQueued,
		Stage:        "queued",
		Message:      "deployment queued",
		Progress:     0,
		Priority:     config.Priority,
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	}
	if err := m.saveStatus(ctx, status); err != nil {
		return "", err
	}

	// Save initial history entry
	if err := m.SaveJobHistory(ctx, JobHistory{
		JobID:        jobID,
		DeploymentID: deploymentID,
		UserID:       userID,
		Event:        "queued",
		Stage:        "queued",
		Message:      "Job queued",
		CreatedAt:    time.Now().UTC(),
	}); err != nil {
		log.Printf("Failed to save job history: %v", err)
	}

	payload := DeploymentJobPayload{
		UserID:       userID,
		DeploymentID: deploymentID,
		RebuildImage: rebuildImage,
		Priority:     config.Priority,
		Timeout:      config.Timeout,
	}

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		_ = m.redisClient.Del(ctx, m.statusKey(jobID)).Err()
		return "", fmt.Errorf("marshal deployment job payload: %w", err)
	}

	task := asynq.NewTask(deploymentJobTaskType, payloadBytes)

	opts := []asynq.Option{
		asynq.Queue(deploymentJobQueueName),
		asynq.TaskID(jobID),
		asynq.MaxRetry(config.MaxRetries),
	}

	// config.Timeout is a ceiling on how long the WORKER may spend processing
	// this task (asynq.Timeout) — not a delay before the worker is allowed to
	// start it. asynq.ProcessAt(time.Now().Add(config.Timeout)) was scheduling
	// every deployment to begin only after its own timeout had already
	// elapsed (e.g. 30 minutes from now with the current config), so jobs sat
	// in the "scheduled" set doing nothing before a worker ever touched them.
	if config.Timeout > 0 {
		opts = append(opts, asynq.Timeout(config.Timeout))
	}

	info, err := m.asynqClient.EnqueueContext(ctx, task, opts...)
	if err != nil {
		_ = m.redisClient.Del(ctx, m.statusKey(jobID)).Err()
		return "", fmt.Errorf("enqueue deployment job: %w", err)
	}

	// Increment metrics
	_ = m.IncrementMetrics(ctx, "total")

	// Remember the app's latest job, so its build log can be found without
	// the job ID (a reloaded dashboard only knows the deployment).
	_ = m.redisClient.Set(ctx, fmt.Sprintf(deploymentLastJobKey, deploymentID), info.ID, jobLogRetention).Err()

	return info.ID, nil
}

// LastJobID returns the most recent job enqueued for a deployment, or "".
func (m *DeploymentJobManager) LastJobID(ctx context.Context, deploymentID string) string {
	if m == nil || m.redisClient == nil {
		return ""
	}
	jobID, err := m.redisClient.Get(ctx, fmt.Sprintf(deploymentLastJobKey, strings.TrimSpace(deploymentID))).Result()
	if err != nil {
		return ""
	}
	return jobID
}

// ============================================================================
// JOB MANAGEMENT
// ============================================================================

func (m *DeploymentJobManager) GetStatus(ctx context.Context, jobID string) (DeploymentJobStatus, bool, error) {
	if m == nil || m.redisClient == nil {
		return DeploymentJobStatus{}, false, fmt.Errorf("deployment job manager is not initialized")
	}

	jobID = strings.TrimSpace(jobID)
	if jobID == "" {
		return DeploymentJobStatus{}, false, fmt.Errorf("jobID is required")
	}

	data, err := m.redisClient.Get(ctx, m.statusKey(jobID)).Bytes()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			return DeploymentJobStatus{}, false, nil
		}
		return DeploymentJobStatus{}, false, fmt.Errorf("load job status: %w", err)
	}

	var status DeploymentJobStatus
	if err := json.Unmarshal(data, &status); err != nil {
		return DeploymentJobStatus{}, false, fmt.Errorf("decode job status: %w", err)
	}

	return status, true, nil
}

func (m *DeploymentJobManager) CancelJob(ctx context.Context, jobID string, userID string) error {
	if m == nil || m.redisClient == nil {
		return fmt.Errorf("deployment job manager is not initialized")
	}

	jobID = strings.TrimSpace(jobID)
	userID = strings.TrimSpace(userID)
	if jobID == "" || userID == "" {
		return fmt.Errorf("jobID and userID are required")
	}

	status, found, err := m.GetStatus(ctx, jobID)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("job not found")
	}
	if status.UserID != userID {
		return fmt.Errorf("unauthorized")
	}

	if status.Status == JobStatusCompleted || status.Status == JobStatusFailed {
		return fmt.Errorf("job already finished")
	}

	status.Status = JobStatusCancelled
	status.Stage = "cancelled"
	status.Message = "Job cancelled by user"
	status.Progress = 100

	now := time.Now().UTC()
	status.CompletedAt = &now

	if err := m.saveStatus(ctx, status); err != nil {
		return err
	}

	// Remove from queue
	inspector := asynq.NewInspector(m.redisOpt)
	if err := inspector.DeleteTask(deploymentJobQueueName, jobID); err != nil {
		log.Printf("Failed to delete task from queue: %v", err)
	}

	// Save history
	if err := m.SaveJobHistory(ctx, JobHistory{
		JobID:        jobID,
		DeploymentID: status.DeploymentID,
		UserID:       userID,
		Event:        "cancelled",
		Stage:        "cancelled",
		Message:      "Job cancelled by user",
		CreatedAt:    time.Now().UTC(),
	}); err != nil {
		log.Printf("Failed to save job history: %v", err)
	}

	return nil
}

// ============================================================================
// STATUS MANAGEMENT
// ============================================================================

func (m *DeploymentJobManager) statusKey(jobID string) string {
	return fmt.Sprintf(deploymentJobStatusKey, strings.TrimSpace(jobID))
}

func (m *DeploymentJobManager) channelKey(jobID string) string {
	return fmt.Sprintf(deploymentJobChannelKey, strings.TrimSpace(jobID))
}

func (m *DeploymentJobManager) saveStatus(ctx context.Context, status DeploymentJobStatus) error {
	status.UpdatedAt = time.Now().UTC()
	if status.CreatedAt.IsZero() {
		status.CreatedAt = status.UpdatedAt
	}

	// Calculate duration if completed
	if status.CompletedAt != nil {
		status.Duration = status.CompletedAt.Sub(status.CreatedAt).String()
	}

	data, err := json.Marshal(status)
	if err != nil {
		return fmt.Errorf("marshal job status: %w", err)
	}

	pipe := m.redisClient.Pipeline()
	pipe.Set(ctx, m.statusKey(status.JobID), data, 0)
	pipe.Publish(ctx, m.channelKey(status.JobID), data)

	_, err = pipe.Exec(ctx)
	if err != nil {
		return fmt.Errorf("save and publish job status: %w", err)
	}

	return nil
}

// ============================================================================
// JOB HISTORY
// ============================================================================

func (m *DeploymentJobManager) SaveJobHistory(ctx context.Context, history JobHistory) error {
	key := fmt.Sprintf("deployment:job:%s:history", history.JobID)

	data, err := json.Marshal(history)
	if err != nil {
		return err
	}

	pipe := m.redisClient.Pipeline()
	pipe.LPush(ctx, key, data)
	pipe.LTrim(ctx, key, 0, 99)
	pipe.Expire(ctx, key, 24*time.Hour)

	_, err = pipe.Exec(ctx)
	return err
}

func (m *DeploymentJobManager) GetJobHistory(ctx context.Context, jobID string, limit int) ([]JobHistory, error) {
	if limit <= 0 || limit > 100 {
		limit = 100
	}

	key := fmt.Sprintf("deployment:job:%s:history", jobID)
	results, err := m.redisClient.LRange(ctx, key, 0, int64(limit-1)).Result()
	if err != nil {
		return nil, err
	}

	history := make([]JobHistory, 0, len(results))
	for _, result := range results {
		var entry JobHistory
		if err := json.Unmarshal([]byte(result), &entry); err != nil {
			continue
		}
		history = append(history, entry)
	}

	return history, nil
}

// ============================================================================
// METRICS
// ============================================================================

func (m *DeploymentJobManager) IncrementMetrics(ctx context.Context, key string) error {
	if m == nil || m.redisClient == nil {
		return fmt.Errorf("deployment job manager is not initialized")
	}
	return m.redisClient.Incr(ctx, "deployment:stats:"+key).Err()
}

func (m *DeploymentJobManager) GetMetrics(ctx context.Context) (JobMetrics, error) {
	if m == nil || m.redisClient == nil {
		return JobMetrics{}, fmt.Errorf("deployment job manager is not initialized")
	}
	inspector := asynq.NewInspector(m.redisOpt)

	queueInfo, err := inspector.GetQueueInfo(deploymentJobQueueName)
	if err != nil {
		return JobMetrics{}, err
	}

	var metrics JobMetrics
	metrics.QueuedJobs = int64(queueInfo.Pending)
	metrics.ActiveJobs = int64(queueInfo.Active)

	if val, err := m.redisClient.Get(ctx, "deployment:stats:total").Int64(); err == nil {
		metrics.TotalJobs = val
	}
	if val, err := m.redisClient.Get(ctx, "deployment:stats:completed").Int64(); err == nil {
		metrics.CompletedJobs = val
	}
	if val, err := m.redisClient.Get(ctx, "deployment:stats:failed").Int64(); err == nil {
		metrics.FailedJobs = val
	}
	if val, err := m.redisClient.Get(ctx, "deployment:stats:cancelled").Int64(); err == nil {
		metrics.CancelledJobs = val
	}

	if metrics.TotalJobs > 0 {
		metrics.SuccessRate = float64(metrics.CompletedJobs) / float64(metrics.TotalJobs) * 100
	}

	return metrics, nil
}

// ============================================================================
// HEALTH CHECK
// ============================================================================

func (m *DeploymentJobManager) HealthCheck(ctx context.Context) (SystemHealth, error) {
	if m == nil || m.redisClient == nil {
		return SystemHealth{}, fmt.Errorf("deployment job manager is not initialized")
	}
	health := SystemHealth{}

	// Check Redis
	if err := m.redisClient.Ping(ctx).Err(); err != nil {
		health.Status = "unhealthy"
		return health, fmt.Errorf("redis unhealthy: %w", err)
	}
	health.Redis = true

	// Check Asynq
	inspector := asynq.NewInspector(m.redisOpt)
	info, err := inspector.GetQueueInfo(deploymentJobQueueName)
	if err != nil {
		health.Status = "unhealthy"
		return health, fmt.Errorf("asynq unhealthy: %w", err)
	}
	health.Asynq = true
	health.QueueLength = info.Pending + info.Active
	health.Workers = intFromEnvOrDefault("ASYNQ_CONCURRENCY", 1)

	// Check Database (deploymentStore is now in globals.go)
	if deploymentStore != nil {
		if err := deploymentStore.HealthCheck(ctx); err != nil {
			health.Status = "unhealthy"
			return health, fmt.Errorf("database unhealthy: %w", err)
		}
		health.Database = true
	}

	// Check Docker - using checkDockerHealth from main.go
	if checkDockerHealth() != "healthy" {
		health.Status = "degraded"
		health.Docker = false
	} else {
		health.Docker = true
	}

	if health.Status == "" {
		health.Status = "healthy"
	}

	return health, nil
}

// Note: checkDockerHealth is now in main.go - DO NOT redeclare here

// ============================================================================
// WORKER HANDLER
// ============================================================================

func (m *DeploymentJobManager) handleDeploymentJobTask(ctx context.Context, task *asynq.Task) error {
	if task == nil {
		return asynq.SkipRetry
	}

	var payload DeploymentJobPayload
	if err := json.Unmarshal(task.Payload(), &payload); err != nil {
		return asynq.SkipRetry
	}

	jobID, _ := asynq.GetTaskID(ctx)
	status, ok, err := m.GetStatus(ctx, jobID)
	if err != nil {
		return asynq.SkipRetry
	}
	if !ok {
		status = DeploymentJobStatus{
			JobID:        jobID,
			DeploymentID: payload.DeploymentID,
			UserID:       payload.UserID,
			Status:       JobStatusQueued,
			Stage:        "queued",
			Message:      "deployment queued",
			Progress:     0,
			CreatedAt:    time.Now().UTC(),
		}
	}

	// Update to active
	now := time.Now().UTC()
	if err := m.updateStatus(ctx, status, func(s *DeploymentJobStatus) {
		s.Status = JobStatusActive
		s.Stage = "starting"
		s.Message = "deployment started"
		s.Progress = 5
		s.StartedAt = &now
	}); err != nil {
		return asynq.SkipRetry
	}

	// Save history
	_ = m.SaveJobHistory(ctx, JobHistory{
		JobID:        jobID,
		DeploymentID: payload.DeploymentID,
		UserID:       payload.UserID,
		Event:        "started",
		Stage:        "starting",
		Message:      "Deployment started",
		CreatedAt:    now,
	})

	// Build log streamed to the dashboard (build_log.go). Stage changes are
	// mirrored into it as "==>" headers so the log reads top to bottom.
	logs := m.newJobLogWriter(jobID, payload.UserID)
	defer logs.Close()

	// Run deployment workflow - uses prepareDeploymentSource and estimateStorageUsageMB from source.go and resources.go
	deployedStatus, runErr := runDeploymentWorkflow(ctx, payload, func(stage string, progress int, message string) {
		logs.Printf("==> %s", message)
		_ = m.updateStatus(ctx, status, func(s *DeploymentJobStatus) {
			s.Status = JobStatusActive
			s.Stage = stage
			s.Message = message
			s.Progress = progress
		})
	}, logs)

	if runErr != nil {
		logs.Printf("==> Deployment failed: %v", runErr)
		// Handle failure
		if deploymentStore != nil {
			if markErr := deploymentStore.MarkDeploymentFailed(ctx, payload.DeploymentID, runErr, payload.UserID); markErr != nil {
				_ = m.updateStatus(ctx, status, func(s *DeploymentJobStatus) {
					s.Message = fmt.Sprintf("deployment failed: %v; failed to update database: %v", runErr, markErr)
					s.Error = runErr.Error()
					s.Status = JobStatusFailed
					s.Stage = "failed"
					s.Progress = 100
				})
				_ = m.IncrementMetrics(ctx, "failed")
				return asynq.SkipRetry
			}
		}

		status.Status = JobStatusFailed
		status.Stage = "failed"
		status.Message = runErr.Error()
		status.Error = runErr.Error()
		status.Progress = 100
		status.JobID = jobID
		status.DeploymentID = payload.DeploymentID
		status.UserID = payload.UserID
		completeTime := time.Now().UTC()
		status.CompletedAt = &completeTime

		if err := m.saveStatus(ctx, status); err != nil {
			return asynq.SkipRetry
		}

		_ = m.IncrementMetrics(ctx, "failed")

		_ = m.SaveJobHistory(ctx, JobHistory{
			JobID:        jobID,
			DeploymentID: payload.DeploymentID,
			UserID:       payload.UserID,
			Event:        "failed",
			Stage:        "failed",
			Message:      runErr.Error(),
			CreatedAt:    completeTime,
		})

		return asynq.SkipRetry
	}

	// Mark as completed
	completeTime := time.Now().UTC()
	deployedStatus.JobID = jobID
	deployedStatus.DeploymentID = payload.DeploymentID
	deployedStatus.UserID = payload.UserID
	deployedStatus.Status = JobStatusCompleted
	deployedStatus.CompletedAt = &completeTime

	if err := m.saveStatus(ctx, deployedStatus); err != nil {
		return asynq.SkipRetry
	}

	_ = m.IncrementMetrics(ctx, "completed")

	_ = m.SaveJobHistory(ctx, JobHistory{
		JobID:        jobID,
		DeploymentID: payload.DeploymentID,
		UserID:       payload.UserID,
		Event:        "completed",
		Stage:        "completed",
		Message:      "Deployment completed successfully",
		CreatedAt:    completeTime,
	})

	return nil
}

// ============================================================================
// STUCK DEPLOYMENT RECONCILIATION
// ============================================================================

const (
	stuckDeploymentReconcileInterval = 5 * time.Minute
	// Grace period between CreateDeploymentAttemptForUser inserting the
	// BUILDING row and the task landing in the queue.
	stuckDeploymentGracePeriod = 2 * time.Minute
	stuckDeploymentMessage     = "deployment was interrupted before it finished (the worker stopped or its status update failed); please redeploy"
)

// RunStuckDeploymentReconciler marks BUILDING deployments FAILED when no
// queued, running, scheduled or retrying task exists for them anymore. Without
// this, a job whose failure couldn't be written to the database (or that was
// lost when the API restarted) leaves the app showing "Building…" forever.
func (m *DeploymentJobManager) RunStuckDeploymentReconciler(ctx context.Context) {
	ticker := time.NewTicker(stuckDeploymentReconcileInterval)
	defer ticker.Stop()

	for {
		if n, err := m.ReconcileStuckDeployments(ctx); err != nil {
			log.Printf("[WARN] stuck deployment reconciler: %v", err)
		} else if n > 0 {
			log.Printf("[INFO] stuck deployment reconciler: marked %d deployment(s) failed", n)
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (m *DeploymentJobManager) ReconcileStuckDeployments(ctx context.Context) (int, error) {
	if m == nil || deploymentStore == nil {
		return 0, fmt.Errorf("deployment job manager is not initialized")
	}

	stale, err := deploymentStore.ListBuildingDeploymentsNotUpdatedSince(ctx, time.Now().Add(-stuckDeploymentGracePeriod))
	if err != nil || len(stale) == 0 {
		return 0, err
	}

	// Collect the deployments that still have a live task. Checked after
	// loading the rows, so a job enqueued in between is still seen here.
	live, err := m.deploymentsWithLiveTasks()
	if err != nil {
		return 0, err
	}

	marked := 0
	for _, d := range stale {
		if live[d.DeploymentID] {
			continue
		}
		if err := deploymentStore.MarkDeploymentFailed(ctx, d.DeploymentID, errors.New(stuckDeploymentMessage), d.OwnerUserID); err != nil {
			log.Printf("[WARN] stuck deployment reconciler: mark %s failed: %v", d.DeploymentID, err)
			continue
		}
		marked++
	}
	return marked, nil
}

func (m *DeploymentJobManager) deploymentsWithLiveTasks() (map[string]bool, error) {
	inspector := asynq.NewInspector(m.redisOpt)
	defer inspector.Close()

	listers := []func(string, ...asynq.ListOption) ([]*asynq.TaskInfo, error){
		inspector.ListPendingTasks,
		inspector.ListActiveTasks,
		inspector.ListScheduledTasks,
		inspector.ListRetryTasks,
	}

	live := make(map[string]bool)
	const pageSize = 500
	for _, list := range listers {
		for page := 1; ; page++ {
			tasks, err := list(deploymentJobQueueName, asynq.PageSize(pageSize), asynq.Page(page))
			if err != nil {
				// A queue that has never had a task doesn't exist yet.
				if errors.Is(err, asynq.ErrQueueNotFound) {
					break
				}
				return nil, fmt.Errorf("list deployment tasks: %w", err)
			}
			for _, task := range tasks {
				var payload DeploymentJobPayload
				if json.Unmarshal(task.Payload, &payload) == nil && payload.DeploymentID != "" {
					live[payload.DeploymentID] = true
				}
			}
			if len(tasks) < pageSize {
				break
			}
		}
	}
	return live, nil
}

func (m *DeploymentJobManager) updateStatus(ctx context.Context, current DeploymentJobStatus, mutate func(*DeploymentJobStatus)) error {
	mutate(&current)
	return m.saveStatus(ctx, current)
}

// ============================================================================
// WEBSOCKET STREAMING
// ============================================================================

func (m *DeploymentJobManager) streamJobStatus(c *gin.Context, currentUser UserRecord, initial DeploymentJobStatus) {
	if m == nil || m.redisClient == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "deployment job manager is not initialized"})
		return
	}

	// logsWebsocketUpgrader is now in logs.go
	conn, err := logsWebsocketUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	// Send initial status
	if err := conn.WriteJSON(initial); err != nil {
		return
	}

	// Subscribe to updates before reading the log backlog, so no line falls
	// in the gap between the two; lines seen in both are dropped by Seq.
	pubsub := m.redisClient.Subscribe(c.Request.Context(), m.channelKey(initial.JobID))
	defer pubsub.Close()
	if _, err := pubsub.Receive(c.Request.Context()); err != nil {
		return
	}

	var lastSeq int64
	if backlog, err := m.JobLogBacklog(c.Request.Context(), initial.JobID, 0); err == nil {
		for _, line := range backlog {
			if line.UserID != currentUser.ID {
				return
			}
			if err := conn.WriteJSON(line); err != nil {
				return
			}
			lastSeq = line.Seq
		}
	}

	ch := pubsub.Channel()
	for {
		select {
		case <-c.Request.Context().Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			// The channel carries both job status updates and build log
			// lines (build_log.go); only log lines have a "type".
			var envelope struct {
				Type   string `json:"type"`
				UserID string `json:"userId"`
				Seq    int64  `json:"seq"`
			}
			if err := json.Unmarshal([]byte(msg.Payload), &envelope); err != nil {
				continue
			}
			if envelope.UserID != currentUser.ID {
				return
			}
			if envelope.Type == jobLogMessageType {
				if envelope.Seq <= lastSeq {
					continue
				}
				lastSeq = envelope.Seq
				if err := conn.WriteMessage(websocket.TextMessage, []byte(msg.Payload)); err != nil {
					return
				}
				continue
			}
			var status DeploymentJobStatus
			if err := json.Unmarshal([]byte(msg.Payload), &status); err != nil {
				continue
			}
			if err := conn.WriteJSON(status); err != nil {
				return
			}
		}
	}
}

// ============================================================================
// DEPLOYMENT WORKFLOW
// ============================================================================

// logs receives the user-facing build output; nil discards it.
func runDeploymentWorkflow(ctx context.Context, payload DeploymentJobPayload, progress func(stage string, progress int, message string), logs io.Writer) (DeploymentJobStatus, error) {
	// deploymentStore is now in globals.go
	if deploymentStore == nil {
		return DeploymentJobStatus{}, fmt.Errorf("deployment store is not initialized")
	}

	deployment, err := deploymentStore.GetDeploymentForUser(ctx, payload.UserID, payload.DeploymentID)
	if err != nil {
		return DeploymentJobStatus{}, err
	}

	if progress != nil {
		progress("loading", 10, "loading deployment details")
	}

	imageName := strings.TrimSpace(deployment.ImageName)
	fastRestart := !payload.RebuildImage && imageName != ""

	// Note: defaultDeployCPU, defaultDeployMemoryMB, defaultDeployApps are in resources.go
	requestedCPU := defaultDeployCPU
	requestedMemoryMB := int64(defaultDeployMemoryMB)
	requestedApps := int64(defaultDeployApps)
	var requestedStorageMB int64

	if fastRestart {
		if progress != nil {
			progress("restarting", 20, "restarting service without rebuild")
		}
	} else {
		if progress != nil {
			progress("cloning", 15, "cloning source repository")
		}

		// prepareDeploymentSource is now in source.go. The reporter lets the
		// clone stream its progress to the console, the job status and the
		// deploy log (git_progress.go).
		localAppPath, err := prepareDeploymentSource(withCloneReporter(ctx, progress, logs), deployment)
		if err != nil {
			return DeploymentJobStatus{}, err
		}
		deployment.AppPath = localAppPath

		// estimateStorageUsageMB is now in resources.go
		requestedStorageMB, err = estimateStorageUsageMB(deployment.AppPath)
		if err != nil {
			return DeploymentJobStatus{}, err
		}
	}

	shouldReserve := deployment.Status == string(DeploymentStatusFailed)
	shouldReleaseOnError := deployment.Status != string(DeploymentStatusDeployed)

	if shouldReserve {
		if err := deploymentStore.ReserveDeploymentResources(ctx, payload.UserID, requestedCPU, requestedMemoryMB, requestedApps, requestedStorageMB); err != nil {
			return DeploymentJobStatus{}, err
		}
	}

	// Build before touching the running container, so the app keeps serving
	// for the whole build and a failed build leaves the old version up.
	if payload.RebuildImage || imageName == "" {
		if progress != nil {
			progress("building", 50, "building application image")
		}
		buildSettings, err := deploymentStore.GetBuildSettings(ctx, deployment.DeploymentID)
		if err != nil {
			if shouldReleaseOnError {
				_ = deploymentStore.ReleaseDeploymentResources(ctx, payload.UserID, requestedCPU, requestedMemoryMB, requestedApps, requestedStorageMB)
			}
			return DeploymentJobStatus{}, err
		}
		imageName, err = BuildCodeWithLogsAndDockerfile(deployment.AppPath, deployment.AppName, logs, buildSettings.DockerfilePath)
		if err != nil {
			if shouldReleaseOnError {
				_ = deploymentStore.ReleaseDeploymentResources(ctx, payload.UserID, requestedCPU, requestedMemoryMB, requestedApps, requestedStorageMB)
			}
			// A monorepo built from its root fails with a bare "no start
			// command"; say what to do about it.
			if buildSettings.DockerfilePath == "" && strings.Contains(err.Error(), "No start command could be found") {
				if hint := monorepoHint(deployment.AppPath); hint != "" {
					err = fmt.Errorf("%w. %s", err, hint)
				}
			}
			return DeploymentJobStatus{}, err
		}

		// Resolve the port the container listens on: an explicit setting, else
		// the custom Dockerfile's EXPOSE, else whatever the service already had
		// (8080 by default). Persisted so the container, Caddy and the UI agree.
		port := buildSettings.ContainerPort
		source := "the service's port setting"
		if port == 0 && buildSettings.DockerfilePath != "" {
			if dockerfile, resolveErr := resolveDockerfile(deployment.AppPath, buildSettings.DockerfilePath); resolveErr == nil {
				port = detectDockerfilePort(dockerfile)
				source = "the Dockerfile's EXPOSE"
			}
		}
		if port > 0 {
			resolved := strconv.Itoa(port)
			if normalizePortMap(deployment.PortMap) != resolved {
				if err := deploymentStore.UpdateDeploymentPortMap(ctx, deployment.DeploymentID, resolved); err != nil {
					return DeploymentJobStatus{}, err
				}
				deployment.PortMap = resolved
			}
			fmt.Fprintf(logSink(logs), "==> Container port %d (from %s)\n", port, source)
		}
	} else if progress != nil {
		progress("deploying", 60, "reusing existing image")
	}

	envMap, err := deploymentStore.LoadDeploymentEnvMap(ctx, payload.UserID, deployment.DeploymentID)
	if err != nil {
		if shouldReleaseOnError {
			_ = deploymentStore.ReleaseDeploymentResources(ctx, payload.UserID, requestedCPU, requestedMemoryMB, requestedApps, requestedStorageMB)
		}
		return DeploymentJobStatus{}, err
	}

	if strings.TrimSpace(deployment.ContainerID) != "" {
		if progress != nil {
			progress("stopping", 70, "stopping existing container")
		}
		// An already-gone container (removed by hand, host reboot) is fine:
		// the goal is only that it isn't running anymore.
		if err := StopAndRemoveContainer(deployment.ContainerID, false); err != nil &&
			!strings.Contains(strings.ToLower(err.Error()), "no such container") {
			return DeploymentJobStatus{}, err
		}
	}

	if progress != nil {
		progress("deploying", 75, "creating and starting container")
	}

	// Apply environment overrides
	for k, v := range payload.EnvOverrides {
		envMap[k] = v
	}

	containerID, err := CreateAndStartContainer(
		imageName,
		deployment.AppName,
		deployment.DeploymentID,
		normalizePortMap(deployment.PortMap),
		loadDockerEnvList(envMap),
		requestedMemoryMB,
		requestedCPU,
	)
	if err != nil {
		if shouldReleaseOnError {
			_ = deploymentStore.ReleaseDeploymentResources(ctx, payload.UserID, requestedCPU, requestedMemoryMB, requestedApps, requestedStorageMB)
		}
		return DeploymentJobStatus{}, err
	}

	if err := deploymentStore.MarkDeploymentDeployed(ctx, deployment.DeploymentID, containerID, deployment.AppName, imageName, payload.UserID); err != nil {
		return DeploymentJobStatus{}, err
	}

	if progress != nil {
		progress("completed", 100, "deployment completed successfully")
	}
	if url := computeDeploymentURL(deployment.AppName, string(DeploymentStatusRunning), containerID); url != "" {
		fmt.Fprintf(logSink(logs), "==> Live at %s\n", url)
	}

	now := time.Now().UTC()
	return DeploymentJobStatus{
		DeploymentID: deployment.DeploymentID,
		UserID:       payload.UserID,
		Status:       JobStatusCompleted,
		Stage:        "completed",
		Message:      "deployment completed successfully",
		Progress:     100,
		UpdatedAt:    now,
	}, nil
}

// ============================================================================
// HANDLERS
// ============================================================================

func deploymentJobStatusHandler(c *gin.Context) {
	// currentAuthUser is now in auth.go
	user, ok := currentAuthUser(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	jobID := strings.TrimSpace(c.Param("jobId"))
	if jobID == "" {
		// sendBadRequest is now in helpers.go
		sendBadRequest(c, "job id is required", nil)
		return
	}

	// deploymentJobs is now in globals.go
	status, found, err := deploymentJobs.GetStatus(c.Request.Context(), jobID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_load_job_status", "details": err.Error()})
		return
	}
	if !found || status.UserID != user.ID {
		c.JSON(http.StatusNotFound, gin.H{"error": "deployment_job_not_found"})
		return
	}

	// Check if WebSocket upgrade requested
	if websocket.IsWebSocketUpgrade(c.Request) {
		deploymentJobs.streamJobStatus(c, user, status)
		return
	}

	c.JSON(http.StatusOK, status)
}

func deploymentJobCancelHandler(c *gin.Context) {
	user, ok := currentAuthUser(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	jobID := strings.TrimSpace(c.Param("jobId"))
	if jobID == "" {
		sendBadRequest(c, "job id is required", nil)
		return
	}

	if err := deploymentJobs.CancelJob(c.Request.Context(), jobID, user.ID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_cancel_job", "details": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"message": "job cancelled successfully",
		"jobId":   jobID,
	})
}

func deploymentJobHistoryHandler(c *gin.Context) {
	user, ok := currentAuthUser(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	jobID := strings.TrimSpace(c.Param("jobId"))
	if jobID == "" {
		sendBadRequest(c, "job id is required", nil)
		return
	}

	// Verify job belongs to user
	status, found, err := deploymentJobs.GetStatus(c.Request.Context(), jobID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_load_job_status", "details": err.Error()})
		return
	}
	if !found || status.UserID != user.ID {
		c.JSON(http.StatusNotFound, gin.H{"error": "deployment_job_not_found"})
		return
	}

	limit := 50
	if l := c.Query("limit"); l != "" {
		if parsed, err := strconv.Atoi(l); err == nil && parsed > 0 && parsed <= 100 {
			limit = parsed
		}
	}

	history, err := deploymentJobs.GetJobHistory(c.Request.Context(), jobID, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_load_job_history", "details": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"jobId":   jobID,
		"history": history,
		"count":   len(history),
	})
}

func deploymentJobMetricsHandler(c *gin.Context) {
	metrics, err := deploymentJobs.GetMetrics(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_load_metrics", "details": err.Error()})
		return
	}

	c.JSON(http.StatusOK, metrics)
}

func deploymentJobHealthHandler(c *gin.Context) {
	health, err := deploymentJobs.HealthCheck(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"status":  "unhealthy",
			"details": err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, health)
}

func deploymentDeployHandler(c *gin.Context) {
	user, deployment, ok := currentUserDeployment(c)
	if !ok {
		return
	}

	// Parse optional config
	var config JobConfig
	config.Priority = PriorityNormal
	config.MaxRetries = 0
	config.Timeout = 30 * time.Minute

	rebuildImage := true
	if rb := c.Query("rebuild"); rb != "" {
		rebuildImage = strings.ToLower(rb) != "false"
	}

	if priority := c.Query("priority"); priority != "" {
		switch strings.ToLower(priority) {
		case "low":
			config.Priority = PriorityLow
		case "high":
			config.Priority = PriorityHigh
		case "urgent":
			config.Priority = PriorityUrgent
		default:
			config.Priority = PriorityNormal
		}
	}

	jobID, err := deploymentJobs.EnqueueDeploymentWithConfig(
		c.Request.Context(),
		user.ID,
		deployment.DeploymentID,
		rebuildImage,
		config,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_enqueue_deployment", "details": err.Error()})
		return
	}

	c.JSON(http.StatusAccepted, gin.H{
		"message":      "deployment queued",
		"deploymentId": deployment.DeploymentID,
		"jobId":        jobID,
		"status":       "queued",
		"priority":     config.Priority,
	})
}

func deploymentJobListHandler(c *gin.Context) {
	user, ok := currentAuthUser(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	// Get all jobs for user from Redis
	statuses, err := deploymentJobs.ListJobsForUser(c.Request.Context(), user.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_list_jobs", "details": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"jobs":  statuses,
		"count": len(statuses),
	})
}

func (m *DeploymentJobManager) ListJobsForUser(ctx context.Context, userID string) ([]DeploymentJobStatus, error) {
	if m == nil || m.redisClient == nil {
		return nil, fmt.Errorf("deployment job manager is not initialized")
	}
	// Scan for keys matching pattern
	pattern := fmt.Sprintf("deployment:job:*")
	var statuses []DeploymentJobStatus

	iter := m.redisClient.Scan(ctx, 0, pattern, 0).Iterator()
	for iter.Next(ctx) {
		key := iter.Val()
		data, err := m.redisClient.Get(ctx, key).Bytes()
		if err != nil {
			continue
		}

		var status DeploymentJobStatus
		if err := json.Unmarshal(data, &status); err != nil {
			continue
		}

		if status.UserID == userID {
			statuses = append(statuses, status)
		}
	}

	if err := iter.Err(); err != nil {
		return nil, err
	}

	return statuses, nil
}

// ============================================================================
// ROUTE SETUP
// ============================================================================

func SetupDeploymentJobRoutes(router *gin.Engine) {
	jobGroup := router.Group("/api/deployments/jobs")
	jobGroup.Use(AuthMiddleware(false))
	{
		jobGroup.GET("/:jobId", deploymentJobStatusHandler)
		jobGroup.DELETE("/:jobId", deploymentJobCancelHandler)
		jobGroup.GET("/:jobId/history", deploymentJobHistoryHandler)
		jobGroup.GET("/metrics", deploymentJobMetricsHandler)
		jobGroup.GET("/health", deploymentJobHealthHandler)
		jobGroup.GET("/", deploymentJobListHandler)
	}
}

// ============================================================================
// NOTE: Missing functions are in other files
// ============================================================================

// Note: prepareDeploymentSource is now in source.go
// Note: estimateStorageUsageMB is now in resources.go
// Note: StopAndRemoveContainer is now in container.go
// Note: CreateAndStartContainer is now in container.go
// Note: normalizePortMap is now in container.go
// Note: loadDockerEnvList is now in envs.go
// Note: BuildCode is now in build.go or fastbuild.go

// ============================================================================
// BUILD HELPERS
// ============================================================================

func buildAsynqRedisClientOpt() (asynq.RedisClientOpt, error) {
	addr := strings.TrimSpace(os.Getenv("REDIS_ADDR"))
	if addr == "" {
		addr = "127.0.0.1:6379"
	}

	password := os.Getenv("REDIS_PASSWORD")
	db := 0
	if rawDB := strings.TrimSpace(os.Getenv("REDIS_DB")); rawDB != "" {
		parsed, err := strconv.Atoi(rawDB)
		if err != nil {
			return asynq.RedisClientOpt{}, fmt.Errorf("parse REDIS_DB: %w", err)
		}
		db = parsed
	}

	return asynq.RedisClientOpt{
		Addr:     addr,
		Password: password,
		DB:       db,
	}, nil
}

func intFromEnvOrDefault(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}

	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 1 {
		return fallback
	}

	return parsed
}

// Note: generateRandomToken is now in helpers.go - DO NOT redeclare here

// ============================================================================
// INITIALIZATION
// ============================================================================

func InitDeploymentJobManager() error {
	config := JobConfig{
		Timeout:          30 * time.Minute,
		MaxRetries:       0,
		RetryDelay:       5 * time.Second,
		GracefulShutdown: 10 * time.Second,
		Priority:         PriorityNormal,
	}

	manager, err := NewDeploymentJobManager(config)
	if err != nil {
		return err
	}

	// deploymentJobs is now in globals.go
	deploymentJobs = manager
	return nil
}