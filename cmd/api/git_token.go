package main

import (
	"context"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// ============================================================================
// PRIVATE REPOSITORY ACCESS TOKENS
// ============================================================================
//
// Each app can carry one access token (GitHub/GitLab personal access token or
// fine-grained token) used only to clone/fetch its repository. It's stored
// AES-GCM encrypted with the env var key, never returned by the API, and
// handed to git as an HTTP header scoped to the repo's host (see gitAuthEnv in
// source.go), so it doesn't end up in URLs, .git/config or error messages.

const maxGitTokenLength = 1024

// SetDeploymentGitToken stores token for the deployment, or clears it when
// token is empty.
func (s *DeploymentStore) SetDeploymentGitToken(ctx context.Context, deploymentID string, token string) error {
	if s == nil || s.pool == nil {
		return &StoreError{Type: ErrDatabase, Message: "deployment store is not initialized"}
	}

	var encrypted, nonce []byte
	if token != "" {
		var err error
		encrypted, nonce, err = encryptEnvValue(token)
		if err != nil {
			return &StoreError{Type: ErrDatabase, Message: "failed to encrypt access token", Err: err}
		}
	}

	tag, err := s.pool.Exec(ctx, `
UPDATE deployments
SET git_token_encrypted = $2, git_token_nonce = $3, updated_at = now()
WHERE id = $1 AND deleted_at IS NULL
`, deploymentID, encrypted, nonce)
	if err != nil {
		return &StoreError{Type: ErrDatabase, Message: "failed to save access token", Err: err}
	}
	if tag.RowsAffected() == 0 {
		return &StoreError{Type: ErrNotFound, Message: "deployment not found"}
	}
	return nil
}

// GetDeploymentGitToken returns the deployment's decrypted token, or "" when
// none is set.
func (s *DeploymentStore) GetDeploymentGitToken(ctx context.Context, deploymentID string) (string, error) {
	if s == nil || s.pool == nil {
		return "", &StoreError{Type: ErrDatabase, Message: "deployment store is not initialized"}
	}

	var encrypted, nonce []byte
	if err := s.pool.QueryRow(ctx, `
SELECT git_token_encrypted, git_token_nonce FROM deployments WHERE id = $1
`, deploymentID).Scan(&encrypted, &nonce); err != nil {
		return "", &StoreError{Type: ErrDatabase, Message: "failed to load access token", Err: err}
	}
	if len(encrypted) == 0 {
		return "", nil
	}

	token, err := decryptEnvValue(encrypted, nonce)
	if err != nil {
		// Almost always APP_ENV_ENCRYPTION_KEY having changed since it was saved.
		return "", &StoreError{Type: ErrDatabase, Message: "failed to decrypt the app's access token; save it again", Err: err}
	}
	return token, nil
}

// normalizeGitToken validates a token from a request. Tokens are opaque, but
// whitespace or control characters would corrupt the Authorization header.
func normalizeGitToken(token string) (string, bool) {
	token = strings.TrimSpace(token)
	if len(token) > maxGitTokenLength {
		return "", false
	}
	for _, r := range token {
		if r < 0x21 || r > 0x7e {
			return "", false
		}
	}
	return token, true
}

// ============================================================================
// HANDLERS
// ============================================================================

type setGitTokenRequest struct {
	Token string `json:"token"`
}

// getAppGitTokenHandler reports whether a token is configured. The token
// itself is write-only.
func getAppGitTokenHandler(c *gin.Context) {
	_, deployment, ok := currentUserDeployment(c)
	if !ok {
		return
	}
	token, err := deploymentStore.GetDeploymentGitToken(c.Request.Context(), deployment.DeploymentID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_load_access_token", "details": err.Error()})
		return
	}
	src, err := deploymentStore.GetDeploymentGitHubSource(c.Request.Context(), deployment.DeploymentID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_load_access_token", "details": err.Error()})
		return
	}
	// githubApp: the service clones through the GitHub App, which takes
	// precedence over any token saved here.
	c.JSON(http.StatusOK, gin.H{"configured": token != "", "githubApp": src.Linked, "githubAppActive": src.Linked && src.InstallationActive})
}

func setAppGitTokenHandler(c *gin.Context) {
	_, deployment, ok := currentUserDeployment(c)
	if !ok {
		return
	}

	var req setGitTokenRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		sendBadRequest(c, "invalid JSON body", err)
		return
	}
	token, valid := normalizeGitToken(req.Token)
	if !valid || token == "" {
		sendBadRequest(c, "token must be a non-empty access token without spaces", nil)
		return
	}

	if err := deploymentStore.SetDeploymentGitToken(c.Request.Context(), deployment.DeploymentID, token); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_save_access_token", "details": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"configured": true})
}

func deleteAppGitTokenHandler(c *gin.Context) {
	_, deployment, ok := currentUserDeployment(c)
	if !ok {
		return
	}
	if err := deploymentStore.SetDeploymentGitToken(c.Request.Context(), deployment.DeploymentID, ""); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_remove_access_token", "details": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"configured": false})
}
