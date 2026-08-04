package main

import (
    "bytes"
    "context"
    "encoding/json"
    "errors"
    "fmt"
    "io"
    "log"
    "net"
    "net/http"
    "os"
    "path/filepath"
    "sort"
    "strings"
    "sync"
    "syscall"
    "time"
)

// ============================================================================
// CONSTANTS AND TYPES
// ============================================================================

const (
    caddyAdminLoadURL = "http://localhost:2019/load"
    defaultHTTPPort   = "80"
    defaultHTTPSPort  = "443"
)

type ContainerInfo struct {
    ContainerName string
    InternalIP    string
    InternalPort  string
    DeploymentID  string
    Labels        map[string]string
    HealthStatus  string
    StartedAt     time.Time
}

type Route struct {
    ContainerName string
    DeploymentID  string
    Hosts         []string
    Target        string
    CreatedAt     time.Time
    UpdatedAt     time.Time
}

type RouteManager struct {
    mu          sync.RWMutex
    routes      map[string]Route
    caddyClient *http.Client
    config      CaddyConfig
}

type CaddyConfig struct {
    HTTPPort      string
    HTTPSPort     string
    EnableTLS     bool
    TLSEmail      string
    TLSAcmeCA     string
    AdminListen   string
    BackupDir     string
    HealthCheck   bool
    LoadBalancing string
}

type BackoffConfig struct {
    InitialDelay time.Duration
    MaxDelay     time.Duration
    Factor       float64
}

type RouteMetrics struct {
    TotalRoutes      int
    ActiveContainers int
    DomainsCount     int
    LastSyncTime     time.Time
    SyncErrors       int
    LastError        error
}

// ============================================================================
// MAIN FUNCTIONS
// ============================================================================

func caddySyncRequired() bool {
    return strings.EqualFold(strings.TrimSpace(os.Getenv("GRAVYFLOW_REQUIRE_CADDY")), "1")
}

func caddyAdminHealthy(ctx context.Context) bool {
    req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost:2019/config/", nil)
    if err != nil {
        return false
    }
    resp, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
    if err != nil {
        return false
    }
    defer resp.Body.Close()
    return resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices
}

// ============================================================================
// IMPROVED: Route Management with Locking
// ============================================================================

func NewRouteManager(config CaddyConfig) *RouteManager {
    return &RouteManager{
        routes:      make(map[string]Route),
        caddyClient: &http.Client{Timeout: 5 * time.Second},
        config:      config,
    }
}

func (rm *RouteManager) AddRoute(ctx context.Context, container ContainerInfo) error {
    rm.mu.Lock()
    defer rm.mu.Unlock()
    
    if container.ContainerName == "" {
        return fmt.Errorf("container name is required")
    }
    
    // Health check if enabled
    if rm.config.HealthCheck {
        if !rm.healthCheckContainer(ctx, container) {
            return fmt.Errorf("container health check failed: %s", container.ContainerName)
        }
    }
    
    route := Route{
        ContainerName: container.ContainerName,
        DeploymentID:  container.DeploymentID,
        Target:        fmt.Sprintf("%s:%s", container.InternalIP, container.InternalPort),
        Hosts:         rm.buildHosts(container),
        CreatedAt:     time.Now(),
        UpdatedAt:     time.Now(),
    }
    
    rm.routes[container.ContainerName] = route
    return rm.syncToCaddy(ctx)
}

func (rm *RouteManager) RemoveRoute(ctx context.Context, containerName string) error {
    rm.mu.Lock()
    defer rm.mu.Unlock()
    
    if _, exists := rm.routes[containerName]; !exists {
        return fmt.Errorf("route not found: %s", containerName)
    }
    
    delete(rm.routes, containerName)
    return rm.syncToCaddy(ctx)
}

func (rm *RouteManager) GetRoutes() []Route {
    rm.mu.RLock()
    defer rm.mu.RUnlock()
    
    routes := make([]Route, 0, len(rm.routes))
    for _, r := range rm.routes {
        routes = append(routes, r)
    }
    return routes
}

func (rm *RouteManager) buildHosts(container ContainerInfo) []string {
    hosts := []string{fmt.Sprintf("%s.localhost", container.ContainerName)}
    
    if container.DeploymentID != "" && deploymentStore != nil {
        verifiedDomains, err := deploymentStore.ListVerifiedDomainsForDeployment(
            context.Background(), container.DeploymentID,
        )
        if err != nil {
            log.Printf("caddy sync: load domains for deployment %s: %v", 
                container.DeploymentID, err)
        } else {
            hosts = append(hosts, verifiedDomains...)
        }
    }
    
    return hosts
}

func (rm *RouteManager) healthCheckContainer(ctx context.Context, container ContainerInfo) bool {
    url := fmt.Sprintf("http://%s:%s/health", container.InternalIP, container.InternalPort)
    req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
    if err != nil {
        return false
    }
    
    client := &http.Client{Timeout: 3 * time.Second}
    resp, err := client.Do(req)
    if err != nil {
        return false
    }
    defer resp.Body.Close()
    
    return resp.StatusCode == http.StatusOK
}

// ============================================================================
// IMPROVED: Caddy Synchronization with Retry and Validation
// ============================================================================

func (rm *RouteManager) syncToCaddy(ctx context.Context) error {
    return rm.syncWithRetry(ctx, func() error {
        return rm.syncToCaddyInternal(ctx)
    })
}

func (rm *RouteManager) syncToCaddyInternal(ctx context.Context) error {
    routes := rm.buildRouteConfig()
    
    // Validate routes before sending
    for _, route := range routes {
        if err := rm.validateRoute(route); err != nil {
            return fmt.Errorf("invalid route: %w", err)
        }
    }
    
    payload := rm.buildCaddyPayload(routes)
    
    body, err := json.Marshal(payload)
    if err != nil {
        return fmt.Errorf("marshal caddy payload: %w", err)
    }
    
    req, err := http.NewRequestWithContext(ctx, http.MethodPost, caddyAdminLoadURL, bytes.NewReader(body))
    if err != nil {
        return fmt.Errorf("create caddy request: %w", err)
    }
    req.Header.Set("Content-Type", "application/json")
    
    // Backup before updating
    if err := rm.backupCaddyConfig(ctx); err != nil {
        log.Printf("Warning: failed to backup config: %v", err)
    }
    
    resp, err := rm.caddyClient.Do(req)
    if err != nil {
        if caddyLoadSucceededDespiteClose(err) {
            return nil
        }
        if !caddySyncRequired() && (isCaddyUnavailable(err) || 
            strings.Contains(strings.ToLower(err.Error()), "connection reset")) {
            log.Printf("caddy sync: admin API error (%v) — continuing", err)
            return nil
        }
        return fmt.Errorf("load caddy config: %w", err)
    }
    defer resp.Body.Close()
    
    if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
        body, _ := io.ReadAll(resp.Body)
        if !caddySyncRequired() {
            log.Printf("caddy sync: load rejected (%s) — continuing: %s", resp.Status, string(body))
            return nil
        }
        return fmt.Errorf("caddy load rejected config: %s - %s", resp.Status, string(body))
    }
    
    return nil
}

func (rm *RouteManager) buildRouteConfig() []map[string]any {
    routes := make([]map[string]any, 0, len(rm.routes))
    
    for _, route := range rm.routes {
        routeConfig := map[string]any{
            "match": []any{
                map[string]any{"host": route.Hosts},
            },
            "handle": []any{
                map[string]any{
                    "handler": "reverse_proxy",
                    "upstreams": []any{
                        map[string]any{"dial": route.Target},
                    },
                },
            },
            "terminal": true,
        }
        routes = append(routes, routeConfig)
    }
    
    return routes
}

func (rm *RouteManager) buildCaddyPayload(routes []map[string]any) map[string]any {
    serverConfig := map[string]any{
        "listen": []string{fmt.Sprintf(":%s", rm.config.HTTPPort)},
        "routes": routes,
    }
    
    // Add HTTPS if enabled
    if rm.config.EnableTLS {
        serverConfig["listen"] = append(serverConfig["listen"].([]string), 
            fmt.Sprintf(":%s", rm.config.HTTPSPort))
        serverConfig["tls"] = map[string]any{
            "automation": map[string]any{
                "policy": "acme",
                "email":  rm.config.TLSEmail,
                "ca":     rm.config.TLSAcmeCA,
            },
        }
    }
    
    adminListen := rm.config.AdminListen
    if adminListen == "" {
        adminListen = "0.0.0.0:2019"
    }
    
    return map[string]any{
        "admin": map[string]any{
            "listen": adminListen,
        },
        "apps": map[string]any{
            "http": map[string]any{
                "servers": map[string]any{
                    "gravyflow": serverConfig,
                },
            },
        },
    }
}

// ============================================================================
// IMPROVED: Sync with Retry
// ============================================================================

func (rm *RouteManager) syncWithRetry(ctx context.Context, fn func() error) error {
    config := BackoffConfig{
        InitialDelay: 1 * time.Second,
        MaxDelay:     30 * time.Second,
        Factor:       2.0,
    }
    
    delay := config.InitialDelay
    maxAttempts := 5
    
    for attempt := 1; attempt <= maxAttempts; attempt++ {
        err := fn()
        if err == nil {
            return nil
        }
        
        if !isRetryableError(err) {
            return err
        }
        
        log.Printf("Sync failed (attempt %d/%d), retrying in %v: %v", 
            attempt, maxAttempts, delay, err)
        
        select {
        case <-ctx.Done():
            return ctx.Err()
        case <-time.After(delay):
        }
        
        // Exponential backoff
        delay = time.Duration(float64(delay) * config.Factor)
        if delay > config.MaxDelay {
            delay = config.MaxDelay
        }
    }
    
    return fmt.Errorf("sync failed after %d attempts", maxAttempts)
}

// ============================================================================
// ADDED: Route Validation
// ============================================================================

func (rm *RouteManager) validateRoute(route map[string]any) error {
    if _, ok := route["match"]; !ok {
        return fmt.Errorf("route missing 'match' field")
    }
    
    if _, ok := route["handle"]; !ok {
        return fmt.Errorf("route missing 'handle' field")
    }
    
    match, ok := route["match"].([]any)
    if !ok || len(match) == 0 {
        return fmt.Errorf("route has invalid match field")
    }
    
    for _, m := range match {
        hostMap, ok := m.(map[string]any)
        if !ok {
            continue
        }
        
        hosts, ok := hostMap["host"].([]any)
        if !ok {
            continue
        }
        
        for _, h := range hosts {
            host, ok := h.(string)
            if !ok {
                continue
            }
            
            if !isValidHostName(host) {
                return fmt.Errorf("invalid hostname: %s", host)
            }
        }
    }
    
    return nil
}

func isValidHostName(host string) bool {
    if host == "" || len(host) > 255 {
        return false
    }
    
    // Check for valid characters
    for _, ch := range host {
        if !((ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || 
             ch == '.' || ch == '-' || ch == '_') {
            return false
        }
    }
    
    return true
}

// ============================================================================
// ADDED: Configuration Backup
// ============================================================================

func (rm *RouteManager) backupCaddyConfig(ctx context.Context) error {
    backupDir := rm.config.BackupDir
    if backupDir == "" {
        backupDir = "/var/lib/caddy/backups"
    }
    
    if err := os.MkdirAll(backupDir, 0755); err != nil {
        return err
    }
    
    timestamp := time.Now().Format("20060102-150405")
    backupFile := filepath.Join(backupDir, fmt.Sprintf("caddy-config-%s.json", timestamp))
    
    req, err := http.NewRequestWithContext(ctx, "GET", "http://localhost:2019/config/", nil)
    if err != nil {
        return err
    }
    
    resp, err := rm.caddyClient.Do(req)
    if err != nil {
        return err
    }
    defer resp.Body.Close()
    
    config, err := io.ReadAll(resp.Body)
    if err != nil {
        return err
    }
    
    return os.WriteFile(backupFile, config, 0644)
}

// ============================================================================
// EXISTING HELPER FUNCTIONS (Improved)
// ============================================================================

func isCaddyUnavailable(err error) bool {
    if err == nil {
        return false
    }
    var opErr *net.OpError
    if errors.As(err, &opErr) {
        if errors.Is(opErr.Err, syscall.ECONNREFUSED) || errors.Is(opErr.Err, syscall.ECONNRESET) {
            return true
        }
    }
    msg := strings.ToLower(err.Error())
    return strings.Contains(msg, "connection refused") ||
        strings.Contains(msg, "connect: connection refused") ||
        strings.Contains(msg, "connection reset by peer") ||
        strings.Contains(msg, "read: connection reset") ||
        strings.Contains(msg, "no such host")
}

func isRetryableError(err error) bool {
    if err == nil {
        return false
    }
    
    msg := strings.ToLower(err.Error())
    retryableMarkers := []string{
        "connection refused",
        "connection reset",
        "timeout",
        "temporary failure",
        "network",
        "dial tcp",
        "unexpected eof",
        "i/o timeout",
        "context deadline exceeded",
    }
    
    for _, marker := range retryableMarkers {
        if strings.Contains(msg, marker) {
            return true
        }
    }
    
    return false
}

func caddyLoadSucceededDespiteClose(err error) bool {
    if err == nil {
        return true
    }
    if !strings.Contains(strings.ToLower(err.Error()), "connection reset") {
        return false
    }
    return caddyAdminHealthy(context.Background())
}

// ============================================================================
// SYNC FUNCTIONS (Using RouteManager)
// ============================================================================

var defaultRouteManager *RouteManager

func init() {
    config := CaddyConfig{
        HTTPPort:    defaultHTTPPort,
        HTTPSPort:   defaultHTTPSPort,
        EnableTLS:   false,
        HealthCheck: true,
        BackupDir:   "/var/lib/caddy/backups",
    }
    defaultRouteManager = NewRouteManager(config)
}

// SyncCaddyRoutesFromRunningContainers rebuilds the Caddy HTTP config from the current live Docker state.
func SyncCaddyRoutesFromRunningContainers() error {
    containers, err := ListRunningManagedContainers()
    if err != nil {
        return err
    }
    
    sort.Slice(containers, func(i, j int) bool {
        return containers[i].ContainerName < containers[j].ContainerName
    })
    
    ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
    defer cancel()
    
    // Sync routes
    for _, container := range containers {
        if container.ContainerName == "" || container.InternalIP == "" || container.InternalPort == "" {
            continue
        }
        
        if err := defaultRouteManager.AddRoute(ctx, container); err != nil {
            log.Printf("Failed to add route for %s: %v", container.ContainerName, err)
        }
    }
    
    return nil
}

// DeleteCaddyRouteForContainer removes routes for a specific container.
func DeleteCaddyRouteForContainer(containerName string) error {
    containerName = strings.TrimSpace(containerName)
    if containerName == "" {
        return &ValidationError{
            Field:   "containerName",
            Code:    "required",
            Message: "containerName is required",
        }
    }
    
    ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
    defer cancel()
    
    return defaultRouteManager.RemoveRoute(ctx, containerName)
}

// cleanupCaddyCertificatesForDomain removes SSL certificates for a custom domain.
func cleanupCaddyCertificatesForDomain(customDomain string) {
    customDomain = normalizeCustomDomain(customDomain)
    if customDomain == "" {
        return
    }
    
    storageRoot := strings.TrimSpace(os.Getenv("CADDY_DATA_DIR"))
    if storageRoot == "" {
        storageRoot = strings.TrimSpace(os.Getenv("CADDY_STORAGE_DIR"))
    }
    if storageRoot == "" {
        return
    }
    
    certificateRoot := filepath.Join(storageRoot, "certificates")
    _ = filepath.WalkDir(certificateRoot, func(path string, entry os.DirEntry, err error) error {
        if err != nil {
            return nil
        }
        if !strings.Contains(strings.ToLower(path), customDomain) {
            return nil
        }
        if entry.IsDir() {
            return os.RemoveAll(path)
        }
        _ = os.Remove(path)
        return nil
    })
}

func normalizeCustomDomain(domain string) string {
    return strings.ToLower(strings.TrimSpace(domain))
}

// ============================================================================
// MISSING FUNCTIONS (Need Implementation)
// ============================================================================

// ListRunningManagedContainers should be implemented to interact with Docker API
func ListRunningManagedContainers() ([]ContainerInfo, error) {
    // Implementation needed
    // Should return containers with managed labels
    return nil, nil
}

var deploymentStore interface {
    ListVerifiedDomainsForDeployment(ctx context.Context, deploymentID string) ([]string, error)
}