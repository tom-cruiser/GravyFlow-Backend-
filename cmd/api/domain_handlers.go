package main

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

type DeploymentDomainRequest struct {
	CustomDomain string `json:"customDomain" binding:"required"`
}

func listAppDomainsHandler(c *gin.Context) {
	user, deployment, ok := currentUserDeployment(c)
	if !ok {
		return
	}

	domains, err := deploymentStore.ListDeploymentDomains(c.Request.Context(), user.ID, deployment.DeploymentID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_list_domains", "details": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"domains": domains, "count": len(domains)})
}

func addAppDomainHandler(c *gin.Context) {
	user, deployment, ok := currentUserDeployment(c)
	if !ok {
		return
	}

	var req DeploymentDomainRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		sendBadRequest(c, "invalid JSON body", err)
		return
	}

	req.CustomDomain = normalizeCustomDomain(req.CustomDomain)
	if req.CustomDomain == "" {
		sendBadRequest(c, "customDomain is required", nil)
		return
	}

	record, err := deploymentStore.UpsertDeploymentDomain(c.Request.Context(), user.ID, deployment.DeploymentID, req.CustomDomain)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "already attached to another deployment") {
			c.JSON(http.StatusConflict, gin.H{"error": "domain_already_in_use", "details": err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_store_domain", "details": err.Error()})
		return
	}

	verifiedRecord, verifyErr := deploymentStore.VerifyDeploymentDomain(c.Request.Context(), user.ID, deployment.DeploymentID, req.CustomDomain)
	if verifyErr == nil {
		if syncErr := SyncCaddyRoutesFromRunningContainers(); syncErr != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_sync_caddy", "details": syncErr.Error()})
			return
		}
		c.JSON(http.StatusOK, gin.H{"domain": verifiedRecord, "status": "verified"})
		return
	}

	challengeName := verifyTXTChallengeName(record.CustomDomain)
	challengeInstructions := fmt.Sprintf("Create a TXT record at %s with value %s, then call the verify endpoint.", challengeName, record.VerificationToken)
	c.JSON(http.StatusAccepted, gin.H{
		"domain":      record,
		"status":      "pending",
		"challenge":   challengeName,
		"instruction": challengeInstructions,
	})
}

func verifyAppDomainHandler(c *gin.Context) {
	user, deployment, ok := currentUserDeployment(c)
	if !ok {
		return
	}

	customDomain := normalizeCustomDomain(c.Param("domain"))
	if customDomain == "" {
		sendBadRequest(c, "domain is required", nil)
		return
	}

	record, err := deploymentStore.VerifyDeploymentDomain(c.Request.Context(), user.ID, deployment.DeploymentID, customDomain)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "domain_not_verified", "details": err.Error()})
		return
	}

	if syncErr := SyncCaddyRoutesFromRunningContainers(); syncErr != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_sync_caddy", "details": syncErr.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"domain": record, "status": "verified"})
}

func deleteAppDomainHandler(c *gin.Context) {
	user, deployment, ok := currentUserDeployment(c)
	if !ok {
		return
	}

	customDomain := normalizeCustomDomain(c.Param("domain"))
	if customDomain == "" {
		sendBadRequest(c, "domain is required", nil)
		return
	}

	if err := deploymentStore.DeleteDeploymentDomain(c.Request.Context(), user.ID, deployment.DeploymentID, customDomain); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_delete_domain", "details": err.Error()})
		return
	}

	cleanupCaddyCertificatesForDomain(customDomain)
	if err := SyncCaddyRoutesFromRunningContainers(); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_sync_caddy", "details": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "domain removed"})
}
