package main

import (
    "context"
    "crypto/aes"
    "crypto/cipher"
    "crypto/rand"
    "fmt"
    "net/http"
    "strings"
    "time"

    "github.com/gin-gonic/gin"
)

// ============================================================================
// TYPES
// ============================================================================

type DeploymentEnvRequest struct {
    Key         string `json:"key" binding:"required"`
    Value       string `json:"value"`
    Category    string `json:"category,omitempty"`
    Sensitive   bool   `json:"sensitive,omitempty"`
    Description string `json:"description,omitempty"`
}

type BulkEnvRequest struct {
    Variables []DeploymentEnvRequest `json:"variables" binding:"required"`
    Overwrite bool                   `json:"overwrite"`
}

type EnvVarValidation struct {
    Key      string   `json:"key"`
    Value    string   `json:"value"`
    Valid    bool     `json:"valid"`
    Errors   []string `json:"errors,omitempty"`
    Warnings []string `json:"warnings,omitempty"`
}

type EnvVarHistory struct {
    ID          string    `json:"id"`
    DeploymentID string   `json:"deploymentId"`
    EnvKey      string    `json:"envKey"`
    Action      string    `json:"action"`
    ChangedBy   string    `json:"changedBy"`
    CreatedAt   time.Time `json:"createdAt"`
}

type EnvExport struct {
    Variables map[string]string `json:"variables"`
    Metadata  struct {
        ExportedAt   time.Time `json:"exportedAt"`
        DeploymentID string    `json:"deploymentId"`
        Count        int       `json:"count"`
    } `json:"metadata"`
}

// ============================================================================
// EXISTING HANDLERS (Enhanced)
// ============================================================================

func listAppEnvHandler(c *gin.Context) {
    user, deployment, ok := currentUserDeployment(c)
    if !ok {
        return
    }

    // Get with categories
    envVars, err := deploymentStore.ListDeploymentEnvVarsWithCategories(
        c.Request.Context(), user.ID, deployment.DeploymentID,
    )
    if err != nil {
        c.JSON(http.StatusInternalServerError, gin.H{
            "error":   "failed_to_list_env",
            "details": err.Error(),
        })
        return
    }

    // Filter sensitive values
    showSensitive := c.DefaultQuery("showSensitive", "false") == "true"
    if !showSensitive {
        for key := range envVars {
            if isSensitiveKey(key) {
                envVars[key] = "***REDACTED***"
            }
        }
    }

    // Group by category
    grouped := make(map[string]map[string]string)
    for key, value := range envVars {
        category := string(getCategoryFromKey(key))
        if grouped[category] == nil {
            grouped[category] = make(map[string]string)
        }
        grouped[category][key] = value
    }

    c.JSON(http.StatusOK, gin.H{
        "envVars":   envVars,
        "grouped":   grouped,
        "count":     len(envVars),
        "redacted":  !showSensitive,
    })
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

    key := normalizeEnvKey(req.Key)
    if key == "" {
        sendBadRequest(c, "key is required", nil)
        return
    }

    // Validate
    validation := validateEnvVar(key, req.Value)
    if !validation.Valid {
        c.JSON(http.StatusBadRequest, gin.H{
            "error":  "validation_failed",
            "validation": validation,
        })
        return
    }

    // Auto-detect category and sensitivity
    if req.Category == "" {
        req.Category = string(getCategoryFromKey(key))
    }
    req.Sensitive = req.Sensitive || isSensitiveKey(key)

    err := deploymentStore.UpsertDeploymentEnvVarWithCategory(
        c.Request.Context(),
        user.ID,
        deployment.DeploymentID,
        key,
        req.Value,
        req.Category,
        req.Sensitive,
        req.Description,
    )
    if err != nil {
        c.JSON(http.StatusInternalServerError, gin.H{
            "error":   "failed_to_store_env",
            "details": err.Error(),
        })
        return
    }

    // Record history
    _ = deploymentStore.RecordEnvVarHistory(
        c.Request.Context(),
        deployment.DeploymentID,
        key,
        "updated",
        user.ID,
    )

    c.JSON(http.StatusCreated, gin.H{
        "message":    "environment variable saved",
        "key":        key,
        "category":   req.Category,
        "sensitive":  req.Sensitive,
        "validation": validation,
    })
}

func deleteAppEnvHandler(c *gin.Context) {
    user, deployment, ok := currentUserDeployment(c)
    if !ok {
        return
    }

    key := normalizeEnvKey(c.Param("key"))
    if key == "" {
        sendBadRequest(c, "env key is required", nil)
        return
    }

    if err := deploymentStore.DeleteDeploymentEnvVar(
        c.Request.Context(), user.ID, deployment.DeploymentID, key,
    ); err != nil {
        c.JSON(http.StatusInternalServerError, gin.H{
            "error":   "failed_to_delete_env",
            "details": err.Error(),
        })
        return
    }

    // Record history
    _ = deploymentStore.RecordEnvVarHistory(
        c.Request.Context(),
        deployment.DeploymentID,
        key,
        "deleted",
        user.ID,
    )

    c.JSON(http.StatusOK, gin.H{
        "message": "environment variable removed",
        "key":     key,
    })
}

// ============================================================================
// NEW HANDLERS
// ============================================================================

func bulkAddAppEnvHandler(c *gin.Context) {
    user, deployment, ok := currentUserDeployment(c)
    if !ok {
        return
    }

    var req BulkEnvRequest
    if err := c.ShouldBindJSON(&req); err != nil {
        sendBadRequest(c, "invalid JSON body", err)
        return
    }

    if len(req.Variables) == 0 {
        sendBadRequest(c, "at least one variable is required", nil)
        return
    }

    if len(req.Variables) > 50 {
        c.JSON(http.StatusBadRequest, gin.H{"error": "maximum 50 variables per request"})
        return
    }

    results := make([]map[string]interface{}, 0, len(req.Variables))
    for _, env := range req.Variables {
        key := normalizeEnvKey(env.Key)
        if key == "" {
            results = append(results, map[string]interface{}{
                "key":    env.Key,
                "status": "error",
                "error":  "key is required",
            })
            continue
        }

        // Check if exists
        exists, _ := deploymentStore.DeploymentEnvVarExists(
            c.Request.Context(), deployment.DeploymentID, key,
        )
        if exists && !req.Overwrite {
            results = append(results, map[string]interface{}{
                "key":    key,
                "status": "skipped",
                "reason": "already exists",
            })
            continue
        }

        // Auto-detect category
        if env.Category == "" {
            env.Category = string(getCategoryFromKey(key))
        }
        env.Sensitive = env.Sensitive || isSensitiveKey(key)

        err := deploymentStore.UpsertDeploymentEnvVarWithCategory(
            c.Request.Context(),
            user.ID,
            deployment.DeploymentID,
            key,
            env.Value,
            env.Category,
            env.Sensitive,
            env.Description,
        )
        if err != nil {
            results = append(results, map[string]interface{}{
                "key":    key,
                "status": "error",
                "error":  err.Error(),
            })
            continue
        }

        results = append(results, map[string]interface{}{
            "key":       key,
            "status":    "saved",
            "category":  env.Category,
            "sensitive": env.Sensitive,
        })
    }

    c.JSON(http.StatusOK, gin.H{
        "results": results,
        "count":   len(results),
    })
}

func envVarHistoryHandler(c *gin.Context) {
    user, deployment, ok := currentUserDeployment(c)
    if !ok {
        return
    }

    limit := 50
    if l := c.Query("limit"); l != "" {
        if parsed, err := fmt.Sscanf(l, "%d", &limit); err == nil && parsed == 1 {
            if limit > 100 {
                limit = 100
            }
        }
    }

    history, err := deploymentStore.GetEnvVarHistory(
        c.Request.Context(), deployment.DeploymentID, limit,
    )
    if err != nil {
        c.JSON(http.StatusInternalServerError, gin.H{
            "error":   "failed_to_get_history",
            "details": err.Error(),
        })
        return
    }

    c.JSON(http.StatusOK, gin.H{
        "history": history,
        "count":   len(history),
    })
}

func validateEnvVarHandler(c *gin.Context) {
    user, deployment, ok := currentUserDeployment(c)
    if !ok {
        return
    }

    var req DeploymentEnvRequest
    if err := c.ShouldBindJSON(&req); err != nil {
        sendBadRequest(c, "invalid JSON body", err)
        return
    }

    key := normalizeEnvKey(req.Key)
    if key == "" {
        sendBadRequest(c, "key is required", nil)
        return
    }

    validation := validateEnvVar(key, req.Value)

    // Also check if already exists
    exists, _ := deploymentStore.DeploymentEnvVarExists(
        c.Request.Context(), deployment.DeploymentID, key,
    )

    c.JSON(http.StatusOK, gin.H{
        "validation": validation,
        "exists":     exists,
        "sensitive":  isSensitiveKey(key),
        "category":   getCategoryFromKey(key),
    })
}

func exportEnvHandler(c *gin.Context) {
    user, deployment, ok := currentUserDeployment(c)
    if !ok {
        return
    }

    envVars, err := deploymentStore.ListDeploymentEnvVars(
        c.Request.Context(), user.ID, deployment.DeploymentID,
    )
    if err != nil {
        c.JSON(http.StatusInternalServerError, gin.H{
            "error":   "failed_to_export_env",
            "details": err.Error(),
        })
        return
    }

    // Remove sensitive values if not authorized
    showSensitive := c.DefaultQuery("showSensitive", "false") == "true"
    if !showSensitive {
        for key := range envVars {
            if isSensitiveKey(key) {
                envVars[key] = "***REDACTED***"
            }
        }
    }

    format := c.DefaultQuery("format", "json")
    switch format {
    case "env":
        c.Header("Content-Disposition", "attachment; filename=environment.env")
        c.Header("Content-Type", "text/plain")
        var envContent strings.Builder
        envContent.WriteString("# Environment Variables\n")
        envContent.WriteString(fmt.Sprintf("# Exported: %s\n", time.Now().Format(time.RFC3339)))
        envContent.WriteString(fmt.Sprintf("# Deployment: %s\n\n", deployment.DeploymentID))
        for key, value := range envVars {
            envContent.WriteString(fmt.Sprintf("%s=%s\n", key, value))
        }
        c.String(http.StatusOK, envContent.String())
    default: // json
        export := EnvExport{
            Variables: envVars,
        }
        export.Metadata.ExportedAt = time.Now()
        export.Metadata.DeploymentID = deployment.DeploymentID
        export.Metadata.Count = len(envVars)
        c.JSON(http.StatusOK, export)
    }
}

func importEnvHandler(c *gin.Context) {
    user, deployment, ok := currentUserDeployment(c)
    if !ok {
        return
    }

    var importData map[string]string
    if err := c.ShouldBindJSON(&importData); err != nil {
        sendBadRequest(c, "invalid JSON body", err)
        return
    }

    if len(importData) == 0 {
        sendBadRequest(c, "no variables to import", nil)
        return
    }

    if len(importData) > 100 {
        c.JSON(http.StatusBadRequest, gin.H{"error": "maximum 100 variables per import"})
        return
    }

    overwrite := c.DefaultQuery("overwrite", "false") == "true"
    results := make([]map[string]interface{}, 0, len(importData))

    for key, value := range importData {
        key = normalizeEnvKey(key)
        if key == "" {
            continue
        }

        // Check if exists
        exists, err := deploymentStore.DeploymentEnvVarExists(
            c.Request.Context(), deployment.DeploymentID, key,
        )
        if err != nil {
            results = append(results, map[string]interface{}{
                "key":    key,
                "status": "error",
                "error":  err.Error(),
            })
            continue
        }

        if exists && !overwrite {
            results = append(results, map[string]interface{}{
                "key":    key,
                "status": "skipped",
                "reason": "already exists (use overwrite=true to replace)",
            })
            continue
        }

        category := string(getCategoryFromKey(key))
        sensitive := isSensitiveKey(key)

        err = deploymentStore.UpsertDeploymentEnvVarWithCategory(
            c.Request.Context(),
            user.ID,
            deployment.DeploymentID,
            key,
            value,
            category,
            sensitive,
            "",
        )
        if err != nil {
            results = append(results, map[string]interface{}{
                "key":    key,
                "status": "error",
                "error":  err.Error(),
            })
            continue
        }

        results = append(results, map[string]interface{}{
            "key":       key,
            "status":    "imported",
            "category":  category,
            "sensitive": sensitive,
        })
    }

    imported := 0
    for _, r := range results {
        if r["status"] == "imported" {
            imported++
        }
    }

    c.JSON(http.StatusOK, gin.H{
        "results":  results,
        "total":    len(importData),
        "imported": imported,
        "skipped":  len(results) - imported,
    })
}

// ============================================================================
// STORE EXTENSIONS
// ============================================================================

func (s *DeploymentStore) ListDeploymentEnvVarsWithCategories(
    ctx context.Context,
    userID string,
    deploymentID string,
) (map[string]map[string]interface{}, error) {
    // This would fetch env vars with categories from the database
    // Return map of key -> {value, category, sensitive, description}
    return nil, nil
}

func (s *DeploymentStore) DeploymentEnvVarExists(
    ctx context.Context,
    deploymentID string,
    key string,
) (bool, error) {
    var exists bool
    err := s.pool.QueryRow(ctx, `
    SELECT EXISTS(SELECT 1 FROM deployment_env_vars WHERE deployment_id = $1 AND env_key = $2)
    `, deploymentID, key).Scan(&exists)
    return exists, err
}

func (s *DeploymentStore) UpsertDeploymentEnvVarWithCategory(
    ctx context.Context,
    userID string,
    deploymentID string,
    key string,
    value string,
    category string,
    sensitive bool,
    description string,
) error {
    // Encrypt value
    encryptedValue, nonce, err := encryptEnvValue(value)
    if err != nil {
        return err
    }

    _, err = s.pool.Exec(ctx, `
    INSERT INTO deployment_env_vars (
        deployment_id, env_key, encrypted_value, nonce, category, sensitive, description
    ) VALUES ($1, $2, $3, $4, $5, $6, $7)
    ON CONFLICT (deployment_id, env_key)
    DO UPDATE SET
        encrypted_value = EXCLUDED.encrypted_value,
        nonce = EXCLUDED.nonce,
        category = EXCLUDED.category,
        sensitive = EXCLUDED.sensitive,
        description = EXCLUDED.description,
        updated_at = now()
    `, deploymentID, key, encryptedValue, nonce, category, sensitive, description)
    
    return err
}

// ============================================================================
// HELPER FUNCTIONS
// ============================================================================

func normalizeEnvKey(key string) string {
    return strings.TrimSpace(key)
}

func getCategoryFromKey(key string) string {
    keyLower := strings.ToLower(key)
    switch {
    case strings.Contains(keyLower, "database") || strings.Contains(keyLower, "postgres") ||
        strings.Contains(keyLower, "mysql") || strings.Contains(keyLower, "mongodb"):
        return "database"
    case strings.Contains(keyLower, "api") || strings.Contains(keyLower, "token") ||
        strings.Contains(keyLower, "secret") || strings.Contains(keyLower, "key") ||
        strings.Contains(keyLower, "password"):
        return "api"
    case strings.Contains(keyLower, "redis") || strings.Contains(keyLower, "cache"):
        return "cache"
    case strings.Contains(keyLower, "queue") || strings.Contains(keyLower, "rabbit") ||
        strings.Contains(keyLower, "kafka"):
        return "queue"
    case strings.Contains(keyLower, "storage") || strings.Contains(keyLower, "s3") ||
        strings.Contains(keyLower, "bucket"):
        return "storage"
    default:
        return "general"
    }
}

func isSensitiveKey(key string) bool {
    sensitiveTerms := []string{
        "secret", "password", "token", "key", "auth",
        "credential", "private", "cert", "pem", "api",
    }
    keyLower := strings.ToLower(key)
    for _, term := range sensitiveTerms {
        if strings.Contains(keyLower, term) {
            return true
        }
    }
    return false
}

// ============================================================================
// ROUTE SETUP
// ============================================================================

func SetupEnvRoutes(router *gin.Engine) {
    envGroup := router.Group("/api/deployments/:id/env")
    envGroup.Use(AuthMiddleware(false))
    {
        // List
        envGroup.GET("/", listAppEnvHandler)
        
        // Add/Update
        envGroup.POST("/", addAppEnvHandler)
        envGroup.POST("/bulk", bulkAddAppEnvHandler)
        
        // Validate
        envGroup.POST("/validate", validateEnvVarHandler)
        
        // History
        envGroup.GET("/history", envVarHistoryHandler)
        
        // Export/Import
        envGroup.GET("/export", exportEnvHandler)
        envGroup.POST("/import", importEnvHandler)
        
        // Delete
        envGroup.DELETE("/:key", deleteAppEnvHandler)
    }
}