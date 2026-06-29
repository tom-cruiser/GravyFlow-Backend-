package main

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

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

	jobID, err := deploymentJobs.EnqueueDeployment(c.Request.Context(), user.ID, deployment.DeploymentID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_restart_service", "details": err.Error()})
		return
	}

	// Audited as "manual_restart" so it stays out of the crash-loop budget the
	// health checker computes from action = 'restart'.
	_ = deploymentStore.RecordDeploymentRestartAudit(c.Request.Context(), deployment.DeploymentID, "manual_restart", "queued", "manual restart via api", deployment.ContainerID, "")

	c.JSON(http.StatusAccepted, gin.H{
		"message":      "service restart queued",
		"deploymentId": deployment.DeploymentID,
		"jobId":        jobID,
		"status":       "queued",
	})
}
