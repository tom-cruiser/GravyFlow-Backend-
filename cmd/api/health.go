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

// ============================================================================
// CONSTANTS
// ============================================================================

const (
	healthCheckerInterval      = 30 * time.Second
	healthCheckerWindow        = 5 * time.Minute
	healthCheckTimeout         = 10 * time.Second
	healthCheckRetryCount      = 3
	healthCheckRetryDelay      = 1 * time.Second
	healthCheckerShutdownGrace = 10 * time.Second
)

// ============================================================================
// TYPES
// ============================================================================

type DeploymentHealthConfigRequest struct {
	HealthCheckPath            string `json:"healthCheckPath"`
	HealthCheckIntervalSeconds int    `json:"healthCheckIntervalSeconds"`
	MaxRestartsBeforeFailing   int    `json:"maxRestartsBeforeFailing"`
	HealthCheckTimeoutSeconds  int    `json:"healthCheckTimeoutSeconds,omitempty"`
	RetryCount                 int    `json:"retryCount,omitempty"`
}

type DeploymentHealthEvent struct {
	DeploymentID string    `json:"deploymentId"`
	Status       string    `json:"status"`
	Stage        string    `json:"stage"`
	Message      string    `json:"message"`
	Action       string    `json:"action"`
	ContainerID  string    `json:"containerId,omitempty"`
	Alert        bool      `json:"alert"`
	RetryCount   int       `json:"retryCount,omitempty"`
	Latency      int64     `json:"latencyMs,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
}

type HealthCheckHistory struct {
	ID          string    `json:"id"`
	DeploymentID string   `json:"deploymentId"`
	Status      string    `json:"status"`
	Message     string    `json:"message"`
	LatencyMs   int64     `json:"latencyMs"`
	CreatedAt   time.Time `json:"createdAt"`
}

type HealthCheckStats struct {
	TotalChecks     int     `json:"totalChecks"`
	Successful      int     `json:"successful"`
	Failed          int     `json:"failed"`
	SuccessRate     float64 `json:"successRate"`
	AvgLatencyMs    int64   `json:"avgLatencyMs"`
	MaxLatencyMs    int64   `json:"maxLatencyMs"`
	LastCheckAt     time.Time `json:"lastCheckAt"`
}

type HealthAlertConfig struct {
	WebhookURL string            `json:"webhookUrl"`
	Headers    map[string]string `json:"headers"`
	Enabled    bool              `json:"enabled"`
	OnRestart  bool              `json:"onRestart"`
	OnFailure  bool              `json:"onFailure"`
}

type HealthCheckCircuitBreaker struct {
	mu           sync.RWMutex
	failures     int
	lastFailure  time.Time
	state        string // "closed", "open", "half-open"
	resetTimeout time.Duration
	threshold    int
}

type DeploymentHealthManager struct {
	redisClient    *redis.Client
	httpClient     *http.Client
	circuitBreaker *HealthCheckCircuitBreaker
	shutdownChan   chan struct{}
	wg             sync.WaitGroup
	mu             sync.RWMutex
	alertConfigs   map[string]HealthAlertConfig
	stats          map[string]HealthCheckStats
}

type dockerClientHandle struct {
	client *dockerclient.Client
}

// ============================================================================
// CIRCUIT BREAKER
// ============================================================================

func NewHealthCheckCircuitBreaker(threshold int, resetTimeout time.Duration) *HealthCheckCircuitBreaker {
	return &HealthCheckCircuitBreaker{
		state:        "closed",
		threshold:    threshold,
		resetTimeout: resetTimeout,
	}
}

func (cb *HealthCheckCircuitBreaker) Allow() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	switch cb.state {
	case "closed":
		return true
	case "open":
		if time.Since(cb.lastFailure) > cb.resetTimeout {
			cb.state = "half-open"
			return true
		}
		return false
	case "half-open":
		return true
	default:
		return true
	}
}

func (cb *HealthCheckCircuitBreaker) RecordSuccess() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	if cb.state == "half-open" {
		cb.state = "closed"
		cb.failures = 0
	}
}

func (cb *HealthCheckCircuitBreaker) RecordFailure() {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	cb.failures++
	cb.lastFailure = time.Now()

	if cb.failures >= cb.threshold {
		cb.state = "open"
		log.Printf("[CIRCUIT BREAKER] Opening circuit due to %d failures", cb.failures)
	}
}

// ============================================================================
// DEPLOYMENT HEALTH MANAGER
// ============================================================================

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
		httpClient: &http.Client{
			Timeout: healthCheckTimeout,
			Transport: &http.Transport{
				MaxIdleConns:    100,
				IdleConnTimeout: 90 * time.Second,
			},
		},
		circuitBreaker: NewHealthCheckCircuitBreaker(5, 30*time.Second),
		shutdownChan:   make(chan struct{}),
		alertConfigs:   make(map[string]HealthAlertConfig),
		stats:          make(map[string]HealthCheckStats),
	}, nil
}

func (m *DeploymentHealthManager) Close() {
	if m == nil || m.redisClient == nil {
		return
	}
	
	log.Println("Health checker: shutting down...")
	m.shutdownChan <- struct{}{}
	m.wg.Wait()
	
	_ = m.redisClient.Close()
	log.Println("Health checker: shutdown complete")
}

func (m *DeploymentHealthManager) Start(ctx context.Context) {
	if m == nil {
		return
	}

	m.wg.Add(1)
	defer m.wg.Done()

	log.Println("Health checker: starting...")

	m.checkOnce(ctx)
	ticker := time.NewTicker(healthCheckerInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Println("Health checker: context cancelled")
			return
		case <-m.shutdownChan:
			log.Println("Health checker: shutdown requested")
			return
		case <-ticker.C:
			m.checkOnce(ctx)
		}
	}
}

// ============================================================================
// HEALTH CHECK LOOP
// ============================================================================

func (m *DeploymentHealthManager) checkOnce(ctx context.Context) {
	if deploymentStore == nil {
		return
	}

	// Check circuit breaker
	if !m.circuitBreaker.Allow() {
		log.Println("Health checker: circuit breaker open, skipping checks")
		return
	}

	targets, err := deploymentStore.ListRunningDeploymentsForHealthCheck(ctx)
	if err != nil {
		log.Printf("Health checker: list running deployments: %v", err)
		return
	}
	if len(targets) == 0 {
		return
	}

	dockerClient, err := newDockerClientHandle()
	if err != nil {
		log.Printf("Health checker: create docker client: %v", err)
		return
	}
	defer dockerClient.Close()

	var waitGroup sync.WaitGroup
	semaphore := make(chan struct{}, 10) // Limit concurrent checks

	for _, target := range targets {
		target := target
		
		// Check if it's time to check
		if target.LastCheckedAt != nil {
			elapsed := time.Since(*target.LastCheckedAt)
			if elapsed < time.Duration(target.HealthCheckIntervalSeconds)*time.Second {
				continue
			}
		}

		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()
			
			m.checkDeployment(ctx, dockerClient, target)
		}()
	}
	waitGroup.Wait()

	// Record success in circuit breaker
	m.circuitBreaker.RecordSuccess()
}

// ============================================================================
// DEPLOYMENT HEALTH CHECK
// ============================================================================

func (m *DeploymentHealthManager) checkDeployment(ctx context.Context, dockerClient *dockerClientHandle, target RunningDeploymentHealthTarget) {
	ctx, cancel := context.WithTimeout(ctx, healthCheckTimeout)
	defer cancel()

	// Mark as checked
	defer func() {
		if err := deploymentStore.MarkDeploymentHealthChecked(ctx, target.DeploymentID); err != nil {
			log.Printf("Health checker: mark checked for %s: %v", target.DeploymentID, err)
		}
	}()

	// Perform health check with retries
	healthy, latency, reason := m.inspectAndProbeWithRetry(ctx, dockerClient, target)
	
	// Record history
	_ = m.recordHealthHistory(ctx, target.DeploymentID, healthy, reason, latency)

	if healthy {
		// Update stats
		m.updateStats(target.DeploymentID, true, latency)
		return
	}

	// Update stats for failure
	m.updateStats(target.DeploymentID, false, latency)
	m.circuitBreaker.RecordFailure()

	// Process restart logic
	m.handleUnhealthyDeployment(ctx, dockerClient, target, reason)
}

// ============================================================================
// INSPECT AND PROBE WITH RETRY
// ============================================================================

func (m *DeploymentHealthManager) inspectAndProbeWithRetry(ctx context.Context, dockerClient *dockerClientHandle, target RunningDeploymentHealthTarget) (bool, int64, string) {
	var lastErr error
	var latency int64

	for attempt := 0; attempt < healthCheckRetryCount; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return false, 0, "health check cancelled"
			case <-time.After(healthCheckRetryDelay):
			}
		}

		healthy, l, err := m.inspectAndProbe(ctx, dockerClient, target)
		if healthy {
			return true, l, "healthy"
		}

		lastErr = err
		latency = l
		log.Printf("Health checker: probe attempt %d failed for %s: %v", attempt+1, target.DeploymentID, err)
	}

	return false, latency, lastErr.Error()
}

// ============================================================================
// INSPECT AND PROBE
// ============================================================================

func (m *DeploymentHealthManager) inspectAndProbe(ctx context.Context, dockerClient *dockerClientHandle, target RunningDeploymentHealthTarget) (bool, int64, error) {
	start := time.Now()

	inspect, err := dockerClient.client.ContainerInspect(ctx, target.ContainerID)
	if err != nil {
		return false, 0, fmt.Errorf("container inspect failed: %v", err)
	}

	if inspect.State == nil || !inspect.State.Running {
		return false, 0, fmt.Errorf("container stopped")
	}

	// Check Docker health status
	if inspect.State.Health != nil {
		status := strings.ToLower(inspect.State.Health.Status)
		if status == "unhealthy" {
			return false, 0, fmt.Errorf("container health reported unhealthy")
		}
		if status == "healthy" {
			return true, time.Since(start).Milliseconds(), nil
		}
	}

	// HTTP probe
	healthPath := strings.TrimSpace(target.HealthCheckPath)
	if healthPath == "" {
		healthPath = "/health"
	}
	if !strings.HasPrefix(healthPath, "/") {
		healthPath = "/" + healthPath
	}

	internalIP := ""
	for _, network := range inspect.NetworkSettings.Networks {
		if network.IPAddress != "" {
			internalIP = network.IPAddress
			break
		}
	}
	
	internalPort := ""
	for port := range inspect.Config.ExposedPorts {
		internalPort = port.Port()
		break
	}
	
	if internalIP == "" || internalPort == "" {
		return false, 0, fmt.Errorf("container has no reachable internal endpoint")
	}

	endpoint := fmt.Sprintf("http://%s:%s%s", internalIP, internalPort, healthPath)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false, 0, fmt.Errorf("create health request: %v", err)
	}
	request.Header.Set("User-Agent", "GravyFlow-HealthChecker/1.0")

	response, err := m.httpClient.Do(request)
	if err != nil {
		return false, time.Since(start).Milliseconds(), fmt.Errorf("health probe failed: %v", err)
	}
	defer response.Body.Close()

	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return false, time.Since(start).Milliseconds(), fmt.Errorf("health probe returned %s", response.Status)
	}

	return true, time.Since(start).Milliseconds(), nil
}

// ============================================================================
// HANDLE UNHEALTHY DEPLOYMENT
// ============================================================================

func (m *DeploymentHealthManager) handleUnhealthyDeployment(ctx context.Context, dockerClient *dockerClientHandle, target RunningDeploymentHealthTarget, reason string) {
	maxRestarts := target.MaxRestartsBeforeFailing
	if maxRestarts < 1 {
		maxRestarts = 3
	}

	recentRestarts, err := deploymentStore.CountRecentDeploymentRestarts(ctx, target.DeploymentID, healthCheckerWindow)
	if err != nil {
		log.Printf("Health checker: count restarts for %s: %v", target.DeploymentID, err)
		return
	}

	// Check if reached max restarts
	if recentRestarts >= maxRestarts {
		m.handleMaxRestartsReached(ctx, target, reason, recentRestarts)
		return
	}

	// Attempt restart
	m.attemptRestart(ctx, target, reason)
}

// ============================================================================
// HANDLE MAX RESTARTS REACHED
// ============================================================================

func (m *DeploymentHealthManager) handleMaxRestartsReached(ctx context.Context, target RunningDeploymentHealthTarget, reason string, recentRestarts int) {
	alertMessage := fmt.Sprintf("deployment failed after %d restarts in %s: %s", 
		recentRestarts, healthCheckerWindow, reason)

	log.Printf("[ALERT] %s", alertMessage)

	_ = deploymentStore.RecordDeploymentRestartAudit(ctx, target.DeploymentID, "restart", "alerted", alertMessage, target.ContainerID, "")
	
	if err := deploymentStore.MarkDeploymentFailed(ctx, target.DeploymentID, errors.New(alertMessage)); err != nil {
		log.Printf("Health checker: mark failed for %s: %v", target.DeploymentID, err)
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

	// Send webhook alert
	m.sendWebhookAlert(ctx, target.DeploymentID, "failure", alertMessage)
}

// ============================================================================
// ATTEMPT RESTART
// ============================================================================

func (m *DeploymentHealthManager) attemptRestart(ctx context.Context, target RunningDeploymentHealthTarget, reason string) {
	log.Printf("Health checker: attempting restart for %s", target.DeploymentID)

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

	// Load environment variables
	envMap, err := deploymentStore.LoadDeploymentEnvMapByDeploymentID(ctx, target.DeploymentID)
	if err != nil {
		log.Printf("Health checker: load env vars for %s: %v", target.DeploymentID, err)
		return
	}

	// Restart container
	restartedContainerID, err := RestartContainer(
		target.ContainerID,
		target.ImageName,
		target.AppName,
		target.DeploymentID,
		normalizePortMap(target.PortMap),
		loadDockerEnvList(envMap),
		defaultDeployMemoryMB,
		defaultDeployCPU,
	)
	if err != nil {
		m.handleRestartFailure(ctx, target, err)
		return
	}

	// Handle successful restart
	m.handleRestartSuccess(ctx, target, restartedContainerID, reason)
}

// ============================================================================
// RESTART SUCCESS/FAILURE HANDLERS
// ============================================================================

func (m *DeploymentHealthManager) handleRestartSuccess(ctx context.Context, target RunningDeploymentHealthTarget, restartedContainerID string, reason string) {
	_ = deploymentStore.RecordDeploymentRestartAudit(ctx, target.DeploymentID, "restart", "succeeded", reason, target.ContainerID, restartedContainerID)
	
	if err := deploymentStore.MarkDeploymentDeployed(ctx, target.DeploymentID, restartedContainerID, target.AppName, target.ImageName); err != nil {
		log.Printf("Health checker: update running deployment for %s: %v", target.DeploymentID, err)
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

	// Send webhook alert
	m.sendWebhookAlert(ctx, target.DeploymentID, "restart", "container restarted successfully")
}

func (m *DeploymentHealthManager) handleRestartFailure(ctx context.Context, target RunningDeploymentHealthTarget, err error) {
	_ = deploymentStore.RecordDeploymentRestartAudit(ctx, target.DeploymentID, "restart", "failed", err.Error(), target.ContainerID, "")
	
	if markErr := deploymentStore.MarkDeploymentFailed(ctx, target.DeploymentID, err); markErr != nil {
		log.Printf("Health checker: mark failed after restart error for %s: %v", target.DeploymentID, markErr)
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

	// Send webhook alert
	m.sendWebhookAlert(ctx, target.DeploymentID, "failure", err.Error())
}

// ============================================================================
// HEALTH HISTORY
// ============================================================================

func (m *DeploymentHealthManager) recordHealthHistory(ctx context.Context, deploymentID string, healthy bool, message string, latency int64) error {
	if m == nil || m.redisClient == nil {
		return nil
	}

	status := "healthy"
	if !healthy {
		status = "unhealthy"
	}

	history := HealthCheckHistory{
		DeploymentID: deploymentID,
		Status:       status,
		Message:      message,
		LatencyMs:    latency,
		CreatedAt:    time.Now().UTC(),
	}

	data, err := json.Marshal(history)
	if err != nil {
		return err
	}

	key := fmt.Sprintf("deployment:health:history:%s", deploymentID)
	pipe := m.redisClient.Pipeline()
	pipe.LPush(ctx, key, data)
	pipe.LTrim(ctx, key, 0, 99) // Keep last 100 entries
	pipe.Expire(ctx, key, 24*time.Hour)

	_, err = pipe.Exec(ctx)
	return err
}

func (m *DeploymentHealthManager) GetHealthHistory(ctx context.Context, deploymentID string, limit int) ([]HealthCheckHistory, error) {
	if m == nil || m.redisClient == nil {
		return nil, nil
	}

	if limit <= 0 || limit > 100 {
		limit = 20
	}

	key := fmt.Sprintf("deployment:health:history:%s", deploymentID)
	results, err := m.redisClient.LRange(ctx, key, 0, int64(limit-1)).Result()
	if err != nil {
		return nil, err
	}

	history := make([]HealthCheckHistory, 0, len(results))
	for _, result := range results {
		var entry HealthCheckHistory
		if err := json.Unmarshal([]byte(result), &entry); err != nil {
			continue
		}
		history = append(history, entry)
	}

	return history, nil
}

// ============================================================================
// STATS MANAGEMENT
// ============================================================================

func (m *DeploymentHealthManager) updateStats(deploymentID string, success bool, latency int64) {
	m.mu.Lock()
	defer m.mu.Unlock()

	stats, ok := m.stats[deploymentID]
	if !ok {
		stats = HealthCheckStats{}
	}

	stats.TotalChecks++
	if success {
		stats.Successful++
	} else {
		stats.Failed++
	}
	
	if stats.TotalChecks > 0 {
		stats.SuccessRate = float64(stats.Successful) / float64(stats.TotalChecks) * 100
	}
	
	// Update latency stats
	stats.AvgLatencyMs = (stats.AvgLatencyMs + latency) / 2
	if latency > stats.MaxLatencyMs {
		stats.MaxLatencyMs = latency
	}
	
	stats.LastCheckAt = time.Now()
	m.stats[deploymentID] = stats
}

func (m *DeploymentHealthManager) GetHealthStats(deploymentID string) (HealthCheckStats, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	stats, ok := m.stats[deploymentID]
	return stats, ok
}

// ============================================================================
// EVENT PUBLISHING
// ============================================================================

func (m *DeploymentHealthManager) publishEvent(ctx context.Context, event DeploymentHealthEvent) {
	if m == nil || m.redisClient == nil {
		return
	}

	payload, err := json.Marshal(event)
	if err != nil {
		return
	}
	
	channel := deploymentHealthChannel(event.DeploymentID)
	if err := m.redisClient.Publish(ctx, channel, payload).Err(); err != nil {
		log.Printf("Health checker: publish event for %s: %v", event.DeploymentID, err)
	}
}

func deploymentHealthChannel(deploymentID string) string {
	return fmt.Sprintf("deployment:health:%s", strings.TrimSpace(deploymentID))
}

// ============================================================================
// WEBHOOK ALERTS
// ============================================================================

func (m *DeploymentHealthManager) SetAlertConfig(deploymentID string, config HealthAlertConfig) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.alertConfigs[deploymentID] = config
}

func (m *DeploymentHealthManager) sendWebhookAlert(ctx context.Context, deploymentID string, eventType string, message string) {
	m.mu.RLock()
	config, ok := m.alertConfigs[deploymentID]
	m.mu.RUnlock()

	if !ok || !config.Enabled {
		return
	}

	if eventType == "restart" && !config.OnRestart {
		return
	}
	if eventType == "failure" && !config.OnFailure {
		return
	}

	payload := map[string]interface{}{
		"deploymentId": deploymentID,
		"event":        eventType,
		"message":      message,
		"timestamp":    time.Now().UTC().Format(time.RFC3339),
	}

	data, err := json.Marshal(payload)
	if err != nil {
		log.Printf("Health checker: marshal webhook payload: %v", err)
		return
	}

	req, err := http.NewRequestWithContext(ctx, "POST", config.WebhookURL, strings.NewReader(string(data)))
	if err != nil {
		log.Printf("Health checker: create webhook request: %v", err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	for key, value := range config.Headers {
		req.Header.Set(key, value)
	}

	resp, err := m.httpClient.Do(req)
	if err != nil {
		log.Printf("Health checker: send webhook alert: %v", err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		log.Printf("Health checker: webhook returned %s", resp.Status)
	}
}

// ============================================================================
// DOCKER CLIENT HELPER
// ============================================================================

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

// ============================================================================
// API HANDLERS
// ============================================================================

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

	// Validate and set defaults
	path := strings.TrimSpace(req.HealthCheckPath)
	if path == "" {
		path = "/health"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	
	interval := req.HealthCheckIntervalSeconds
	if interval < 5 {
		interval = 30
	}
	
	maxRestarts := req.MaxRestartsBeforeFailing
	if maxRestarts < 1 {
		maxRestarts = 3
	}

	timeout := req.HealthCheckTimeoutSeconds
	if timeout < 1 {
		timeout = 10
	}

	retryCount := req.RetryCount
	if retryCount < 1 {
		retryCount = 3
	}

	if err := deploymentStore.UpsertDeploymentHealthConfig(
		c.Request.Context(), 
		user.ID, 
		deployment.DeploymentID, 
		path, 
		interval, 
		maxRestarts,
	); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":   "failed_to_save_health_config",
			"details": err.Error(),
		})
		return
	}

	config, err := deploymentStore.GetDeploymentHealthConfigForUser(
		c.Request.Context(), 
		user.ID, 
		deployment.DeploymentID,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":   "failed_to_load_health_config",
			"details": err.Error(),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"healthConfig": config,
		"timeout":      timeout,
		"retryCount":   retryCount,
	})
}

func getDeploymentHealthHandler(c *gin.Context) {
	user, deployment, ok := currentUserDeployment(c)
	if !ok {
		return
	}

	config, err := deploymentStore.GetDeploymentHealthConfigForUser(
		c.Request.Context(), 
		user.ID, 
		deployment.DeploymentID,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":   "failed_to_load_health_config",
			"details": err.Error(),
		})
		return
	}

	// Get stats
	var stats HealthCheckStats
	if deploymentHealthManager != nil {
		if s, ok := deploymentHealthManager.GetHealthStats(deployment.DeploymentID); ok {
			stats = s
		}
	}

	// Get recent history
	var history []HealthCheckHistory
	if deploymentHealthManager != nil {
		if h, err := deploymentHealthManager.GetHealthHistory(
			c.Request.Context(), deployment.DeploymentID, 10,
		); err == nil {
			history = h
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"healthConfig": config,
		"stats":        stats,
		"history":      history,
		"count":        len(history),
	})
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

	config, err := deploymentStore.GetDeploymentHealthConfigForUser(
		c.Request.Context(), 
		user.ID, 
		deployment.DeploymentID,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":   "failed_to_load_health_config",
			"details": err.Error(),
		})
		return
	}

	conn, err := logsWebsocketUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	// Send initial config
	if err := conn.WriteJSON(gin.H{"healthConfig": config}); err != nil {
		return
	}

	pubsub := deploymentHealthManager.redisClient.Subscribe(
		c.Request.Context(), 
		deploymentHealthChannel(deployment.DeploymentID),
	)
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

func setHealthAlertHandler(c *gin.Context) {
	user, deployment, ok := currentUserDeployment(c)
	if !ok {
		return
	}

	var config HealthAlertConfig
	if err := c.ShouldBindJSON(&config); err != nil {
		sendBadRequest(c, "invalid JSON body", err)
		return
	}

	if config.Enabled && config.WebhookURL == "" {
		sendBadRequest(c, "webhookUrl is required when enabled", nil)
		return
	}

	if deploymentHealthManager != nil {
		deploymentHealthManager.SetAlertConfig(deployment.DeploymentID, config)
	}

	c.JSON(http.StatusOK, gin.H{
		"message": "alert configuration saved",
		"config":  config,
	})
}

// ============================================================================
// ROUTE SETUP
// ============================================================================

func SetupHealthRoutes(router *gin.Engine) {
	healthGroup := router.Group("/api/deployments/:id/health")
	healthGroup.Use(AuthMiddleware(false))
	{
		healthGroup.GET("/", getDeploymentHealthHandler)
		healthGroup.POST("/config", updateDeploymentHealthConfigHandler)
		healthGroup.GET("/stream", streamDeploymentHealthHandler)
		healthGroup.POST("/alerts", setHealthAlertHandler)
	}
}

// ============================================================================
// INITIALIZATION
// ============================================================================

func InitHealthChecker(ctx context.Context) error {
	manager, err := newDeploymentHealthManager()
	if err != nil {
		return err
	}
	
	deploymentHealthManager = manager
	
	// Start health checker
	go deploymentHealthManager.Start(ctx)
	
	log.Println("Health checker initialized")
	return nil
}