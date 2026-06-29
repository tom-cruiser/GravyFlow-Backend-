package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	dockerclient "github.com/docker/docker/client"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

const (
	healthCheckerInterval = 30 * time.Second
	healthCheckerWindow   = 5 * time.Minute
)

type DeploymentHealthConfigRequest struct {
	HealthCheckPath            string `json:"healthCheckPath"`
	HealthCheckIntervalSeconds int    `json:"healthCheckIntervalSeconds"`
	MaxRestartsBeforeFailing   int    `json:"maxRestartsBeforeFailing"`
}

type DeploymentHealthEvent struct {
	DeploymentID string    `json:"deploymentId"`
	Status       string    `json:"status"`
	Stage        string    `json:"stage"`
	Message      string    `json:"message"`
	Action       string    `json:"action"`
	ContainerID  string    `json:"containerId,omitempty"`
	Alert        bool      `json:"alert"`
	CreatedAt    time.Time `json:"createdAt"`
}

type DeploymentHealthManager struct {
	redisClient *redis.Client
	httpClient  *http.Client
}

type dockerClientHandle struct {
	client *dockerclient.Client
}

var deploymentHealthManager *DeploymentHealthManager

func newDeploymentHealthManager() (*DeploymentHealthManager, error) {
	redisOpt, err := buildAsynqRedisClientOpt()
	if err != nil {
		return nil, err
	}

	client := redis.NewClient(&redis.Options{
		Addr:     redisOpt.Addr,
		Username: redisOpt.Username,
		Password: redisOpt.Password,
		DB:       redisOpt.DB,
	})

	pingCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Ping(pingCtx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("ping redis for health manager: %w", err)
	}

	return &DeploymentHealthManager{
		redisClient: client,
		httpClient:  &http.Client{Timeout: 5 * time.Second},
	}, nil
}

func (m *DeploymentHealthManager) Close() {
	if m == nil || m.redisClient == nil {
		return
	}
	_ = m.redisClient.Close()
}

func (m *DeploymentHealthManager) Start(ctx context.Context) {
	if m == nil {
		return
	}

	m.checkOnce(ctx)
	ticker := time.NewTicker(healthCheckerInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.checkOnce(ctx)
		}
	}
}

func (m *DeploymentHealthManager) checkOnce(ctx context.Context) {
	if deploymentStore == nil {
		return
	}

	targets, err := deploymentStore.ListRunningDeploymentsForHealthCheck(ctx)
	if err != nil {
		log.Printf("health checker: list running deployments: %v", err)
		return
	}
	if len(targets) == 0 {
		return
	}

	dockerClient, err := newDockerClientHandle()
	if err != nil {
		log.Printf("health checker: create docker client: %v", err)
		return
	}
	defer dockerClient.Close()

	var waitGroup sync.WaitGroup
	for _, target := range targets {
		target := target
		if target.LastCheckedAt != nil && time.Since(*target.LastCheckedAt) < time.Duration(target.HealthCheckIntervalSeconds)*time.Second {
			continue
		}

		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			m.checkDeployment(ctx, dockerClient, target)
		}()
	}
	waitGroup.Wait()
}

func (m *DeploymentHealthManager) checkDeployment(ctx context.Context, dockerClient *dockerClientHandle, target RunningDeploymentHealthTarget) {
	defer func() {
		if err := deploymentStore.MarkDeploymentHealthChecked(ctx, target.DeploymentID); err != nil {
			log.Printf("health checker: mark checked for %s: %v", target.DeploymentID, err)
		}
	}()

	healthy, reason := m.inspectAndProbe(ctx, dockerClient, target)
	if healthy {
		return
	}

	maxRestarts := target.MaxRestartsBeforeFailing
	if maxRestarts < 1 {
		maxRestarts = 3
	}

	recentRestarts, err := deploymentStore.CountRecentDeploymentRestarts(ctx, target.DeploymentID, healthCheckerWindow)
	if err != nil {
		log.Printf("health checker: count restarts for %s: %v", target.DeploymentID, err)
		return
	}
	if recentRestarts >= maxRestarts {
		alertMessage := fmt.Sprintf("deployment failed after %d restarts in %s: %s", recentRestarts, healthCheckerWindow, reason)
		_ = deploymentStore.RecordDeploymentRestartAudit(ctx, target.DeploymentID, "restart", "alerted", alertMessage, target.ContainerID, "")
		if err := deploymentStore.MarkDeploymentFailed(ctx, target.DeploymentID, errors.New(alertMessage)); err != nil {
			log.Printf("health checker: mark failed for %s: %v", target.DeploymentID, err)
		}
		m.publishEvent(ctx, DeploymentHealthEvent{
			DeploymentID: target.DeploymentID,
			Status:       "failed",
			Stage:        "alert",
			Message:      alertMessage,
			Action:       "alert",
			ContainerID:  target.ContainerID,
			Alert:        true,
			CreatedAt:    time.Now().UTC(),
		})
		log.Printf("health checker alert: %s", alertMessage)
		return
	}

	_ = deploymentStore.RecordDeploymentRestartAudit(ctx, target.DeploymentID, "restart", "attempted", reason, target.ContainerID, "")
	m.publishEvent(ctx, DeploymentHealthEvent{
		DeploymentID: target.DeploymentID,
		Status:       "restarting",
		Stage:        "restart_attempt",
		Message:      reason,
		Action:       "restart",
		ContainerID:  target.ContainerID,
		CreatedAt:    time.Now().UTC(),
	})

	envMap, err := deploymentStore.LoadDeploymentEnvMapByDeploymentID(ctx, target.DeploymentID)
	if err != nil {
		log.Printf("health checker: load env vars for %s: %v", target.DeploymentID, err)
		return
	}

	restartedContainerID, err := RestartContainer(target.ContainerID, target.ImageName, target.AppName, target.DeploymentID, target.PortMap, loadDockerEnvList(envMap), defaultDeployMemoryMB, defaultDeployCPU)
	if err != nil {
		_ = deploymentStore.RecordDeploymentRestartAudit(ctx, target.DeploymentID, "restart", "failed", err.Error(), target.ContainerID, "")
		if markErr := deploymentStore.MarkDeploymentFailed(ctx, target.DeploymentID, err); markErr != nil {
			log.Printf("health checker: mark failed after restart error for %s: %v", target.DeploymentID, markErr)
		}
		m.publishEvent(ctx, DeploymentHealthEvent{
			DeploymentID: target.DeploymentID,
			Status:       "failed",
			Stage:        "restart_failed",
			Message:      err.Error(),
			Action:       "restart",
			ContainerID:  target.ContainerID,
			Alert:        true,
			CreatedAt:    time.Now().UTC(),
		})
		return
	}

	_ = deploymentStore.RecordDeploymentRestartAudit(ctx, target.DeploymentID, "restart", "succeeded", reason, target.ContainerID, restartedContainerID)
	if err := deploymentStore.MarkDeploymentDeployed(ctx, target.DeploymentID, restartedContainerID, target.AppName, target.ImageName); err != nil {
		log.Printf("health checker: update running deployment for %s: %v", target.DeploymentID, err)
	}
	m.publishEvent(ctx, DeploymentHealthEvent{
		DeploymentID: target.DeploymentID,
		Status:       string(DeploymentStatusRunning),
		Stage:        "restarted",
		Message:      "container restarted successfully",
		Action:       "restart",
		ContainerID:  restartedContainerID,
		CreatedAt:    time.Now().UTC(),
	})
}

func (m *DeploymentHealthManager) inspectAndProbe(ctx context.Context, dockerClient *dockerClientHandle, target RunningDeploymentHealthTarget) (bool, string) {
	inspect, err := dockerClient.client.ContainerInspect(ctx, target.ContainerID)
	if err != nil {
		return false, fmt.Sprintf("container inspect failed: %v", err)
	}

	if inspect.State == nil || !inspect.State.Running {
		return false, "container stopped"
	}
	if inspect.State.Health != nil {
		if strings.ToLower(inspect.State.Health.Status) == "unhealthy" {
			return false, "container health reported unhealthy"
		}
		if strings.ToLower(inspect.State.Health.Status) == "healthy" {
			return true, "healthy"
		}
	}

	hostPort, _, err := parsePortMap(target.PortMap)
	if err != nil {
		return false, fmt.Sprintf("parse port map: %v", err)
	}

	healthPath := strings.TrimSpace(target.HealthCheckPath)
	if healthPath == "" {
		healthPath = "/health"
	}
	if !strings.HasPrefix(healthPath, "/") {
		healthPath = "/" + healthPath
	}

	endpoint := fmt.Sprintf("http://127.0.0.1:%s%s", hostPort, healthPath)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false, fmt.Sprintf("create health request: %v", err)
	}

	response, err := m.httpClient.Do(request)
	if err != nil {
		return false, fmt.Sprintf("health probe failed: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return false, fmt.Sprintf("health probe returned %s", response.Status)
	}

	return true, "healthy"
}

func (m *DeploymentHealthManager) publishEvent(ctx context.Context, event DeploymentHealthEvent) {
	if m == nil || m.redisClient == nil {
		return
	}

	payload, err := json.Marshal(event)
	if err != nil {
		return
	}
	if err := m.redisClient.Publish(ctx, deploymentHealthChannel(event.DeploymentID), payload).Err(); err != nil {
		log.Printf("health checker: publish event for %s: %v", event.DeploymentID, err)
	}
}

func deploymentHealthChannel(deploymentID string) string {
	return fmt.Sprintf("deployment:health:%s", strings.TrimSpace(deploymentID))
}

func updateDeploymentHealthConfigHandler(c *gin.Context) {
	user, deployment, ok := currentUserDeployment(c)
	if !ok {
		return
	}

	var req DeploymentHealthConfigRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		sendBadRequest(c, "invalid JSON body", err)
		return
	}

	path := strings.TrimSpace(req.HealthCheckPath)
	if path == "" {
		path = "/health"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	interval := req.HealthCheckIntervalSeconds
	if interval < 1 {
		interval = 30
	}
	maxRestarts := req.MaxRestartsBeforeFailing
	if maxRestarts < 1 {
		maxRestarts = 3
	}

	if err := deploymentStore.UpsertDeploymentHealthConfig(c.Request.Context(), user.ID, deployment.DeploymentID, path, interval, maxRestarts); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_save_health_config", "details": err.Error()})
		return
	}

	config, err := deploymentStore.GetDeploymentHealthConfigForUser(c.Request.Context(), user.ID, deployment.DeploymentID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_load_health_config", "details": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"healthConfig": config})
}

func streamDeploymentHealthHandler(c *gin.Context) {
	user, deployment, ok := currentUserDeployment(c)
	if !ok {
		return
	}

	if deploymentHealthManager == nil || deploymentHealthManager.redisClient == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "health checker is not initialized"})
		return
	}

	config, err := deploymentStore.GetDeploymentHealthConfigForUser(c.Request.Context(), user.ID, deployment.DeploymentID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_load_health_config", "details": err.Error()})
		return
	}

	conn, err := logsWebsocketUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	if err := conn.WriteJSON(gin.H{"healthConfig": config}); err != nil {
		return
	}

	pubsub := deploymentHealthManager.redisClient.Subscribe(c.Request.Context(), deploymentHealthChannel(deployment.DeploymentID))
	defer pubsub.Close()
	channel := pubsub.Channel()

	for {
		select {
		case <-c.Request.Context().Done():
			return
		case msg, ok := <-channel:
			if !ok {
				return
			}
			var event DeploymentHealthEvent
			if err := json.Unmarshal([]byte(msg.Payload), &event); err != nil {
				continue
			}
			if event.DeploymentID != deployment.DeploymentID {
				continue
			}
			if err := conn.WriteJSON(event); err != nil {
				return
			}
		}
	}
}

func newDockerClientHandle() (*dockerClientHandle, error) {
	client, err := dockerclient.NewClientWithOpts(dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation())
	if err != nil {
		return nil, err
	}
	return &dockerClientHandle{client: client}, nil
}

func (h *dockerClientHandle) Close() error {
	if h == nil || h.client == nil {
		return nil
	}
	return h.client.Close()
}