package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
)

const (
	deploymentJobTaskType   = "deployment:execute"
	deploymentJobQueueName  = "deployments"
	deploymentJobStatusKey  = "deployment:job:%s"
	deploymentJobChannelKey = "deployment:job:%s:events"
)

type DeploymentJobPayload struct {
	DeploymentID string `json:"deploymentId"`
	UserID       string `json:"userId"`
}

type DeploymentJobStatus struct {
	JobID        string    `json:"jobId"`
	DeploymentID string    `json:"deploymentId"`
	UserID       string    `json:"userId"`
	Status       string    `json:"status"`
	Stage        string    `json:"stage"`
	Message      string    `json:"message"`
	Progress     int       `json:"progress"`
	Error        string    `json:"error,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

type DeploymentJobManager struct {
	redisClient *redis.Client
	asynqClient  *asynq.Client
	asynqServer  *asynq.Server
}

var deploymentJobs *DeploymentJobManager

func newDeploymentJobManager() (*DeploymentJobManager, error) {
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
	concurrency := intFromEnvOrDefault("ASYNQ_CONCURRENCY", 4)
	asynqServer := asynq.NewServer(redisOpt, asynq.Config{
		Concurrency: concurrency,
		Queues: map[string]int{
			deploymentJobQueueName: 1,
		},
	})

	pingCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := redisClient.Ping(pingCtx).Err(); err != nil {
		asynqClient.Close()
		_ = redisClient.Close()
		return nil, fmt.Errorf("ping redis: %w", err)
	}

	return &DeploymentJobManager{
		redisClient: redisClient,
		asynqClient:  asynqClient,
		asynqServer:  asynqServer,
	}, nil
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

func (m *DeploymentJobManager) EnqueueDeployment(ctx context.Context, userID string, deploymentID string) (string, error) {
	if m == nil || m.asynqClient == nil || m.redisClient == nil {
		return "", fmt.Errorf("deployment job manager is not initialized")
	}

	userID = strings.TrimSpace(userID)
	deploymentID = strings.TrimSpace(deploymentID)
	if userID == "" || deploymentID == "" {
		return "", fmt.Errorf("userID and deploymentID are required")
	}

	jobID, err := generateRandomToken(16)
	if err != nil {
		return "", fmt.Errorf("generate job id: %w", err)
	}

	status := DeploymentJobStatus{
		JobID:        jobID,
		DeploymentID: deploymentID,
		UserID:       userID,
		Status:       "queued",
		Stage:        "queued",
		Message:      "deployment queued",
		Progress:     0,
		CreatedAt:    time.Now().UTC(),
		UpdatedAt:    time.Now().UTC(),
	}
	if err := m.saveStatus(ctx, status); err != nil {
		return "", err
	}

	payloadBytes, err := json.Marshal(DeploymentJobPayload{UserID: userID, DeploymentID: deploymentID})
	if err != nil {
		_ = m.redisClient.Del(ctx, m.statusKey(jobID)).Err()
		return "", fmt.Errorf("marshal deployment job payload: %w", err)
	}

	task := asynq.NewTask(deploymentJobTaskType, payloadBytes)
	info, err := m.asynqClient.EnqueueContext(ctx, task, asynq.Queue(deploymentJobQueueName), asynq.TaskID(jobID), asynq.MaxRetry(0))
	if err != nil {
		_ = m.redisClient.Del(ctx, m.statusKey(jobID)).Err()
		return "", fmt.Errorf("enqueue deployment job: %w", err)
	}

	return info.ID, nil
}

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

	data, err := json.Marshal(status)
	if err != nil {
		return fmt.Errorf("marshal job status: %w", err)
	}

	if err := m.redisClient.Set(ctx, m.statusKey(status.JobID), data, 0).Err(); err != nil {
		return fmt.Errorf("persist job status: %w", err)
	}
	if err := m.redisClient.Publish(ctx, m.channelKey(status.JobID), data).Err(); err != nil {
		return fmt.Errorf("publish job status: %w", err)
	}

	return nil
}

func (m *DeploymentJobManager) updateStatus(ctx context.Context, current DeploymentJobStatus, mutate func(*DeploymentJobStatus)) error {
	mutate(&current)
	return m.saveStatus(ctx, current)
}

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
			Status:       "queued",
			Stage:        "queued",
			Message:      "deployment queued",
			Progress:     0,
			CreatedAt:    time.Now().UTC(),
		}
	}

	if err := m.updateStatus(ctx, status, func(s *DeploymentJobStatus) {
		s.Status = "active"
		s.Stage = "starting"
		s.Message = "deployment started"
		s.Progress = 5
	}); err != nil {
		return asynq.SkipRetry
	}

	deployedStatus, runErr := runDeploymentWorkflow(ctx, payload, func(stage string, progress int, message string) {
		_ = m.updateStatus(ctx, status, func(s *DeploymentJobStatus) {
			s.Status = "active"
			s.Stage = stage
			s.Message = message
			s.Progress = progress
		})
	})
	if runErr != nil {
		if deploymentStore != nil {
			if markErr := deploymentStore.MarkDeploymentFailed(ctx, payload.DeploymentID, runErr); markErr != nil {
				_ = m.updateStatus(ctx, status, func(s *DeploymentJobStatus) {
					s.Message = fmt.Sprintf("deployment failed: %v; failed to update database: %v", runErr, markErr)
					s.Error = runErr.Error()
					s.Status = "failed"
					s.Stage = "failed"
					s.Progress = 100
				})
				return asynq.SkipRetry
			}
		}

		status.Status = "failed"
		status.Stage = "failed"
		status.Message = runErr.Error()
		status.Error = runErr.Error()
		status.Progress = 100
		status.JobID = jobID
		status.DeploymentID = payload.DeploymentID
		status.UserID = payload.UserID
		if err := m.saveStatus(ctx, status); err != nil {
			return asynq.SkipRetry
		}
		return asynq.SkipRetry
	}

	deployedStatus.JobID = jobID
	deployedStatus.DeploymentID = payload.DeploymentID
	deployedStatus.UserID = payload.UserID
	if err := m.saveStatus(ctx, deployedStatus); err != nil {
		return asynq.SkipRetry
	}

	return nil
}

func (m *DeploymentJobManager) streamJobStatus(c *gin.Context, currentUser UserRecord, initial DeploymentJobStatus) {
	if m == nil || m.redisClient == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "deployment job manager is not initialized"})
		return
	}

	conn, err := logsWebsocketUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	if err := conn.WriteJSON(initial); err != nil {
		return
	}

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

func deploymentJobStatusHandler(c *gin.Context) {
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

	status, found, err := deploymentJobs.GetStatus(c.Request.Context(), jobID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_load_job_status", "details": err.Error()})
		return
	}
	if !found || status.UserID != user.ID {
		c.JSON(http.StatusNotFound, gin.H{"error": "deployment_job_not_found"})
		return
	}

	if websocket.IsWebSocketUpgrade(c.Request) {
		deploymentJobs.streamJobStatus(c, user, status)
		return
	}

	c.JSON(http.StatusOK, status)
}

func deploymentDeployHandler(c *gin.Context) {
	user, deployment, ok := currentUserDeployment(c)
	if !ok {
		return
	}

	jobID, err := deploymentJobs.EnqueueDeployment(c.Request.Context(), user.ID, deployment.DeploymentID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_enqueue_deployment", "details": err.Error()})
		return
	}

	c.JSON(http.StatusAccepted, gin.H{
		"message":      "deployment queued",
		"deploymentId": deployment.DeploymentID,
		"jobId":        jobID,
		"status":       "queued",
	})
}

func runDeploymentWorkflow(ctx context.Context, payload DeploymentJobPayload, progress func(stage string, progress int, message string)) (DeploymentJobStatus, error) {
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

	requestedCPU := defaultDeployCPU
	requestedMemoryMB := int64(defaultDeployMemoryMB)
	requestedApps := int64(defaultDeployApps)
	requestedStorageMB, err := estimateStorageUsageMB(deployment.AppPath)
	if err != nil {
		return DeploymentJobStatus{}, err
	}

	shouldReserve := deployment.Status == string(DeploymentStatusFailed)
	shouldReleaseOnError := deployment.Status != string(DeploymentStatusDeployed)

	if shouldReserve {
		if err := deploymentStore.ReserveDeploymentResources(ctx, payload.UserID, requestedCPU, requestedMemoryMB, requestedApps, requestedStorageMB); err != nil {
			return DeploymentJobStatus{}, err
		}
	}

	if progress != nil {
		progress("building", 25, "building application image")
	}

	if strings.TrimSpace(deployment.ContainerID) != "" {
		if progress != nil {
			progress("stopping", 15, "stopping existing container")
		}
		if err := StopAndRemoveContainer(deployment.ContainerID); err != nil {
			return DeploymentJobStatus{}, err
		}
	}

	imageName := strings.TrimSpace(deployment.ImageName)
	if imageName == "" {
		imageName, err = BuildCode(deployment.AppPath, deployment.AppName)
		if err != nil {
			if shouldReleaseOnError {
				_ = deploymentStore.ReleaseDeploymentResources(ctx, payload.UserID, requestedCPU, requestedMemoryMB, requestedApps, requestedStorageMB)
			}
			return DeploymentJobStatus{}, err
		}
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

	containerID, err := CreateAndStartContainer(imageName, deployment.AppName, deployment.DeploymentID, deployment.PortMap, loadDockerEnvList(envMap), requestedMemoryMB, requestedCPU)
	if err != nil {
		if shouldReleaseOnError {
			_ = deploymentStore.ReleaseDeploymentResources(ctx, payload.UserID, requestedCPU, requestedMemoryMB, requestedApps, requestedStorageMB)
		}
		return DeploymentJobStatus{}, err
	}

	if err := deploymentStore.MarkDeploymentDeployed(ctx, deployment.DeploymentID, containerID, deployment.AppName); err != nil {
		return DeploymentJobStatus{}, err
	}

	if progress != nil {
		progress("completed", 100, "deployment completed successfully")
	}

	now := time.Now().UTC()
	return DeploymentJobStatus{
		DeploymentID: deployment.DeploymentID,
		UserID:       payload.UserID,
		Status:       "completed",
		Stage:        "completed",
		Message:      "deployment completed successfully",
		Progress:     100,
		UpdatedAt:    now,
	}, nil
}

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