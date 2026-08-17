package main

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
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
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ============================================================================
// CONSTANTS
// ============================================================================

const (
	defaultDeploymentOwnerEmail = "system@gravyflow.local"
	defaultDeploymentOwnerName  = "GravyFlow System"
	defaultSystemPasswordHash   = "system-managed"
	defaultMaxRetries           = 3
	defaultRetryDelay           = 5 * time.Second
)

type DeploymentStatus string

const (
	DeploymentStatusBuilding DeploymentStatus = "BUILDING"
	DeploymentStatusDeployed DeploymentStatus = "DEPLOYED"
	DeploymentStatusFailed   DeploymentStatus = "FAILED"
	DeploymentStatusRunning  DeploymentStatus = "RUNNING"
	DeploymentStatusStopped  DeploymentStatus = "STOPPED"
	DeploymentStatusPaused   DeploymentStatus = "PAUSED"
)

// ============================================================================
// TYPES
// ============================================================================

type UserRecord struct {
	ID            string
	Email         string
	DisplayName   string
	PasswordHash  string
	IsAdmin       bool
	Status        string
	DeletedAt     *time.Time
	DeletedReason string
	MFAEnabled    bool
	MFATOTPSecret string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// Account status values for Module A (User & Team Administration). Stored in
// users.status as plain TEXT (no DB-level CHECK constraint, consistent with
// deployments.status elsewhere in this schema) and validated in Go.
const (
	UserStatusActive    = "active"
	UserStatusSuspended = "suspended"
	UserStatusFlagged   = "flagged"
	UserStatusDeleted   = "deleted"
)

func isValidUserStatus(status string) bool {
	switch status {
	case UserStatusActive, UserStatusSuspended, UserStatusFlagged, UserStatusDeleted:
		return true
	default:
		return false
	}
}

type ProjectRecord struct {
	ID          string
	OwnerUserID string
	Slug        string
	DisplayName string
	RepoURL     string
	CreatedAt   time.Time
	UpdatedAt   time.Time
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
	StartedAt     *time.Time
	FinishedAt    *time.Time
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

type Pagination struct {
	Limit  int
	Offset int
	Order  string
}

type PaginatedResult struct {
	Items      []DeploymentRecord
	TotalCount int
	Page       int
	PerPage    int
	TotalPages int
}

type StoreErrorType string

const (
	ErrNotFound      StoreErrorType = "NOT_FOUND"
	ErrConflict      StoreErrorType = "CONFLICT"
	ErrInvalidInput  StoreErrorType = "INVALID_INPUT"
	ErrUnauthorized  StoreErrorType = "UNAUTHORIZED"
	ErrDatabase      StoreErrorType = "DATABASE_ERROR"
	ErrTimeout       StoreErrorType = "TIMEOUT"
	ErrRateLimit     StoreErrorType = "RATE_LIMIT"
	ErrQuotaExceeded StoreErrorType = "QUOTA_EXCEEDED"
)

type StoreError struct {
	Type    StoreErrorType
	Code    string
	Message string
	Err     error
}

func (e *StoreError) Error() string {
	if e == nil {
		return ""
	}
	if e.Err != nil {
		return fmt.Sprintf("%s: %s: %v", e.Type, e.Message, e.Err)
	}
	return fmt.Sprintf("%s: %s", e.Type, e.Message)
}

type PoolStats struct {
	TotalConnections   int32
	IdleConnections    int32
	ActiveConnections  int32
	MaxConnections     int32
	AcquireCount       int64
	HitCount           int64
	MissCount          int64
	TimeoutCount       int64
	NewConnsCount      int64
	DestroyCount       int64
	MaxLifetimeDestroy int64
	IdleDestroyCount   int64
}

type DeploymentChange struct {
	ID           string
	DeploymentID string
	Field        string
	OldValue     string
	NewValue     string
	ChangedBy    string
	CreatedAt    time.Time
}

type MigrationRecord struct {
	Version     int
	Description string
	AppliedAt   time.Time
}

type QueryLogger struct {
	pool     *pgxpool.Pool
	enabled  bool
	slowTime time.Duration
}

// ============================================================================
// DEPLOYMENT STORE
// ============================================================================

type DeploymentStore struct {
	pool     *pgxpool.Pool
	statusMu sync.Mutex
	logger   *QueryLogger
}

// Note: deploymentStore variable is now in globals.go
// var deploymentStore *DeploymentStore
// var initStoreOnce sync.Once

// ============================================================================
// INITIALIZATION
// ============================================================================

func NewDeploymentStore(ctx context.Context) (*DeploymentStore, error) {
	connString, err := buildPostgresConnString()
	if err != nil {
		return nil, &StoreError{
			Type:    ErrDatabase,
			Message: "failed to build connection string",
			Err:     err,
		}
	}

	config, err := pgxpool.ParseConfig(connString)
	if err != nil {
		return nil, &StoreError{
			Type:    ErrDatabase,
			Message: "failed to parse connection string",
			Err:     err,
		}
	}

	// Configure pool
	if maxConns, err := int32FromEnv("PGMAXCONNS", 10); err == nil {
		config.MaxConns = maxConns
	} else {
		return nil, err
	}
	if minConns, err := int32FromEnv("PGMINCONNS", 2); err == nil {
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
		return nil, &StoreError{
			Type:    ErrDatabase,
			Message: "failed to create connection pool",
			Err:     err,
		}
	}

	// Ping with retry
	if err := pingWithRetry(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}

	store := &DeploymentStore{
		pool: pool,
		logger: &QueryLogger{
			pool:     pool,
			enabled:  os.Getenv("PGQUERYLOGGING") == "1",
			slowTime: 100 * time.Millisecond,
		},
	}

	// Run migrations
	if err := store.CreateTablesIfNotExists(ctx); err != nil {
		pool.Close()
		return nil, err
	}

	// These were previously defined but never invoked at startup, so the
	// quotas/resource_usage/quota_history/quota_alerts and deployment env var
	// tables they guard were only ever created on a fresh docker-entrypoint
	// initdb run (via db/schema.sql), never on an existing database. Wiring
	// them in here is required for Module C (quota overrides) to work at all.
	if err := store.CreateQuotaTablesIfNotExists(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	if err := store.CreateEnvTablesIfNotExists(ctx); err != nil {
		pool.Close()
		return nil, err
	}

	if err := store.BootstrapAdminUsers(ctx); err != nil {
		pool.Close()
		return nil, err
	}

	return store, nil
}

func newDeploymentStore(ctx context.Context) (*DeploymentStore, error) {
	return NewDeploymentStore(ctx)
}

func InitDeploymentStore(ctx context.Context) error {
	var err error
	initStoreOnce.Do(func() {
		deploymentStore, err = NewDeploymentStore(ctx)
	})
	return err
}

func GetDeploymentStore() *DeploymentStore {
	return deploymentStore
}

func (s *DeploymentStore) Close() {
	if s == nil || s.pool == nil {
		return
	}
	s.pool.Close()
	log.Println("Database connections closed")
}

func (s *DeploymentStore) Shutdown(ctx context.Context) error {
	log.Println("Shutting down database connections...")
	s.Close()
	log.Println("Database shutdown complete")
	return nil
}

// ============================================================================
// CONNECTION HELPERS
// ============================================================================

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

	return &StoreError{
		Type:    ErrDatabase,
		Message: "postgres not reachable after attempts",
		Err:     lastErr,
	}
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

// Note: envOrDefault, durationFromEnv, hashToken, generateRandomToken, isRetryableError
// are now in helpers.go

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

// ============================================================================
// QUERY LOGGER
// ============================================================================

func (l *QueryLogger) Query(ctx context.Context, sql string, args ...interface{}) (pgx.Rows, error) {
	if !l.enabled {
		return l.pool.Query(ctx, sql, args...)
	}

	start := time.Now()
	rows, err := l.pool.Query(ctx, sql, args...)
	duration := time.Since(start)

	if duration > l.slowTime {
		log.Printf("[DB SLOW] %v: %s", duration, sql)
	} else {
		log.Printf("[DB] %v: %s", duration, sql)
	}

	return rows, err
}

func (l *QueryLogger) QueryRow(ctx context.Context, sql string, args ...interface{}) pgx.Row {
	if !l.enabled {
		return l.pool.QueryRow(ctx, sql, args...)
	}

	start := time.Now()
	row := l.pool.QueryRow(ctx, sql, args...)
	duration := time.Since(start)

	if duration > l.slowTime {
		log.Printf("[DB SLOW] %v: %s", duration, sql)
	} else {
		log.Printf("[DB] %v: %s", duration, sql)
	}

	return row
}

func (l *QueryLogger) Exec(ctx context.Context, sql string, args ...interface{}) (pgconn.CommandTag, error) {
	if !l.enabled {
		return l.pool.Exec(ctx, sql, args...)
	}

	start := time.Now()
	tag, err := l.pool.Exec(ctx, sql, args...)
	duration := time.Since(start)

	if duration > l.slowTime {
		log.Printf("[DB SLOW] %v: %s", duration, sql)
	} else {
		log.Printf("[DB] %v: %s", duration, sql)
	}

	return tag, err
}

// ============================================================================
// MIGRATIONS
// ============================================================================

func (s *DeploymentStore) CreateTablesIfNotExists(ctx context.Context) error {
	migrations := []struct {
		version int
		sql     string
	}{
		{1, `
CREATE TABLE IF NOT EXISTS users (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email TEXT UNIQUE NOT NULL,
    password_hash TEXT NOT NULL,
    display_name TEXT NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT now(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT now()
)`},
		{2, `
CREATE TABLE IF NOT EXISTS projects (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    slug TEXT NOT NULL,
    display_name TEXT NOT NULL,
    repo_url TEXT,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT now(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT now(),
    UNIQUE(owner_user_id, slug)
)`},
		{3, `
CREATE TABLE IF NOT EXISTS deployments (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    app_name TEXT NOT NULL,
    source_repo_url TEXT NOT NULL,
    app_path TEXT NOT NULL,
    port_map TEXT NOT NULL,
    image_name TEXT,
    container_id TEXT,
    container_name TEXT,
    status TEXT DEFAULT 'BUILDING',
    status_message TEXT,
    started_at TIMESTAMP WITH TIME ZONE,
    finished_at TIMESTAMP WITH TIME ZONE,
    deleted_at TIMESTAMP WITH TIME ZONE,
    deleted_reason TEXT,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT now(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT now()
)`},
		{4, `
CREATE TABLE IF NOT EXISTS refresh_tokens (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash TEXT NOT NULL,
    expires_at TIMESTAMP WITH TIME ZONE NOT NULL,
    revoked_at TIMESTAMP WITH TIME ZONE,
    last_used_at TIMESTAMP WITH TIME ZONE,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT now(),
    UNIQUE(token_hash)
)`},
		{5, `
CREATE TABLE IF NOT EXISTS api_keys (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name TEXT NOT NULL,
    key_prefix TEXT NOT NULL,
    key_hash TEXT NOT NULL,
    expires_at TIMESTAMP WITH TIME ZONE,
    revoked_at TIMESTAMP WITH TIME ZONE,
    last_used_at TIMESTAMP WITH TIME ZONE,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT now(),
    UNIQUE(key_hash)
)`},
		{6, `
CREATE TABLE IF NOT EXISTS deployment_health_configs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    deployment_id UUID NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
    health_check_path TEXT DEFAULT '/health',
    health_check_interval_seconds INTEGER DEFAULT 30,
    max_restarts_before_failing INTEGER DEFAULT 3,
    last_checked_at TIMESTAMP WITH TIME ZONE,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT now(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT now(),
    UNIQUE(deployment_id)
)`},
		{7, `
CREATE TABLE IF NOT EXISTS deployment_restart_audits (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    deployment_id UUID NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
    action TEXT NOT NULL,
    outcome TEXT NOT NULL,
    reason TEXT NOT NULL,
    previous_container_id TEXT,
    new_container_id TEXT,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT now()
)`},
		{8, `
CREATE TABLE IF NOT EXISTS deployment_env_vars (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    deployment_id UUID NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
    env_key TEXT NOT NULL,
    encrypted_value BYTEA NOT NULL,
    nonce BYTEA NOT NULL,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT now(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT now(),
    UNIQUE(deployment_id, env_key)
)`},
		{9, `
CREATE TABLE IF NOT EXISTS user_resource_accounting (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    deployment_limit INTEGER DEFAULT 10,
    api_key_limit INTEGER DEFAULT 50,
    storage_limit BIGINT DEFAULT 1073741824,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT now(),
    updated_at TIMESTAMP WITH TIME ZONE DEFAULT now(),
    UNIQUE(user_id)
)`},
		{10, `
CREATE TABLE IF NOT EXISTS schema_migrations (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    version INTEGER NOT NULL UNIQUE,
    description TEXT NOT NULL,
    applied_at TIMESTAMP WITH TIME ZONE DEFAULT now()
)`},
		{11, `
CREATE TABLE IF NOT EXISTS deployment_changes (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    deployment_id UUID NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
    field TEXT NOT NULL,
    old_value TEXT,
    new_value TEXT,
    changed_by UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT now()
)`},
		// domains is created out-of-band by db/schema.sql on initial provisioning
		// (not by an earlier numbered migration here), so it may already exist
		// without expires_at on environments provisioned before that column was
		// added. Guarded so it's a no-op if the table doesn't exist yet either.
		{12, `
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = 'public' AND table_name = 'domains') THEN
        ALTER TABLE domains ADD COLUMN IF NOT EXISTS expires_at TIMESTAMP WITH TIME ZONE;
    END IF;
END $$`},
		// Admin Control Panel (Module A): account status/role columns on users.
		{13, `
DO $$
BEGIN
    ALTER TABLE users ADD COLUMN IF NOT EXISTS is_admin BOOLEAN NOT NULL DEFAULT FALSE;
    ALTER TABLE users ADD COLUMN IF NOT EXISTS status TEXT NOT NULL DEFAULT 'active';
    ALTER TABLE users ADD COLUMN IF NOT EXISTS deleted_at TIMESTAMP WITH TIME ZONE;
    ALTER TABLE users ADD COLUMN IF NOT EXISTS deleted_reason TEXT;
    ALTER TABLE users ADD COLUMN IF NOT EXISTS last_login_at TIMESTAMP WITH TIME ZONE;
    ALTER TABLE users ADD COLUMN IF NOT EXISTS mfa_enabled BOOLEAN NOT NULL DEFAULT FALSE;
    ALTER TABLE users ADD COLUMN IF NOT EXISTS mfa_totp_secret TEXT;

    -- deployment_env_vars was created by migration 8 without these columns;
    -- envs.go's ListDeploymentEnvVarsWithValues (used by both the self-service
    -- env manager and the admin secret inspector) has always selected them, so
    -- on any database provisioned before this migration that query 500s. Add
    -- them here so it actually works instead of only on a fresh install.
    ALTER TABLE deployment_env_vars ADD COLUMN IF NOT EXISTS category TEXT DEFAULT 'general';
    ALTER TABLE deployment_env_vars ADD COLUMN IF NOT EXISTS sensitive BOOLEAN DEFAULT FALSE;
    ALTER TABLE deployment_env_vars ADD COLUMN IF NOT EXISTS description TEXT;
END $$`},
		// Admin Control Panel (Module D): immutable audit log. A BEFORE
		// UPDATE/DELETE trigger enforces immutability at the database level, not
		// just by omitting update/delete endpoints in the API.
		//
		// db/schema.sql (fresh-install path only, see the CreateTablesIfNotExists
		// doc comment) already defines an older audit_logs shape
		// (user_id/resource_type/resource_id/user_agent) that predates this
		// migration. Rather than CREATE TABLE IF NOT EXISTS — a no-op against
		// that older shape — this ALTERs it onto parity with what RecordAuditLog
		// in admin_audit.go actually writes, on both a fresh install and an
		// existing database that never had audit_logs at all. The added columns
		// are nullable at the DB level (Go always populates them) so this is
		// safe to run even if legacy rows already exist.
		{14, `
CREATE TABLE IF NOT EXISTS audit_logs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    created_at TIMESTAMP WITH TIME ZONE DEFAULT now()
);
ALTER TABLE audit_logs ADD COLUMN IF NOT EXISTS actor_user_id UUID REFERENCES users(id) ON DELETE SET NULL;
ALTER TABLE audit_logs ADD COLUMN IF NOT EXISTS actor_email TEXT;
ALTER TABLE audit_logs ADD COLUMN IF NOT EXISTS action TEXT;
ALTER TABLE audit_logs ADD COLUMN IF NOT EXISTS target_type TEXT;
ALTER TABLE audit_logs ADD COLUMN IF NOT EXISTS target_id TEXT;
ALTER TABLE audit_logs ADD COLUMN IF NOT EXISTS details JSONB;
ALTER TABLE audit_logs ADD COLUMN IF NOT EXISTS ip_address TEXT;
-- db/schema.sql's fresh-install shape declares resource_type NOT NULL;
-- RecordAuditLog never populates it (it writes target_type instead), so an
-- unrelaxed constraint would reject every insert on a database provisioned
-- from that file.
ALTER TABLE audit_logs ALTER COLUMN resource_type DROP NOT NULL;
-- db/schema.sql's fresh-install shape declares ip_address INET; RecordAuditLog
-- always writes c.ClientIP() as plain text, and an empty/malformed value (no
-- client IP in a test context, a masked value, etc.) fails Postgres's inet
-- validation on insert. Widen unconditionally to TEXT — a no-op if it's
-- already TEXT, a safe conversion if it's still INET from schema.sql.
ALTER TABLE audit_logs ALTER COLUMN ip_address TYPE TEXT USING ip_address::text;

CREATE INDEX IF NOT EXISTS idx_audit_logs_created_at ON audit_logs(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_audit_logs_actor ON audit_logs(actor_user_id);
CREATE INDEX IF NOT EXISTS idx_audit_logs_target ON audit_logs(target_type, target_id);

CREATE OR REPLACE FUNCTION prevent_audit_log_mutation() RETURNS trigger AS $body$
BEGIN
    RAISE EXCEPTION 'audit_logs is immutable: % is not permitted', TG_OP;
END;
$body$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS audit_logs_no_update ON audit_logs;
CREATE TRIGGER audit_logs_no_update BEFORE UPDATE ON audit_logs
    FOR EACH ROW EXECUTE FUNCTION prevent_audit_log_mutation();

DROP TRIGGER IF EXISTS audit_logs_no_delete ON audit_logs;
CREATE TRIGGER audit_logs_no_delete BEFORE DELETE ON audit_logs
    FOR EACH ROW EXECUTE FUNCTION prevent_audit_log_mutation()`},
		// Admin Control Panel (Module C): credit ledger + fraud/abuse risk alerts,
		// and (Module A) short-lived read-only impersonation grants.
		{15, `
CREATE TABLE IF NOT EXISTS credit_ledger (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    amount_cents BIGINT NOT NULL,
    entry_type TEXT NOT NULL,
    reason TEXT NOT NULL,
    issued_by UUID NOT NULL REFERENCES users(id),
    created_at TIMESTAMP WITH TIME ZONE DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_credit_ledger_user_id ON credit_ledger(user_id);

CREATE TABLE IF NOT EXISTS risk_alerts (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    deployment_id UUID REFERENCES deployments(id) ON DELETE CASCADE,
    risk_score INTEGER NOT NULL,
    reason TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'open',
    resolved_by UUID REFERENCES users(id),
    resolved_at TIMESTAMP WITH TIME ZONE,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_risk_alerts_status ON risk_alerts(status);
CREATE INDEX IF NOT EXISTS idx_risk_alerts_user_id ON risk_alerts(user_id);

CREATE TABLE IF NOT EXISTS impersonation_grants (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    admin_user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    target_user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    reason TEXT,
    expires_at TIMESTAMP WITH TIME ZONE NOT NULL,
    revoked_at TIMESTAMP WITH TIME ZONE,
    created_at TIMESTAMP WITH TIME ZONE DEFAULT now()
)`},
		// AdminHardDeleteUser (Module A) must be able to delete an admin who
		// has ever acted on someone ELSE's records — e.g. changed another
		// user's quota or issued them credit. quota_history.changed_by and
		// credit_ledger.issued_by were both NOT NULL REFERENCES users(id) with
		// no ON DELETE clause (defaults to RESTRICT), which blocks deleting
		// that admin entirely as long as the history row exists. Widen both to
		// nullable + ON DELETE SET NULL, the same pattern already used by
		// audit_logs.actor_user_id, so the target's own quota/credit rows
		// still cascade-delete via user_id, but a now-gone actor just leaves a
		// null "changed_by" instead of blocking deletion.
		{16, `
ALTER TABLE quota_history ALTER COLUMN changed_by DROP NOT NULL;
ALTER TABLE quota_history DROP CONSTRAINT IF EXISTS quota_history_changed_by_fkey;
ALTER TABLE quota_history ADD CONSTRAINT quota_history_changed_by_fkey FOREIGN KEY (changed_by) REFERENCES users(id) ON DELETE SET NULL;

ALTER TABLE credit_ledger ALTER COLUMN issued_by DROP NOT NULL;
ALTER TABLE credit_ledger DROP CONSTRAINT IF EXISTS credit_ledger_issued_by_fkey;
ALTER TABLE credit_ledger ADD CONSTRAINT credit_ledger_issued_by_fkey FOREIGN KEY (issued_by) REFERENCES users(id) ON DELETE SET NULL;

ALTER TABLE risk_alerts DROP CONSTRAINT IF EXISTS risk_alerts_resolved_by_fkey;
ALTER TABLE risk_alerts ADD CONSTRAINT risk_alerts_resolved_by_fkey FOREIGN KEY (resolved_by) REFERENCES users(id) ON DELETE SET NULL;

-- audit_logs.actor_user_id was declared ON DELETE SET NULL, but Postgres
-- implements that as an UPDATE against audit_logs, and the BEFORE UPDATE
-- immutability trigger (migration 14) unconditionally rejects every UPDATE —
-- including this system-generated one. That makes AdminHardDeleteUser fail
-- for any user who ever appears as an actor in the audit log, i.e. almost
-- every admin. The fix is to drop the FK outright: an immutable log must
-- survive the actor being deleted unchanged, not have it nulled out, and
-- actor_email is already stored as the durable, human-readable identifier
-- for exactly this reason.
ALTER TABLE audit_logs DROP CONSTRAINT IF EXISTS audit_logs_actor_user_id_fkey`},
		// Admin Control Panel (Module A): user search/filtering also needs to
		// match by GitHub handle, alongside the existing email/user-id/workspace
		// filters in AdminListUsers. There is no OAuth/GitHub-linking flow in
		// this codebase yet, so the column is a plain nullable TEXT an operator
		// (or a future linking flow) can populate directly.
		{17, `ALTER TABLE users ADD COLUMN IF NOT EXISTS github_handle TEXT`},
	}

	for _, migration := range migrations {
		if err := s.applyMigration(ctx, migration.version, migration.sql); err != nil {
			return fmt.Errorf("migration %d failed: %w", migration.version, err)
		}
	}

	return nil
}

func (s *DeploymentStore) applyMigration(ctx context.Context, version int, sql string) error {
	// Check if migration already applied
	var exists bool
	err := s.pool.QueryRow(ctx, `
SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version = $1)
`, version).Scan(&exists)
	if err != nil {
		// Table might not exist yet, create it
		if strings.Contains(err.Error(), "relation \"schema_migrations\" does not exist") {
			_, err := s.pool.Exec(ctx, `
CREATE TABLE IF NOT EXISTS schema_migrations (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    version INTEGER NOT NULL UNIQUE,
    description TEXT NOT NULL,
    applied_at TIMESTAMP WITH TIME ZONE DEFAULT now()
)`)
			if err != nil {
				return err
			}
		} else {
			return err
		}
	}

	if exists {
		return nil
	}

	// Apply migration
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, sql); err != nil {
		return err
	}

	if _, err := tx.Exec(ctx, `
INSERT INTO schema_migrations (version, description) VALUES ($1, $2)
`, version, fmt.Sprintf("Migration %d", version)); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

// ============================================================================
// USER OPERATIONS
// ============================================================================

func (s *DeploymentStore) CreateUser(ctx context.Context, email string, displayName string, passwordHash string) (UserRecord, error) {
	if s == nil || s.pool == nil {
		return UserRecord{}, &StoreError{
			Type:    ErrDatabase,
			Message: "deployment store is not initialized",
		}
	}

	email = strings.ToLower(strings.TrimSpace(email))
	displayName = strings.TrimSpace(displayName)
	passwordHash = strings.TrimSpace(passwordHash)

	if err := s.validateUserInput(email, displayName, passwordHash); err != nil {
		return UserRecord{}, err
	}

	// Check if user already exists
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM users WHERE email = $1)`, email).Scan(&exists); err != nil {
		return UserRecord{}, &StoreError{
			Type:    ErrDatabase,
			Message: "failed to check user existence",
			Err:     err,
		}
	}
	if exists {
		return UserRecord{}, &StoreError{
			Type:    ErrConflict,
			Message: "user already exists",
		}
	}

	var user UserRecord
	err := s.pool.QueryRow(ctx, `
INSERT INTO users (email, password_hash, display_name)
VALUES ($1, $2, $3)
RETURNING id::text, email, display_name, password_hash, is_admin, status, deleted_at, COALESCE(deleted_reason, ''), mfa_enabled, COALESCE(mfa_totp_secret, ''), created_at, updated_at
`, email, passwordHash, displayName).Scan(
		&user.ID, &user.Email, &user.DisplayName, &user.PasswordHash,
		&user.IsAdmin, &user.Status, &user.DeletedAt, &user.DeletedReason,
		&user.MFAEnabled, &user.MFATOTPSecret, &user.CreatedAt, &user.UpdatedAt,
	)
	if err != nil {
		return UserRecord{}, &StoreError{
			Type:    ErrDatabase,
			Message: "failed to create user",
			Err:     err,
		}
	}

	if err := s.EnsureResourceAccounting(ctx, user.ID); err != nil {
		return UserRecord{}, err
	}

	return user, nil
}

func (s *DeploymentStore) GetUserByEmail(ctx context.Context, email string) (UserRecord, error) {
	if s == nil || s.pool == nil {
		return UserRecord{}, &StoreError{
			Type:    ErrDatabase,
			Message: "deployment store is not initialized",
		}
	}

	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return UserRecord{}, &StoreError{
			Type:    ErrInvalidInput,
			Message: "email is required",
		}
	}

	var user UserRecord
	err := s.pool.QueryRow(ctx, `
SELECT id::text, email, display_name, password_hash, is_admin, status, deleted_at, COALESCE(deleted_reason, ''), mfa_enabled, COALESCE(mfa_totp_secret, ''), created_at, updated_at
FROM users
WHERE email = $1
`, email).Scan(
		&user.ID, &user.Email, &user.DisplayName, &user.PasswordHash,
		&user.IsAdmin, &user.Status, &user.DeletedAt, &user.DeletedReason,
		&user.MFAEnabled, &user.MFATOTPSecret, &user.CreatedAt, &user.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return UserRecord{}, &StoreError{
				Type:    ErrNotFound,
				Message: "user not found",
			}
		}
		return UserRecord{}, &StoreError{
			Type:    ErrDatabase,
			Message: "failed to get user",
			Err:     err,
		}
	}

	return user, nil
}

func (s *DeploymentStore) GetUserByID(ctx context.Context, userID string) (UserRecord, error) {
	if s == nil || s.pool == nil {
		return UserRecord{}, &StoreError{
			Type:    ErrDatabase,
			Message: "deployment store is not initialized",
		}
	}

	userID = strings.TrimSpace(userID)
	if userID == "" {
		return UserRecord{}, &StoreError{
			Type:    ErrInvalidInput,
			Message: "userID is required",
		}
	}

	var user UserRecord
	err := s.pool.QueryRow(ctx, `
SELECT id::text, email, display_name, password_hash, is_admin, status, deleted_at, COALESCE(deleted_reason, ''), mfa_enabled, COALESCE(mfa_totp_secret, ''), created_at, updated_at
FROM users
WHERE id = $1
`, userID).Scan(
		&user.ID, &user.Email, &user.DisplayName, &user.PasswordHash,
		&user.IsAdmin, &user.Status, &user.DeletedAt, &user.DeletedReason,
		&user.MFAEnabled, &user.MFATOTPSecret, &user.CreatedAt, &user.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return UserRecord{}, &StoreError{
				Type:    ErrNotFound,
				Message: "user not found",
			}
		}
		return UserRecord{}, &StoreError{
			Type:    ErrDatabase,
			Message: "failed to get user",
			Err:     err,
		}
	}

	return user, nil
}

// UpdateLastLogin stamps last_login_at on a successful login. Best-effort:
// callers should log rather than fail the login request if this errors.
func (s *DeploymentStore) UpdateLastLogin(ctx context.Context, userID string) error {
	if s == nil || s.pool == nil {
		return &StoreError{Type: ErrDatabase, Message: "deployment store is not initialized"}
	}
	_, err := s.pool.Exec(ctx, `UPDATE users SET last_login_at = now() WHERE id = $1`, userID)
	return err
}

func (s *DeploymentStore) UpdateUser(ctx context.Context, userID string, displayName string) error {
	if s == nil || s.pool == nil {
		return &StoreError{
			Type:    ErrDatabase,
			Message: "deployment store is not initialized",
		}
	}

	userID = strings.TrimSpace(userID)
	displayName = strings.TrimSpace(displayName)

	if userID == "" {
		return &StoreError{
			Type:    ErrInvalidInput,
			Message: "userID is required",
		}
	}
	if displayName == "" {
		return &StoreError{
			Type:    ErrInvalidInput,
			Message: "displayName is required",
		}
	}

	result, err := s.pool.Exec(ctx, `
UPDATE users SET display_name = $1, updated_at = now()
WHERE id = $2
`, displayName, userID)
	if err != nil {
		return &StoreError{
			Type:    ErrDatabase,
			Message: "failed to update user",
			Err:     err,
		}
	}
	if result.RowsAffected() == 0 {
		return &StoreError{
			Type:    ErrNotFound,
			Message: "user not found",
		}
	}

	return nil
}

func (s *DeploymentStore) DeleteUser(ctx context.Context, userID string) error {
	if s == nil || s.pool == nil {
		return &StoreError{
			Type:    ErrDatabase,
			Message: "deployment store is not initialized",
		}
	}

	userID = strings.TrimSpace(userID)
	if userID == "" {
		return &StoreError{
			Type:    ErrInvalidInput,
			Message: "userID is required",
		}
	}

	result, err := s.pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID)
	if err != nil {
		return &StoreError{
			Type:    ErrDatabase,
			Message: "failed to delete user",
			Err:     err,
		}
	}
	if result.RowsAffected() == 0 {
		return &StoreError{
			Type:    ErrNotFound,
			Message: "user not found",
		}
	}

	return nil
}

// ============================================================================
// RESOURCE ACCOUNTING
// ============================================================================

func (s *DeploymentStore) EnsureResourceAccounting(ctx context.Context, userID string) error {
	if s == nil || s.pool == nil {
		return &StoreError{
			Type:    ErrDatabase,
			Message: "deployment store is not initialized",
		}
	}

	// 1. Ensure user_resource_accounting table entry
	_, err := s.pool.Exec(ctx, `
INSERT INTO user_resource_accounting (user_id, deployment_limit, api_key_limit, storage_limit)
VALUES ($1, 10, 50, 1073741824) -- 10 deployments, 50 API keys, 1GB storage
ON CONFLICT (user_id) DO NOTHING
`, userID)
	if err != nil {
		return &StoreError{
			Type:    ErrDatabase,
			Message: "failed to ensure resource accounting",
			Err:     err,
		}
	}

	// 2. Ensure quotas table entry
	_, err = s.pool.Exec(ctx, `
INSERT INTO quotas (user_id)
VALUES ($1)
ON CONFLICT (user_id) DO NOTHING
`, userID)
	if err != nil {
		return &StoreError{
			Type:    ErrDatabase,
			Message: "failed to ensure default user quota",
			Err:     err,
		}
	}

	// 3. Ensure resource_usage table entry
	_, err = s.pool.Exec(ctx, `
INSERT INTO resource_usage (user_id)
VALUES ($1)
ON CONFLICT (user_id) DO NOTHING
`, userID)
	if err != nil {
		return &StoreError{
			Type:    ErrDatabase,
			Message: "failed to ensure default resource usage tracking",
			Err:     err,
		}
	}

	return nil
}


func (s *DeploymentStore) GetUserResourceLimits(ctx context.Context, userID string) (map[string]interface{}, error) {
	var deploymentLimit, apiKeyLimit, storageLimit int64
	err := s.pool.QueryRow(ctx, `
SELECT deployment_limit, api_key_limit, storage_limit
FROM user_resource_accounting
WHERE user_id = $1
`, userID).Scan(&deploymentLimit, &apiKeyLimit, &storageLimit)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, &StoreError{
				Type:    ErrNotFound,
				Message: "resource accounting not found",
			}
		}
		return nil, &StoreError{
			Type:    ErrDatabase,
			Message: "failed to get resource limits",
			Err:     err,
		}
	}

	return map[string]interface{}{
		"deployment_limit": deploymentLimit,
		"api_key_limit":    apiKeyLimit,
		"storage_limit":    storageLimit,
	}, nil
}

// ============================================================================
// REFRESH TOKEN OPERATIONS
// ============================================================================

func (s *DeploymentStore) StoreRefreshToken(ctx context.Context, userID string, tokenHash string, expiresAt time.Time) error {
	if s == nil || s.pool == nil {
		return &StoreError{
			Type:    ErrDatabase,
			Message: "deployment store is not initialized",
		}
	}

	userID = strings.TrimSpace(userID)
	tokenHash = strings.TrimSpace(tokenHash)
	if userID == "" || tokenHash == "" {
		return &StoreError{
			Type:    ErrInvalidInput,
			Message: "userID and tokenHash are required",
		}
	}

	_, err := s.pool.Exec(ctx, `
INSERT INTO refresh_tokens (user_id, token_hash, expires_at)
VALUES ($1, $2, $3)
`, userID, tokenHash, expiresAt)
	if err != nil {
		return &StoreError{
			Type:    ErrDatabase,
			Message: "failed to store refresh token",
			Err:     err,
		}
	}

	return nil
}

// ConsumeRefreshToken atomically marks the refresh token identified by rawToken
// as revoked/used, returning the associated user ID. It does NOT issue or store
// a replacement token — callers that need to rotate the token must separately
// call StoreRefreshToken with a freshly generated hash after this succeeds.
func (s *DeploymentStore) ConsumeRefreshToken(ctx context.Context, rawToken string) (string, error) {
	if s == nil || s.pool == nil {
		return "", &StoreError{
			Type:    ErrDatabase,
			Message: "deployment store is not initialized",
		}
	}

	rawToken = strings.TrimSpace(rawToken)
	if rawToken == "" {
		return "", &StoreError{
			Type:    ErrInvalidInput,
			Message: "rawToken is required",
		}
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return "", &StoreError{
			Type:    ErrDatabase,
			Message: "failed to begin transaction",
			Err:     err,
		}
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
		if errors.Is(err, pgx.ErrNoRows) {
			return "", &StoreError{
				Type:    ErrNotFound,
				Message: "refresh token not found",
			}
		}
		return "", &StoreError{
			Type:    ErrDatabase,
			Message: "failed to consume refresh token",
			Err:     err,
		}
	}

	if revokedAt != nil || time.Now().After(expiresAt) {
		return "", &StoreError{
			Type:    ErrUnauthorized,
			Message: "refresh token is no longer valid",
		}
	}

	if _, err := tx.Exec(ctx, `
UPDATE refresh_tokens
SET revoked_at = now(), last_used_at = now()
WHERE token_hash = $1
`, oldHash); err != nil {
		return "", &StoreError{
			Type:    ErrDatabase,
			Message: "failed to revoke refresh token",
			Err:     err,
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return "", &StoreError{
			Type:    ErrDatabase,
			Message: "failed to commit transaction",
			Err:     err,
		}
	}

	return userID, nil
}

func (s *DeploymentStore) RevokeAllUserTokens(ctx context.Context, userID string) error {
	if s == nil || s.pool == nil {
		return &StoreError{
			Type:    ErrDatabase,
			Message: "deployment store is not initialized",
		}
	}

	_, err := s.pool.Exec(ctx, `
UPDATE refresh_tokens
SET revoked_at = now()
WHERE user_id = $1 AND revoked_at IS NULL
`, userID)
	if err != nil {
		return &StoreError{
			Type:    ErrDatabase,
			Message: "failed to revoke tokens",
			Err:     err,
		}
	}

	return nil
}

func (s *DeploymentStore) CleanupExpiredTokens(ctx context.Context, maxAge time.Duration) error {
	if s == nil || s.pool == nil {
		return &StoreError{
			Type:    ErrDatabase,
			Message: "deployment store is not initialized",
		}
	}

	_, err := s.pool.Exec(ctx, `
DELETE FROM refresh_tokens 
WHERE expires_at < now() - $1 
   OR (revoked_at IS NOT NULL AND revoked_at < now() - $1)
`, maxAge)
	if err != nil {
		return &StoreError{
			Type:    ErrDatabase,
			Message: "failed to cleanup expired tokens",
			Err:     err,
		}
	}

	return nil
}

// ============================================================================
// API KEY OPERATIONS
// ============================================================================

func (s *DeploymentStore) StoreAPIKey(ctx context.Context, userID string, name string, keyPrefix string, keyHash string, expiresAt *time.Time) error {
	if s == nil || s.pool == nil {
		return &StoreError{
			Type:    ErrDatabase,
			Message: "deployment store is not initialized",
		}
	}

	userID = strings.TrimSpace(userID)
	name = strings.TrimSpace(name)
	keyPrefix = strings.TrimSpace(keyPrefix)
	keyHash = strings.TrimSpace(keyHash)

	if err := s.validateAPIKeyInput(userID, name, keyPrefix, keyHash); err != nil {
		return err
	}

	// Check key limit
	var keyCount int64
	if err := s.pool.QueryRow(ctx, `
SELECT COUNT(*) FROM api_keys WHERE user_id = $1 AND revoked_at IS NULL
`, userID).Scan(&keyCount); err != nil {
		return &StoreError{
			Type:    ErrDatabase,
			Message: "failed to count API keys",
			Err:     err,
		}
	}

	limits, err := s.GetUserResourceLimits(ctx, userID)
	if err == nil {
		if limit, ok := limits["api_key_limit"].(int64); ok && keyCount >= limit {
			return &StoreError{
				Type:    ErrQuotaExceeded,
				Message: fmt.Sprintf("API key limit exceeded (%d)", limit),
			}
		}
	}

	_, err = s.pool.Exec(ctx, `
INSERT INTO api_keys (user_id, name, key_prefix, key_hash, expires_at)
VALUES ($1, $2, $3, $4, $5)
`, userID, name, keyPrefix, keyHash, expiresAt)
	if err != nil {
		return &StoreError{
			Type:    ErrDatabase,
			Message: "failed to store API key",
			Err:     err,
		}
	}

	return nil
}

func (s *DeploymentStore) GetUserByAPIKey(ctx context.Context, rawKey string) (UserRecord, error) {
	if s == nil || s.pool == nil {
		return UserRecord{}, &StoreError{
			Type:    ErrDatabase,
			Message: "deployment store is not initialized",
		}
	}

	rawKey = strings.TrimSpace(rawKey)
	if rawKey == "" {
		return UserRecord{}, &StoreError{
			Type:    ErrInvalidInput,
			Message: "API key is required",
		}
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
		if errors.Is(err, pgx.ErrNoRows) {
			return UserRecord{}, &StoreError{
				Type:    ErrNotFound,
				Message: "API key not found or expired",
			}
		}
		return UserRecord{}, &StoreError{
			Type:    ErrDatabase,
			Message: "failed to validate API key",
			Err:     err,
		}
	}

	return s.GetUserByID(ctx, userID)
}

func (s *DeploymentStore) RevokeAPIKey(ctx context.Context, userID string, keyID string) error {
	if s == nil || s.pool == nil {
		return &StoreError{
			Type:    ErrDatabase,
			Message: "deployment store is not initialized",
		}
	}

	result, err := s.pool.Exec(ctx, `
UPDATE api_keys
SET revoked_at = now()
WHERE id = $1 AND user_id = $2
`, keyID, userID)
	if err != nil {
		return &StoreError{
			Type:    ErrDatabase,
			Message: "failed to revoke API key",
			Err:     err,
		}
	}
	if result.RowsAffected() == 0 {
		return &StoreError{
			Type:    ErrNotFound,
			Message: "API key not found",
		}
	}

	return nil
}

func (s *DeploymentStore) ListAPIKeys(ctx context.Context, userID string) ([]map[string]interface{}, error) {
	if s == nil || s.pool == nil {
		return nil, &StoreError{
			Type:    ErrDatabase,
			Message: "deployment store is not initialized",
		}
	}

	rows, err := s.pool.Query(ctx, `
SELECT id::text, name, key_prefix, created_at, expires_at, revoked_at, last_used_at
FROM api_keys
WHERE user_id = $1
ORDER BY created_at DESC
`, userID)
	if err != nil {
		return nil, &StoreError{
			Type:    ErrDatabase,
			Message: "failed to list API keys",
			Err:     err,
		}
	}
	defer rows.Close()

	var keys []map[string]interface{}
	for rows.Next() {
		var id, name, keyPrefix string
		var createdAt, expiresAt, revokedAt, lastUsedAt *time.Time
		if err := rows.Scan(&id, &name, &keyPrefix, &createdAt, &expiresAt, &revokedAt, &lastUsedAt); err != nil {
			return nil, &StoreError{
				Type:    ErrDatabase,
				Message: "failed to scan API key",
				Err:     err,
			}
		}
		keys = append(keys, map[string]interface{}{
			"id":           id,
			"name":         name,
			"key_prefix":   keyPrefix,
			"created_at":   createdAt,
			"expires_at":   expiresAt,
			"revoked_at":   revokedAt,
			"last_used_at": lastUsedAt,
		})
	}

	return keys, nil
}

// ============================================================================
// DEPLOYMENT OPERATIONS
// ============================================================================

func (s *DeploymentStore) CreateDeploymentAttemptForUser(ctx context.Context, ownerUserID string, appName string, repoURL string, appPath string, portMap string, imageName string) (string, error) {
	if s == nil || s.pool == nil {
		return "", &StoreError{
			Type:    ErrDatabase,
			Message: "deployment store is not initialized",
		}
	}

	ownerUserID = strings.TrimSpace(ownerUserID)
	appName = strings.TrimSpace(appName)
	repoURL = strings.TrimSpace(repoURL)
	appPath = strings.TrimSpace(appPath)
	portMap = strings.TrimSpace(portMap)
	imageName = strings.TrimSpace(imageName)

	if err := s.validateDeploymentInput(ownerUserID, appName, repoURL, appPath, portMap); err != nil {
		return "", err
	}

	// Check deployment limit
	var deploymentCount int64
	if err := s.pool.QueryRow(ctx, `
SELECT COUNT(*) FROM deployments WHERE owner_user_id = $1 AND deleted_at IS NULL
`, ownerUserID).Scan(&deploymentCount); err != nil {
		return "", &StoreError{
			Type:    ErrDatabase,
			Message: "failed to count deployments",
			Err:     err,
		}
	}

	limits, err := s.GetUserResourceLimits(ctx, ownerUserID)
	if err == nil {
		if limit, ok := limits["deployment_limit"].(int64); ok && deploymentCount >= limit {
			return "", &StoreError{
				Type:    ErrQuotaExceeded,
				Message: fmt.Sprintf("deployment limit exceeded (%d)", limit),
			}
		}
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return "", &StoreError{
			Type:    ErrDatabase,
			Message: "failed to begin transaction",
			Err:     err,
		}
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
		return "", &StoreError{
			Type:    ErrDatabase,
			Message: "failed to create deployment",
			Err:     err,
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return "", &StoreError{
			Type:    ErrDatabase,
			Message: "failed to commit transaction",
			Err:     err,
		}
	}

	return deploymentID, nil
}

func (s *DeploymentStore) CreateDeploymentAttempt(ctx context.Context, appName string, repoURL string, imageName string) (string, error) {
	if s == nil || s.pool == nil {
		return "", &StoreError{
			Type:    ErrDatabase,
			Message: "deployment store is not initialized",
		}
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return "", &StoreError{
			Type:    ErrDatabase,
			Message: "failed to begin transaction",
			Err:     err,
		}
	}
	defer func() { _ = tx.Rollback(ctx) }()

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
		return "", &StoreError{
			Type:    ErrDatabase,
			Message: "failed to create deployment",
			Err:     err,
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return "", &StoreError{
			Type:    ErrDatabase,
			Message: "failed to commit transaction",
			Err:     err,
		}
	}

	return deploymentID, nil
}

func (s *DeploymentStore) UpdateDeploymentStatus(ctx context.Context, deploymentID string, status DeploymentStatus, statusMessage string, containerID string, containerName string, imageName string, changedBy string) error {
	if s == nil || s.pool == nil {
		return &StoreError{
			Type:    ErrDatabase,
			Message: "deployment store is not initialized",
		}
	}

	deploymentID = strings.TrimSpace(deploymentID)
	if deploymentID == "" {
		return &StoreError{
			Type:    ErrInvalidInput,
			Message: "deploymentID is required",
		}
	}

	s.statusMu.Lock()
	defer s.statusMu.Unlock()

	// Get current status for audit
	var oldStatus string
	if err := s.pool.QueryRow(ctx, `SELECT status FROM deployments WHERE id = $1`, deploymentID).Scan(&oldStatus); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return &StoreError{
				Type:    ErrNotFound,
				Message: "deployment not found",
			}
		}
		return &StoreError{
			Type:    ErrDatabase,
			Message: "failed to get current status",
			Err:     err,
		}
	}

	result, err := s.pool.Exec(ctx, `
UPDATE deployments
SET status = $2,
	status_message = $3,
	container_id = CASE WHEN $4 <> '' THEN $4 ELSE container_id END,
	container_name = CASE WHEN $5 <> '' THEN $5 ELSE container_name END,
	image_name = CASE WHEN $6 <> '' THEN $6 ELSE image_name END,
	started_at = COALESCE(started_at, now()),
	finished_at = CASE
		WHEN $2 IN ('DEPLOYED', 'FAILED', 'RUNNING') THEN COALESCE(finished_at, now())
		ELSE finished_at
	END,
	updated_at = now()
WHERE id = $1
`, deploymentID, string(status), statusMessage, containerID, containerName, imageName)
	if err != nil {
		return &StoreError{
			Type:    ErrDatabase,
			Message: "failed to update deployment status",
			Err:     err,
		}
	}
	if result.RowsAffected() == 0 {
		return &StoreError{
			Type:    ErrNotFound,
			Message: "deployment not found",
		}
	}

	// Track change if status changed
	if oldStatus != string(status) {
		if err := s.TrackDeploymentChange(ctx, deploymentID, "status", oldStatus, string(status), changedBy); err != nil {
			log.Printf("Warning: failed to track status change: %v", err)
		}
	}

	return nil
}

func (s *DeploymentStore) UpdateDeploymentAppPath(ctx context.Context, deploymentID string, appPath string) error {
	if s == nil || s.pool == nil {
		return &StoreError{
			Type:    ErrDatabase,
			Message: "deployment store is not initialized",
		}
	}

	deploymentID = strings.TrimSpace(deploymentID)
	appPath = strings.TrimSpace(appPath)
	if deploymentID == "" || appPath == "" {
		return &StoreError{
			Type:    ErrInvalidInput,
			Message: "deploymentID and appPath are required",
		}
	}

	result, err := s.pool.Exec(ctx, `
UPDATE deployments
SET app_path = $2, updated_at = now()
WHERE id = $1
`, deploymentID, appPath)
	if err != nil {
		return &StoreError{
			Type:    ErrDatabase,
			Message: "failed to update app path",
			Err:     err,
		}
	}
	if result.RowsAffected() == 0 {
		return &StoreError{
			Type:    ErrNotFound,
			Message: "deployment not found",
		}
	}

	return nil
}

func (s *DeploymentStore) MarkDeploymentFailed(ctx context.Context, deploymentID string, cause error, changedBy string) error {
	if cause == nil {
		cause = fmt.Errorf("deployment failed")
	}
	return s.UpdateDeploymentStatus(ctx, deploymentID, DeploymentStatusFailed, cause.Error(), "", "", "", changedBy)
}

func (s *DeploymentStore) MarkDeploymentDeployed(ctx context.Context, deploymentID string, containerID string, containerName string, imageName string, changedBy string) error {
	return s.UpdateDeploymentStatus(ctx, deploymentID, DeploymentStatusRunning, "deployment completed successfully", containerID, containerName, imageName, changedBy)
}

func (s *DeploymentStore) GetDeploymentForUser(ctx context.Context, userID string, deploymentID string) (DeploymentRecord, error) {
	if s == nil || s.pool == nil {
		return DeploymentRecord{}, &StoreError{
			Type:    ErrDatabase,
			Message: "deployment store is not initialized",
		}
	}

	userID = strings.TrimSpace(userID)
	deploymentID = strings.TrimSpace(deploymentID)
	if userID == "" || deploymentID == "" {
		return DeploymentRecord{}, &StoreError{
			Type:    ErrInvalidInput,
			Message: "userID and deploymentID are required",
		}
	}

	var deployment DeploymentRecord
	var startedAt, finishedAt *time.Time
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
	d.started_at,
	d.finished_at,
	d.created_at,
	d.updated_at
FROM deployments d
WHERE d.owner_user_id = $1 AND d.id = $2 AND d.deleted_at IS NULL
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
		&startedAt,
		&finishedAt,
		&deployment.CreatedAt,
		&deployment.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return DeploymentRecord{}, &StoreError{
				Type:    ErrNotFound,
				Message: "deployment not found",
			}
		}
		return DeploymentRecord{}, &StoreError{
			Type:    ErrDatabase,
			Message: "failed to get deployment",
			Err:     err,
		}
	}
	deployment.StartedAt = startedAt
	deployment.FinishedAt = finishedAt

	return deployment, nil
}

func (s *DeploymentStore) ListDeploymentsForUser(ctx context.Context, userID string) ([]DeploymentRecord, error) {
	result, err := s.ListDeploymentsForUserWithPagination(ctx, userID, Pagination{
		Limit:  100,
		Offset: 0,
		Order:  "created_at DESC",
	})
	if err != nil {
		return nil, err
	}
	return result.Items, nil
}

func (s *DeploymentStore) ListDeploymentsForUserWithPagination(ctx context.Context, userID string, pagination Pagination) (PaginatedResult, error) {
	if s == nil || s.pool == nil {
		return PaginatedResult{}, &StoreError{
			Type:    ErrDatabase,
			Message: "deployment store is not initialized",
		}
	}

	userID = strings.TrimSpace(userID)
	if userID == "" {
		return PaginatedResult{}, &StoreError{
			Type:    ErrInvalidInput,
			Message: "userID is required",
		}
	}

	if pagination.Limit == 0 || pagination.Limit > 100 {
		pagination.Limit = 20
	}
	if pagination.Order == "" {
		pagination.Order = "created_at DESC"
	}

	// Get total count
	var totalCount int
	if err := s.pool.QueryRow(ctx, `
SELECT COUNT(*) FROM deployments WHERE owner_user_id = $1 AND deleted_at IS NULL
`, userID).Scan(&totalCount); err != nil {
		return PaginatedResult{}, &StoreError{
			Type:    ErrDatabase,
			Message: "failed to count deployments",
			Err:     err,
		}
	}

	// Get paginated items
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
	d.started_at,
	d.finished_at,
	d.created_at,
	d.updated_at
FROM deployments d
WHERE d.owner_user_id = $1 AND d.deleted_at IS NULL
ORDER BY `+pagination.Order+`
LIMIT $2 OFFSET $3
`, userID, pagination.Limit, pagination.Offset)
	if err != nil {
		return PaginatedResult{}, &StoreError{
			Type:    ErrDatabase,
			Message: "failed to list deployments",
			Err:     err,
		}
	}
	defer rows.Close()

	deployments := make([]DeploymentRecord, 0)
	for rows.Next() {
		var deployment DeploymentRecord
		var startedAt, finishedAt *time.Time
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
			&startedAt,
			&finishedAt,
			&deployment.CreatedAt,
			&deployment.UpdatedAt,
		); err != nil {
			return PaginatedResult{}, &StoreError{
				Type:    ErrDatabase,
				Message: "failed to scan deployment",
				Err:     err,
			}
		}
		deployment.StartedAt = startedAt
		deployment.FinishedAt = finishedAt
		deployments = append(deployments, deployment)
	}

	totalPages := (totalCount + pagination.Limit - 1) / pagination.Limit
	page := pagination.Offset/pagination.Limit + 1

	return PaginatedResult{
		Items:      deployments,
		TotalCount: totalCount,
		Page:       page,
		PerPage:    pagination.Limit,
		TotalPages: totalPages,
	}, nil
}

func (s *DeploymentStore) GetDeploymentStatus(ctx context.Context, deploymentID string) (string, error) {
	var status string
	err := s.pool.QueryRow(ctx, `
SELECT status::text FROM deployments WHERE id = $1 AND deleted_at IS NULL
`, deploymentID).Scan(&status)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", &StoreError{
				Type:    ErrNotFound,
				Message: "deployment not found",
			}
		}
		return "", &StoreError{
			Type:    ErrDatabase,
			Message: "failed to get deployment status",
			Err:     err,
		}
	}
	return status, nil
}

func (s *DeploymentStore) GetActiveDeploymentsForUser(ctx context.Context, userID string) ([]DeploymentRecord, error) {
	if s == nil || s.pool == nil {
		return nil, &StoreError{
			Type:    ErrDatabase,
			Message: "deployment store is not initialized",
		}
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
	d.started_at,
	d.finished_at,
	d.created_at,
	d.updated_at
FROM deployments d
WHERE d.owner_user_id = $1 AND d.status IN ('RUNNING', 'DEPLOYED') AND d.deleted_at IS NULL
`, userID)
	if err != nil {
		return nil, &StoreError{
			Type:    ErrDatabase,
			Message: "failed to list active deployments",
			Err:     err,
		}
	}
	defer rows.Close()

	var deployments []DeploymentRecord
	for rows.Next() {
		var deployment DeploymentRecord
		var startedAt, finishedAt *time.Time
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
			&startedAt,
			&finishedAt,
			&deployment.CreatedAt,
			&deployment.UpdatedAt,
		); err != nil {
			return nil, &StoreError{
				Type:    ErrDatabase,
				Message: "failed to scan deployment",
				Err:     err,
			}
		}
		deployment.StartedAt = startedAt
		deployment.FinishedAt = finishedAt
		deployments = append(deployments, deployment)
	}

	return deployments, nil
}

// ============================================================================
// HEALTH CHECK OPERATIONS
// ============================================================================

func (s *DeploymentStore) UpsertDeploymentHealthConfig(ctx context.Context, userID string, deploymentID string, healthCheckPath string, intervalSeconds int, maxRestarts int) error {
	if s == nil || s.pool == nil {
		return &StoreError{
			Type:    ErrDatabase,
			Message: "deployment store is not initialized",
		}
	}

	userID = strings.TrimSpace(userID)
	deploymentID = strings.TrimSpace(deploymentID)
	healthCheckPath = strings.TrimSpace(healthCheckPath)

	if userID == "" || deploymentID == "" {
		return &StoreError{
			Type:    ErrInvalidInput,
			Message: "userID and deploymentID are required",
		}
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
		return &StoreError{
			Type:    ErrDatabase,
			Message: "failed to upsert health config",
			Err:     err,
		}
	}

	return nil
}

func (s *DeploymentStore) GetDeploymentHealthConfigForUser(ctx context.Context, userID string, deploymentID string) (DeploymentHealthConfigRecord, error) {
	if s == nil || s.pool == nil {
		return DeploymentHealthConfigRecord{}, &StoreError{
			Type:    ErrDatabase,
			Message: "deployment store is not initialized",
		}
	}

	userID = strings.TrimSpace(userID)
	deploymentID = strings.TrimSpace(deploymentID)
	if userID == "" || deploymentID == "" {
		return DeploymentHealthConfigRecord{}, &StoreError{
			Type:    ErrInvalidInput,
			Message: "userID and deploymentID are required",
		}
	}

	if _, err := s.GetDeploymentForUser(ctx, userID, deploymentID); err != nil {
		return DeploymentHealthConfigRecord{}, err
	}

	config, err := s.getDeploymentHealthConfig(ctx, deploymentID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
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
		return DeploymentHealthConfigRecord{}, &StoreError{
			Type:    ErrDatabase,
			Message: "failed to get health config",
			Err:     err,
		}
	}

	return config, nil
}

func (s *DeploymentStore) ListRunningDeploymentsForHealthCheck(ctx context.Context) ([]RunningDeploymentHealthTarget, error) {
	if s == nil || s.pool == nil {
		return nil, &StoreError{
			Type:    ErrDatabase,
			Message: "deployment store is not initialized",
		}
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
	d.started_at,
	d.finished_at,
	d.created_at,
	d.updated_at,
	COALESCE(cfg.health_check_path, '/health'),
	COALESCE(cfg.health_check_interval_seconds, 30),
	COALESCE(cfg.max_restarts_before_failing, 3),
	cfg.last_checked_at
FROM deployments d
LEFT JOIN deployment_health_configs cfg ON cfg.deployment_id = d.id
WHERE d.status = 'RUNNING' AND d.deleted_at IS NULL
ORDER BY d.updated_at ASC
`)
	if err != nil {
		return nil, &StoreError{
			Type:    ErrDatabase,
			Message: "failed to list running deployments",
			Err:     err,
		}
	}
	defer rows.Close()

	targets := make([]RunningDeploymentHealthTarget, 0)
	for rows.Next() {
		var target RunningDeploymentHealthTarget
		var startedAt, finishedAt *time.Time
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
			&startedAt,
			&finishedAt,
			&target.CreatedAt,
			&target.UpdatedAt,
			&target.HealthCheckPath,
			&target.HealthCheckIntervalSeconds,
			&target.MaxRestartsBeforeFailing,
			&target.LastCheckedAt,
		); err != nil {
			return nil, &StoreError{
				Type:    ErrDatabase,
				Message: "failed to scan running deployment",
				Err:     err,
			}
		}
		target.StartedAt = startedAt
		target.FinishedAt = finishedAt
		targets = append(targets, target)
	}
	if err := rows.Err(); err != nil {
		return nil, &StoreError{
			Type:    ErrDatabase,
			Message: "failed to iterate running deployments",
			Err:     err,
		}
	}

	return targets, nil
}

func (s *DeploymentStore) MarkDeploymentHealthChecked(ctx context.Context, deploymentID string) error {
	if s == nil || s.pool == nil {
		return &StoreError{
			Type:    ErrDatabase,
			Message: "deployment store is not initialized",
		}
	}

	deploymentID = strings.TrimSpace(deploymentID)
	if deploymentID == "" {
		return &StoreError{
			Type:    ErrInvalidInput,
			Message: "deploymentID is required",
		}
	}

	_, err := s.pool.Exec(ctx, `
UPDATE deployment_health_configs
SET last_checked_at = now(), updated_at = now()
WHERE deployment_id = $1
`, deploymentID)
	if err != nil {
		return &StoreError{
			Type:    ErrDatabase,
			Message: "failed to mark health checked",
			Err:     err,
		}
	}

	return nil
}

// ============================================================================
// RESTART AUDIT OPERATIONS
// ============================================================================

func (s *DeploymentStore) RecordDeploymentRestartAudit(ctx context.Context, deploymentID string, action string, outcome string, reason string, previousContainerID string, newContainerID string) error {
	if s == nil || s.pool == nil {
		return &StoreError{
			Type:    ErrDatabase,
			Message: "deployment store is not initialized",
		}
	}

	deploymentID = strings.TrimSpace(deploymentID)
	action = strings.TrimSpace(action)
	outcome = strings.TrimSpace(outcome)
	reason = strings.TrimSpace(reason)
	previousContainerID = strings.TrimSpace(previousContainerID)
	newContainerID = strings.TrimSpace(newContainerID)

	if deploymentID == "" || action == "" || outcome == "" || reason == "" {
		return &StoreError{
			Type:    ErrInvalidInput,
			Message: "deploymentID, action, outcome, and reason are required",
		}
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
		return &StoreError{
			Type:    ErrDatabase,
			Message: "failed to record restart audit",
			Err:     err,
		}
	}

	return nil
}

func (s *DeploymentStore) CountRecentDeploymentRestarts(ctx context.Context, deploymentID string, window time.Duration) (int, error) {
	if s == nil || s.pool == nil {
		return 0, &StoreError{
			Type:    ErrDatabase,
			Message: "deployment store is not initialized",
		}
	}

	deploymentID = strings.TrimSpace(deploymentID)
	if deploymentID == "" {
		return 0, &StoreError{
			Type:    ErrInvalidInput,
			Message: "deploymentID is required",
		}
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
		return 0, &StoreError{
			Type:    ErrDatabase,
			Message: "failed to count restarts",
			Err:     err,
		}
	}

	return count, nil
}

// ============================================================================
// ENVIRONMENT VARIABLES
// ============================================================================

func (s *DeploymentStore) LoadDeploymentEnvMapByDeploymentID(ctx context.Context, deploymentID string) (map[string]string, error) {
	if s == nil || s.pool == nil {
		return nil, &StoreError{
			Type:    ErrDatabase,
			Message: "deployment store is not initialized",
		}
	}

	deploymentID = strings.TrimSpace(deploymentID)
	if deploymentID == "" {
		return nil, &StoreError{
			Type:    ErrInvalidInput,
			Message: "deploymentID is required",
		}
	}

	rows, err := s.pool.Query(ctx, `
SELECT env_key, encrypted_value, nonce
FROM deployment_env_vars
WHERE deployment_id = $1
ORDER BY env_key ASC
`, deploymentID)
	if err != nil {
		return nil, &StoreError{
			Type:    ErrDatabase,
			Message: "failed to load env vars",
			Err:     err,
		}
	}
	defer rows.Close()

	envMap := make(map[string]string)
	for rows.Next() {
		var key string
		var encryptedValue []byte
		var nonce []byte
		if err := rows.Scan(&key, &encryptedValue, &nonce); err != nil {
			return nil, &StoreError{
				Type:    ErrDatabase,
				Message: "failed to scan env var",
				Err:     err,
			}
		}
		value, err := decryptEnvValue(encryptedValue, nonce)
		if err != nil {
			return nil, err
		}
		envMap[key] = value
	}
	if err := rows.Err(); err != nil {
		return nil, &StoreError{
			Type:    ErrDatabase,
			Message: "failed to iterate env vars",
			Err:     err,
		}
	}

	return envMap, nil
}

func (s *DeploymentStore) StoreDeploymentEnvVar(ctx context.Context, deploymentID string, key string, value string) error {
	if s == nil || s.pool == nil {
		return &StoreError{
			Type:    ErrDatabase,
			Message: "deployment store is not initialized",
		}
	}

	deploymentID = strings.TrimSpace(deploymentID)
	key = strings.TrimSpace(key)
	value = strings.TrimSpace(value)

	if deploymentID == "" || key == "" {
		return &StoreError{
			Type:    ErrInvalidInput,
			Message: "deploymentID and key are required",
		}
	}

	// Encrypt the value
	encrypted, nonce, err := encryptEnvValue(value)
	if err != nil {
		return &StoreError{
			Type:    ErrDatabase,
			Message: "failed to encrypt env var",
			Err:     err,
		}
	}

	_, err = s.pool.Exec(ctx, `
INSERT INTO deployment_env_vars (deployment_id, env_key, encrypted_value, nonce)
VALUES ($1, $2, $3, $4)
ON CONFLICT (deployment_id, env_key)
DO UPDATE SET encrypted_value = EXCLUDED.encrypted_value, nonce = EXCLUDED.nonce, updated_at = now()
`, deploymentID, key, encrypted, nonce)
	if err != nil {
		return &StoreError{
			Type:    ErrDatabase,
			Message: "failed to store env var",
			Err:     err,
		}
	}

	return nil
}

func (s *DeploymentStore) DeleteDeploymentEnvVar(ctx context.Context, deploymentID string, key string) error {
	if s == nil || s.pool == nil {
		return &StoreError{
			Type:    ErrDatabase,
			Message: "deployment store is not initialized",
		}
	}

	result, err := s.pool.Exec(ctx, `
DELETE FROM deployment_env_vars WHERE deployment_id = $1 AND env_key = $2
`, deploymentID, key)
	if err != nil {
		return &StoreError{
			Type:    ErrDatabase,
			Message: "failed to delete env var",
			Err:     err,
		}
	}
	if result.RowsAffected() == 0 {
		return &StoreError{
			Type:    ErrNotFound,
			Message: "env var not found",
		}
	}

	return nil
}

// ============================================================================
// CHANGE TRACKING
// ============================================================================

func (s *DeploymentStore) TrackDeploymentChange(ctx context.Context, deploymentID string, field string, oldValue string, newValue string, changedBy string) error {
	if s == nil || s.pool == nil {
		return &StoreError{
			Type:    ErrDatabase,
			Message: "deployment store is not initialized",
		}
	}

	_, err := s.pool.Exec(ctx, `
INSERT INTO deployment_changes (deployment_id, field, old_value, new_value, changed_by)
VALUES ($1, $2, $3, $4, $5)
`, deploymentID, field, oldValue, newValue, changedBy)
	if err != nil {
		return &StoreError{
			Type:    ErrDatabase,
			Message: "failed to track change",
			Err:     err,
		}
	}

	return nil
}

func (s *DeploymentStore) GetDeploymentChanges(ctx context.Context, deploymentID string, limit int) ([]DeploymentChange, error) {
	if s == nil || s.pool == nil {
		return nil, &StoreError{
			Type:    ErrDatabase,
			Message: "deployment store is not initialized",
		}
	}

	if limit == 0 || limit > 100 {
		limit = 50
	}

	rows, err := s.pool.Query(ctx, `
SELECT id::text, deployment_id::text, field, old_value, new_value, changed_by::text, created_at
FROM deployment_changes
WHERE deployment_id = $1
ORDER BY created_at DESC
LIMIT $2
`, deploymentID, limit)
	if err != nil {
		return nil, &StoreError{
			Type:    ErrDatabase,
			Message: "failed to get deployment changes",
			Err:     err,
		}
	}
	defer rows.Close()

	var changes []DeploymentChange
	for rows.Next() {
		var change DeploymentChange
		if err := rows.Scan(
			&change.ID,
			&change.DeploymentID,
			&change.Field,
			&change.OldValue,
			&change.NewValue,
			&change.ChangedBy,
			&change.CreatedAt,
		); err != nil {
			return nil, &StoreError{
				Type:    ErrDatabase,
				Message: "failed to scan change",
				Err:     err,
			}
		}
		changes = append(changes, change)
	}

	return changes, nil
}

// ============================================================================
// SOFT DELETE OPERATIONS
// ============================================================================

func (s *DeploymentStore) SoftDeleteDeployment(ctx context.Context, deploymentID string, reason string) error {
	if s == nil || s.pool == nil {
		return &StoreError{
			Type:    ErrDatabase,
			Message: "deployment store is not initialized",
		}
	}

	deploymentID = strings.TrimSpace(deploymentID)
	reason = strings.TrimSpace(reason)
	if deploymentID == "" || reason == "" {
		return &StoreError{
			Type:    ErrInvalidInput,
			Message: "deploymentID and reason are required",
		}
	}

	result, err := s.pool.Exec(ctx, `
UPDATE deployments
SET deleted_at = now(), deleted_reason = $1, updated_at = now()
WHERE id = $2 AND deleted_at IS NULL
`, reason, deploymentID)
	if err != nil {
		return &StoreError{
			Type:    ErrDatabase,
			Message: "failed to soft delete deployment",
			Err:     err,
		}
	}
	if result.RowsAffected() == 0 {
		return &StoreError{
			Type:    ErrNotFound,
			Message: "deployment not found or already deleted",
		}
	}

	return nil
}

func (s *DeploymentStore) RestoreDeployment(ctx context.Context, deploymentID string) error {
	if s == nil || s.pool == nil {
		return &StoreError{
			Type:    ErrDatabase,
			Message: "deployment store is not initialized",
		}
	}

	result, err := s.pool.Exec(ctx, `
UPDATE deployments
SET deleted_at = NULL, deleted_reason = NULL, updated_at = now()
WHERE id = $1 AND deleted_at IS NOT NULL
`, deploymentID)
	if err != nil {
		return &StoreError{
			Type:    ErrDatabase,
			Message: "failed to restore deployment",
			Err:     err,
		}
	}
	if result.RowsAffected() == 0 {
		return &StoreError{
			Type:    ErrNotFound,
			Message: "deployment not found or already restored",
		}
	}

	return nil
}

// ============================================================================
// POOL STATISTICS
// ============================================================================

func (s *DeploymentStore) GetPoolStats() PoolStats {
	if s == nil || s.pool == nil {
		return PoolStats{}
	}

	stat := s.pool.Stat()
	return PoolStats{
		TotalConnections:   stat.TotalConns(),
		IdleConnections:    stat.IdleConns(),
		ActiveConnections:  int32(stat.AcquiredConns()),
		MaxConnections:     stat.MaxConns(),
		AcquireCount:       stat.AcquireCount(),
		NewConnsCount:      stat.NewConnsCount(),
		MaxLifetimeDestroy: stat.MaxLifetimeDestroyCount(),
		IdleDestroyCount:   stat.MaxIdleDestroyCount(),
	}
}

func (s *DeploymentStore) HealthCheck(ctx context.Context) error {
	if s == nil || s.pool == nil {
		return &StoreError{
			Type:    ErrDatabase,
			Message: "deployment store is not initialized",
		}
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	if err := s.pool.Ping(ctx); err != nil {
		return &StoreError{
			Type:    ErrDatabase,
			Message: "health check failed",
			Err:     err,
		}
	}

	return nil
}

// ============================================================================
// PRIVATE HELPERS
// ============================================================================

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
		return "", &StoreError{
			Type:    ErrDatabase,
			Message: "failed to upsert system user",
			Err:     err,
		}
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
		return "", &StoreError{
			Type:    ErrDatabase,
			Message: "failed to upsert project",
			Err:     err,
		}
	}

	return projectID, nil
}

func (s *DeploymentStore) validateUserInput(email, displayName, passwordHash string) error {
	if email == "" {
		return &StoreError{
			Type:    ErrInvalidInput,
			Message: "email is required",
		}
	}
	if displayName == "" {
		return &StoreError{
			Type:    ErrInvalidInput,
			Message: "displayName is required",
		}
	}
	if passwordHash == "" {
		return &StoreError{
			Type:    ErrInvalidInput,
			Message: "passwordHash is required",
		}
	}
	// Basic email validation
	if !strings.Contains(email, "@") {
		return &StoreError{
			Type:    ErrInvalidInput,
			Message: "invalid email format",
		}
	}
	return nil
}

func (s *DeploymentStore) validateAPIKeyInput(userID, name, keyPrefix, keyHash string) error {
	if userID == "" {
		return &StoreError{
			Type:    ErrInvalidInput,
			Message: "userID is required",
		}
	}
	if name == "" {
		return &StoreError{
			Type:    ErrInvalidInput,
			Message: "name is required",
		}
	}
	if keyPrefix == "" {
		return &StoreError{
			Type:    ErrInvalidInput,
			Message: "keyPrefix is required",
		}
	}
	if keyHash == "" {
		return &StoreError{
			Type:    ErrInvalidInput,
			Message: "keyHash is required",
		}
	}
	return nil
}

func (s *DeploymentStore) validateDeploymentInput(ownerUserID, appName, repoURL, appPath, portMap string) error {
	if ownerUserID == "" {
		return &StoreError{
			Type:    ErrInvalidInput,
			Message: "ownerUserID is required",
		}
	}
	if appName == "" {
		return &StoreError{
			Type:    ErrInvalidInput,
			Message: "appName is required",
		}
	}
	if repoURL == "" {
		return &StoreError{
			Type:    ErrInvalidInput,
			Message: "repoURL is required",
		}
	}
	if appPath == "" {
		return &StoreError{
			Type:    ErrInvalidInput,
			Message: "appPath is required",
		}
	}
	if portMap == "" {
		return &StoreError{
			Type:    ErrInvalidInput,
			Message: "portMap is required",
		}
	}
	return nil
}

// ============================================================================
// CRYPTO HELPERS
// ============================================================================

// Note: hashToken, generateRandomToken, encryptEnvValue, decryptEnvValue,
// slugifyName, and error helpers are now in helpers.go

// envAESKey derives a valid 32-byte AES-256 key from the configured secret.
// AES requires exactly 16, 24, or 32 bytes — using the raw env value directly
// breaks if it isn't one of those lengths. We always SHA-256 hash it so the
// output is always exactly 32 bytes regardless of the input length.
func envAESKey() []byte {
	raw := strings.TrimSpace(os.Getenv("ENV_ENCRYPTION_KEY"))
	if raw == "" {
		raw = "default-encryption-key-change-me"
	}
	sum := sha256.Sum256([]byte(raw))
	return sum[:]
}

func encryptEnvValue(value string) ([]byte, []byte, error) {
	key := envAESKey()

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, nil, err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, err
	}

	encrypted := gcm.Seal(nil, nonce, []byte(value), nil)
	return encrypted, nonce, nil
}

func decryptEnvValue(encryptedValue []byte, nonce []byte) (string, error) {
	key := envAESKey()

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	decrypted, err := gcm.Open(nil, nonce, encryptedValue, nil)
	if err != nil {
		return "", err
	}

	return string(decrypted), nil
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

// Note: StoreError.Error(), IsNotFoundError(), IsConflictError(), IsUnauthorizedError()
// are now in helpers.go

// ============================================================================
// EXAMPLE USAGE - Removed (moved to example_test.go)
// ============================================================================
// Example usage has been moved to example_test.go file