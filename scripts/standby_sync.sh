#!/bin/sh
# Keeps the local "postgres" service a warm standby of the primary (Neon)
# database, and keeps dated backups of it. Runs as the postgres-standby-sync
# service's main process:
#
#   every SYNC_INTERVAL_SECONDS:
#     1. pg_dump the primary (SOURCE_DATABASE_URL) to /backup/primary-latest.dump
#     2. sanity-check the dump (it must contain the core tables)
#     3. replace the local database's public schema with it in ONE
#        transaction — a failure leaves the standby exactly as it was
#     4. keep one dump per day for BACKUP_RETENTION_DAYS
#
# Neon has logical replication off (wal_level=replica), so this is a
# snapshot copy: the standby lags the primary by up to one interval.
#
# Failover safety: while GRAVYFLOW_DB_TARGET=local the API writes to the
# standby, so syncing would overwrite those writes with stale primary data.
# The loop pauses instead. If the primary is unreachable the dump fails and
# the standby is left untouched.
set -u

: "${SOURCE_DATABASE_URL:?set DATABASE_URL (the primary) in .env}"
: "${POSTGRES_HOST:?}" "${POSTGRES_DB:?}" "${POSTGRES_USER:?}" "${POSTGRES_PASSWORD:?}"
BACKUP_DIR="${BACKUP_DIR:-/backup}"
INTERVAL="${SYNC_INTERVAL_SECONDS:-900}"
RETENTION_DAYS="${BACKUP_RETENTION_DAYS:-7}"
# Tables whose absence means the dump is not a real GravyFlow database (an
# emptied or wrong primary) — never let that wipe the standby.
REQUIRED_TABLES="users deployments quotas schema_migrations"

export PGPASSWORD="$POSTGRES_PASSWORD"
log() { echo "[standby-sync] $(date -u +%Y-%m-%dT%H:%M:%SZ) $*"; }

sync_once() {
  latest="$BACKUP_DIR/primary-latest.dump"
  partial="$latest.partial"

  if ! pg_dump "$SOURCE_DATABASE_URL" -Fc --no-owner --no-acl -f "$partial"; then
    rm -f "$partial"
    log "primary unreachable or dump failed — standby left unchanged"
    return 1
  fi

  toc="$(pg_restore -l "$partial")"
  for t in $REQUIRED_TABLES; do
    if ! printf '%s\n' "$toc" | grep -q " TABLE public $t "; then
      log "dump has no table '$t' — refusing to restore it over the standby (kept as $partial.rejected)"
      mv "$partial" "$partial.rejected"
      return 1
    fi
  done
  mv "$partial" "$latest"

  # Replace the whole public schema rather than pg_restore --clean, which
  # can't drop objects the standby has but the primary doesn't (e.g. its
  # initdb schema.sql tables still referencing users). One transaction with
  # ON_ERROR_STOP: any failure rolls back to the previous standby.
  if ! { echo "SET client_min_messages = warning; DROP SCHEMA IF EXISTS public CASCADE; CREATE SCHEMA public;"
         pg_restore --no-owner --no-acl -f - "$latest"; } |
       psql -X -q -v ON_ERROR_STOP=1 --single-transaction \
         -h "$POSTGRES_HOST" -U "$POSTGRES_USER" -d "$POSTGRES_DB" >/dev/null; then
    log "restore failed — rolled back, standby unchanged"
    return 1
  fi

  daily="$BACKUP_DIR/primary-$(date -u +%Y%m%d).dump"
  [ -f "$daily" ] || cp "$latest" "$daily"
  find "$BACKUP_DIR" -maxdepth 1 -name 'primary-2*.dump' -type f -mtime +"$RETENTION_DAYS" -print -delete |
    sed 's/^/[standby-sync] pruned /'
  date -u +%Y-%m-%dT%H:%M:%SZ > "$BACKUP_DIR/last-sync"
  log "standby synced ($(du -h "$latest" | cut -f1) dump)"
}

until pg_isready -q -h "$POSTGRES_HOST" -U "$POSTGRES_USER" -d "$POSTGRES_DB"; do sleep 2; done

while :; do
  if [ "$(printf '%s' "${GRAVYFLOW_DB_TARGET:-}" | tr 'A-Z' 'a-z')" = "local" ]; then
    log "GRAVYFLOW_DB_TARGET=local — the standby is live; sync paused"
  else
    sync_once
  fi
  sleep "$INTERVAL"
done
