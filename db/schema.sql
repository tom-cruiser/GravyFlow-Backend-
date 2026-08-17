-- ============================================================================
-- EXTENSIONS
-- ============================================================================

CREATE EXTENSION IF NOT EXISTS pgcrypto;

-- ============================================================================
-- ENUM TYPES
-- ============================================================================

DO $$
BEGIN
    CREATE TYPE deployment_status AS ENUM ('BUILDING', 'DEPLOYED', 'FAILED', 'RUNNING', 'STOPPED', 'PAUSED');
EXCEPTION
    WHEN duplicate_object THEN NULL;
END $$;

DO $$
BEGIN
    CREATE TYPE deployment_action AS ENUM ('created', 'updated', 'deployed', 'restarted', 'stopped', 'failed');
EXCEPTION
    WHEN duplicate_object THEN NULL;
END $$;

DO $$
BEGIN
    CREATE TYPE notification_type AS ENUM ('email', 'webhook', 'slack');
EXCEPTION
    WHEN duplicate_object THEN NULL;
END $$;

-- ============================================================================
-- USERS
-- ============================================================================

CREATE TABLE IF NOT EXISTS users (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    display_name TEXT NOT NULL,
    avatar_url TEXT,
    is_admin BOOLEAN NOT NULL DEFAULT FALSE,
    -- Account Status Management (Admin Control Panel, Module A): active,
    -- suspended, flagged, or deleted. is_active predates this and is kept
    -- only so nothing relying on it directly breaks; the Go API reads/writes
    -- status exclusively (see cmd/api/db.go UserRecord).
    is_active BOOLEAN NOT NULL DEFAULT TRUE,
    status TEXT NOT NULL DEFAULT 'active',
    deleted_reason TEXT,
    mfa_enabled BOOLEAN NOT NULL DEFAULT FALSE,
    mfa_totp_secret TEXT,
    -- Admin Control Panel (Module E: Admin Profile & Credentials
    -- Management). Hashed (SHA-256) one-time MFA fallback codes — see
    -- cmd/api/admin_profile.go's SetMFARecoveryCodes. password_changed_at is
    -- an audit/UX timestamp only; no forced-rotation policy reads it yet.
    mfa_recovery_codes TEXT[],
    password_changed_at TIMESTAMPTZ,
    -- Admin Control Panel (Module A): search/filter by GitHub handle. No
    -- OAuth/GitHub-linking flow exists yet, so this is a plain nullable field.
    github_handle TEXT,
    last_login_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_users_email ON users (email);
CREATE INDEX IF NOT EXISTS idx_users_created_at ON users (created_at DESC);
CREATE INDEX IF NOT EXISTS idx_users_deleted_at ON users (deleted_at) WHERE deleted_at IS NULL;

-- ============================================================================
-- REFRESH TOKENS
-- ============================================================================

CREATE TABLE IF NOT EXISTS refresh_tokens (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash TEXT NOT NULL UNIQUE,
    expires_at TIMESTAMPTZ NOT NULL,
    revoked_at TIMESTAMPTZ,
    last_used_at TIMESTAMPTZ,
    user_agent TEXT,
    ip_address INET,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_refresh_tokens_user_id ON refresh_tokens (user_id);
CREATE INDEX IF NOT EXISTS idx_refresh_tokens_expires_at ON refresh_tokens (expires_at);
CREATE INDEX IF NOT EXISTS idx_refresh_tokens_revoked_at ON refresh_tokens (revoked_at) WHERE revoked_at IS NOT NULL;

-- ============================================================================
-- API KEYS
-- ============================================================================

CREATE TABLE IF NOT EXISTS api_keys (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    key_prefix TEXT NOT NULL,
    key_hash TEXT NOT NULL UNIQUE,
    name TEXT NOT NULL,
    expires_at TIMESTAMPTZ,
    revoked_at TIMESTAMPTZ,
    last_used_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_api_keys_user_id ON api_keys (user_id);
CREATE INDEX IF NOT EXISTS idx_api_keys_key_hash ON api_keys (key_hash);
CREATE INDEX IF NOT EXISTS idx_api_keys_expires_at ON api_keys (expires_at);

-- ============================================================================
-- QUOTAS
-- ============================================================================

CREATE TABLE IF NOT EXISTS quotas (
    user_id UUID PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    max_cpu NUMERIC(10,2) NOT NULL DEFAULT 2.00,
    max_memory_mb INTEGER NOT NULL DEFAULT 1024,
    max_apps INTEGER NOT NULL DEFAULT 3,
    max_storage_mb INTEGER NOT NULL DEFAULT 2048,
    max_bandwidth_gb INTEGER NOT NULL DEFAULT 100,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- ============================================================================
-- RESOURCE USAGE
-- ============================================================================

CREATE TABLE IF NOT EXISTS resource_usage (
    user_id UUID PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    current_cpu NUMERIC(10,2) NOT NULL DEFAULT 0,
    current_memory_mb INTEGER NOT NULL DEFAULT 0,
    current_apps INTEGER NOT NULL DEFAULT 0,
    current_storage_mb INTEGER NOT NULL DEFAULT 0,
    current_bandwidth_gb INTEGER NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_quotas_max_apps ON quotas (max_apps);
CREATE INDEX IF NOT EXISTS idx_resource_usage_current_apps ON resource_usage (current_apps);

-- ============================================================================
-- QUOTA HISTORY
-- ============================================================================

CREATE TABLE IF NOT EXISTS quota_history (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    field TEXT NOT NULL,
    old_value TEXT,
    new_value TEXT,
    -- Nullable + SET NULL, not RESTRICT: an admin who changed someone else's
    -- quota must still be hard-deletable (Admin Control Panel, Module A).
    changed_by UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_quota_history_user_id ON quota_history (user_id);
CREATE INDEX IF NOT EXISTS idx_quota_history_created_at ON quota_history (created_at);

-- ============================================================================
-- QUOTA ALERTS
-- ============================================================================

CREATE TABLE IF NOT EXISTS quota_alerts (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    resource TEXT NOT NULL,
    current FLOAT NOT NULL,
    "limit" FLOAT NOT NULL,
    percentage FLOAT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(user_id, resource)
);

CREATE INDEX IF NOT EXISTS idx_quota_alerts_user_id ON quota_alerts (user_id);
CREATE INDEX IF NOT EXISTS idx_quota_alerts_created_at ON quota_alerts (created_at);

-- ============================================================================
-- PROJECTS
-- ============================================================================

CREATE TABLE IF NOT EXISTS projects (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    slug TEXT NOT NULL,
    display_name TEXT NOT NULL,
    description TEXT,
    repo_url TEXT NOT NULL,
    default_branch TEXT DEFAULT 'main',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at TIMESTAMPTZ,
    UNIQUE (owner_user_id, slug)
);

CREATE INDEX IF NOT EXISTS idx_projects_owner_user_id ON projects (owner_user_id);
CREATE INDEX IF NOT EXISTS idx_projects_deleted_at ON projects (deleted_at) WHERE deleted_at IS NULL;

-- ============================================================================
-- DEPLOYMENTS
-- ============================================================================

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
    status deployment_status NOT NULL DEFAULT 'BUILDING',
    status_message TEXT,
    commit_hash TEXT,
    branch TEXT DEFAULT 'main',
    started_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at TIMESTAMPTZ,
    last_restart_at TIMESTAMPTZ,
    deleted_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_deployments_owner_user_id ON deployments (owner_user_id);
CREATE INDEX IF NOT EXISTS idx_deployments_project_id ON deployments (project_id);
CREATE INDEX IF NOT EXISTS idx_deployments_status ON deployments (status);
CREATE INDEX IF NOT EXISTS idx_deployments_started_at ON deployments (started_at DESC);
CREATE INDEX IF NOT EXISTS idx_deployments_container_id ON deployments (container_id);
CREATE INDEX IF NOT EXISTS idx_deployments_commit_hash ON deployments (commit_hash);
CREATE INDEX IF NOT EXISTS idx_deployments_deleted_at ON deployments (deleted_at) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_deployments_status_owner ON deployments (status, owner_user_id);

-- ============================================================================
-- DEPLOYMENT HISTORY
-- ============================================================================

CREATE TABLE IF NOT EXISTS deployment_history (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    deployment_id UUID NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
    action deployment_action NOT NULL,
    previous_state JSONB,
    new_state JSONB,
    changed_by UUID NOT NULL REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_deployment_history_deployment_id ON deployment_history (deployment_id);
CREATE INDEX IF NOT EXISTS idx_deployment_history_created_at ON deployment_history (created_at DESC);

-- ============================================================================
-- ENVIRONMENT VARIABLES
-- ============================================================================

CREATE TABLE IF NOT EXISTS deployment_env_vars (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    deployment_id UUID NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
    env_key TEXT NOT NULL,
    encrypted_value BYTEA NOT NULL,
    nonce BYTEA NOT NULL,
    category TEXT DEFAULT 'general',
    sensitive BOOLEAN DEFAULT FALSE,
    description TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (deployment_id, env_key)
);

CREATE INDEX IF NOT EXISTS idx_deployment_env_vars_deployment_id ON deployment_env_vars (deployment_id);
CREATE INDEX IF NOT EXISTS idx_deployment_env_vars_env_key ON deployment_env_vars (env_key);
CREATE INDEX IF NOT EXISTS idx_deployment_env_vars_category ON deployment_env_vars (category);

-- ============================================================================
-- ENV VAR HISTORY
-- ============================================================================

CREATE TABLE IF NOT EXISTS env_var_history (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    deployment_id UUID NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
    env_key TEXT NOT NULL,
    action TEXT NOT NULL,
    changed_by UUID NOT NULL REFERENCES users(id),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_env_var_history_deployment_id ON env_var_history (deployment_id);
CREATE INDEX IF NOT EXISTS idx_env_var_history_created_at ON env_var_history (created_at DESC);

-- ============================================================================
-- HEALTH CONFIGURATIONS
-- ============================================================================

CREATE TABLE IF NOT EXISTS deployment_health_configs (
    deployment_id UUID PRIMARY KEY REFERENCES deployments(id) ON DELETE CASCADE,
    health_check_path TEXT NOT NULL DEFAULT '/health',
    health_check_interval_seconds INTEGER NOT NULL DEFAULT 30,
    health_check_timeout_seconds INTEGER NOT NULL DEFAULT 10,
    max_restarts_before_failing INTEGER NOT NULL DEFAULT 3,
    last_checked_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_deployment_health_configs_last_checked_at ON deployment_health_configs (last_checked_at);

-- ============================================================================
-- RESTART AUDITS
-- ============================================================================

CREATE TABLE IF NOT EXISTS deployment_restart_audits (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    deployment_id UUID NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
    action TEXT NOT NULL,
    outcome TEXT NOT NULL,
    reason TEXT NOT NULL,
    previous_container_id TEXT,
    new_container_id TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_deployment_restart_audits_deployment_id_created_at 
    ON deployment_restart_audits (deployment_id, created_at DESC);

-- ============================================================================
-- HEALTH HISTORY
-- ============================================================================

CREATE TABLE IF NOT EXISTS health_history (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    deployment_id UUID NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
    status TEXT NOT NULL,
    message TEXT,
    latency_ms INTEGER,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_health_history_deployment_id ON health_history (deployment_id);
CREATE INDEX IF NOT EXISTS idx_health_history_created_at ON health_history (created_at DESC);

-- ============================================================================
-- DOMAINS
-- ============================================================================

CREATE TABLE IF NOT EXISTS domains (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    deployment_id UUID NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
    app_id UUID REFERENCES deployments(id) ON DELETE CASCADE,
    custom_domain TEXT NOT NULL UNIQUE,
    verified BOOLEAN NOT NULL DEFAULT FALSE,
    verification_token TEXT NOT NULL DEFAULT '',
    verified_at TIMESTAMPTZ,
    expires_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_domains_project_id ON domains (project_id);
CREATE INDEX IF NOT EXISTS idx_domains_deployment_id ON domains (deployment_id);
CREATE INDEX IF NOT EXISTS idx_domains_app_id ON domains (app_id);
CREATE INDEX IF NOT EXISTS idx_domains_custom_domain ON domains (custom_domain);
CREATE INDEX IF NOT EXISTS idx_domains_verified ON domains (verified);

-- ============================================================================
-- DOMAIN VERIFICATION HISTORY
-- ============================================================================

CREATE TABLE IF NOT EXISTS domain_verification_history (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    domain_id UUID NOT NULL REFERENCES domains(id) ON DELETE CASCADE,
    status TEXT NOT NULL,
    message TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_domain_verification_history_domain_id ON domain_verification_history (domain_id);

-- ============================================================================
-- DOMAIN REDIRECTS
-- ============================================================================

CREATE TABLE IF NOT EXISTS domain_redirects (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    deployment_id UUID NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
    from_domain TEXT NOT NULL UNIQUE,
    to_domain TEXT NOT NULL,
    permanent BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_domain_redirects_from_domain ON domain_redirects (from_domain);
CREATE INDEX IF NOT EXISTS idx_domain_redirects_deployment_id ON domain_redirects (deployment_id);

-- ============================================================================
-- SSL CERTIFICATES
-- ============================================================================

CREATE TABLE IF NOT EXISTS ssl_certificates (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    domain_id UUID NOT NULL REFERENCES domains(id) ON DELETE CASCADE,
    issuer TEXT NOT NULL,
    valid_from TIMESTAMPTZ NOT NULL,
    valid_to TIMESTAMPTZ NOT NULL,
    serial TEXT NOT NULL,
    status TEXT DEFAULT 'active',
    last_checked TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(domain_id)
);

CREATE INDEX IF NOT EXISTS idx_ssl_certificates_valid_to ON ssl_certificates (valid_to);
CREATE INDEX IF NOT EXISTS idx_ssl_certificates_status ON ssl_certificates (status);

-- ============================================================================
-- JOBS
-- ============================================================================

CREATE TABLE IF NOT EXISTS jobs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    deployment_id UUID NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    type TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending',
    progress INTEGER DEFAULT 0,
    message TEXT,
    error TEXT,
    started_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_jobs_deployment_id ON jobs (deployment_id);
CREATE INDEX IF NOT EXISTS idx_jobs_user_id ON jobs (user_id);
CREATE INDEX IF NOT EXISTS idx_jobs_status ON jobs (status);
CREATE INDEX IF NOT EXISTS idx_jobs_created_at ON jobs (created_at DESC);

-- ============================================================================
-- NOTIFICATIONS
-- ============================================================================

CREATE TABLE IF NOT EXISTS notification_settings (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    type notification_type NOT NULL,
    target TEXT NOT NULL,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    on_deployment_start BOOLEAN NOT NULL DEFAULT FALSE,
    on_deployment_success BOOLEAN NOT NULL DEFAULT TRUE,
    on_deployment_failure BOOLEAN NOT NULL DEFAULT TRUE,
    on_restart BOOLEAN NOT NULL DEFAULT FALSE,
    on_alert BOOLEAN NOT NULL DEFAULT TRUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(user_id, type, target)
);

CREATE INDEX IF NOT EXISTS idx_notification_settings_user_id ON notification_settings (user_id);

-- ============================================================================
-- WEBHOOKS
-- ============================================================================

CREATE TABLE IF NOT EXISTS webhooks (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    url TEXT NOT NULL,
    secret TEXT,
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    events TEXT[] DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_webhooks_user_id ON webhooks (user_id);

-- ============================================================================
-- TEAMS / ORGANIZATIONS (Multi-tenant)
-- ============================================================================

CREATE TABLE IF NOT EXISTS teams (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name TEXT NOT NULL,
    slug TEXT NOT NULL UNIQUE,
    owner_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_teams_slug ON teams (slug);
CREATE INDEX IF NOT EXISTS idx_teams_owner_id ON teams (owner_id);

CREATE TABLE IF NOT EXISTS team_members (
    team_id UUID NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role TEXT NOT NULL DEFAULT 'member',
    joined_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (team_id, user_id)
);

CREATE INDEX IF NOT EXISTS idx_team_members_user_id ON team_members (user_id);

-- ============================================================================
-- DEPLOYMENT METRICS
-- ============================================================================

CREATE TABLE IF NOT EXISTS deployment_metrics (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    deployment_id UUID NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
    cpu_usage NUMERIC(10,2),
    memory_usage INTEGER,
    network_in BIGINT,
    network_out BIGINT,
    disk_read BIGINT,
    disk_write BIGINT,
    response_time_ms INTEGER,
    requests_total INTEGER,
    errors_total INTEGER,
    recorded_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_deployment_metrics_deployment_id ON deployment_metrics (deployment_id);
CREATE INDEX IF NOT EXISTS idx_deployment_metrics_recorded_at ON deployment_metrics (recorded_at DESC);

-- ============================================================================
-- AUDIT LOGS
-- ============================================================================

-- Admin Control Panel, Module D. actor_user_id/actor_email/target_type/
-- target_id are what cmd/api/admin_audit.go's RecordAuditLog actually writes;
-- user_id/resource_type/resource_id/user_agent predate that and are kept
-- only for backward compatibility with anything still reading them directly.
CREATE TABLE IF NOT EXISTS audit_logs (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID REFERENCES users(id) ON DELETE SET NULL,
    -- No REFERENCES: the BEFORE UPDATE immutability trigger below rejects
    -- Postgres's own ON DELETE SET NULL cascade just like it rejects any
    -- other UPDATE, so a real FK here would make AdminHardDeleteUser fail for
    -- any user who ever appears as an actor. actor_email is the durable,
    -- human-readable identifier that's expected to outlive the actor's row.
    actor_user_id UUID,
    actor_email TEXT,
    action TEXT NOT NULL,
    resource_type TEXT, -- legacy alias for target_type; RecordAuditLog doesn't populate it
    resource_id UUID,
    target_type TEXT,
    target_id TEXT,
    details JSONB,
    -- TEXT, not INET: RecordAuditLog (cmd/api/admin_audit.go) always writes
    -- c.ClientIP() as plain text and an empty/masked value would fail INET's
    -- validation on insert.
    ip_address TEXT,
    user_agent TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_audit_logs_user_id ON audit_logs (user_id);
CREATE INDEX IF NOT EXISTS idx_audit_logs_created_at ON audit_logs (created_at DESC);
CREATE INDEX IF NOT EXISTS idx_audit_logs_resource ON audit_logs (resource_type, resource_id);
CREATE INDEX IF NOT EXISTS idx_audit_logs_actor ON audit_logs (actor_user_id);
CREATE INDEX IF NOT EXISTS idx_audit_logs_target ON audit_logs (target_type, target_id);

-- Immutable Log Enforcement: reject any UPDATE/DELETE at the database level,
-- not just by omitting those endpoints from the API.
CREATE OR REPLACE FUNCTION prevent_audit_log_mutation() RETURNS trigger AS $$
BEGIN
    RAISE EXCEPTION 'audit_logs is immutable: % is not permitted', TG_OP;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS audit_logs_no_update ON audit_logs;
CREATE TRIGGER audit_logs_no_update BEFORE UPDATE ON audit_logs
    FOR EACH ROW EXECUTE FUNCTION prevent_audit_log_mutation();

DROP TRIGGER IF EXISTS audit_logs_no_delete ON audit_logs;
CREATE TRIGGER audit_logs_no_delete BEFORE DELETE ON audit_logs
    FOR EACH ROW EXECUTE FUNCTION prevent_audit_log_mutation();

-- ============================================================================
-- CREDIT LEDGER (Admin Control Panel, Module C)
-- ============================================================================

CREATE TABLE IF NOT EXISTS credit_ledger (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    amount_cents BIGINT NOT NULL,
    entry_type TEXT NOT NULL,
    reason TEXT NOT NULL,
    -- Nullable + SET NULL, not RESTRICT: an admin who issued someone else
    -- credit must still be hard-deletable.
    issued_by UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_credit_ledger_user_id ON credit_ledger (user_id);

-- ============================================================================
-- RISK ALERTS (Admin Control Panel, Module C: Fraud & Abuse System)
-- ============================================================================

CREATE TABLE IF NOT EXISTS risk_alerts (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    deployment_id UUID REFERENCES deployments(id) ON DELETE CASCADE,
    risk_score INTEGER NOT NULL,
    reason TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'open',
    resolved_by UUID REFERENCES users(id) ON DELETE SET NULL,
    resolved_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_risk_alerts_status ON risk_alerts (status);
CREATE INDEX IF NOT EXISTS idx_risk_alerts_user_id ON risk_alerts (user_id);

-- ============================================================================
-- IMPERSONATION GRANTS (Admin Control Panel, Module A: Impersonation Mode)
-- ============================================================================

CREATE TABLE IF NOT EXISTS impersonation_grants (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    admin_user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    target_user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    reason TEXT,
    expires_at TIMESTAMPTZ NOT NULL,
    revoked_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_impersonation_grants_target ON impersonation_grants (target_user_id);

-- ============================================================================
-- MIGRATION HELPER FUNCTIONS
-- ============================================================================

-- Migration for domains table if needed
DO $$
DECLARE
    has_hostname BOOLEAN;
    has_verification_status BOOLEAN;
    update_sql TEXT;
BEGIN
    -- Check if old columns exist
    SELECT EXISTS (
        SELECT 1
        FROM information_schema.columns
        WHERE table_name = 'domains' AND column_name = 'hostname'
    ), EXISTS (
        SELECT 1
        FROM information_schema.columns
        WHERE table_name = 'domains' AND column_name = 'verification_status'
    )
    INTO has_hostname, has_verification_status;

    IF has_hostname OR has_verification_status THEN
        update_sql := 'UPDATE domains SET app_id = COALESCE(app_id, deployment_id)';

        IF has_hostname THEN
            update_sql := update_sql || ', custom_domain = COALESCE(custom_domain, hostname)';
        END IF;

        IF has_verification_status THEN
            update_sql := update_sql || ', verified = COALESCE(verified, verification_status = ''verified'')';
            update_sql := update_sql || ', verified_at = COALESCE(verified_at, CASE WHEN verification_status = ''verified'' THEN created_at ELSE NULL END)';
        END IF;

        update_sql := update_sql || ' WHERE app_id IS NULL';
        IF has_hostname THEN
            update_sql := update_sql || ' OR custom_domain IS NULL';
        END IF;

        EXECUTE update_sql;
    END IF;
END $$;

-- ============================================================================
-- VIEWS FOR COMMON QUERIES
-- ============================================================================

-- View: Deployment summary with health status
CREATE OR REPLACE VIEW deployment_summary AS
SELECT 
    d.id,
    d.app_name,
    d.status,
    d.status_message,
    d.container_id,
    d.started_at,
    d.finished_at,
    d.created_at,
    u.email as owner_email,
    p.display_name as project_name,
    hc.health_check_path,
    hc.last_checked_at,
    s.valid_to as ssl_expiry,
    COUNT(ra.id) as restart_count
FROM deployments d
LEFT JOIN users u ON u.id = d.owner_user_id
LEFT JOIN projects p ON p.id = d.project_id
LEFT JOIN deployment_health_configs hc ON hc.deployment_id = d.id
LEFT JOIN ssl_certificates s ON s.domain_id IN (
    SELECT id FROM domains WHERE deployment_id = d.id
)
LEFT JOIN deployment_restart_audits ra ON ra.deployment_id = d.id AND ra.created_at > now() - interval '24 hours'
WHERE d.deleted_at IS NULL
GROUP BY d.id, u.email, p.display_name, hc.health_check_path, hc.last_checked_at, s.valid_to;

-- View: User resource usage summary
CREATE OR REPLACE VIEW user_resource_summary AS
SELECT 
    u.id as user_id,
    u.email,
    u.display_name,
    q.max_cpu,
    q.max_memory_mb,
    q.max_apps,
    q.max_storage_mb,
    r.current_cpu,
    r.current_memory_mb,
    r.current_apps,
    r.current_storage_mb,
    (q.max_cpu - r.current_cpu) as available_cpu,
    (q.max_memory_mb - r.current_memory_mb) as available_memory_mb,
    (q.max_apps - r.current_apps) as available_apps,
    (q.max_storage_mb - r.current_storage_mb) as available_storage_mb,
    CASE 
        WHEN q.max_cpu > 0 THEN (r.current_cpu / q.max_cpu * 100) 
        ELSE 0 
    END as cpu_usage_percent,
    CASE 
        WHEN q.max_memory_mb > 0 THEN (r.current_memory_mb::float / q.max_memory_mb::float * 100) 
        ELSE 0 
    END as memory_usage_percent,
    CASE 
        WHEN q.max_apps > 0 THEN (r.current_apps::float / q.max_apps::float * 100) 
        ELSE 0 
    END as app_usage_percent,
    CASE 
        WHEN q.max_storage_mb > 0 THEN (r.current_storage_mb::float / q.max_storage_mb::float * 100) 
        ELSE 0 
    END as storage_usage_percent
FROM users u
JOIN quotas q ON q.user_id = u.id
JOIN resource_usage r ON r.user_id = u.id
WHERE u.deleted_at IS NULL;