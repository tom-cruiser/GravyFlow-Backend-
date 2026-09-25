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

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
)

// ============================================================================
// CONSTANTS AND TYPES
// ============================================================================

const (
	defaultHTTPPort  = "80"
	defaultHTTPSPort = "443"
)

// caddyAdminURL is the Caddy admin API base URL. Under docker-compose the API
// container reaches Caddy by service name (CADDY_ADMIN_URL=http://caddy:2019);
// "localhost" there would be the API container itself.
func caddyAdminURL() string {
	return strings.TrimRight(envOrDefault("CADDY_ADMIN_URL", "http://localhost:2019"), "/")
}

// appsBaseDomain is the domain every app is served under as
// "<appName>.<domain>". "localhost" only resolves on the server itself; set
// GRAVYFLOW_APPS_DOMAIN to a wildcard DNS domain (*.apps.example.com) to make
// apps reachable from outside.
func appsBaseDomain() string {
	return strings.Trim(strings.ToLower(envOrDefault("GRAVYFLOW_APPS_DOMAIN", "localhost")), ".")
}

func appsURLScheme() string {
	return envOrDefault("GRAVYFLOW_APPS_URL_SCHEME", "http")
}

// appsNetworkName is the Docker network app containers and Caddy share. It is
// deliberately not gravyflow-network, so user apps can't reach Postgres,
// Redis or the API.
func appsNetworkName() string {
	return envOrDefault("GRAVYFLOW_APPS_NETWORK", "gravyflow-apps")
}

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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, caddyAdminURL()+"/config/", nil)
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
// ROUTE MANAGEMENT
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
	hosts := []string{fmt.Sprintf("%s.%s", container.ContainerName, appsBaseDomain())}
	
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
// CADDY SYNC
// ============================================================================

func (rm *RouteManager) syncToCaddy(ctx context.Context) error {
	return rm.syncWithRetry(ctx, func() error {
		return rm.syncToCaddyInternal(ctx)
	})
}

func (rm *RouteManager) syncToCaddyInternal(ctx context.Context) error {
	routes := rm.buildRouteConfig()
	
	for _, route := range routes {
		if err := rm.validateRoute(route); err != nil {
			return fmt.Errorf("invalid route: %w", err)
		}
	}
	
	payload := rm.buildCaddyPayload(ctx, routes)
	
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal caddy payload: %w", err)
	}
	
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, caddyAdminURL()+"/load", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create caddy request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	
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

func (rm *RouteManager) buildCaddyPayload(ctx context.Context, routes []map[string]any) map[string]any {
	servers := map[string]any{}

	if rm.config.EnableTLS {
		// TLS on: the real proxy routes move to the HTTPS listener (with
		// cert automation), and a second server on the HTTP port either
		// redirects to HTTPS (force_https deployments) or proxies plainly
		// (everyone else) — see buildHTTPPortRoutes. A single shared server
		// on both ports can't distinguish "came in over HTTP" from "came in
		// over HTTPS" with a plain host matcher, hence the split.
		servers["gravyflow"] = map[string]any{
			"listen": []string{fmt.Sprintf(":%s", rm.config.HTTPSPort)},
			"routes": routes,
			"tls": map[string]any{
				"automation": map[string]any{
					"policy": "acme",
					"email":  rm.config.TLSEmail,
					"ca":     rm.config.TLSAcmeCA,
				},
			},
		}
		servers["gravyflow-http"] = map[string]any{
			"listen": []string{fmt.Sprintf(":%s", rm.config.HTTPPort)},
			"routes": rm.buildHTTPPortRoutes(ctx, routes),
		}
	} else {
		// TLS off (default/dev): one plain HTTP server, unchanged from
		// before force_https existed. force_https has nothing to redirect
		// to without a real HTTPS listener, so it's a no-op until
		// CADDY_ENABLE_TLS is set.
		servers["gravyflow"] = map[string]any{
			"listen": []string{fmt.Sprintf(":%s", rm.config.HTTPPort)},
			"routes": routes,
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
				"servers": servers,
			},
		},
	}
}

// buildHTTPPortRoutes is only used when EnableTLS is on. Hosts whose
// deployment has force_https enabled get a 308 redirect to HTTPS ahead of
// the plain proxy routes; every other host keeps working over plain HTTP.
func (rm *RouteManager) buildHTTPPortRoutes(ctx context.Context, proxyRoutes []map[string]any) []map[string]any {
	forceHTTPS := rm.loadForceHTTPSFlags(ctx)
	if len(forceHTTPS) == 0 {
		return proxyRoutes
	}

	redirectRoutes := make([]map[string]any, 0, len(rm.routes))
	for _, route := range rm.routes {
		if !forceHTTPS[route.DeploymentID] {
			continue
		}
		redirectRoutes = append(redirectRoutes, map[string]any{
			"match": []any{
				map[string]any{"host": route.Hosts},
			},
			"handle": []any{
				map[string]any{
					"handler":     "static_response",
					"status_code": 308,
					"headers": map[string]any{
						"Location": []string{"https://{http.request.host}{http.request.uri}"},
					},
				},
			},
			"terminal": true,
		})
	}
	// Redirect routes are matched first (terminal), so forced hosts never
	// reach the plain-proxy entries appended after them; non-forced hosts
	// fall through to those same entries as before.
	return append(redirectRoutes, proxyRoutes...)
}

func (rm *RouteManager) loadForceHTTPSFlags(ctx context.Context) map[string]bool {
	if deploymentStore == nil {
		return nil
	}
	deploymentIDs := make([]string, 0, len(rm.routes))
	seen := make(map[string]bool, len(rm.routes))
	for _, route := range rm.routes {
		if route.DeploymentID != "" && !seen[route.DeploymentID] {
			seen[route.DeploymentID] = true
			deploymentIDs = append(deploymentIDs, route.DeploymentID)
		}
	}
	if len(deploymentIDs) == 0 {
		return nil
	}
	flags, err := deploymentStore.GetForceHTTPSByDeploymentIDs(ctx, deploymentIDs)
	if err != nil {
		log.Printf("caddy: failed to load force-https settings, no HTTP->HTTPS redirects will be added: %v", err)
		return nil
	}
	return flags
}

// ============================================================================
// SYNC WITH RETRY
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
		
		delay = time.Duration(float64(delay) * config.Factor)
		if delay > config.MaxDelay {
			delay = config.MaxDelay
		}
	}
	
	return fmt.Errorf("sync failed after %d attempts", maxAttempts)
}

// ============================================================================
// ROUTE VALIDATION
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
	
	for _, ch := range host {
		if !((ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || 
			 ch == '.' || ch == '-' || ch == '_') {
			return false
		}
	}
	
	return true
}

// ============================================================================
// CONFIGURATION BACKUP
// ============================================================================

// backupCaddyConfig snapshots the live config before each load. Opt-in via
// CADDY_BACKUP_DIR: it writes one file per sync (i.e. per deploy) and never
// prunes them.
func (rm *RouteManager) backupCaddyConfig(ctx context.Context) error {
	backupDir := rm.config.BackupDir
	if backupDir == "" {
		return nil
	}
	
	if err := os.MkdirAll(backupDir, 0755); err != nil {
		return err
	}
	
	timestamp := time.Now().Format("20060102-150405")
	backupFile := filepath.Join(backupDir, fmt.Sprintf("caddy-config-%s.json", timestamp))
	
	req, err := http.NewRequestWithContext(ctx, "GET", caddyAdminURL()+"/config/", nil)
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
// HELPER FUNCTIONS
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
// SYNC FUNCTIONS
// ============================================================================

func init() {
	config := CaddyConfig{
		HTTPPort:  envOrDefault("CADDY_HTTP_PORT", defaultHTTPPort),
		HTTPSPort: envOrDefault("CADDY_HTTPS_PORT", defaultHTTPSPort),
		// Off by default: real Let's Encrypt issuance needs this server to
		// be publicly reachable on the HTTP/HTTPS ports above (ACME
		// HTTP-01/TLS-ALPN-01 challenges), which a local dev box isn't. Set
		// CADDY_ENABLE_TLS=true (plus CADDY_TLS_EMAIL) once deployed
		// somewhere with a public IP/domain.
		EnableTLS: strings.EqualFold(strings.TrimSpace(os.Getenv("CADDY_ENABLE_TLS")), "true"),
		TLSEmail:  strings.TrimSpace(os.Getenv("CADDY_TLS_EMAIL")),
		TLSAcmeCA: envOrDefault("CADDY_TLS_ACME_CA", "https://acme-v02.api.letsencrypt.org/directory"),
		// Off: most apps have no /health endpoint and aren't ready the
		// instant their container starts, so probing here kept routes from
		// ever being added. Container liveness is Docker's restart policy's job.
		HealthCheck: false,
		BackupDir:   strings.TrimSpace(os.Getenv("CADDY_BACKUP_DIR")),
	}
	defaultRouteManager = NewRouteManager(config)
}

// caddyTLSEnabled reports whether this server is configured to obtain real
// TLS certificates, so domain_handlers.go can report a domain as
// "ssl_provisioning" rather than immediately "active" once it's reachable.
func caddyTLSEnabled() bool {
	if defaultRouteManager == nil {
		return false
	}
	defaultRouteManager.mu.RLock()
	defer defaultRouteManager.mu.RUnlock()
	return defaultRouteManager.config.EnableTLS
}

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
	
	return defaultRouteManager.ReplaceRoutes(ctx, containers)
}

// ReplaceRoutes makes the routing table exactly the given running containers
// and pushes it to Caddy once. Calling AddRoute per container instead re-posts
// the full config N times per deploy and never drops routes for containers
// that have since stopped.
func (rm *RouteManager) ReplaceRoutes(ctx context.Context, containers []ContainerInfo) error {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	
	now := time.Now()
	routes := make(map[string]Route, len(containers))
	for _, container := range containers {
		if container.ContainerName == "" || container.InternalIP == "" || container.InternalPort == "" {
			continue
		}
		createdAt := now
		if existing, ok := rm.routes[container.ContainerName]; ok {
			createdAt = existing.CreatedAt
		}
		routes[container.ContainerName] = Route{
			ContainerName: container.ContainerName,
			DeploymentID:  container.DeploymentID,
			Target:        fmt.Sprintf("%s:%s", container.InternalIP, container.InternalPort),
			Hosts:         rm.buildHosts(container),
			CreatedAt:     createdAt,
			UpdatedAt:     now,
		}
	}
	rm.routes = routes
	return rm.syncToCaddy(ctx)
}

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
		if !certificatePathMatchesDomain(path, customDomain) {
			return nil
		}
		if entry.IsDir() {
			return os.RemoveAll(path)
		}
		_ = os.Remove(path)
		return nil
	})
}

// certificatePathMatchesDomain reports whether path belongs exclusively to
// domain, using exact path-component comparison rather than a substring
// match. A substring match would (for example) treat "a.com" as matching a
// path for "banana.com", causing cross-tenant certificate deletion.
func certificatePathMatchesDomain(path string, domain string) bool {
	if domain == "" {
		return false
	}
	for _, part := range strings.Split(strings.ToLower(path), string(filepath.Separator)) {
		if part == domain {
			return true
		}
		// Certificate files are typically named "<domain>.crt", "<domain>.key",
		// "<domain>.json", etc. - compare the exact name with its extension
		// stripped so "banana.com.crt" doesn't match domain "a.com".
		if ext := filepath.Ext(part); ext != "" && strings.TrimSuffix(part, ext) == domain {
			return true
		}
	}
	return false
}

func normalizeCustomDomain(domain string) string {
	return strings.ToLower(strings.TrimSpace(domain))
}

// ============================================================================
// CONTAINER DISCOVERY
// ============================================================================

// ListRunningManagedContainers returns every currently-running container that
// this application manages (i.e. tagged with the gravyflow.managed-by label),
// so that Caddy routes can be synced to reflect what's actually deployed.
func ListRunningManagedContainers() ([]ContainerInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dockerClient, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("create docker client: %w", err)
	}
	defer dockerClient.Close()

	args := filters.NewArgs(
		filters.Arg("label", managedByLabelKey+"="+managedByLabelValue),
		filters.Arg("status", "running"),
	)

	containers, err := dockerClient.ContainerList(ctx, container.ListOptions{
		Filters: args,
	})
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}

	result := make([]ContainerInfo, 0, len(containers))
	for _, c := range containers {
		name := strings.TrimPrefix(firstOrEmpty(c.Names), "/")
		if appName := c.Labels["gravyflow.app-name"]; appName != "" {
			name = appName
		}

		// Prefer the address on the apps network: that's the one Caddy can
		// reach. Fall back to any address for containers created before
		// apps were attached to it.
		internalIP := ""
		if c.NetworkSettings != nil {
			if ep := c.NetworkSettings.Networks[appsNetworkName()]; ep != nil && ep.IPAddress != "" {
				internalIP = ep.IPAddress
			}
			for _, ep := range c.NetworkSettings.Networks {
				if internalIP == "" && ep != nil && ep.IPAddress != "" {
					internalIP = ep.IPAddress
				}
			}
		}

		result = append(result, ContainerInfo{
			ContainerName: name,
			InternalIP:    internalIP,
			InternalPort:  c.Labels["gravyflow.internal-port"],
			DeploymentID:  c.Labels["gravyflow.deployment-id"],
			Labels:        c.Labels,
			HealthStatus:  c.Status,
			StartedAt:     time.Unix(c.Created, 0),
		})
	}

	return result, nil
}

func firstOrEmpty(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}
// ============================================================================
// ROUTE RECONCILER
// ============================================================================

const defaultCaddyReconcileInterval = 30 * time.Second

// RunCaddyRouteReconciler keeps Caddy's routes in step with the running app
// containers. Routes are otherwise only pushed when this API starts a
// container or changes a domain, so two things silently break every app:
//   - Caddy restarts and reloads its bare Caddyfile, dropping all routes, and
//   - Docker's restart policy brings a crashed app back on a new IP, leaving
//     its route pointing at the old one.
// It pushes once at startup, then again whenever either drift is seen.
func RunCaddyRouteReconciler(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = defaultCaddyReconcileInterval
	}
	reconcile := func(force bool) {
		containers, err := ListRunningManagedContainers()
		if err != nil {
			log.Printf("caddy reconcile: list containers: %v", err)
			return
		}
		if !force && !defaultRouteManager.routesDrifted(ctx, containers) {
			return
		}
		syncCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if err := defaultRouteManager.ReplaceRoutes(syncCtx, containers); err != nil {
			log.Printf("caddy reconcile: sync failed: %v", err)
			return
		}
		log.Printf("caddy reconcile: pushed %d app route(s)", len(containers))
	}

	reconcile(true)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			reconcile(false)
		}
	}
}

// routesDrifted reports whether the running containers' targets differ from
// the routes last pushed, or Caddy no longer holds our server (it restarted).
func (rm *RouteManager) routesDrifted(ctx context.Context, containers []ContainerInfo) bool {
	rm.mu.RLock()
	want := 0
	for _, c := range containers {
		if c.ContainerName == "" || c.InternalIP == "" || c.InternalPort == "" {
			continue
		}
		want++
		r, ok := rm.routes[c.ContainerName]
		if !ok || r.Target != fmt.Sprintf("%s:%s", c.InternalIP, c.InternalPort) {
			rm.mu.RUnlock()
			return true
		}
	}
	drifted := want != len(rm.routes)
	rm.mu.RUnlock()
	if drifted {
		return true
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, caddyAdminURL()+"/config/apps/http/servers/gravyflow", nil)
	if err != nil {
		return false
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		// Caddy unreachable: nothing to push to yet; retry next tick.
		return false
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	// Caddy answers "null" for a path that doesn't exist.
	return resp.StatusCode != http.StatusOK || strings.TrimSpace(string(body)) == "null"
}
