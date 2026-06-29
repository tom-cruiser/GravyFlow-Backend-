package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	defaultDeploymentOwnerEmail = "system@gravyflow.local"
	defaultDeploymentOwnerName  = "GravyFlow System"
	defaultSystemPasswordHash   = "system-managed"
)

type DeploymentStatus string

const (
	DeploymentStatusBuilding DeploymentStatus = "BUILDING"
	DeploymentStatusDeployed DeploymentStatus = "DEPLOYED"
	DeploymentStatusFailed   DeploymentStatus = "FAILED"
	DeploymentStatusRunning  DeploymentStatus = "RUNNING"
)

type DeploymentStore struct {
	pool     *pgxpool.Pool
	statusMu sync.Mutex
}

type UserRecord struct {
	ID           string
	Email        string
	DisplayName  string
	PasswordHash string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

type DeploymentRecord struct {
	DeploymentID  string
	ProjectID     string
	AppName       string
	SourceRepoURL string
	AppPath       string
	PortMap       string
	ImageName     string
	ContainerID   string
	ContainerName string
	Status        string
	StatusMessage string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

type DeploymentHealthConfigRecord struct {
	DeploymentID               string
	HealthCheckPath            string
	HealthCheckIntervalSeconds int
	MaxRestartsBeforeFailing   int
	LastCheckedAt              *time.Time
	CreatedAt                  time.Time
	UpdatedAt                  time.Time
}

type RunningDeploymentHealthTarget struct {
	DeploymentRecord
	OwnerUserID                string
	HealthCheckPath            string
	HealthCheckIntervalSeconds int
	MaxRestartsBeforeFailing   int
	LastCheckedAt              *time.Time
}

type DeploymentRestartAuditRecord struct {
	ID                  string
	DeploymentID        string
	Action              string
	Outcome             string
	Reason              string
	PreviousContainerID string
	NewContainerID      string
	CreatedAt           time.Time
}

var deploymentStore *DeploymentStore

func newDeploymentStore(ctx context.Context) (*DeploymentStore, error) {
	connString, err := buildPostgresConnString()
	if err != nil {
		return nil, err
	}

	config, err := pgxpool.ParseConfig(connString)
	if err != nil {
		return nil, fmt.Errorf("parse postgres connection string: %w", err)
	}

	if maxConns, err := int32FromEnv("PGMAXCONNS", 4); err == nil {
		config.MaxConns = maxConns
	} else {
		return nil, err
	}
	if minConns, err := int32FromEnv("PGMINCONNS", 0); err == nil {
		config.MinConns = minConns
	} else {
		return nil, err
	}
	if maxConnLifetime, err := durationFromEnv("PGMAXCONNLIFETIME", 30*time.Minute); err == nil {
		config.MaxConnLifetime = maxConnLifetime
	} else {
		return nil, err
	}
	if maxConnIdleTime, err := durationFromEnv("PGMAXCONNIDLETIME", 5*time.Minute); err == nil {
		config.MaxConnIdleTime = maxConnIdleTime
	} else {
		return nil, err
	}
	if healthCheckPeriod, err := durationFromEnv("PGHEALTHCHECKPERIOD", time.Minute); err == nil {
		config.HealthCheckPeriod = healthCheckPeriod
	} else {
		return nil, err
	}

	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("connect postgres: %w", err)
	}

	// pgxpool connects lazily, so a successful NewWithConfig does NOT mean the
	// database is reachable. Ping with retry so startup blocks until Postgres is
	// actually accepting connections (covers the window before it is healthy,
	// even when not gated by compose's depends_on: service_healthy).
	if err := pingWithRetry(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}

	return &DeploymentStore{pool: pool}, nil
}

// pingWithRetry blocks until the pool can reach Postgres or the attempt budget
// is exhausted. Tunable via PGCONNECTATTEMPTS and PGCONNECTRETRYINTERVAL.
func pingWithRetry(ctx context.Context, pool *pgxpool.Pool) error {
	attempts, err := int32FromEnv("PGCONNECTATTEMPTS", 30)
	if err != nil {
		return err
	}
	if attempts < 1 {
		attempts = 1
	}

	interval, err := durationFromEnv("PGCONNECTRETRYINTERVAL", time.Second)
	if err != nil {
		return err
	}

	var lastErr error
	for attempt := int32(1); attempt <= attempts; attempt++ {
		pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		lastErr = pool.Ping(pingCtx)
		cancel()
		if lastErr == nil {
			if attempt > 1 {
				log.Printf("postgres reachable after %d attempt(s)", attempt)
			}
			return nil
		}

		log.Printf("postgres not ready (attempt %d/%d): %v", attempt, attempts, lastErr)
		if attempt < attempts {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(interval):
			}
		}
	}

	return fmt.Errorf("postgres not reachable after %d attempt(s): %w", attempts, lastErr)
}

func (s *DeploymentStore) Close() {
	if s == nil || s.pool == nil {
		return
	}
	s.pool.Close()
}

func (s *DeploymentStore) CreateUser(ctx context.Context, email string, displayName string, passwordHash string) (UserRecord, error) {
	if s == nil || s.pool == nil {
		return UserRecord{}, fmt.Errorf("deployment store is not initialized")
	}

	email = strings.ToLower(strings.TrimSpace(email))
	displayName = strings.TrimSpace(displayName)
	passwordHash = strings.TrimSpace(passwordHash)
	if email == "" || displayName == "" || passwordHash == "" {
		return UserRecord{}, fmt.Errorf("email, displayName, and passwordHash are required")
	}

	var user UserRecord
	err := s.pool.QueryRow(ctx, `
INSERT INTO users (email, password_hash, display_name)
VALUES ($1, $2, $3)
RETURNING id::text, email, display_name, password_hash, created_at, updated_at
`, email, passwordHash, displayName).Scan(&user.ID, &user.Email, &user.DisplayName, &user.PasswordHash, &user.CreatedAt, &user.UpdatedAt)
	if err != nil {
		return UserRecord{}, fmt.Errorf("create user: %w", err)
	}
	if err := s.EnsureResourceAccounting(ctx, user.ID); err != nil {
		return UserRecord{}, err
	}

	return user, nil
}

func (s *DeploymentStore) GetUserByEmail(ctx context.Context, email string) (UserRecord, error) {
	if s == nil || s.pool == nil {
		return UserRecord{}, fmt.Errorf("deployment store is not initialized")
	}

	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return UserRecord{}, fmt.Errorf("email is required")
	}

	var user UserRecord
	err := s.pool.QueryRow(ctx, `
SELECT id::text, email, display_name, password_hash, created_at, updated_at
FROM users
WHERE email = $1
`, email).Scan(&user.ID, &user.Email, &user.DisplayName, &user.PasswordHash, &user.CreatedAt, &user.UpdatedAt)
	if err != nil {
		return UserRecord{}, fmt.Errorf("get user by email: %w", err)
	}
	if err := s.EnsureResourceAccounting(ctx, user.ID); err != nil {
		return UserRecord{}, err
	}

	return user, nil
}

func (s *DeploymentStore) GetUserByID(ctx context.Context, userID string) (UserRecord, error) {
	if s == nil || s.pool == nil {
		return UserRecord{}, fmt.Errorf("deployment store is not initialized")
	}

	userID = strings.TrimSpace(userID)
	if userID == "" {
		return UserRecord{}, fmt.Errorf("userID is required")
	}

	var user UserRecord
	err := s.pool.QueryRow(ctx, `
SELECT id::text, email, display_name, password_hash, created_at, updated_at
FROM users
WHERE id = $1
`, userID).Scan(&user.ID, &user.Email, &user.DisplayName, &user.PasswordHash, &user.CreatedAt, &user.UpdatedAt)
	if err != nil {
		return UserRecord{}, fmt.Errorf("get user by id: %w", err)
	}
	if err := s.EnsureResourceAccounting(ctx, user.ID); err != nil {
		return UserRecord{}, err
	}

	return user, nil
}

func (s *DeploymentStore) StoreRefreshToken(ctx context.Context, userID string, tokenHash string, expiresAt time.Time) error {
	if s == nil || s.pool == nil {
		return fmt.Errorf("deployment store is not initialized")
	}

	userID = strings.TrimSpace(userID)
	tokenHash = strings.TrimSpace(tokenHash)
	if userID == "" || tokenHash == "" {
		return fmt.Errorf("userID and tokenHash are required")
	}

	_, err := s.pool.Exec(ctx, `
INSERT INTO refresh_tokens (user_id, token_hash, expires_at)
VALUES ($1, $2, $3)
`, userID, tokenHash, expiresAt)
	if err != nil {
		return fmt.Errorf("store refresh token: %w", err)
	}

	return nil
}

func (s *DeploymentStore) ConsumeRefreshToken(ctx context.Context, rawToken string, newTokenHash string, newExpiresAt time.Time) (string, error) {
	if s == nil || s.pool == nil {
		return "", fmt.Errorf("deployment store is not initialized")
	}

	rawToken = strings.TrimSpace(rawToken)
	newTokenHash = strings.TrimSpace(newTokenHash)
	if rawToken == "" || newTokenHash == "" {
		return "", fmt.Errorf("rawToken and newTokenHash are required")
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return "", fmt.Errorf("begin refresh token transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	oldHash := hashToken(rawToken)
	var userID string
	var revokedAt *time.Time
	var expiresAt time.Time
	if err := tx.QueryRow(ctx, `
SELECT user_id::text, revoked_at, expires_at
FROM refresh_tokens
WHERE token_hash = $1
FOR UPDATE
`, oldHash).Scan(&userID, &revokedAt, &expiresAt); err != nil {
		return "", fmt.Errorf("consume refresh token: %w", err)
	}
	if revokedAt != nil || time.Now().After(expiresAt) {
		return "", fmt.Errorf("refresh token is no longer valid")
	}

	if _, err := tx.Exec(ctx, `
UPDATE refresh_tokens
SET revoked_at = now(), last_used_at = now()
WHERE token_hash = $1
`, oldHash); err != nil {
		return "", fmt.Errorf("revoke refresh token: %w", err)
	}

	if _, err := tx.Exec(ctx, `
INSERT INTO refresh_tokens (user_id, token_hash, expires_at)
VALUES ($1, $2, $3)
`, userID, newTokenHash, newExpiresAt); err != nil {
		return "", fmt.Errorf("store rotated refresh token: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("commit refresh token rotation: %w", err)
	}

	return userID, nil
}

func (s *DeploymentStore) StoreAPIKey(ctx context.Context, userID string, name string, keyPrefix string, keyHash string, expiresAt *time.Time) error {
	if s == nil || s.pool == nil {
		return fmt.Errorf("deployment store is not initialized")
	}

	userID = strings.TrimSpace(userID)
	name = strings.TrimSpace(name)
	keyPrefix = strings.TrimSpace(keyPrefix)
	keyHash = strings.TrimSpace(keyHash)
	if userID == "" || name == "" || keyPrefix == "" || keyHash == "" {
		return fmt.Errorf("userID, name, keyPrefix, and keyHash are required")
	}

	_, err := s.pool.Exec(ctx, `
INSERT INTO api_keys (user_id, name, key_prefix, key_hash, expires_at)
VALUES ($1, $2, $3, $4, $5)
`, userID, name, keyPrefix, keyHash, expiresAt)
	if err != nil {
		return fmt.Errorf("store api key: %w", err)
	}

	return nil
}

func (s *DeploymentStore) GetUserByAPIKey(ctx context.Context, rawKey string) (UserRecord, error) {
	if s == nil || s.pool == nil {
		return UserRecord{}, fmt.Errorf("deployment store is not initialized")
	}

	rawKey = strings.TrimSpace(rawKey)
	if rawKey == "" {
		return UserRecord{}, fmt.Errorf("api key is required")
	}

	keyHash := hashToken(rawKey)
	var userID string
	if err := s.pool.QueryRow(ctx, `
UPDATE api_keys
SET last_used_at = now()
WHERE key_hash = $1
  AND revoked_at IS NULL
  AND (expires_at IS NULL OR expires_at > now())
RETURNING user_id::text
`, keyHash).Scan(&userID); err != nil {
		return UserRecord{}, fmt.Errorf("api key not found or expired: %w", err)
	}

	return s.GetUserByID(ctx, userID)
}

func (s *DeploymentStore) CreateDeploymentAttemptForUser(ctx context.Context, ownerUserID string, appName string, repoURL string, appPath string, portMap string, imageName string) (string, error) {
	if s == nil || s.pool == nil {
		return "", fmt.Errorf("deployment store is not initialized")
	}

	ownerUserID = strings.TrimSpace(ownerUserID)
	appName = strings.TrimSpace(appName)
	repoURL = strings.TrimSpace(repoURL)
	appPath = strings.TrimSpace(appPath)
	portMap = strings.TrimSpace(portMap)
	imageName = strings.TrimSpace(imageName)
	if ownerUserID == "" || appName == "" || repoURL == "" || appPath == "" || portMap == "" {
		return "", fmt.Errorf("ownerUserID, appName, repoURL, appPath, and portMap are required")
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return "", fmt.Errorf("begin deployment transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	projectID, err := s.upsertProject(ctx, tx, ownerUserID, appName, repoURL)
	if err != nil {
		return "", err
	}

	var deploymentID string
	if err := tx.QueryRow(ctx, `
INSERT INTO deployments (
	owner_user_id,
	project_id,
	app_name,
	source_repo_url,
	app_path,
	port_map,
	image_name,
	status,
	status_message
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING id::text
`, ownerUserID, projectID, appName, repoURL, appPath, portMap, imageName, DeploymentStatusBuilding, "deployment queued").Scan(&deploymentID); err != nil {
		return "", fmt.Errorf("insert deployment record: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("commit deployment record: %w", err)
	}

	return deploymentID, nil
}

func buildPostgresConnString() (string, error) {
	if rawURL := strings.TrimSpace(os.Getenv("DATABASE_URL")); rawURL != "" {
		return rawURL, nil
	}

	host := envOrDefault("PGHOST", "localhost")
	dbName := envOrDefault("PGDATABASE", "gravyflow")
	port := strings.TrimSpace(os.Getenv("PGPORT"))
	if port == "" {
		if useLocalDevPostgresDefaults(host, dbName) {
			port = "5433"
		} else {
			port = "5432"
		}
	}
	sslMode := envOrDefault("PGSSLMODE", "disable")
	user := strings.TrimSpace(os.Getenv("PGUSER"))
	password := os.Getenv("PGPASSWORD")
	if useLocalDevPostgresDefaults(host, dbName) {
		user = "gravyflow"
		password = "gravyflow"
	}

	u := &url.URL{
		Scheme: "postgres",
		Host:   net.JoinHostPort(host, port),
		Path:   "/" + dbName,
	}
	if user != "" {
		if password != "" {
			u.User = url.UserPassword(user, password)
		} else {
			u.User = url.User(user)
		}
	}

	query := u.Query()
	query.Set("sslmode", sslMode)
	u.RawQuery = query.Encode()

	return u.String(), nil
}

func useLocalDevPostgresDefaults(host string, dbName string) bool {
	host = strings.TrimSpace(host)
	dbName = strings.TrimSpace(dbName)
	return dbName == "gravyflow" && (host == "localhost" || host == "127.0.0.1")
}

func envOrDefault(key string, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}

	return value
}

func int32FromEnv(key string, fallback int32) (int32, error) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback, nil
	}

	parsed, err := strconv.ParseInt(value, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", key, err)
	}
	if parsed < 0 {
		return 0, fmt.Errorf("%s must be non-negative", key)
	}

	return int32(parsed), nil
}

func durationFromEnv(key string, fallback time.Duration) (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback, nil
	}

	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", key, err)
	}
	if parsed < 0 {
		return 0, fmt.Errorf("%s must be non-negative", key)
	}

	return parsed, nil
}

func (s *DeploymentStore) CreateDeploymentAttempt(ctx context.Context, appName string, repoURL string, imageName string) (string, error) {
	if s == nil || s.pool == nil {
		return "", fmt.Errorf("deployment store is not initialized")
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return "", fmt.Errorf("begin deployment transaction: %w", err)
	}
	defer func() {
		_ = tx.Rollback(ctx)
	}()

	ownerID, err := s.upsertSystemUser(ctx, tx)
	if err != nil {
		return "", err
	}

	projectID, err := s.upsertProject(ctx, tx, ownerID, appName, repoURL)
	if err != nil {
		return "", err
	}

	var deploymentID string
	if err := tx.QueryRow(ctx, `
INSERT INTO deployments (
	owner_user_id,
	project_id,
	app_name,
	source_repo_url,
	app_path,
	port_map,
	image_name,
	status,
	status_message
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING id::text
`, ownerID, projectID, appName, repoURL, repoURL, "8080:80", imageName, DeploymentStatusBuilding, "deployment queued").Scan(&deploymentID); err != nil {
		return "", fmt.Errorf("insert deployment record: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("commit deployment record: %w", err)
	}

	return deploymentID, nil
}

func (s *DeploymentStore) UpdateDeploymentStatus(ctx context.Context, deploymentID string, status DeploymentStatus, statusMessage string, containerID string, containerName string) error {
	if s == nil || s.pool == nil {
		return fmt.Errorf("deployment store is not initialized")
	}

	deploymentID = strings.TrimSpace(deploymentID)
	if deploymentID == "" {
		return fmt.Errorf("deploymentID is required")
	}

	s.statusMu.Lock()
	defer s.statusMu.Unlock()

	_, err := s.pool.Exec(ctx, `
UPDATE deployments
SET status = $2,
	status_message = $3,
	container_id = CASE WHEN $4 <> '' THEN $4 ELSE container_id END,
	container_name = CASE WHEN $5 <> '' THEN $5 ELSE container_name END,
	started_at = COALESCE(started_at, now()),
	finished_at = CASE
		WHEN $2 = 'DEPLOYED' OR $2 = 'FAILED' THEN COALESCE(finished_at, now())
		ELSE finished_at
	END,
	updated_at = now()
WHERE id = $1
`, deploymentID, status, statusMessage, containerID, containerName)
	if err != nil {
		return fmt.Errorf("update deployment %s status: %w", deploymentID, err)
	}

	return nil
}

func (s *DeploymentStore) MarkDeploymentFailed(ctx context.Context, deploymentID string, cause error) error {
	if cause == nil {
		cause = fmt.Errorf("deployment failed")
	}

	return s.UpdateDeploymentStatus(ctx, deploymentID, DeploymentStatusFailed, cause.Error(), "", "")
}

func (s *DeploymentStore) MarkDeploymentDeployed(ctx context.Context, deploymentID string, containerID string, containerName string) error {
	return s.UpdateDeploymentStatus(ctx, deploymentID, DeploymentStatusRunning, "deployment completed successfully", containerID, containerName)
}

func (s *DeploymentStore) UpsertDeploymentHealthConfig(ctx context.Context, userID string, deploymentID string, healthCheckPath string, intervalSeconds int, maxRestarts int) error {
	if s == nil || s.pool == nil {
		return fmt.Errorf("deployment store is not initialized")
	}

	userID = strings.TrimSpace(userID)
	deploymentID = strings.TrimSpace(deploymentID)
	healthCheckPath = strings.TrimSpace(healthCheckPath)
	if userID == "" || deploymentID == "" {
		return fmt.Errorf("userID and deploymentID are required")
	}
	if healthCheckPath == "" {
		healthCheckPath = "/health"
	}
	if !strings.HasPrefix(healthCheckPath, "/") {
		healthCheckPath = "/" + healthCheckPath
	}
	if intervalSeconds < 5 {
		intervalSeconds = 5
	}
	if maxRestarts < 1 {
		maxRestarts = 1
	}

	if _, err := s.GetDeploymentForUser(ctx, userID, deploymentID); err != nil {
		return err
	}

	_, err := s.pool.Exec(ctx, `
INSERT INTO deployment_health_configs (
	deployment_id,
	health_check_path,
	health_check_interval_seconds,
	max_restarts_before_failing,
	last_checked_at,
	updated_at
) VALUES ($1, $2, $3, $4, now(), now())
ON CONFLICT (deployment_id)
DO UPDATE SET
	health_check_path = EXCLUDED.health_check_path,
	health_check_interval_seconds = EXCLUDED.health_check_interval_seconds,
	max_restarts_before_failing = EXCLUDED.max_restarts_before_failing,
	last_checked_at = EXCLUDED.last_checked_at,
	updated_at = now()
`, deploymentID, healthCheckPath, intervalSeconds, maxRestarts)
	if err != nil {
		return fmt.Errorf("upsert deployment health config: %w", err)
	}

	return nil
}

func (s *DeploymentStore) GetDeploymentHealthConfigForUser(ctx context.Context, userID string, deploymentID string) (DeploymentHealthConfigRecord, error) {
	if s == nil || s.pool == nil {
		return DeploymentHealthConfigRecord{}, fmt.Errorf("deployment store is not initialized")
	}

	userID = strings.TrimSpace(userID)
	deploymentID = strings.TrimSpace(deploymentID)
	if userID == "" || deploymentID == "" {
		return DeploymentHealthConfigRecord{}, fmt.Errorf("userID and deploymentID are required")
	}

	if _, err := s.GetDeploymentForUser(ctx, userID, deploymentID); err != nil {
		return DeploymentHealthConfigRecord{}, err
	}

	config, err := s.getDeploymentHealthConfig(ctx, deploymentID)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "no rows") {
			return DeploymentHealthConfigRecord{
				DeploymentID:               deploymentID,
				HealthCheckPath:            "/health",
				HealthCheckIntervalSeconds: 30,
				MaxRestartsBeforeFailing:   3,
			}, nil
		}
		return DeploymentHealthConfigRecord{}, err
	}

	return config, nil
}

func (s *DeploymentStore) getDeploymentHealthConfig(ctx context.Context, deploymentID string) (DeploymentHealthConfigRecord, error) {
	var config DeploymentHealthConfigRecord
	config.HealthCheckPath = "/health"
	config.HealthCheckIntervalSeconds = 30
	config.MaxRestartsBeforeFailing = 3

	err := s.pool.QueryRow(ctx, `
SELECT
	deployment_id::text,
	COALESCE(health_check_path, '/health'),
	COALESCE(health_check_interval_seconds, 30),
	COALESCE(max_restarts_before_failing, 3),
	last_checked_at,
	created_at,
	updated_at
FROM deployment_health_configs
WHERE deployment_id = $1
`, deploymentID).Scan(
		&config.DeploymentID,
		&config.HealthCheckPath,
		&config.HealthCheckIntervalSeconds,
		&config.MaxRestartsBeforeFailing,
		&config.LastCheckedAt,
		&config.CreatedAt,
		&config.UpdatedAt,
	)
	if err != nil {
		return DeploymentHealthConfigRecord{}, fmt.Errorf("get deployment health config: %w", err)
	}

	return config, nil
}

func (s *DeploymentStore) ListRunningDeploymentsForHealthCheck(ctx context.Context) ([]RunningDeploymentHealthTarget, error) {
	if s == nil || s.pool == nil {
		return nil, fmt.Errorf("deployment store is not initialized")
	}

	rows, err := s.pool.Query(ctx, `
SELECT
	d.id::text,
	d.owner_user_id::text,
	d.project_id::text,
	d.app_name,
	d.source_repo_url,
	d.app_path,
	d.port_map,
	COALESCE(d.image_name, ''),
	COALESCE(d.container_id, ''),
	COALESCE(d.container_name, ''),
	d.status::text,
	COALESCE(d.status_message, ''),
	d.created_at,
	d.updated_at,
	COALESCE(cfg.health_check_path, '/health'),
	COALESCE(cfg.health_check_interval_seconds, 30),
	COALESCE(cfg.max_restarts_before_failing, 3),
	cfg.last_checked_at
FROM deployments d
LEFT JOIN deployment_health_configs cfg ON cfg.deployment_id = d.id
WHERE d.status = 'RUNNING'
ORDER BY d.updated_at ASC
`)
	if err != nil {
		return nil, fmt.Errorf("list running deployments for health check: %w", err)
	}
	defer rows.Close()

	targets := make([]RunningDeploymentHealthTarget, 0)
	for rows.Next() {
		var target RunningDeploymentHealthTarget
		if err := rows.Scan(
			&target.DeploymentID,
			&target.OwnerUserID,
			&target.ProjectID,
			&target.AppName,
			&target.SourceRepoURL,
			&target.AppPath,
			&target.PortMap,
			&target.ImageName,
			&target.ContainerID,
			&target.ContainerName,
			&target.Status,
			&target.StatusMessage,
			&target.CreatedAt,
			&target.UpdatedAt,
			&target.HealthCheckPath,
			&target.HealthCheckIntervalSeconds,
			&target.MaxRestartsBeforeFailing,
			&target.LastCheckedAt,
		); err != nil {
			return nil, fmt.Errorf("scan running deployment for health check: %w", err)
		}
		targets = append(targets, target)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate running deployments for health check: %w", err)
	}

	return targets, nil
}

func (s *DeploymentStore) MarkDeploymentHealthChecked(ctx context.Context, deploymentID string) error {
	if s == nil || s.pool == nil {
		return fmt.Errorf("deployment store is not initialized")
	}

	deploymentID = strings.TrimSpace(deploymentID)
	if deploymentID == "" {
		return fmt.Errorf("deploymentID is required")
	}

	_, err := s.pool.Exec(ctx, `
UPDATE deployment_health_configs
SET last_checked_at = now(), updated_at = now()
WHERE deployment_id = $1
`, deploymentID)
	if err != nil {
		return fmt.Errorf("mark deployment health checked: %w", err)
	}

	return nil
}

func (s *DeploymentStore) RecordDeploymentRestartAudit(ctx context.Context, deploymentID string, action string, outcome string, reason string, previousContainerID string, newContainerID string) error {
	if s == nil || s.pool == nil {
		return fmt.Errorf("deployment store is not initialized")
	}

	deploymentID = strings.TrimSpace(deploymentID)
	action = strings.TrimSpace(action)
	outcome = strings.TrimSpace(outcome)
	reason = strings.TrimSpace(reason)
	previousContainerID = strings.TrimSpace(previousContainerID)
	newContainerID = strings.TrimSpace(newContainerID)
	if deploymentID == "" || action == "" || outcome == "" || reason == "" {
		return fmt.Errorf("deploymentID, action, outcome, and reason are required")
	}

	_, err := s.pool.Exec(ctx, `
INSERT INTO deployment_restart_audits (
	deployment_id,
	action,
	outcome,
	reason,
	previous_container_id,
	new_container_id
) VALUES ($1, $2, $3, $4, $5, $6)
`, deploymentID, action, outcome, reason, previousContainerID, newContainerID)
	if err != nil {
		return fmt.Errorf("insert deployment restart audit: %w", err)
	}

	return nil
}

func (s *DeploymentStore) CountRecentDeploymentRestarts(ctx context.Context, deploymentID string, window time.Duration) (int, error) {
	if s == nil || s.pool == nil {
		return 0, fmt.Errorf("deployment store is not initialized")
	}

	deploymentID = strings.TrimSpace(deploymentID)
	if deploymentID == "" {
		return 0, fmt.Errorf("deploymentID is required")
	}
	if window <= 0 {
		window = 5 * time.Minute
	}

	var count int
	windowSeconds := int64(window.Seconds())
	if windowSeconds < 1 {
		windowSeconds = int64((5 * time.Minute).Seconds())
	}

	if err := s.pool.QueryRow(ctx, `
SELECT COUNT(*)
FROM deployment_restart_audits
WHERE deployment_id = $1
  AND action = 'restart'
  AND created_at >= now() - ($2 * interval '1 second')
`, deploymentID, windowSeconds).Scan(&count); err != nil {
		return 0, fmt.Errorf("count recent deployment restarts: %w", err)
	}

	return count, nil
}

func (s *DeploymentStore) LoadDeploymentEnvMapByDeploymentID(ctx context.Context, deploymentID string) (map[string]string, error) {
	if s == nil || s.pool == nil {
		return nil, fmt.Errorf("deployment store is not initialized")
	}

	deploymentID = strings.TrimSpace(deploymentID)
	if deploymentID == "" {
		return nil, fmt.Errorf("deploymentID is required")
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

func (s *DeploymentStore) upsertSystemUser(ctx context.Context, tx pgx.Tx) (string, error) {
	var userID string
	if err := tx.QueryRow(ctx, `
INSERT INTO users (email, password_hash, display_name)
VALUES ($1, $2, $3)
ON CONFLICT (email)
DO UPDATE SET
	password_hash = EXCLUDED.password_hash,
	display_name = EXCLUDED.display_name,
	updated_at = now()
RETURNING id::text
`, defaultDeploymentOwnerEmail, defaultSystemPasswordHash, defaultDeploymentOwnerName).Scan(&userID); err != nil {
		return "", fmt.Errorf("upsert deployment owner: %w", err)
	}

	return userID, nil
}

func (s *DeploymentStore) upsertProject(ctx context.Context, tx pgx.Tx, ownerID string, appName string, repoURL string) (string, error) {
	var projectID string
	projectSlug := slugifyName(appName)
	if projectSlug == "" {
		projectSlug = "deployment"
	}

	if err := tx.QueryRow(ctx, `
INSERT INTO projects (owner_user_id, slug, display_name, repo_url)
VALUES ($1, $2, $3, $4)
ON CONFLICT (owner_user_id, slug)
DO UPDATE SET
	display_name = EXCLUDED.display_name,
	repo_url = EXCLUDED.repo_url,
	updated_at = now()
RETURNING id::text
`, ownerID, projectSlug, appName, repoURL).Scan(&projectID); err != nil {
		return "", fmt.Errorf("upsert project: %w", err)
	}

	return projectID, nil
}

func (s *DeploymentStore) ListDeploymentsForUser(ctx context.Context, userID string) ([]DeploymentRecord, error) {
	if s == nil || s.pool == nil {
		return nil, fmt.Errorf("deployment store is not initialized")
	}

	userID = strings.TrimSpace(userID)
	if userID == "" {
		return nil, fmt.Errorf("userID is required")
	}

	rows, err := s.pool.Query(ctx, `
SELECT
	d.id::text,
	d.project_id::text,
	d.app_name,
	d.source_repo_url,
	d.app_path,
	d.port_map,
	COALESCE(d.image_name, ''),
	COALESCE(d.container_id, ''),
	COALESCE(d.container_name, ''),
	d.status::text,
	COALESCE(d.status_message, ''),
	d.created_at,
	d.updated_at
FROM deployments d
WHERE d.owner_user_id = $1
ORDER BY d.created_at DESC
`, userID)
	if err != nil {
		return nil, fmt.Errorf("list deployments for user: %w", err)
	}
	defer rows.Close()

	deployments := make([]DeploymentRecord, 0)
	for rows.Next() {
		var deployment DeploymentRecord
		if err := rows.Scan(
			&deployment.DeploymentID,
			&deployment.ProjectID,
			&deployment.AppName,
			&deployment.SourceRepoURL,
			&deployment.AppPath,
			&deployment.PortMap,
			&deployment.ImageName,
			&deployment.ContainerID,
			&deployment.ContainerName,
			&deployment.Status,
			&deployment.StatusMessage,
			&deployment.CreatedAt,
			&deployment.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan deployment: %w", err)
		}
		deployments = append(deployments, deployment)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate deployments: %w", err)
	}

	return deployments, nil
}

func (s *DeploymentStore) GetDeploymentForUser(ctx context.Context, userID string, deploymentID string) (DeploymentRecord, error) {
	if s == nil || s.pool == nil {
		return DeploymentRecord{}, fmt.Errorf("deployment store is not initialized")
	}

	userID = strings.TrimSpace(userID)
	deploymentID = strings.TrimSpace(deploymentID)
	if userID == "" || deploymentID == "" {
		return DeploymentRecord{}, fmt.Errorf("userID and deploymentID are required")
	}

	var deployment DeploymentRecord
	err := s.pool.QueryRow(ctx, `
SELECT
	d.id::text,
	d.project_id::text,
	d.app_name,
	d.source_repo_url,
	d.app_path,
	d.port_map,
	COALESCE(d.image_name, ''),
	COALESCE(d.container_id, ''),
	COALESCE(d.container_name, ''),
	d.status::text,
	COALESCE(d.status_message, ''),
	d.created_at,
	d.updated_at
FROM deployments d
WHERE d.owner_user_id = $1 AND d.id = $2
`, userID, deploymentID).Scan(
		&deployment.DeploymentID,
		&deployment.ProjectID,
		&deployment.AppName,
		&deployment.SourceRepoURL,
		&deployment.AppPath,
		&deployment.PortMap,
		&deployment.ImageName,
		&deployment.ContainerID,
		&deployment.ContainerName,
		&deployment.Status,
		&deployment.StatusMessage,
		&deployment.CreatedAt,
		&deployment.UpdatedAt,
	)
	if err != nil {
		return DeploymentRecord{}, fmt.Errorf("get deployment for user: %w", err)
	}

	return deployment, nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func generateRandomToken(numBytes int) (string, error) {
	buf := make([]byte, numBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}

	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func slugifyName(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return ""
	}

	var builder strings.Builder
	builder.Grow(len(value))
	previousDash := false

	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			builder.WriteRune(r)
			previousDash = false
		case r == '-' || r == '_' || r == ' ' || r == '.':
			if !previousDash {
				builder.WriteRune('-')
				previousDash = true
			}
		default:
			if !previousDash {
				builder.WriteRune('-')
				previousDash = true
			}
		}
	}

	return strings.Trim(builder.String(), "-")
}
