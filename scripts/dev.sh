#!/usr/bin/env bash

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

# ── Postgres ─────────────────────────────────────────────────────────────────
# If a postgres container already exists (running or stopped), reuse it.
# Only create a fresh one if it doesn't exist at all.
if docker ps --format '{{.Names}}' | grep -qx 'postgres'; then
	echo "postgres container already running, reusing it"
elif docker ps -a --format '{{.Names}}' | grep -qx 'postgres'; then
	echo "starting stopped postgres container"
	docker start postgres >/dev/null
else
	echo "creating postgres container"
	docker run -d \
		--name postgres \
		-e POSTGRES_DB=gravyflow \
		-e POSTGRES_USER=gravyflow \
		-e POSTGRES_PASSWORD=gravyflow \
		-p 5433:5432 \
		-v gravyflow-postgres-data:/var/lib/postgresql/data \
		-v "$repo_root/db/schema.sql:/docker-entrypoint-initdb.d/001-schema.sql:ro" \
		postgres:15
fi

# ── Redis ─────────────────────────────────────────────────────────────────────
if docker ps --format '{{.Names}}' | grep -qx 'redis'; then
	echo "redis container already running, reusing it"
elif docker ps -a --format '{{.Names}}' | grep -qx 'redis'; then
	echo "starting stopped redis container"
	docker start redis >/dev/null
else
	echo "creating redis container"
	docker run -d \
		--name redis \
		-p 6379:6379 \
		-v gravyflow-redis-data:/data \
		redis:7-alpine >/dev/null
fi

# ── Wait for both to be ready ─────────────────────────────────────────────────
echo "waiting for postgres and redis..."
for _ in $(seq 1 60); do
	if docker exec postgres pg_isready -U gravyflow -d gravyflow >/dev/null 2>&1 && \
		docker exec redis redis-cli ping >/dev/null 2>&1; then
		echo "postgres and redis are ready"
		break
	fi
	sleep 1
done

export PGHOST=127.0.0.1
export PGPORT=5433
export PGDATABASE=gravyflow
export PGUSER=gravyflow
export PGPASSWORD=gravyflow
export REDIS_ADDR=127.0.0.1:6379

exec go run ./cmd/api
