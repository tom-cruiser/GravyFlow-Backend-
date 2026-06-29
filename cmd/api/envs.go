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
)

type DeploymentEnvRecord struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

func (s *DeploymentStore) UpsertDeploymentEnvVar(ctx context.Context, userID string, deploymentID string, key string, value string) error {
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

	encryptedValue, nonce, err := encryptEnvValue(value)
	if err != nil {
		return err
	}

	_, err = s.pool.Exec(ctx, `
INSERT INTO deployment_env_vars (deployment_id, env_key, encrypted_value, nonce)
VALUES ($1, $2, $3, $4)
ON CONFLICT (deployment_id, env_key)
DO UPDATE SET encrypted_value = EXCLUDED.encrypted_value,
	nonce = EXCLUDED.nonce,
	updated_at = now()
`, deploymentID, key, encryptedValue, nonce)
	if err != nil {
		return fmt.Errorf("upsert deployment env var: %w", err)
	}

	return nil
}

func (s *DeploymentStore) ListDeploymentEnvVars(ctx context.Context, userID string, deploymentID string) ([]DeploymentEnvRecord, error) {
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
SELECT env_key
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
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, fmt.Errorf("scan deployment env var: %w", err)
		}
		results = append(results, DeploymentEnvRecord{Key: key, Value: "****"})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate deployment env vars: %w", err)
	}

	return results, nil
}

func (s *DeploymentStore) DeleteDeploymentEnvVar(ctx context.Context, userID string, deploymentID string, key string) error {
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

	_, err := s.pool.Exec(ctx, `
DELETE FROM deployment_env_vars
WHERE deployment_id = $1 AND env_key = $2
`, deploymentID, key)
	if err != nil {
		return fmt.Errorf("delete deployment env var: %w", err)
	}

	return nil
}

func (s *DeploymentStore) LoadDeploymentEnvMap(ctx context.Context, userID string, deploymentID string) (map[string]string, error) {
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

func normalizeEnvKey(key string) string {
	return strings.ToUpper(strings.TrimSpace(key))
}

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
