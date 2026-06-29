CREATE EXTENSION IF NOT EXISTS pgcrypto;

DO $$
BEGIN
    CREATE TYPE deployment_status AS ENUM ('BUILDING', 'DEPLOYED', 'FAILED', 'RUNNING');
EXCEPTION
    WHEN duplicate_object THEN NULL;
END $$;

CREATE TABLE IF NOT EXISTS users (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    email TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    display_name TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_users_created_at ON users (created_at DESC);

CREATE TABLE IF NOT EXISTS refresh_tokens (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token_hash TEXT NOT NULL UNIQUE,
    expires_at TIMESTAMPTZ NOT NULL,
    revoked_at TIMESTAMPTZ,
    last_used_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_refresh_tokens_user_id ON refresh_tokens (user_id);
CREATE INDEX IF NOT EXISTS idx_refresh_tokens_expires_at ON refresh_tokens (expires_at);

CREATE TABLE IF NOT EXISTS api_keys (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    key_prefix TEXT NOT NULL UNIQUE,
    key_hash TEXT NOT NULL UNIQUE,
    name TEXT NOT NULL,
    expires_at TIMESTAMPTZ,
    revoked_at TIMESTAMPTZ,
    last_used_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_api_keys_user_id ON api_keys (user_id);
CREATE INDEX IF NOT EXISTS idx_api_keys_expires_at ON api_keys (expires_at);

CREATE TABLE IF NOT EXISTS quotas (
    user_id UUID PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    max_cpu NUMERIC(10,2) NOT NULL DEFAULT 2.00,
    max_memory_mb INTEGER NOT NULL DEFAULT 1024,
    max_apps INTEGER NOT NULL DEFAULT 3,
    max_storage_mb INTEGER NOT NULL DEFAULT 2048,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS resource_usage (
    user_id UUID PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    current_cpu NUMERIC(10,2) NOT NULL DEFAULT 0,
    current_memory_mb INTEGER NOT NULL DEFAULT 0,
    current_apps INTEGER NOT NULL DEFAULT 0,
    current_storage_mb INTEGER NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_quotas_max_apps ON quotas (max_apps);
CREATE INDEX IF NOT EXISTS idx_resource_usage_current_apps ON resource_usage (current_apps);

CREATE TABLE IF NOT EXISTS projects (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    owner_user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    slug TEXT NOT NULL,
    display_name TEXT NOT NULL,
    repo_url TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (owner_user_id, slug)
);

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
    status deployment_status NOT NULL,
    status_message TEXT,
    started_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    finished_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_deployments_owner_user_id ON deployments (owner_user_id);
CREATE INDEX IF NOT EXISTS idx_deployments_project_id ON deployments (project_id);
CREATE INDEX IF NOT EXISTS idx_deployments_status ON deployments (status);
CREATE INDEX IF NOT EXISTS idx_deployments_started_at ON deployments (started_at DESC);
CREATE INDEX IF NOT EXISTS idx_deployments_container_id ON deployments (container_id);

CREATE TABLE IF NOT EXISTS deployment_env_vars (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    deployment_id UUID NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
    env_key TEXT NOT NULL,
    encrypted_value BYTEA NOT NULL,
    nonce BYTEA NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (deployment_id, env_key)
);

CREATE INDEX IF NOT EXISTS idx_deployment_env_vars_deployment_id ON deployment_env_vars (deployment_id);
CREATE INDEX IF NOT EXISTS idx_deployment_env_vars_env_key ON deployment_env_vars (env_key);

CREATE TABLE IF NOT EXISTS deployment_health_configs (
    deployment_id UUID PRIMARY KEY REFERENCES deployments(id) ON DELETE CASCADE,
    health_check_path TEXT NOT NULL DEFAULT '/health',
    health_check_interval_seconds INTEGER NOT NULL DEFAULT 30,
    max_restarts_before_failing INTEGER NOT NULL DEFAULT 3,
    last_checked_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

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

CREATE INDEX IF NOT EXISTS idx_deployment_health_configs_last_checked_at ON deployment_health_configs (last_checked_at);
CREATE INDEX IF NOT EXISTS idx_deployment_restart_audits_deployment_id_created_at ON deployment_restart_audits (deployment_id, created_at DESC);

CREATE TABLE IF NOT EXISTS domains (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    deployment_id UUID NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
    app_id UUID NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
    custom_domain TEXT NOT NULL UNIQUE,
    verified BOOLEAN NOT NULL DEFAULT FALSE,
    verification_token TEXT NOT NULL DEFAULT '',
    verified_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_domains_project_id ON domains (project_id);
CREATE INDEX IF NOT EXISTS idx_domains_deployment_id ON domains (deployment_id);
CREATE INDEX IF NOT EXISTS idx_domains_app_id ON domains (app_id);
CREATE INDEX IF NOT EXISTS idx_domains_verified ON domains (verified);

ALTER TABLE IF EXISTS domains
    ADD COLUMN IF NOT EXISTS app_id UUID,
    ADD COLUMN IF NOT EXISTS custom_domain TEXT,
    ADD COLUMN IF NOT EXISTS verified BOOLEAN NOT NULL DEFAULT FALSE,
    ADD COLUMN IF NOT EXISTS verification_token TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS verified_at TIMESTAMPTZ;

DO $$
DECLARE
    has_hostname BOOLEAN;
    has_verification_status BOOLEAN;
    update_sql TEXT;
BEGIN
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