#!/bin/sh
# Copy the local standby BACK to the primary (Neon) after a failover, so the
# writes made while GRAVYFLOW_DB_TARGET=local aren't lost. Run from the repo
# root on the host, with the API stopped so nothing writes during the copy:
#
#   docker compose stop api
#   sh scripts/db_failback.sh
#   # then set GRAVYFLOW_DB_TARGET=remote in .env and: docker compose up -d
#
# This REPLACES the primary's contents with the standby's.
set -eu
cd "$(dirname "$0")/.."

printf 'This overwrites the PRIMARY database (DATABASE_URL) with the local standby. Type "failback" to continue: '
read -r answer
[ "$answer" = "failback" ] || { echo "aborted"; exit 1; }

docker compose exec -T postgres sh -c 'pg_dump -U "$POSTGRES_USER" -d "$POSTGRES_DB" -Fc --no-owner --no-acl' > standby-failback.dump
echo "standby dumped to standby-failback.dump ($(du -h standby-failback.dump | cut -f1))"

# The postgres container has the matching client; the primary URL comes from
# .env via compose, so it's never echoed here.
docker compose run --rm -T --no-deps -v "$PWD/standby-failback.dump:/tmp/f.dump:ro" --entrypoint sh postgres-standby-sync -c \
  '{ echo "DROP SCHEMA IF EXISTS public CASCADE; CREATE SCHEMA public;"; pg_restore --no-owner --no-acl -f - /tmp/f.dump; } |
   psql -X -q -v ON_ERROR_STOP=1 --single-transaction "$SOURCE_DATABASE_URL" >/dev/null'
echo "primary restored from the standby; keep standby-failback.dump until you've checked it"
