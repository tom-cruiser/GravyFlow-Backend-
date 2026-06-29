package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
)

type CreateAppRequest struct {
	Name string `json:"name" binding:"required"`
	Repo string `json:"repo" binding:"required"`
}

type AppResponse struct {
	Name string `json:"name"`
	Repo string `json:"repo"`
}

func main() {
	if err := run(); err != nil {
		log.Fatalf("server stopped: %v", err)
	}
}

func run() error {
	store, err := newDeploymentStore(context.Background())
	if err != nil {
		return err
	}
	deploymentStore = store
	defer deploymentStore.Close()

	jobs, err := newDeploymentJobManager()
	if err != nil {
		return fmt.Errorf("init deployment job manager: %w", err)
	}
	deploymentJobs = jobs
	defer deploymentJobs.Close()

	go func() {
		if err := jobs.asynqServer.Run(jobs.ServeMux()); err != nil {
			log.Printf("asynq server stopped: %v", err)
		}
	}()

	router := gin.New()
	router.Use(gin.Logger(), gin.Recovery(), corsMiddleware())

	api := router.Group("/api")
	{
		api.GET("/health", healthHandler)
		api.POST("/auth/register", registerHandler)
		api.POST("/auth/login", loginHandler)
		api.POST("/auth/refresh", refreshHandler)
		api.POST("/auth/api-keys", AuthMiddleware(true), createAPIKeyHandler)
		api.GET("/apps", AuthMiddleware(true), listAppsHandler)
		api.POST("/apps", AuthMiddleware(true), createAppHandler)
		api.POST("/apps/:id/deploy", AuthMiddleware(true), deploymentDeployHandler)
		api.POST("/apps/:id/restart", AuthMiddleware(true), restartAppHandler)
		api.GET("/apps/:id/logs", AuthMiddleware(true), streamAppLogsHandler)

		// Environment variables (encrypted at rest by the control plane).
		api.GET("/apps/:id/env", AuthMiddleware(true), listAppEnvHandler)
		api.POST("/apps/:id/env", AuthMiddleware(true), addAppEnvHandler)
		api.DELETE("/apps/:id/env/:key", AuthMiddleware(true), deleteAppEnvHandler)

		// Custom domains + DNS challenge verification.
		api.GET("/apps/:id/domains", AuthMiddleware(true), listAppDomainsHandler)
		api.POST("/apps/:id/domains", AuthMiddleware(true), addAppDomainHandler)
		api.POST("/apps/:id/domains/:domain/verify", AuthMiddleware(true), verifyAppDomainHandler)
		api.DELETE("/apps/:id/domains/:domain", AuthMiddleware(true), deleteAppDomainHandler)
		api.GET("/jobs/:jobId", AuthMiddleware(true), deploymentJobStatusHandler)
		api.GET("/jobs/:jobId/stream", AuthMiddleware(true), deploymentJobStatusHandler)
		api.GET("/users/:id/quota", AuthMiddleware(true), quotaSummaryHandler)
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	if err := router.Run(":" + port); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}

	return nil
}

func healthHandler(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

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

	// Default port map: expose container port 8080 on host port 8080
	portMap := "8080:8080"

	deploymentID, err := deploymentStore.CreateDeploymentAttemptForUser(
		c.Request.Context(),
		user.ID,
		req.Name,
		req.Repo,
		req.Repo, // appPath — use repo URL as path until clone step
		portMap,
		"", // imageName — built by the worker
	)
	if err != nil {
		if strings.Contains(err.Error(), "quota exceeded") {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "quota_exceeded", "details": err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_create_app", "details": err.Error()})
		return
	}

	jobID, err := deploymentJobs.EnqueueDeployment(c.Request.Context(), user.ID, deploymentID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_enqueue_deployment", "details": err.Error()})
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
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_list_apps", "details": err.Error()})
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
		c.JSON(http.StatusForbidden, gin.H{"error": "forbidden", "details": "quota access is limited to the authenticated user"})
		return
	}

	summary, err := deploymentStore.GetQuotaSummary(c.Request.Context(), user.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_load_quota", "details": err.Error()})
		return
	}

	c.JSON(http.StatusOK, summary)
}

func sendBadRequest(c *gin.Context, message string, err error) {
	payload := gin.H{"error": message}
	if err != nil {
		payload["details"] = err.Error()
	}

	c.JSON(http.StatusBadRequest, payload)
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
					c.Header("Access-Control-Allow-Headers", "Authorization, Content-Type, X-API-Key")
					c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
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
