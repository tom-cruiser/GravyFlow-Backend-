package main

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

type DeploymentEnvRequest struct {
	Key   string `json:"key" binding:"required"`
	Value string `json:"value"`
}

func listAppEnvHandler(c *gin.Context) {
	user, deployment, ok := currentUserDeployment(c)
	if !ok {
		return
	}

	envVars, err := deploymentStore.ListDeploymentEnvVars(c.Request.Context(), user.ID, deployment.DeploymentID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_list_env", "details": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"envVars": envVars, "count": len(envVars)})
}

func addAppEnvHandler(c *gin.Context) {
	user, deployment, ok := currentUserDeployment(c)
	if !ok {
		return
	}

	var req DeploymentEnvRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		sendBadRequest(c, "invalid JSON body", err)
		return
	}

	if strings.TrimSpace(req.Key) == "" {
		sendBadRequest(c, "key is required", nil)
		return
	}

	if err := deploymentStore.UpsertDeploymentEnvVar(c.Request.Context(), user.ID, deployment.DeploymentID, req.Key, req.Value); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_store_env", "details": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, gin.H{"message": "environment variable saved", "key": normalizeEnvKey(req.Key)})
}

func deleteAppEnvHandler(c *gin.Context) {
	user, deployment, ok := currentUserDeployment(c)
	if !ok {
		return
	}

	key := strings.TrimSpace(c.Param("key"))
	if key == "" {
		sendBadRequest(c, "env key is required", nil)
		return
	}

	if err := deploymentStore.DeleteDeploymentEnvVar(c.Request.Context(), user.ID, deployment.DeploymentID, key); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_delete_env", "details": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "environment variable removed", "key": normalizeEnvKey(key)})
}
