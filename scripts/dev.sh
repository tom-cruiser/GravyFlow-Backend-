#!/usr/bin/env bash

# ============================================================================
# GRAVYFLOW DEVELOPMENT ENVIRONMENT SETUP
# ============================================================================
# This script sets up PostgreSQL and Redis containers for local development,
# and runs the application with the appropriate environment variables.
# ============================================================================

set -euo pipefail

# ============================================================================
# CONFIGURATION
# ============================================================================

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
CYAN='\033[0;36m'
NC='\033[0m' # No Color

# Container names
POSTGRES_CONTAINER="gravyflow-postgres"
REDIS_CONTAINER="gravyflow-redis"

# Ports
POSTGRES_HOST_PORT="${POSTGRES_PORT:-5433}"
POSTGRES_CONTAINER_PORT="5432"
REDIS_HOST_PORT="${REDIS_PORT:-6379}"
REDIS_CONTAINER_PORT="6379"

# ============================================================================
# DATABASE CREDENTIALS - YOUR CUSTOM CONFIGURATION
# ============================================================================

# Database credentials (YOUR CUSTOM VALUES)
POSTGRES_DB="${POSTGRES_DB:-gravyflow}"
POSTGRES_USER="${POSTGRES_USER:-postgres}"
POSTGRES_PASSWORD="${POSTGRES_PASSWORD:-blackey333Vi@32}"

# Timeouts
HEALTH_CHECK_TIMEOUT="${HEALTH_CHECK_TIMEOUT:-60}"
HEALTH_CHECK_INTERVAL="${HEALTH_CHECK_INTERVAL:-1}"

# Application
APP_PATH="${APP_PATH:-./cmd/api}"
BUILD_COMMAND="${BUILD_COMMAND:-go run}"

# ============================================================================
# GENERATE SECRETS
# ============================================================================

generate_secret() {
    if command -v openssl &> /dev/null; then
        openssl rand -base64 32 2>/dev/null || head -c 32 /dev/urandom | base64
    else
        head -c 32 /dev/urandom | base64
    fi
}

# ============================================================================
# HELPER FUNCTIONS
# ============================================================================

print_success() {
    echo -e "${GREEN}✓${NC} $1"
}

print_error() {
    echo -e "${RED}✗${NC} $1"
}

print_info() {
    echo -e "${BLUE}ℹ${NC} $1"
}

print_warning() {
    echo -e "${YELLOW}⚠${NC} $1"
}

print_header() {
    echo -e "\n${CYAN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}"
    echo -e "${CYAN}  $1${NC}"
    echo -e "${CYAN}━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━${NC}\n"
}

check_docker() {
    if ! command -v docker &> /dev/null; then
        print_error "Docker is not installed or not in PATH"
        echo "Please install Docker: https://docs.docker.com/get-docker/"
        exit 1
    fi

    if ! docker info &> /dev/null; then
        print_error "Docker daemon is not running"
        echo "Please start Docker Desktop or the Docker daemon"
        exit 1
    fi
}

check_port() {
    local port=$1
    if command -v lsof &> /dev/null && lsof -i ":$port" &> /dev/null; then
        print_warning "Port $port is already in use"
        return 1
    fi
    return 0
}

container_exists() {
    docker ps -a --format '{{.Names}}' | grep -qx "$1"
}

container_running() {
    docker ps --format '{{.Names}}' | grep -qx "$1"
}

ensure_container() {
    local name=$1
    local image=$2
    local port_mapping=$3
    local volume=$4
    local env_vars=$5
    local extra_args=$6

    if container_running "$name"; then
        print_success "$name container already running, reusing it"
        return 0
    fi

    if container_exists "$name"; then
        print_info "Starting stopped $name container"
        docker start "$name" >/dev/null
        print_success "$name container started"
        return 0
    fi

    print_info "Creating $name container"
    
    local cmd="docker run -d \
        --name $name \
        --restart unless-stopped \
        -p $port_mapping \
        -v $volume \
        $env_vars \
        $extra_args \
        $image"

    eval "$cmd" >/dev/null 2>&1
    
    if [ $? -eq 0 ]; then
        print_success "$name container created and started"
        return 0
    else
        print_error "Failed to create $name container"
        return 1
    fi
}

wait_for_service() {
    local name=$1
    local check_command=$2
    local timeout=$3
    local interval=$4

    print_info "Waiting for $name to be ready..."
    
    local elapsed=0
    while [ $elapsed -lt $timeout ]; do
        if eval "$check_command" &> /dev/null; then
            print_success "$name is ready"
            return 0
        fi
        sleep $interval
        elapsed=$((elapsed + interval))
        
        # Show progress
        if [ $((elapsed % 5)) -eq 0 ]; then
            echo -n "."
        fi
    done
    
    echo ""
    print_error "Timeout waiting for $name to be ready"
    return 1
}

# ============================================================================
# ENVIRONMENT SETUP
# ============================================================================

setup_environment() {
    print_header "Setting up development environment"
    
    # Check Docker
    check_docker
    
    # Check ports
    local ports_ok=true
    if ! check_port $POSTGRES_HOST_PORT; then
        ports_ok=false
        print_warning "PostgreSQL port $POSTGRES_HOST_PORT is in use"
    fi
    if ! check_port $REDIS_HOST_PORT; then
        ports_ok=false
        print_warning "Redis port $REDIS_HOST_PORT is in use"
    fi
    
    if [ "$ports_ok" = false ]; then
        print_warning "Some ports are in use. You can set different ports via environment variables:"
        echo "  POSTGRES_PORT=<port> $0"
        echo "  REDIS_PORT=<port> $0"
        read -p "Continue anyway? (y/N) " -n 1 -r
        echo
        if [[ ! $REPLY =~ ^[Yy]$ ]]; then
            exit 1
        fi
    fi
    
    # Generate secrets if not set
    if [ -z "${AUTH_JWT_SECRET:-}" ]; then
        AUTH_JWT_SECRET="$(generate_secret)"
        print_warning "Generated temporary AUTH_JWT_SECRET"
    fi
    
    if [ -z "${APP_ENV_ENCRYPTION_KEY:-}" ]; then
        APP_ENV_ENCRYPTION_KEY="$(generate_secret)"
        print_warning "Generated temporary APP_ENV_ENCRYPTION_KEY"
    fi
    
    export AUTH_JWT_SECRET
    export APP_ENV_ENCRYPTION_KEY
}

# ============================================================================
# POSTGRESQL SETUP
# ============================================================================

setup_postgres() {
    print_header "Setting up PostgreSQL"
    
    local env_vars="-e POSTGRES_DB=$POSTGRES_DB \
        -e POSTGRES_USER=$POSTGRES_USER \
        -e POSTGRES_PASSWORD=$POSTGRES_PASSWORD"
    
    local volume="$POSTGRES_CONTAINER-data:/var/lib/postgresql/data"
    local port_mapping="$POSTGRES_HOST_PORT:$POSTGRES_CONTAINER_PORT"
    local extra_args=""
    
    # Mount schema if exists
    local schema_path="${repo_root}/db/schema.sql"
    if [ -f "$schema_path" ]; then
        extra_args="-v $schema_path:/docker-entrypoint-initdb.d/001-schema.sql:ro"
        print_info "Schema file found: $schema_path"
    else
        print_warning "Schema file not found at: $schema_path"
    fi
    
    ensure_container "$POSTGRES_CONTAINER" "postgres:15" "$port_mapping" "$volume" "$env_vars" "$extra_args"
    
    # Wait for PostgreSQL
    local check_cmd="docker exec $POSTGRES_CONTAINER pg_isready -U $POSTGRES_USER -d $POSTGRES_DB"
    wait_for_service "PostgreSQL" "$check_cmd" "$HEALTH_CHECK_TIMEOUT" "$HEALTH_CHECK_INTERVAL"
    
    # Apply schema if already exists (for updates)
    if [ -f "$schema_path" ]; then
        print_info "Applying database schema..."
        docker exec -i "$POSTGRES_CONTAINER" psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" < "$schema_path" 2>/dev/null || true
        print_success "Schema applied"
    fi
    
    # Verify database connection
    print_info "Verifying database connection..."
    if docker exec "$POSTGRES_CONTAINER" psql -U "$POSTGRES_USER" -d "$POSTGRES_DB" -c "SELECT 1" &> /dev/null; then
        print_success "Database connection verified"
    else
        print_warning "Could not verify database connection"
    fi
}

# ============================================================================
# REDIS SETUP
# ============================================================================

setup_redis() {
    print_header "Setting up Redis"
    
    local volume="$REDIS_CONTAINER-data:/data"
    local port_mapping="$REDIS_HOST_PORT:$REDIS_CONTAINER_PORT"
    local env_vars=""
    local extra_args=""
    
    ensure_container "$REDIS_CONTAINER" "redis:7-alpine" "$port_mapping" "$volume" "$env_vars" "$extra_args"
    
    # Wait for Redis
    local check_cmd="docker exec $REDIS_CONTAINER redis-cli ping"
    wait_for_service "Redis" "$check_cmd" "$HEALTH_CHECK_TIMEOUT" "$HEALTH_CHECK_INTERVAL"
    
    # Verify Redis connection
    print_info "Verifying Redis connection..."
    if docker exec "$REDIS_CONTAINER" redis-cli ping | grep -q "PONG"; then
        print_success "Redis connection verified"
    else
        print_warning "Could not verify Redis connection"
    fi
}

# ============================================================================
# CLEANUP
# ============================================================================

cleanup_containers() {
    print_header "Cleaning up containers"
    
    local containers=("$POSTGRES_CONTAINER" "$REDIS_CONTAINER")
    
    for container in "${containers[@]}"; do
        if container_exists "$container"; then
            print_info "Removing $container..."
            docker rm -f "$container" >/dev/null 2>&1
            print_success "$container removed"
        fi
    done
    
    print_success "Cleanup complete"
}

cleanup_volumes() {
    print_header "Cleaning up volumes"
    
    read -p "This will delete all data! Are you sure? (y/N) " -n 1 -r
    echo
    if [[ ! $REPLY =~ ^[Yy]$ ]]; then
        print_info "Cleanup cancelled"
        return
    fi
    
    local volumes=("$POSTGRES_CONTAINER-data" "$REDIS_CONTAINER-data")
    
    for volume in "${volumes[@]}"; do
        if docker volume ls -q | grep -qx "$volume"; then
            print_info "Removing volume $volume..."
            docker volume rm "$volume" >/dev/null 2>&1
            print_success "Volume $volume removed"
        fi
    done
    
    print_success "Volume cleanup complete"
}

# ============================================================================
# STATUS
# ============================================================================

show_status() {
    print_header "Container Status"
    
    # PostgreSQL status
    if container_running "$POSTGRES_CONTAINER"; then
        print_success "PostgreSQL: Running (port $POSTGRES_HOST_PORT)"
        print_info "  Database: $POSTGRES_DB"
        print_info "  User: $POSTGRES_USER"
        print_info "  Password: ********"
    elif container_exists "$POSTGRES_CONTAINER"; then
        print_warning "PostgreSQL: Stopped"
    else
        print_warning "PostgreSQL: Not created"
    fi
    
    # Redis status
    if container_running "$REDIS_CONTAINER"; then
        print_success "Redis: Running (port $REDIS_HOST_PORT)"
    elif container_exists "$REDIS_CONTAINER"; then
        print_warning "Redis: Stopped"
    else
        print_warning "Redis: Not created"
    fi
    
    echo ""
    
    # Show connection strings
    print_info "Connection Strings:"
    echo "  PostgreSQL: postgresql://$POSTGRES_USER:$POSTGRES_PASSWORD@localhost:$POSTGRES_HOST_PORT/$POSTGRES_DB"
    echo "  Redis: redis://localhost:$REDIS_HOST_PORT"
    echo ""
    
    print_info "API Environment Variables:"
    echo "  AUTH_JWT_SECRET: ${AUTH_JWT_SECRET:0:10}..."
    echo "  APP_ENV_ENCRYPTION_KEY: ${APP_ENV_ENCRYPTION_KEY:0:10}..."
}

# ============================================================================
# RUN APPLICATION
# ============================================================================

run_application() {
    print_header "Starting Application"
    
    # Set environment variables with your custom credentials
    export PGHOST=127.0.0.1
    export PGPORT=$POSTGRES_HOST_PORT
    export PGDATABASE=$POSTGRES_DB
    export PGUSER=$POSTGRES_USER
    export PGPASSWORD=$POSTGRES_PASSWORD
    export REDIS_ADDR=127.0.0.1:$REDIS_HOST_PORT
    
    # Application-specific environment variables
    export DATABASE_URL="postgres://$PGUSER:$PGPASSWORD@$PGHOST:$PGPORT/$PGDATABASE?sslmode=disable"
    export PORT=8080
    export ENVIRONMENT=development
    export LOG_LEVEL=debug
    
    # Generate secrets if not set
    if [ -z "${AUTH_JWT_SECRET:-}" ]; then
        export AUTH_JWT_SECRET="$(generate_secret)"
    fi
    
    if [ -z "${APP_ENV_ENCRYPTION_KEY:-}" ]; then
        export APP_ENV_ENCRYPTION_KEY="$(generate_secret)"
    fi
    
    # Show environment
    print_info "Environment:"
    echo "  PGHOST=$PGHOST"
    echo "  PGPORT=$PGPORT"
    echo "  PGDATABASE=$PGDATABASE"
    echo "  PGUSER=$PGUSER"
    echo "  PGPASSWORD=********"
    echo "  REDIS_ADDR=$REDIS_ADDR"
    echo "  DATABASE_URL=postgres://$PGUSER:***@$PGHOST:$PGPORT/$PGDATABASE"
    echo "  PORT=$PORT"
    echo "  AUTH_JWT_SECRET=${AUTH_JWT_SECRET:0:10}..."
    echo ""
    
    # Build or run
    cd "$repo_root"
    
    if [ "$BUILD_COMMAND" = "go run" ]; then
        print_info "Running application with: $BUILD_COMMAND $APP_PATH"
        echo -e "${GREEN}Application logs:${NC}\n"
        exec $BUILD_COMMAND "$APP_PATH"
    else
        print_info "Building application..."
        $BUILD_COMMAND
        print_success "Build complete"
        exec ./"$APP_PATH"
    fi
}

# ============================================================================
# HELP
# ============================================================================

show_help() {
    cat << EOF
GRAVYFLOW Development Environment Setup

Usage: $0 [COMMAND]

Commands:
    start       Start the development environment (default)
    stop        Stop all containers
    restart     Restart all containers
    status      Show container status
    logs        Show container logs
    clean       Remove containers (keeps data)
    clean-all   Remove containers and volumes (deletes data)
    shell       Open a shell in the PostgreSQL container
    help        Show this help message

Environment Variables:
    POSTGRES_PORT       PostgreSQL host port (default: 5433)
    REDIS_PORT          Redis host port (default: 6379)
    POSTGRES_DB         Database name (default: gravyflow)
    POSTGRES_USER       Database user (default: postgres)
    POSTGRES_PASSWORD   Database password (default: blackey333Vi@32)
    HEALTH_CHECK_TIMEOUT Health check timeout in seconds (default: 60)
    APP_PATH           Application path (default: ./cmd/api)
    BUILD_COMMAND      Build command (default: go run)
    AUTH_JWT_SECRET    JWT signing secret (auto-generated)
    APP_ENV_ENCRYPTION_KEY Encryption key (auto-generated)

Examples:
    $0                  # Start development environment
    $0 status          # Show container status
    $0 logs            # Show container logs
    $0 clean-all       # Clean everything
    POSTGRES_PORT=5434 $0  # Use different PostgreSQL port

EOF
}

# ============================================================================
# COMMAND HANDLING
# ============================================================================

handle_command() {
    local command="${1:-start}"
    
    case "$command" in
        start)
            setup_environment
            setup_postgres
            setup_redis
            show_status
            run_application
            ;;
        stop)
            print_header "Stopping containers"
            docker stop "$POSTGRES_CONTAINER" "$REDIS_CONTAINER" 2>/dev/null || true
            print_success "Containers stopped"
            ;;
        restart)
            print_header "Restarting containers"
            docker restart "$POSTGRES_CONTAINER" "$REDIS_CONTAINER" 2>/dev/null || true
            print_success "Containers restarted"
            ;;
        status)
            show_status
            ;;
        logs)
            print_header "Container Logs"
            echo -e "${YELLOW}PostgreSQL logs:${NC}"
            docker logs --tail 50 "$POSTGRES_CONTAINER" 2>&1 || echo "Container not found"
            echo ""
            echo -e "${YELLOW}Redis logs:${NC}"
            docker logs --tail 50 "$REDIS_CONTAINER" 2>&1 || echo "Container not found"
            ;;
        shell)
            print_header "Opening shell in PostgreSQL container"
            docker exec -it "$POSTGRES_CONTAINER" psql -U "$POSTGRES_USER" -d "$POSTGRES_DB"
            ;;
        clean)
            cleanup_containers
            ;;
        clean-all)
            cleanup_containers
            cleanup_volumes
            ;;
        help|--help|-h)
            show_help
            ;;
        *)
            print_error "Unknown command: $command"
            echo "Use '$0 help' for available commands"
            exit 1
            ;;
    esac
}

# ============================================================================
# MAIN
# ============================================================================

# Find repository root
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"

# Handle command
handle_command "$@"