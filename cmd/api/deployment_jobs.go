package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

	concurrency := intFromEnvOrDefault("ASYNQ_CONCURRENCY", 1)
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

	if config.Timeout > 0 {
		opts = append(opts, asynq.ProcessAt(time.Now().Add(config.Timeout)))
	}

	info, err := m.asynqClient.EnqueueContext(ctx, task, opts...)
	if err != nil {
		_ = m.redisClient.Del(ctx, m.statusKey(jobID)).Err()
		return "", fmt.Errorf("enqueue deployment job: %w", err)
	}

	// Increment metrics
	_ = m.IncrementMetrics(ctx, "total")

	return info.ID, nil
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

	// Run deployment workflow - uses prepareDeploymentSource and estimateStorageUsageMB from source.go and resources.go
	deployedStatus, runErr := runDeploymentWorkflow(ctx, payload, func(stage string, progress int, message string) {
		_ = m.updateStatus(ctx, status, func(s *DeploymentJobStatus) {
			s.Status = JobStatusActive
			s.Stage = stage
			s.Message = message
			s.Progress = progress
		})
	})

	if runErr != nil {
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

	// Subscribe to updates
	pubsub := m.redisClient.Subscribe(c.Request.Context(), m.channelKey(initial.JobID))
	defer pubsub.Close()

	ch := pubsub.Channel()
	for {
		select {
		case <-c.Request.Context().Done():
			return
		case msg, ok := <-ch:
			if !ok {
				return
			}
			var status DeploymentJobStatus
			if err := json.Unmarshal([]byte(msg.Payload), &status); err != nil {
				continue
			}
			if status.UserID != currentUser.ID {
				return
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

func runDeploymentWorkflow(ctx context.Context, payload DeploymentJobPayload, progress func(stage string, progress int, message string)) (DeploymentJobStatus, error) {
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

		// prepareDeploymentSource is now in source.go
		localAppPath, err := prepareDeploymentSource(ctx, deployment)
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

	if strings.TrimSpace(deployment.ContainerID) != "" {
		if progress != nil {
			progress("stopping", 35, "stopping existing container")
		}
		if err := StopAndRemoveContainer(deployment.ContainerID, false); err != nil {
			return DeploymentJobStatus{}, err
		}
	}

	if payload.RebuildImage || imageName == "" {
		if progress != nil {
			progress("building", 50, "building application image")
		}
		imageName, err = BuildCode(deployment.AppPath, deployment.AppName)
		if err != nil {
			if shouldReleaseOnError {
				_ = deploymentStore.ReleaseDeploymentResources(ctx, payload.UserID, requestedCPU, requestedMemoryMB, requestedApps, requestedStorageMB)
			}
			return DeploymentJobStatus{}, err
		}
	} else if progress != nil {
		progress("deploying", 60, "reusing existing image")
	}

	if progress != nil {
		progress("deploying", 75, "creating and starting container")
	}

	envMap, err := deploymentStore.LoadDeploymentEnvMap(ctx, payload.UserID, deployment.DeploymentID)
	if err != nil {
		if shouldReleaseOnError {
			_ = deploymentStore.ReleaseDeploymentResources(ctx, payload.UserID, requestedCPU, requestedMemoryMB, requestedApps, requestedStorageMB)
		}
		return DeploymentJobStatus{}, err
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