package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	dockerclient "github.com/docker/docker/client"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"golang.org/x/time/rate"
)

// ============================================================================
// CONSTANTS
// ============================================================================

const (
	defaultPort           = "8080"
	defaultShutdownGrace  = 30 * time.Second
	defaultWriteTimeout   = 15 * time.Second
	defaultReadTimeout    = 15 * time.Second
	defaultIdleTimeout    = 60 * time.Second
	requestIDHeader       = "X-Request-ID"
)

// ============================================================================
// TYPES
// ============================================================================

type CreateAppRequest struct {
	Name string `json:"name" binding:"required"`
	Repo string `json:"repo" binding:"required"`
}

type AppResponse struct {
	Name string `json:"name"`
	Repo string `json:"repo"`
}

type ServerConfig struct {
	Port           string
	ShutdownGrace  time.Duration
	WriteTimeout   time.Duration
	ReadTimeout    time.Duration
	IdleTimeout    time.Duration
	RateLimit      rate.Limit
	RateLimitBurst int
	Debug          bool
}

// ============================================================================
// MAIN
// ============================================================================

func main() {
	if err := run(); err != nil {
		log.Fatalf("server stopped: %v", err)
	}
}

func run() error {
	// Load configuration
	config := loadConfig()

	// Setup logging
	setupLogging(config.Debug)

	// Initialize database
	store, err := newDeploymentStore(context.Background())
	if err != nil {
		return fmt.Errorf("init deployment store: %w", err)
	}
	deploymentStore = store
	defer deploymentStore.Close()

	// Initialize job manager
	jobs, err := newDeploymentJobManager()
	if err != nil {
		return fmt.Errorf("init deployment job manager: %w", err)
	}
	deploymentJobs = jobs
	defer deploymentJobs.Close()

	// Start worker
	go startWorker(jobs)

	// Setup router
	router := setupRouter(config)

	// Create server
	srv := &http.Server{
		Addr:         ":" + config.Port,
		Handler:      router,
		WriteTimeout: config.WriteTimeout,
		ReadTimeout:  config.ReadTimeout,
		IdleTimeout:  config.IdleTimeout,
	}

	// Start server with graceful shutdown
	return runServer(srv, config)
}

// ============================================================================
// CONFIGURATION
// ============================================================================

func loadConfig() ServerConfig {
	port := os.Getenv("PORT")
	if port == "" {
		port = defaultPort
	}

	return ServerConfig{
		Port:           port,
		ShutdownGrace:  getDurationEnv("SHUTDOWN_GRACE", defaultShutdownGrace),
		WriteTimeout:   getDurationEnv("WRITE_TIMEOUT", defaultWriteTimeout),
		ReadTimeout:    getDurationEnv("READ_TIMEOUT", defaultReadTimeout),
		IdleTimeout:    getDurationEnv("IDLE_TIMEOUT", defaultIdleTimeout),
		RateLimit:      rate.Limit(getFloatEnv("RATE_LIMIT", 100)),
		RateLimitBurst: getIntEnv("RATE_LIMIT_BURST", 200),
		Debug:          os.Getenv("DEBUG") == "true",
	}
}

func getDurationEnv(key string, fallback time.Duration) time.Duration {
	if val := os.Getenv(key); val != "" {
		if d, err := time.ParseDuration(val); err == nil {
			return d
		}
	}
	return fallback
}

func getIntEnv(key string, fallback int) int {
	if val := os.Getenv(key); val != "" {
		if i, err := strconv.Atoi(val); err == nil {
			return i
		}
	}
	return fallback
}

func getFloatEnv(key string, fallback float64) float64 {
	if val := os.Getenv(key); val != "" {
		if f, err := strconv.ParseFloat(val, 64); err == nil {
			return f
		}
	}
	return fallback
}

// ============================================================================
// LOGGING
// ============================================================================

func setupLogging(debug bool) {
	if debug {
		log.SetFlags(log.LstdFlags | log.Lshortfile)
	} else {
		log.SetFlags(log.LstdFlags)
	}
	log.Printf("Starting GravyFlow API server (debug: %v)", debug)
}

func logRequest(c *gin.Context) {
	// Skip logging for health checks
	if c.Request.URL.Path == "/api/health" {
		return
	}
	
	status := c.Writer.Status()
	method := c.Request.Method
	path := c.Request.URL.Path
	clientIP := c.ClientIP()
	requestID := c.GetString("requestID")
	
	log.Printf("[%s] %s %s %d from %s", requestID, method, path, status, clientIP)
}

// ============================================================================
// WORKER
// ============================================================================

func startWorker(jobs *DeploymentJobManager) {
	log.Println("Starting deployment worker...")
	if err := jobs.asynqServer.Run(jobs.ServeMux()); err != nil {
		log.Fatalf("deployment worker stopped: %v (restart the API)", err)
	}
	log.Fatal("deployment worker exited unexpectedly (restart the API)")
}

// ============================================================================
// ROUTER SETUP
// ============================================================================

func setupRouter(config ServerConfig) *gin.Engine {
	// Set Gin mode
	if !config.Debug {
		gin.SetMode(gin.ReleaseMode)
	}

	router := gin.New()
	
	// Global middleware
	router.Use(gin.LoggerWithConfig(gin.LoggerConfig{
		SkipPaths: []string{"/api/health", "/metrics"},
	}))
	router.Use(gin.Recovery())
	router.Use(requestIDMiddleware())
	router.Use(corsMiddleware())
	router.Use(rateLimitMiddleware(config))
	router.Use(responseTimeMiddleware())

	// Health check endpoints
	router.GET("/health", healthHandler)
	router.GET("/api/health", healthHandler)
	
	// Metrics endpoint (for Prometheus)
	if os.Getenv("ENABLE_METRICS") == "true" {
		router.GET("/metrics", gin.WrapH(promhttp.Handler()))
	}

	// API routes
	api := router.Group("/api/v1")
	{
		// Public routes
		api.POST("/auth/register", registerHandler)
		api.POST("/auth/login", loginHandler)
		api.POST("/auth/refresh", refreshHandler)

		// Protected routes
		protected := api.Group("/")
		protected.Use(AuthMiddleware(false))
		{
			// Auth
			protected.POST("/auth/api-keys", AuthMiddleware(true), createAPIKeyHandler)
			
			// Apps
			protected.GET("/apps", listAppsHandler)
			protected.POST("/apps", createAppHandler)
			protected.POST("/apps/:id/deploy", deploymentDeployHandler)
			// restartAppHandler is now in restart_handlers.go
			protected.POST("/apps/:id/restart", restartAppHandler)
			protected.GET("/apps/:id/logs", streamAppLogsHandler)
			protected.GET("/apps/:id/deploy-log", deployLogHandler)

			// Environment variables
			protected.GET("/apps/:id/env", listAppEnvHandler)
			protected.POST("/apps/:id/env", addAppEnvHandler)
			protected.DELETE("/apps/:id/env/:key", deleteAppEnvHandler)

			// Custom domains
			protected.GET("/apps/:id/domains", listAppDomainsHandler)
			protected.POST("/apps/:id/domains", addAppDomainHandler)
			protected.POST("/apps/:id/domains/:domain/verify", verifyAppDomainHandler)
			protected.DELETE("/apps/:id/domains/:domain", deleteAppDomainHandler)

			// Jobs
			protected.GET("/jobs/:jobId", deploymentJobStatusHandler)
			protected.GET("/jobs/:jobId/stream", deploymentJobStatusHandler)

			// Quota
			protected.GET("/users/:id/quota", quotaSummaryHandler)
		}
	}

	// 404 handler
	router.NoRoute(func(c *gin.Context) {
		c.JSON(http.StatusNotFound, gin.H{
			"error":   "not_found",
			"message": "Endpoint not found",
			"path":    c.Request.URL.Path,
		})
	})

	return router
}

// ============================================================================
// MIDDLEWARE
// ============================================================================

func requestIDMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		requestID := c.GetHeader(requestIDHeader)
		if requestID == "" {
			requestID = uuid.New().String()
		}
		c.Set("requestID", requestID)
		c.Header(requestIDHeader, requestID)
		c.Next()
	}
}

func rateLimitMiddleware(config ServerConfig) gin.HandlerFunc {
	limiter := rate.NewLimiter(config.RateLimit, config.RateLimitBurst)
	
	return func(c *gin.Context) {
		if !limiter.Allow() {
			c.JSON(http.StatusTooManyRequests, gin.H{
				"error":   "rate_limit_exceeded",
				"message": "Too many requests. Please try again later.",
				"retry_after": "60",
			})
			c.Abort()
			return
		}
		c.Next()
	}
}

func responseTimeMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		
		duration := time.Since(start)
		
		// Log slow requests
		if duration > 5*time.Second {
			requestID := c.GetString("requestID")
			log.Printf("[SLOW] %s %s took %v", requestID, c.Request.URL.Path, duration)
		}
	}
}

func corsMiddleware() gin.HandlerFunc {
	allowedOriginPrefixes := []string{
		"http://localhost:",
		"https://localhost:",
		"http://127.0.0.1:",
		"https://127.0.0.1:",
	}

	return func(c *gin.Context) {
		origin := strings.TrimSpace(c.GetHeader("Origin"))
		if origin != "" {
			for _, prefix := range allowedOriginPrefixes {
				if strings.HasPrefix(origin, prefix) {
					c.Header("Access-Control-Allow-Origin", origin)
					c.Header("Vary", "Origin")
					c.Header("Access-Control-Allow-Credentials", "true")
					c.Header("Access-Control-Allow-Headers", "Authorization, Content-Type, X-API-Key, X-Request-ID")
					c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
					c.Header("Access-Control-Expose-Headers", "X-Request-ID")
					break
				}
			}
		}

		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}

		c.Next()
	}
}

// ============================================================================
// SERVER
// ============================================================================

func runServer(srv *http.Server, config ServerConfig) error {
	// Start server in goroutine
	go func() {
		log.Printf("Server starting on port %s", config.Port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("Server failed: %v", err)
		}
	}()

	// Wait for interrupt signal
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Printf("Shutting down server...")

	// Create shutdown context with timeout
	ctx, cancel := context.WithTimeout(context.Background(), config.ShutdownGrace)
	defer cancel()

	// Shutdown server
	if err := srv.Shutdown(ctx); err != nil {
		return fmt.Errorf("server shutdown failed: %w", err)
	}

	log.Println("Server stopped gracefully")
	return nil
}

// ============================================================================
// HANDLERS
// ============================================================================

func healthHandler(c *gin.Context) {
	// Detailed health check
	health := gin.H{
		"status":    "ok",
		"timestamp": time.Now().UTC().Format(time.RFC3339),
		"version":   "1.0.0",
		"services": gin.H{
			"database": checkDatabaseHealth(),
			"redis":    checkRedisHealth(),
			"docker":   checkDockerHealth(),
		},
	}
	
	// Check if all services are healthy
	allHealthy := true
	for _, svc := range health["services"].(gin.H) {
		if svc != "healthy" {
			allHealthy = false
			break
		}
	}
	
	if !allHealthy {
		c.JSON(http.StatusServiceUnavailable, health)
		return
	}
	
	c.JSON(http.StatusOK, health)
}

func checkDatabaseHealth() string {
	if deploymentStore == nil {
		return "unavailable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	
	if err := deploymentStore.HealthCheck(ctx); err != nil {
		log.Printf("Database health check failed: %v", err)
		return "unhealthy"
	}
	return "healthy"
}

func checkRedisHealth() string {
	if deploymentJobs == nil || deploymentJobs.redisClient == nil {
		return "unavailable"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	
	if err := deploymentJobs.redisClient.Ping(ctx).Err(); err != nil {
		log.Printf("Redis health check failed: %v", err)
		return "unhealthy"
	}
	return "healthy"
}

func checkDockerHealth() string {
	// Quick Docker daemon check
	client, err := dockerclient.NewClientWithOpts(dockerclient.FromEnv, dockerclient.WithAPIVersionNegotiation())
	if err != nil {
		return "unavailable"
	}
	defer client.Close()
	
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	
	if _, err := client.Ping(ctx); err != nil {
		log.Printf("Docker health check failed: %v", err)
		return "unhealthy"
	}
	return "healthy"
}

// ============================================================================
// EXISTING HANDLERS (Updated)
// ============================================================================

func createAppHandler(c *gin.Context) {
	user, ok := currentAuthUser(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	var req CreateAppRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		sendBadRequest(c, "invalid JSON body", err)
		return
	}

	req.Name = strings.TrimSpace(req.Name)
	req.Repo = strings.TrimSpace(req.Repo)
	if req.Name == "" || req.Repo == "" {
		sendBadRequest(c, "name and repo are required", nil)
		return
	}

	// Validate repo URL
	if !strings.HasPrefix(req.Repo, "http") && !strings.HasPrefix(req.Repo, "git@") {
		sendBadRequest(c, "invalid repo URL format", nil)
		return
	}

	portMap := allocatePortMap("8080")

	deploymentID, err := deploymentStore.CreateDeploymentAttemptForUser(
		c.Request.Context(),
		user.ID,
		req.Name,
		req.Repo,
		req.Repo,
		portMap,
		"",
	)
	if err != nil {
		if strings.Contains(err.Error(), "quota exceeded") {
			c.JSON(http.StatusUnprocessableEntity, gin.H{
				"error":   "quota_exceeded",
				"details": err.Error(),
			})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":   "failed_to_create_app",
			"details": err.Error(),
			"request_id": c.GetString("requestID"),
		})
		return
	}

	jobID, err := deploymentJobs.EnqueueDeployment(c.Request.Context(), user.ID, deploymentID, true)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":   "failed_to_enqueue_deployment",
			"details": err.Error(),
			"request_id": c.GetString("requestID"),
		})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"message":      "app created and deployment queued",
		"deploymentId": deploymentID,
		"jobId":        jobID,
		"app": AppResponse{
			Name: req.Name,
			Repo: req.Repo,
		},
	})
}

func listAppsHandler(c *gin.Context) {
	user, ok := currentAuthUser(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	deployments, err := deploymentStore.ListDeploymentsForUser(c.Request.Context(), user.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":   "failed_to_list_apps",
			"details": err.Error(),
			"request_id": c.GetString("requestID"),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"apps":  deployments,
		"count": len(deployments),
	})
}

func quotaSummaryHandler(c *gin.Context) {
	user, ok := currentAuthUser(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	requestedUserID := strings.TrimSpace(c.Param("id"))
	if requestedUserID == "" {
		sendBadRequest(c, "user id is required", nil)
		return
	}
	if requestedUserID != user.ID {
		c.JSON(http.StatusForbidden, gin.H{
			"error":   "forbidden",
			"details": "quota access is limited to the authenticated user",
		})
		return
	}

	summary, err := deploymentStore.GetQuotaSummary(c.Request.Context(), user.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":   "failed_to_load_quota",
			"details": err.Error(),
			"request_id": c.GetString("requestID"),
		})
		return
	}

	c.JSON(http.StatusOK, summary)
}

// Note: restartAppHandler is now in restart_handlers.go
// The duplicate in main.go has been removed

// ============================================================================
// UTILITY FUNCTIONS
// ============================================================================

func sendBadRequest(c *gin.Context, message string, err error) {
	payload := gin.H{
		"error":   message,
		"request_id": c.GetString("requestID"),
	}
	if err != nil {
		payload["details"] = err.Error()
	}
	c.JSON(http.StatusBadRequest, payload)
}

// ============================================================================
// REQUIRED IMPORTS (Add to existing imports)
// ============================================================================

// Add these imports to your existing import block:
/*
import (
    "github.com/google/uuid"
    "github.com/prometheus/client_golang/prometheus"
    "github.com/prometheus/client_golang/prometheus/promhttp"
    "golang.org/x/time/rate"
)
*/

// ============================================================================
// USAGE EXAMPLES
// ============================================================================

/*
ENVIRONMENT VARIABLES:

# Server
PORT=8080
SHUTDOWN_GRACE=30s
WRITE_TIMEOUT=15s
READ_TIMEOUT=15s
IDLE_TIMEOUT=60s
RATE_LIMIT=100
RATE_LIMIT_BURST=200
DEBUG=false
ENABLE_METRICS=true

# Database
DATABASE_URL=postgresql://...
PGHOST=localhost
PGPORT=5432
PGDATABASE=gravyflow
PGUSER=user
PGPASSWORD=password

# Redis
REDIS_ADDR=localhost:6379
REDIS_PASSWORD=password
REDIS_DB=0

# JWT
AUTH_JWT_SECRET=your-secret-key

# Encryption
APP_ENV_ENCRYPTION_KEY=your-encryption-key
*/