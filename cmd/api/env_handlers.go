package main

import (
	"context"
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
// HELPER FUNCTIONS (Unique to env_handlers.go)
// ============================================================================

// Note: normalizeEnvKey, getCategoryFromKey, isSensitiveKey, validateEnvVar
// are now in envs.go - DO NOT redeclare here

// ============================================================================
// EXISTING HANDLERS (Enhanced)
// ============================================================================

func listAppEnvHandler(c *gin.Context) {
	user, deployment, ok := currentUserDeployment(c)
	if !ok {
		return
	}

	records, err := deploymentStore.ListDeploymentEnvVars(
		c.Request.Context(), user.ID, deployment.DeploymentID,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":   "failed_to_list_env",
			"details": err.Error(),
		})
		return
	}

	// ListDeploymentEnvVars already redacts values to "****".
	// Return a flat array matching the frontend EnvItem shape: {key, value}.
	type envItem struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	items := make([]envItem, 0, len(records))
	for _, r := range records {
		items = append(items, envItem{Key: r.Key, Value: r.Value})
	}

	c.JSON(http.StatusOK, gin.H{
		"envVars": items,
		"count":   len(items),
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

	// Ownership is already verified by currentUserDeployment above.
	// Pass deploymentID from the verified deployment record (not raw from URL).
	if err := deploymentStore.DeleteDeploymentEnvVar(
		c.Request.Context(), deployment.DeploymentID, key,
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
	_, deployment, ok := currentUserDeployment(c)
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

	// Validate + encrypt every entry up front (all local, no DB) so only one
	// query touches the database regardless of how many variables came in.
	// A later duplicate key in the request wins, matching dotenv "last one
	// wins" semantics the import preview already applies client-side.
	type preparedVar struct {
		encryptedValue []byte
		nonce          []byte
		category       string
		sensitive      bool
		description    string
	}
	prepared := make(map[string]preparedVar, len(req.Variables))
	order := make([]string, 0, len(req.Variables))

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

		// Validate key/value the same way the single-add handler does, so
		// malformed entries (containing "=", spaces, etc.) can't slip in via
		// the bulk path.
		validation := validateEnvVar(key, env.Value)
		if !validation.Valid {
			results = append(results, map[string]interface{}{
				"key":        key,
				"status":     "error",
				"error":      "validation_failed",
				"validation": validation,
			})
			continue
		}

		if env.Category == "" {
			env.Category = string(getCategoryFromKey(key))
		}
		env.Sensitive = env.Sensitive || isSensitiveKey(key)

		encryptedValue, nonce, err := encryptEnvValue(env.Value)
		if err != nil {
			results = append(results, map[string]interface{}{
				"key":    key,
				"status": "error",
				"error":  err.Error(),
			})
			continue
		}

		if _, exists := prepared[key]; !exists {
			order = append(order, key)
		}
		prepared[key] = preparedVar{
			encryptedValue: encryptedValue,
			nonce:          nonce,
			category:       env.Category,
			sensitive:      env.Sensitive,
			description:    env.Description,
		}
	}

	if len(order) > 0 {
		keys := make([]string, len(order))
		encryptedValues := make([][]byte, len(order))
		nonces := make([][]byte, len(order))
		categories := make([]string, len(order))
		sensitives := make([]bool, len(order))
		descriptions := make([]string, len(order))
		for i, key := range order {
			v := prepared[key]
			keys[i] = key
			encryptedValues[i] = v.encryptedValue
			nonces[i] = v.nonce
			categories[i] = v.category
			sensitives[i] = v.sensitive
			descriptions[i] = v.description
		}

		saved, err := deploymentStore.bulkUpsertEnvVarsFast(
			c.Request.Context(), deployment.DeploymentID,
			keys, encryptedValues, nonces, categories, sensitives, descriptions,
			req.Overwrite,
		)
		if err != nil {
			for _, key := range order {
				results = append(results, map[string]interface{}{
					"key":    key,
					"status": "error",
					"error":  err.Error(),
				})
			}
		} else {
			for _, key := range order {
				if saved[key] {
					v := prepared[key]
					results = append(results, map[string]interface{}{
						"key":       key,
						"status":    "saved",
						"category":  v.category,
						"sensitive": v.sensitive,
					})
				} else {
					results = append(results, map[string]interface{}{
						"key":    key,
						"status": "skipped",
						"reason": "already exists",
					})
				}
			}
		}
	}

	c.JSON(http.StatusOK, gin.H{
		"results": results,
		"count":   len(results),
	})
}

func envVarHistoryHandler(c *gin.Context) {
	_, deployment, ok := currentUserDeployment(c)
	if !ok {
		return
	}

	limit := 50
	if l := c.Query("limit"); l != "" {
		if parsed, err := fmt.Sscanf(l, "%d", &limit); err == nil && parsed == 1 {
			if limit <= 0 || limit > 100 {
				limit = 50
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
	_, deployment, ok := currentUserDeployment(c)
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

	// currentUserDeployment already verified that this user owns/may access
	// this deployment, which is the same authorization gate used by the
	// other sensitive env operations in this file (add/delete/bulk/import).
	// That's what makes it safe to fetch real values here instead of the
	// always-redacted "****" placeholders from ListDeploymentEnvVars.
	showSensitive := c.DefaultQuery("showSensitive", "false") == "true"

	envRecords, err := deploymentStore.ListDeploymentEnvVarsWithValues(
		c.Request.Context(), user.ID, deployment.DeploymentID, showSensitive,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error":   "failed_to_export_env",
			"details": err.Error(),
		})
		return
	}

	// ListDeploymentEnvVarsWithValues already redacts sensitive values to
	// "***REDACTED***" when showSensitive is false; non-sensitive values are
	// returned in full.
	envVars := make(map[string]string, len(envRecords))
	for _, record := range envRecords {
		envVars[record.Key] = record.Value
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

		// Validate key/value the same way the single-add handler does, so
		// malformed entries (containing "=", spaces, etc.) can't slip in via
		// import.
		validation := validateEnvVar(key, value)
		if !validation.Valid {
			results = append(results, map[string]interface{}{
				"key":        key,
				"status":     "error",
				"error":      "validation_failed",
				"validation": validation,
			})
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
	// Encrypt value - encryptEnvValue is in db.go
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

// bulkUpsertEnvVarsFast upserts every already-validated, already-encrypted
// variable in a single round trip via unnest(), instead of one exists-check
// plus one upsert per key. That N*2-round-trip pattern is cheap against a
// local Postgres but, against a remote/pooled DB (e.g. Neon), each round
// trip costs the full network latency — a 20-variable import was measured
// taking 20+ seconds and blowing past the server's WRITE_TIMEOUT. Batching
// collapses that to one round trip regardless of N (up to the 50-variable
// request cap).
//
// overwrite selects the conflict behaviour for the whole batch: DO UPDATE
// (every key ends up "saved") or DO NOTHING (a pre-existing key is silently
// skipped and simply absent from the returned set). Returns the set of keys
// that were actually inserted or updated; any key not in it already existed
// and was skipped.
func (s *DeploymentStore) bulkUpsertEnvVarsFast(
	ctx context.Context,
	deploymentID string,
	keys []string,
	encryptedValues [][]byte,
	nonces [][]byte,
	categories []string,
	sensitives []bool,
	descriptions []string,
	overwrite bool,
) (map[string]bool, error) {
	conflictClause := "ON CONFLICT (deployment_id, env_key) DO NOTHING"
	if overwrite {
		conflictClause = `ON CONFLICT (deployment_id, env_key) DO UPDATE SET
			encrypted_value = EXCLUDED.encrypted_value,
			nonce = EXCLUDED.nonce,
			category = EXCLUDED.category,
			sensitive = EXCLUDED.sensitive,
			description = EXCLUDED.description,
			updated_at = now()`
	}

	rows, err := s.pool.Query(ctx, `
	INSERT INTO deployment_env_vars (deployment_id, env_key, encrypted_value, nonce, category, sensitive, description)
	SELECT $1, k, v, n, c, s, d
	FROM unnest($2::text[], $3::bytea[], $4::bytea[], $5::text[], $6::bool[], $7::text[]) AS t(k, v, n, c, s, d)
	`+conflictClause+`
	RETURNING env_key
	`, deploymentID, keys, encryptedValues, nonces, categories, sensitives, descriptions)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	saved := make(map[string]bool, len(keys))
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		saved[key] = true
	}
	return saved, rows.Err()
}

// Route registration for all of these handlers lives in main.go's
// setupRouter, under /api/v1/apps/:id/env/... (the prefix the frontend
// actually calls). This file used to also define a SetupEnvRoutes that
// registered the same handlers again under /api/deployments/:id/env/...,
// but that function was never called from main.go, so validateEnvVarHandler,
// envVarHistoryHandler, exportEnvHandler and importEnvHandler were
// unreachable 404s despite being fully implemented. It was removed rather
// than wired up, to avoid two divergent URL namespaces for the same feature.