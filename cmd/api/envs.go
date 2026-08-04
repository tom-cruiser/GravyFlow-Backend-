package main

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// ============================================================================
// TYPES
// ============================================================================

type DeploymentEnvRecord struct {
	ID          string     `json:"id,omitempty"`
	DeploymentID string    `json:"deploymentId,omitempty"`
	Key         string     `json:"key"`
	Value       string     `json:"value,omitempty"`
	Category    string     `json:"category,omitempty"`
	Sensitive   bool       `json:"sensitive,omitempty"`
	Description string     `json:"description,omitempty"`
	CreatedAt   time.Time  `json:"createdAt,omitempty"`
	UpdatedAt   time.Time  `json:"updatedAt,omitempty"`
}

type EnvVarHistory struct {
	ID          string    `json:"id"`
	DeploymentID string   `json:"deploymentId"`
	EnvKey      string    `json:"envKey"`
	Action      string    `json:"action"`
	ChangedBy   string    `json:"changedBy"`
	CreatedAt   time.Time `json:"createdAt"`
}

type EnvVarValidation struct {
	Key      string   `json:"key"`
	Value    string   `json:"value"`
	Valid    bool     `json:"valid"`
	Errors   []string `json:"errors,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
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
// ENCRYPTION FUNCTIONS
// ============================================================================

func encryptEnvValue(value string) ([]byte, []byte, error) {
	key, err := envEncryptionKey()
	if err != nil {
		return nil, nil, err
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, nil, fmt.Errorf("create cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, fmt.Errorf("create gcm: %w", err)
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, fmt.Errorf("generate nonce: %w", err)
	}

	sealed := gcm.Seal(nil, nonce, []byte(value), nil)
	return sealed, nonce, nil
}

func decryptEnvValue(ciphertext []byte, nonce []byte) (string, error) {
	key, err := envEncryptionKey()
	if err != nil {
		return "", err
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("create cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("create gcm: %w", err)
	}

	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("decrypt env value: %w", err)
	}

	return string(plaintext), nil
}

func envEncryptionKey() ([]byte, error) {
	raw := strings.TrimSpace(os.Getenv("APP_ENV_ENCRYPTION_KEY"))
	if raw == "" {
		raw = strings.TrimSpace(os.Getenv("ENV_ENCRYPTION_KEY"))
	}
	if raw == "" {
		return nil, fmt.Errorf("APP_ENV_ENCRYPTION_KEY is required")
	}

	if decoded, err := base64.StdEncoding.DecodeString(raw); err == nil && len(decoded) == 32 {
		return decoded, nil
	}
	if decoded, err := hex.DecodeString(raw); err == nil && len(decoded) == 32 {
		return decoded, nil
	}

	sum := sha256.Sum256([]byte(raw))
	return sum[:], nil
}

// ============================================================================
// KEY NORMALIZATION
// ============================================================================

func normalizeEnvKey(key string) string {
	return strings.ToUpper(strings.TrimSpace(key))
}

// ============================================================================
// CATEGORY DETECTION
// ============================================================================

func getCategoryFromKey(key string) string {
	keyLower := strings.ToLower(key)
	switch {
	case strings.Contains(keyLower, "database") || strings.Contains(keyLower, "postgres") ||
		strings.Contains(keyLower, "mysql") || strings.Contains(keyLower, "mongodb") ||
		strings.Contains(keyLower, "redis") || strings.Contains(keyLower, "cassandra"):
		return "database"
	case strings.Contains(keyLower, "api") || strings.Contains(keyLower, "token") ||
		strings.Contains(keyLower, "secret") || strings.Contains(keyLower, "key") ||
		strings.Contains(keyLower, "password") || strings.Contains(keyLower, "auth"):
		return "api"
	case strings.Contains(keyLower, "cache") || strings.Contains(keyLower, "memcached"):
		return "cache"
	case strings.Contains(keyLower, "queue") || strings.Contains(keyLower, "rabbit") ||
		strings.Contains(keyLower, "kafka") || strings.Contains(keyLower, "sqs"):
		return "queue"
	case strings.Contains(keyLower, "storage") || strings.Contains(keyLower, "s3") ||
		strings.Contains(keyLower, "bucket") || strings.Contains(keyLower, "minio"):
		return "storage"
	case strings.Contains(keyLower, "debug") || strings.Contains(keyLower, "trace") ||
		strings.Contains(keyLower, "verbose") || strings.Contains(keyLower, "log"):
		return "logging"
	default:
		return "general"
	}
}

func isSensitiveKey(key string) bool {
	sensitiveTerms := []string{
		"secret", "password", "token", "key", "auth", "credential",
		"private", "cert", "pem", "api", "access", "refresh",
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
// VALIDATION
// ============================================================================

func validateEnvVar(key string, value string) EnvVarValidation {
	validation := EnvVarValidation{
		Key:   key,
		Value: value,
		Valid: true,
	}

	// Check key format
	if key == "" {
		validation.Valid = false
		validation.Errors = append(validation.Errors, "key cannot be empty")
	} else {
		// Key should only contain alphanumeric, underscore, and dash
		validKey := true
		for _, r := range key {
			if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
				(r >= '0' && r <= '9') || r == '_' || r == '-') {
				validKey = false
				break
			}
		}
		if !validKey {
			validation.Valid = false
			validation.Errors = append(validation.Errors,
				"key can only contain alphanumeric, underscore, and dash characters")
		}
	}

	// Check for common sensitive patterns
	if isSensitiveKey(key) && value == "" {
		validation.Warnings = append(validation.Warnings,
			"sensitive key has empty value")
	}

	// Check for common misconfigurations
	if strings.Contains(value, " ") && !strings.Contains(value, "=") {
		validation.Warnings = append(validation.Warnings,
			"value contains spaces which might be unintended")
	}

	// Warn about common insecure patterns
	if strings.Contains(value, "password") && strings.Contains(value, "123") {
		validation.Warnings = append(validation.Warnings,
			"value appears to contain a weak password pattern")
	}

	// Warn about overly long keys
	if len(key) > 64 {
		validation.Warnings = append(validation.Warnings,
			"key is very long (max 64 characters recommended)")
	}

	return validation
}

// ============================================================================
// DOCKER HELPER
// ============================================================================

func loadDockerEnvList(envMap map[string]string) []string {
	if len(envMap) == 0 {
		return nil
	}

	keys := make([]string, 0, len(envMap))
	for key := range envMap {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	envList := make([]string, 0, len(keys))
	for _, key := range keys {
		envList = append(envList, fmt.Sprintf("%s=%s", key, envMap[key]))
	}

	return envList
}

// ============================================================================
// UPSERT ENVIRONMENT VARIABLE
// ============================================================================

func (s *DeploymentStore) UpsertDeploymentEnvVar(
	ctx context.Context,
	userID string,
	deploymentID string,
	key string,
	value string,
) error {
	return s.UpsertDeploymentEnvVarWithOptions(ctx, userID, deploymentID, key, value, "", false, "")
}

func (s *DeploymentStore) UpsertDeploymentEnvVarWithOptions(
	ctx context.Context,
	userID string,
	deploymentID string,
	key string,
	value string,
	category string,
	sensitive bool,
	description string,
) error {
	if s == nil || s.pool == nil {
		return fmt.Errorf("deployment store is not initialized")
	}

	userID = strings.TrimSpace(userID)
	deploymentID = strings.TrimSpace(deploymentID)
	key = normalizeEnvKey(key)
	if userID == "" || deploymentID == "" || key == "" {
		return fmt.Errorf("userID, deploymentID, and key are required")
	}

	if _, err := s.GetDeploymentForUser(ctx, userID, deploymentID); err != nil {
		return err
	}

	// Validate
	validation := validateEnvVar(key, value)
	if !validation.Valid {
		return fmt.Errorf("validation failed: %s", strings.Join(validation.Errors, ", "))
	}

	// Auto-detect category and sensitivity
	if category == "" {
		category = getCategoryFromKey(key)
	}
	if !sensitive {
		sensitive = isSensitiveKey(key)
	}

	encryptedValue, nonce, err := encryptEnvValue(value)
	if err != nil {
		return err
	}

	// Check if exists to record history
	var exists bool
	if err := s.pool.QueryRow(ctx, `
	SELECT EXISTS(SELECT 1 FROM deployment_env_vars WHERE deployment_id = $1 AND env_key = $2)
	`, deploymentID, key).Scan(&exists); err != nil {
		return err
	}

	_, err = s.pool.Exec(ctx, `
INSERT INTO deployment_env_vars (deployment_id, env_key, encrypted_value, nonce, category, sensitive, description)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (deployment_id, env_key)
DO UPDATE SET
	encrypted_value = EXCLUDED.encrypted_value,
	nonce = EXCLUDED.nonce,
	category = EXCLUDED.category,
	sensitive = EXCLUDED.sensitive,
	description = EXCLUDED.description,
	updated_at = now()
`, deploymentID, key, encryptedValue, nonce, category, sensitive, description)
	if err != nil {
		return fmt.Errorf("upsert deployment env var: %w", err)
	}

	// Record history
	action := "created"
	if exists {
		action = "updated"
	}
	_ = s.RecordEnvVarHistory(ctx, deploymentID, key, action, userID)

	return nil
}

// ============================================================================
// LIST ENVIRONMENT VARIABLES
// ============================================================================

func (s *DeploymentStore) ListDeploymentEnvVars(
	ctx context.Context,
	userID string,
	deploymentID string,
) ([]DeploymentEnvRecord, error) {
	if s == nil || s.pool == nil {
		return nil, fmt.Errorf("deployment store is not initialized")
	}

	userID = strings.TrimSpace(userID)
	deploymentID = strings.TrimSpace(deploymentID)
	if userID == "" || deploymentID == "" {
		return nil, fmt.Errorf("userID and deploymentID are required")
	}

	if _, err := s.GetDeploymentForUser(ctx, userID, deploymentID); err != nil {
		return nil, err
	}

	rows, err := s.pool.Query(ctx, `
SELECT env_key, category, sensitive, description, created_at, updated_at
FROM deployment_env_vars
WHERE deployment_id = $1
ORDER BY env_key ASC
`, deploymentID)
	if err != nil {
		return nil, fmt.Errorf("list deployment env vars: %w", err)
	}
	defer rows.Close()

	results := make([]DeploymentEnvRecord, 0)
	for rows.Next() {
		var record DeploymentEnvRecord
		var description *string
		if err := rows.Scan(
			&record.Key,
			&record.Category,
			&record.Sensitive,
			&description,
			&record.CreatedAt,
			&record.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan deployment env var: %w", err)
		}
		if description != nil {
			record.Description = *description
		}
		record.Value = "****" // Redacted
		results = append(results, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate deployment env vars: %w", err)
	}

	return results, nil
}

func (s *DeploymentStore) ListDeploymentEnvVarsWithValues(
	ctx context.Context,
	userID string,
	deploymentID string,
	includeSensitive bool,
) ([]DeploymentEnvRecord, error) {
	if s == nil || s.pool == nil {
		return nil, fmt.Errorf("deployment store is not initialized")
	}

	userID = strings.TrimSpace(userID)
	deploymentID = strings.TrimSpace(deploymentID)
	if userID == "" || deploymentID == "" {
		return nil, fmt.Errorf("userID and deploymentID are required")
	}

	if _, err := s.GetDeploymentForUser(ctx, userID, deploymentID); err != nil {
		return nil, err
	}

	rows, err := s.pool.Query(ctx, `
SELECT env_key, encrypted_value, nonce, category, sensitive, description, created_at, updated_at
FROM deployment_env_vars
WHERE deployment_id = $1
ORDER BY env_key ASC
`, deploymentID)
	if err != nil {
		return nil, fmt.Errorf("list deployment env vars: %w", err)
	}
	defer rows.Close()

	results := make([]DeploymentEnvRecord, 0)
	for rows.Next() {
		var record DeploymentEnvRecord
		var encryptedValue []byte
		var nonce []byte
		var description *string
		if err := rows.Scan(
			&record.Key,
			&encryptedValue,
			&nonce,
			&record.Category,
			&record.Sensitive,
			&description,
			&record.CreatedAt,
			&record.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan deployment env var: %w", err)
		}
		if description != nil {
			record.Description = *description
		}

		if record.Sensitive && !includeSensitive {
			record.Value = "***REDACTED***"
		} else {
			value, err := decryptEnvValue(encryptedValue, nonce)
			if err != nil {
				return nil, err
			}
			record.Value = value
		}
		results = append(results, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate deployment env vars: %w", err)
	}

	return results, nil
}

// ============================================================================
// LOAD ENVIRONMENT MAP
// ============================================================================

func (s *DeploymentStore) LoadDeploymentEnvMap(
	ctx context.Context,
	userID string,
	deploymentID string,
) (map[string]string, error) {
	if s == nil || s.pool == nil {
		return nil, fmt.Errorf("deployment store is not initialized")
	}

	userID = strings.TrimSpace(userID)
	deploymentID = strings.TrimSpace(deploymentID)
	if userID == "" || deploymentID == "" {
		return nil, fmt.Errorf("userID and deploymentID are required")
	}

	if _, err := s.GetDeploymentForUser(ctx, userID, deploymentID); err != nil {
		return nil, err
	}

	rows, err := s.pool.Query(ctx, `
SELECT env_key, encrypted_value, nonce
FROM deployment_env_vars
WHERE deployment_id = $1
ORDER BY env_key ASC
`, deploymentID)
	if err != nil {
		return nil, fmt.Errorf("load deployment env vars: %w", err)
	}
	defer rows.Close()

	envMap := make(map[string]string)
	for rows.Next() {
		var key string
		var encryptedValue []byte
		var nonce []byte
		if err := rows.Scan(&key, &encryptedValue, &nonce); err != nil {
			return nil, fmt.Errorf("scan deployment env var: %w", err)
		}
		value, err := decryptEnvValue(encryptedValue, nonce)
		if err != nil {
			return nil, err
		}
		envMap[key] = value
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate deployment env vars: %w", err)
	}

	return envMap, nil
}

// ============================================================================
// DELETE ENVIRONMENT VARIABLE
// ============================================================================

func (s *DeploymentStore) DeleteDeploymentEnvVar(
	ctx context.Context,
	userID string,
	deploymentID string,
	key string,
) error {
	if s == nil || s.pool == nil {
		return fmt.Errorf("deployment store is not initialized")
	}

	userID = strings.TrimSpace(userID)
	deploymentID = strings.TrimSpace(deploymentID)
	key = normalizeEnvKey(key)
	if userID == "" || deploymentID == "" || key == "" {
		return fmt.Errorf("userID, deploymentID, and key are required")
	}

	if _, err := s.GetDeploymentForUser(ctx, userID, deploymentID); err != nil {
		return err
	}

	// Record history before deletion
	_ = s.RecordEnvVarHistory(ctx, deploymentID, key, "deleted", userID)

	_, err := s.pool.Exec(ctx, `
DELETE FROM deployment_env_vars
WHERE deployment_id = $1 AND env_key = $2
`, deploymentID, key)
	if err != nil {
		return fmt.Errorf("delete deployment env var: %w", err)
	}

	return nil
}

// ============================================================================
// CHECK EXISTS
// ============================================================================

func (s *DeploymentStore) DeploymentEnvVarExists(
	ctx context.Context,
	deploymentID string,
	key string,
) (bool, error) {
	if s == nil || s.pool == nil {
		return false, fmt.Errorf("deployment store is not initialized")
	}

	deploymentID = strings.TrimSpace(deploymentID)
	key = normalizeEnvKey(key)
	if deploymentID == "" || key == "" {
		return false, fmt.Errorf("deploymentID and key are required")
	}

	var exists bool
	err := s.pool.QueryRow(ctx, `
SELECT EXISTS(SELECT 1 FROM deployment_env_vars WHERE deployment_id = $1 AND env_key = $2)
`, deploymentID, key).Scan(&exists)
	return exists, err
}

// ============================================================================
// ENVIRONMENT VARIABLE HISTORY
// ============================================================================

func (s *DeploymentStore) RecordEnvVarHistory(
	ctx context.Context,
	deploymentID string,
	envKey string,
	action string,
	changedBy string,
) error {
	if s == nil || s.pool == nil {
		return fmt.Errorf("deployment store is not initialized")
	}

	_, err := s.pool.Exec(ctx, `
INSERT INTO env_var_history (deployment_id, env_key, action, changed_by)
VALUES ($1, $2, $3, $4)
`, deploymentID, envKey, action, changedBy)
	return err
}

func (s *DeploymentStore) GetEnvVarHistory(
	ctx context.Context,
	deploymentID string,
	limit int,
) ([]EnvVarHistory, error) {
	if s == nil || s.pool == nil {
		return nil, fmt.Errorf("deployment store is not initialized")
	}

	if limit == 0 || limit > 100 {
		limit = 50
	}

	rows, err := s.pool.Query(ctx, `
SELECT id::text, deployment_id::text, env_key, action, changed_by::text, created_at
FROM env_var_history
WHERE deployment_id = $1
ORDER BY created_at DESC
LIMIT $2
`, deploymentID, limit)
	if err != nil {
		return nil, fmt.Errorf("get env var history: %w", err)
	}
	defer rows.Close()

	var history []EnvVarHistory
	for rows.Next() {
		var record EnvVarHistory
		if err := rows.Scan(
			&record.ID,
			&record.DeploymentID,
			&record.EnvKey,
			&record.Action,
			&record.ChangedBy,
			&record.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan env var history: %w", err)
		}
		history = append(history, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate env var history: %w", err)
	}

	return history, nil
}

// ============================================================================
// BULK OPERATIONS
// ============================================================================

type BulkEnvVar struct {
	Key         string `json:"key"`
	Value       string `json:"value"`
	Category    string `json:"category,omitempty"`
	Sensitive   bool   `json:"sensitive,omitempty"`
	Description string `json:"description,omitempty"`
}

func (s *DeploymentStore) BulkUpsertEnvVars(
	ctx context.Context,
	userID string,
	deploymentID string,
	vars []BulkEnvVar,
	overwrite bool,
) ([]map[string]interface{}, error) {
	if s == nil || s.pool == nil {
		return nil, fmt.Errorf("deployment store is not initialized")
	}

	if len(vars) == 0 {
		return nil, fmt.Errorf("no variables provided")
	}
	if len(vars) > 50 {
		return nil, fmt.Errorf("maximum 50 variables per bulk operation")
	}

	if _, err := s.GetDeploymentForUser(ctx, userID, deploymentID); err != nil {
		return nil, err
	}

	results := make([]map[string]interface{}, 0, len(vars))

	for _, v := range vars {
		key := normalizeEnvKey(v.Key)
		if key == "" {
			results = append(results, map[string]interface{}{
				"key":    v.Key,
				"status": "error",
				"error":  "key is required",
			})
			continue
		}

		// Check if exists
		exists, err := s.DeploymentEnvVarExists(ctx, deploymentID, key)
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
				"reason": "already exists",
			})
			continue
		}

		// Auto-detect category and sensitivity
		category := v.Category
		if category == "" {
			category = getCategoryFromKey(key)
		}
		sensitive := v.Sensitive || isSensitiveKey(key)

		err = s.UpsertDeploymentEnvVarWithOptions(
			ctx,
			userID,
			deploymentID,
			key,
			v.Value,
			category,
			sensitive,
			v.Description,
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
			"category":  category,
			"sensitive": sensitive,
		})
	}

	return results, nil
}

// ============================================================================
// EXPORT/IMPORT
// ============================================================================

func (s *DeploymentStore) ExportEnvVars(
	ctx context.Context,
	userID string,
	deploymentID string,
	includeSensitive bool,
) (EnvExport, error) {
	if s == nil || s.pool == nil {
		return EnvExport{}, fmt.Errorf("deployment store is not initialized")
	}

	records, err := s.ListDeploymentEnvVarsWithValues(ctx, userID, deploymentID, includeSensitive)
	if err != nil {
		return EnvExport{}, err
	}

	export := EnvExport{
		Variables: make(map[string]string),
	}
	export.Metadata.ExportedAt = time.Now()
	export.Metadata.DeploymentID = deploymentID
	export.Metadata.Count = len(records)

	for _, record := range records {
		export.Variables[record.Key] = record.Value
	}

	return export, nil
}

func (s *DeploymentStore) ImportEnvVars(
	ctx context.Context,
	userID string,
	deploymentID string,
	variables map[string]string,
	overwrite bool,
) ([]map[string]interface{}, error) {
	if s == nil || s.pool == nil {
		return nil, fmt.Errorf("deployment store is not initialized")
	}

	if len(variables) == 0 {
		return nil, fmt.Errorf("no variables to import")
	}
	if len(variables) > 100 {
		return nil, fmt.Errorf("maximum 100 variables per import")
	}

	if _, err := s.GetDeploymentForUser(ctx, userID, deploymentID); err != nil {
		return nil, err
	}

	var bulkVars []BulkEnvVar
	for key, value := range variables {
		bulkVars = append(bulkVars, BulkEnvVar{
			Key:   key,
			Value: value,
		})
	}

	return s.BulkUpsertEnvVars(ctx, userID, deploymentID, bulkVars, overwrite)
}

// ============================================================================
// ENVIRONMENT VARIABLE GROUPS
// ============================================================================

func (s *DeploymentStore) GetEnvVarsByCategory(
	ctx context.Context,
	userID string,
	deploymentID string,
) (map[string][]DeploymentEnvRecord, error) {
	if s == nil || s.pool == nil {
		return nil, fmt.Errorf("deployment store is not initialized")
	}

	records, err := s.ListDeploymentEnvVars(ctx, userID, deploymentID)
	if err != nil {
		return nil, err
	}

	grouped := make(map[string][]DeploymentEnvRecord)
	for _, record := range records {
		category := record.Category
		if category == "" {
			category = getCategoryFromKey(record.Key)
		}
		grouped[category] = append(grouped[category], record)
	}

	return grouped, nil
}

// ============================================================================
// ENVIRONMENT VARIABLE SUMMARY
// ============================================================================

type EnvVarSummary struct {
	TotalCount     int            `json:"totalCount"`
	CategoryCounts map[string]int `json:"categoryCounts"`
	SensitiveCount int            `json:"sensitiveCount"`
	HasDescription int            `json:"hasDescription"`
	LastUpdated    time.Time      `json:"lastUpdated"`
}

func (s *DeploymentStore) GetEnvVarSummary(
	ctx context.Context,
	userID string,
	deploymentID string,
) (EnvVarSummary, error) {
	if s == nil || s.pool == nil {
		return EnvVarSummary{}, fmt.Errorf("deployment store is not initialized")
	}

	var summary EnvVarSummary
	summary.CategoryCounts = make(map[string]int)

	// Get aggregated stats
	err := s.pool.QueryRow(ctx, `
SELECT 
	COUNT(*) as total,
	COUNT(CASE WHEN sensitive = TRUE THEN 1 END) as sensitive,
	COUNT(CASE WHEN description IS NOT NULL AND description != '' THEN 1 END) as has_desc,
	MAX(updated_at) as last_updated
FROM deployment_env_vars
WHERE deployment_id = $1
`, deploymentID).Scan(
		&summary.TotalCount,
		&summary.SensitiveCount,
		&summary.HasDescription,
		&summary.LastUpdated,
	)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return summary, nil
		}
		return EnvVarSummary{}, fmt.Errorf("get env var summary: %w", err)
	}

	// Get category counts
	rows, err := s.pool.Query(ctx, `
SELECT category, COUNT(*) as count
FROM deployment_env_vars
WHERE deployment_id = $1
GROUP BY category
ORDER BY count DESC
`, deploymentID)
	if err != nil {
		return EnvVarSummary{}, fmt.Errorf("get category counts: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var category string
		var count int
		if err := rows.Scan(&category, &count); err != nil {
			return EnvVarSummary{}, fmt.Errorf("scan category count: %w", err)
		}
		summary.CategoryCounts[category] = count
	}

	return summary, nil
}

// ============================================================================
// DATABASE MIGRATIONS
// ============================================================================

func (s *DeploymentStore) CreateEnvTablesIfNotExists(ctx context.Context) error {
	if s == nil || s.pool == nil {
		return fmt.Errorf("deployment store is not initialized")
	}

	// Create env vars table
	_, err := s.pool.Exec(ctx, `
CREATE TABLE IF NOT EXISTS deployment_env_vars (
	id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	deployment_id UUID NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
	env_key TEXT NOT NULL,
	encrypted_value BYTEA NOT NULL,
	nonce BYTEA NOT NULL,
	category TEXT DEFAULT 'general',
	sensitive BOOLEAN DEFAULT FALSE,
	description TEXT,
	created_at TIMESTAMP WITH TIME ZONE DEFAULT now(),
	updated_at TIMESTAMP WITH TIME ZONE DEFAULT now(),
	UNIQUE(deployment_id, env_key)
)
`)
	if err != nil {
		return err
	}

	// Create env var history table
	_, err = s.pool.Exec(ctx, `
CREATE TABLE IF NOT EXISTS env_var_history (
	id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	deployment_id UUID NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
	env_key TEXT NOT NULL,
	action TEXT NOT NULL,
	changed_by UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	created_at TIMESTAMP WITH TIME ZONE DEFAULT now()
)
`)
	if err != nil {
		return err
	}

	// Create indexes
	_, err = s.pool.Exec(ctx, `
CREATE INDEX IF NOT EXISTS idx_env_vars_deployment_id ON deployment_env_vars(deployment_id);
CREATE INDEX IF NOT EXISTS idx_env_vars_env_key ON deployment_env_vars(env_key);
CREATE INDEX IF NOT EXISTS idx_env_vars_category ON deployment_env_vars(category);
CREATE INDEX IF NOT EXISTS idx_env_vars_sensitive ON deployment_env_vars(sensitive);
CREATE INDEX IF NOT EXISTS idx_env_history_deployment_id ON env_var_history(deployment_id);
CREATE INDEX IF NOT EXISTS idx_env_history_env_key ON env_var_history(env_key);
CREATE INDEX IF NOT EXISTS idx_env_history_created_at ON env_var_history(created_at);
`)
	if err != nil {
		return err
	}

	return nil
}

// ============================================================================
// USAGE EXAMPLES
// ============================================================================

/*
EXAMPLE USAGE:

1. Initialize database:
   err := store.CreateEnvTablesIfNotExists(ctx)

2. Add environment variable:
   err := store.UpsertDeploymentEnvVarWithOptions(
       ctx,
       "user-123",
       "deploy-456",
       "DATABASE_URL",
       "postgresql://user:pass@localhost:5432/db",
       "database",
       true,
       "Main database connection string",
   )

3. List variables (redacted):
   vars, err := store.ListDeploymentEnvVars(ctx, "user-123", "deploy-456")
   for _, v := range vars {
       fmt.Printf("%s: %s\n", v.Key, v.Value) // Value is "****"
   }

4. List variables with values:
   vars, err := store.ListDeploymentEnvVarsWithValues(ctx, "user-123", "deploy-456", true)
   for _, v := range vars {
       fmt.Printf("%s: %s\n", v.Key, v.Value) // Actual value
   }

5. Load for container:
   envMap, err := store.LoadDeploymentEnvMap(ctx, "user-123", "deploy-456")
   envList := loadDockerEnvList(envMap)
   container.Config.Env = envList

6. Bulk import:
   vars := []BulkEnvVar{
       {Key: "NODE_ENV", Value: "production"},
       {Key: "PORT", Value: "3000"},
       {Key: "API_KEY", Value: "secret-123"},
   }
   results, err := store.BulkUpsertEnvVars(ctx, "user-123", "deploy-456", vars, true)

7. Get summary:
   summary, err := store.GetEnvVarSummary(ctx, "user-123", "deploy-456")
   fmt.Printf("Total: %d, Sensitive: %d\n", summary.TotalCount, summary.SensitiveCount)

8. Get history:
   history, err := store.GetEnvVarHistory(ctx, "deploy-456", 20)
   for _, h := range history {
       fmt.Printf("%s: %s %s\n", h.CreatedAt, h.EnvKey, h.Action)
   }
*/