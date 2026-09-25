#!/bin/sh
# Nightly logical backup for the postgres-backup service (compose profile
# "backup"). Runs under busybox crond, which starts jobs with an empty
# environment, so the entrypoint snapshots the needed variables into
# /etc/backup.env first.
set -eu

# pg_dump lives in /usr/local/bin in the postgres image, which cron's bare
# environment leaves off PATH.
export PATH="/usr/local/bin:/usr/bin:/bin:${PATH:-}"

[ -f /etc/backup.env ] && . /etc/backup.env

: "${POSTGRES_HOST:?}" "${POSTGRES_DB:?}" "${POSTGRES_USER:?}" "${POSTGRES_PASSWORD:?}"
BACKUP_DIR="${BACKUP_DIR:-/backup}"
RETENTION_DAYS="${BACKUP_RETENTION_DAYS:-7}"

stamp="$(date -u +%Y%m%dT%H%M%SZ)"
target="$BACKUP_DIR/${POSTGRES_DB}-${stamp}.dump"
partial="$target.partial"

# Custom format (-Fc) is compressed and restores with pg_restore. Write to a
# .partial file first so a failed dump never looks like a good backup.
PGPASSWORD="$POSTGRES_PASSWORD" pg_dump -h "$POSTGRES_HOST" -U "$POSTGRES_USER" -d "$POSTGRES_DB" -Fc -f "$partial"
mv "$partial" "$target"
echo "[backup] wrote $target ($(du -h "$target" | cut -f1))"

find "$BACKUP_DIR" -maxdepth 1 -name "${POSTGRES_DB}-*.dump" -type f -mtime +"$RETENTION_DAYS" -print -delete |
  sed 's/^/[backup] pruned /'
find "$BACKUP_DIR" -maxdepth 1 -name '*.partial' -type f -mmin +120 -delete
