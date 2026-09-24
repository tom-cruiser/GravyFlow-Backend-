package main

import (
	"bufio"
	"context"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/client"
	"github.com/gin-gonic/gin"
	"github.com/hibiken/asynq"
)

// ============================================================================
// System Health — one admin view of every dependency the API relies on
// (Postgres, Redis, the asynq job queue, Docker, Caddy, the GitHub App) plus
// the Go process and the host it runs on. Unlike the public /api/health,
// which only answers healthy/unhealthy for load balancers, this reports
// latencies and the numbers an SRE needs to tell *why* something is slow.
// ============================================================================

const (
	componentHealthy       = "healthy"
	componentDegraded      = "degraded"
	componentUnhealthy     = "unhealthy"
	componentNotConfigured = "not_configured"

	systemCheckTimeout = 5 * time.Second
	// A dependency that answers but takes longer than this is reported as
	// degraded rather than healthy.
	slowDependencyThreshold = 500 * time.Millisecond
)

var processStartedAt = time.Now()

type ComponentHealth struct {
	Name      string         `json:"name"`
	Status    string         `json:"status"`
	LatencyMs float64        `json:"latencyMs"`
	Error     string         `json:"error,omitempty"`
	Details   map[string]any `json:"details,omitempty"`
}

type ProcessInfo struct {
	PID           int     `json:"pid"`
	GoVersion     string  `json:"goVersion"`
	Version       string  `json:"version"`
	StartedAt     string  `json:"startedAt"`
	UptimeSeconds float64 `json:"uptimeSeconds"`
	Goroutines    int     `json:"goroutines"`
	HeapAllocB    uint64  `json:"heapAllocBytes"`
	HeapSysB      uint64  `json:"heapSysBytes"`
	SysB          uint64  `json:"sysBytes"`
	NumGC         uint32  `json:"numGc"`
	LastGCPauseUs float64 `json:"lastGcPauseUs"`
}

type HostInfo struct {
	Hostname         string    `json:"hostname"`
	OS               string    `json:"os"`
	Kernel           string    `json:"kernel"`
	Arch             string    `json:"arch"`
	CPUCount         int       `json:"cpuCount"`
	CPUUsagePercent  *float64  `json:"cpuUsagePercent,omitempty"`
	LoadAverage      []float64 `json:"loadAverage,omitempty"`
	MemoryTotalB     uint64    `json:"memoryTotalBytes"`
	MemoryAvailableB uint64    `json:"memoryAvailableBytes"`
	DiskPath         string    `json:"diskPath"`
	DiskTotalB       uint64    `json:"diskTotalBytes"`
	DiskFreeB        uint64    `json:"diskFreeBytes"`
	UptimeSeconds    float64   `json:"uptimeSeconds"`
	// True when the API runs inside a container: the host numbers above are
	// then the container's view (/proc is shared with the host for load and
	// uptime, but memory/disk reflect the container). The Docker component's
	// details carry the real host's CPU/memory as the daemon reports them.
	Containerized bool `json:"containerized"`
}

type SystemHealthReport struct {
	Status     string            `json:"status"`
	CheckedAt  time.Time         `json:"checkedAt"`
	Components []ComponentHealth `json:"components"`
	Process    ProcessInfo       `json:"process"`
	Host       HostInfo          `json:"host"`
}

// ============================================================================
// HANDLER
// ============================================================================

func adminSystemHealthHandler(c *gin.Context) {
	ctx := c.Request.Context()

	checks := []func(context.Context) ComponentHealth{
		checkPostgresComponent,
		checkRedisComponent,
		checkJobQueueComponent,
		checkDockerComponent,
		checkCaddyComponent,
		checkGitHubAppComponent,
	}
	components := make([]ComponentHealth, len(checks))
	var wg sync.WaitGroup
	for i, check := range checks {
		wg.Add(1)
		go func(i int, check func(context.Context) ComponentHealth) {
			defer wg.Done()
			checkCtx, cancel := context.WithTimeout(ctx, systemCheckTimeout)
			defer cancel()
			components[i] = check(checkCtx)
		}(i, check)
	}

	// Host CPU usage needs two /proc/stat samples; take them while the
	// dependency checks are in flight rather than adding to the latency.
	host := collectHostInfo()
	wg.Wait()

	c.JSON(http.StatusOK, SystemHealthReport{
		Status:     overallStatus(components),
		CheckedAt:  time.Now().UTC(),
		Components: components,
		Process:    collectProcessInfo(),
		Host:       host,
	})
}

// overallStatus: any hard dependency down → unhealthy; anything slow or down
// that isn't a hard dependency → degraded. GitHub App and Caddy being
// unconfigured is a valid setup (local dev), so it doesn't count.
func overallStatus(components []ComponentHealth) string {
	status := componentHealthy
	for _, comp := range components {
		switch comp.Status {
		case componentUnhealthy:
			if comp.Name == "PostgreSQL" || comp.Name == "Redis" || comp.Name == "Docker" {
				return componentUnhealthy
			}
			status = componentDegraded
		case componentDegraded:
			status = componentDegraded
		}
	}
	return status
}

func timedStatus(latency time.Duration) string {
	if latency > slowDependencyThreshold {
		return componentDegraded
	}
	return componentHealthy
}

func msSince(start time.Time) float64 {
	return float64(time.Since(start).Microseconds()) / 1000
}

// ============================================================================
// DEPENDENCY CHECKS
// ============================================================================

func checkPostgresComponent(ctx context.Context) ComponentHealth {
	comp := ComponentHealth{Name: "PostgreSQL"}
	if deploymentStore == nil || deploymentStore.pool == nil {
		comp.Status = componentUnhealthy
		comp.Error = "deployment store is not initialized"
		return comp
	}

	start := time.Now()
	if err := deploymentStore.pool.Ping(ctx); err != nil {
		comp.Status = componentUnhealthy
		comp.LatencyMs = msSince(start)
		comp.Error = err.Error()
		return comp
	}
	latency := time.Since(start)
	comp.LatencyMs = float64(latency.Microseconds()) / 1000
	comp.Status = timedStatus(latency)

	pool := deploymentStore.GetPoolStats()
	details := map[string]any{
		"poolTotalConns":    pool.TotalConnections,
		"poolIdleConns":     pool.IdleConnections,
		"poolAcquiredConns": pool.ActiveConnections,
		"poolMaxConns":      pool.MaxConnections,
		"poolAcquireCount":  pool.AcquireCount,
	}

	var (
		version        string
		dbName         string
		dbSizeBytes    int64
		serverConns    int
		maxConnections string
		startedAt      time.Time
	)
	err := deploymentStore.pool.QueryRow(ctx, `
SELECT
	current_setting('server_version'),
	current_database(),
	pg_database_size(current_database()),
	(SELECT COUNT(*) FROM pg_stat_activity WHERE datname = current_database()),
	current_setting('max_connections'),
	pg_postmaster_start_time()
`).Scan(&version, &dbName, &dbSizeBytes, &serverConns, &maxConnections, &startedAt)
	if err != nil {
		// Ping worked, so the DB is reachable; the stats query needing
		// privileges we don't have shouldn't flip the status.
		details["statsError"] = err.Error()
	} else {
		details["version"] = version
		details["database"] = dbName
		details["sizeBytes"] = dbSizeBytes
		details["serverConnections"] = serverConns
		details["maxConnections"] = maxConnections
		details["serverStartedAt"] = startedAt.UTC()
	}

	var migrationVersion *int
	if err := deploymentStore.pool.QueryRow(ctx, `SELECT MAX(version) FROM schema_migrations`).Scan(&migrationVersion); err == nil && migrationVersion != nil {
		details["schemaVersion"] = *migrationVersion
	}

	comp.Details = details
	return comp
}

func checkRedisComponent(ctx context.Context) ComponentHealth {
	comp := ComponentHealth{Name: "Redis"}
	if deploymentJobs == nil || deploymentJobs.redisClient == nil {
		comp.Status = componentUnhealthy
		comp.Error = "job manager is not initialized"
		return comp
	}

	start := time.Now()
	if err := deploymentJobs.redisClient.Ping(ctx).Err(); err != nil {
		comp.Status = componentUnhealthy
		comp.LatencyMs = msSince(start)
		comp.Error = err.Error()
		return comp
	}
	latency := time.Since(start)
	comp.LatencyMs = float64(latency.Microseconds()) / 1000
	comp.Status = timedStatus(latency)

	details := map[string]any{}
	if info, err := deploymentJobs.redisClient.Info(ctx, "server", "clients", "memory", "stats").Result(); err == nil {
		fields := parseRedisInfo(info)
		for _, key := range []string{"redis_version", "uptime_in_seconds", "connected_clients", "blocked_clients", "used_memory", "used_memory_peak", "maxmemory", "total_commands_processed", "instantaneous_ops_per_sec", "keyspace_hits", "keyspace_misses", "evicted_keys"} {
			if v, ok := fields[key]; ok {
				if n, err := strconv.ParseInt(v, 10, 64); err == nil {
					details[key] = n
				} else {
					details[key] = v
				}
			}
		}
	} else {
		details["infoError"] = err.Error()
	}
	comp.Details = details
	return comp
}

func parseRedisInfo(info string) map[string]string {
	fields := make(map[string]string)
	scanner := bufio.NewScanner(strings.NewReader(info))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if key, value, ok := strings.Cut(line, ":"); ok {
			fields[key] = value
		}
	}
	return fields
}

var (
	asynqInspectorOnce sync.Once
	asynqInspector     *asynq.Inspector
	asynqInspectorErr  error
)

func sharedAsynqInspector() (*asynq.Inspector, error) {
	asynqInspectorOnce.Do(func() {
		opt, err := buildAsynqRedisClientOpt()
		if err != nil {
			asynqInspectorErr = err
			return
		}
		asynqInspector = asynq.NewInspector(opt)
	})
	return asynqInspector, asynqInspectorErr
}

func checkJobQueueComponent(ctx context.Context) ComponentHealth {
	comp := ComponentHealth{Name: "Job queue"}
	inspector, err := sharedAsynqInspector()
	if err != nil {
		comp.Status = componentUnhealthy
		comp.Error = err.Error()
		return comp
	}

	start := time.Now()
	// asynq's Inspector has no context-aware variant; bound it ourselves.
	type result struct {
		info    *asynq.QueueInfo
		servers []*asynq.ServerInfo
		err     error
	}
	done := make(chan result, 1)
	go func() {
		info, err := inspector.GetQueueInfo(deploymentJobQueueName)
		servers, _ := inspector.Servers()
		done <- result{info, servers, err}
	}()

	var res result
	select {
	case res = <-done:
	case <-ctx.Done():
		comp.Status = componentUnhealthy
		comp.LatencyMs = msSince(start)
		comp.Error = "timed out querying queue"
		return comp
	}
	comp.LatencyMs = msSince(start)

	workers := 0
	for _, srv := range res.servers {
		if srv.Status == "active" {
			workers++
		}
	}

	if res.err != nil {
		// asynq returns an error for a queue that has never had a task
		// enqueued; that's an empty queue, not an outage.
		if strings.Contains(strings.ToLower(res.err.Error()), "does not exist") {
			comp.Status = componentHealthy
			comp.Details = map[string]any{"queue": deploymentJobQueueName, "size": 0, "workers": workers}
			if workers == 0 {
				comp.Status = componentDegraded
				comp.Error = "no active workers are consuming the queue"
			}
			return comp
		}
		comp.Status = componentUnhealthy
		comp.Error = res.err.Error()
		return comp
	}

	info := res.info
	comp.Status = componentHealthy
	comp.Details = map[string]any{
		"queue":          info.Queue,
		"size":           info.Size,
		"pending":        info.Pending,
		"active":         info.Active,
		"scheduled":      info.Scheduled,
		"retry":          info.Retry,
		"archived":       info.Archived,
		"processedToday": info.Processed,
		"failedToday":    info.Failed,
		"processedTotal": info.ProcessedTotal,
		"failedTotal":    info.FailedTotal,
		"latencyMs":      info.Latency.Milliseconds(),
		"paused":         info.Paused,
		"workers":        workers,
	}
	switch {
	case info.Paused:
		comp.Status = componentDegraded
		comp.Error = "queue is paused"
	case workers == 0:
		comp.Status = componentDegraded
		comp.Error = "no active workers are consuming the queue"
	case info.Latency > 5*time.Minute:
		comp.Status = componentDegraded
		comp.Error = "oldest pending job has waited over 5 minutes"
	}
	return comp
}

func checkDockerComponent(ctx context.Context) ComponentHealth {
	comp := ComponentHealth{Name: "Docker"}
	dockerClient, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		comp.Status = componentUnhealthy
		comp.Error = err.Error()
		return comp
	}
	defer dockerClient.Close()

	start := time.Now()
	ping, err := dockerClient.Ping(ctx)
	if err != nil {
		comp.Status = componentUnhealthy
		comp.LatencyMs = msSince(start)
		comp.Error = err.Error()
		return comp
	}
	latency := time.Since(start)
	comp.LatencyMs = float64(latency.Microseconds()) / 1000
	comp.Status = timedStatus(latency)

	details := map[string]any{"apiVersion": ping.APIVersion}
	if info, err := dockerClient.Info(ctx); err == nil {
		details["serverVersion"] = info.ServerVersion
		details["containers"] = info.Containers
		details["containersRunning"] = info.ContainersRunning
		details["containersStopped"] = info.ContainersStopped
		details["containersPaused"] = info.ContainersPaused
		details["images"] = info.Images
		details["storageDriver"] = info.Driver
		details["hostOs"] = info.OperatingSystem
		details["hostKernel"] = info.KernelVersion
		details["hostArch"] = info.Architecture
		details["hostCpus"] = info.NCPU
		details["hostMemoryBytes"] = info.MemTotal
		if len(info.Warnings) > 0 {
			details["warnings"] = info.Warnings
		}
	} else {
		details["infoError"] = err.Error()
	}
	comp.Details = details
	return comp
}

func checkCaddyComponent(ctx context.Context) ComponentHealth {
	comp := ComponentHealth{Name: "Caddy"}
	details := map[string]any{"adminUrl": caddyAdminURL(), "required": caddySyncRequired()}
	if defaultRouteManager != nil {
		details["routes"] = len(defaultRouteManager.GetRoutes())
	}
	comp.Details = details

	start := time.Now()
	ok := caddyAdminHealthy(ctx)
	latency := time.Since(start)
	comp.LatencyMs = float64(latency.Microseconds()) / 1000
	switch {
	case ok:
		comp.Status = timedStatus(latency)
	case caddySyncRequired():
		comp.Status = componentUnhealthy
		comp.Error = "Caddy admin API is unreachable"
	default:
		// Without GRAVYFLOW_REQUIRE_CADDY deployments still run; they just
		// aren't reachable through the proxy.
		comp.Status = componentDegraded
		comp.Error = "Caddy admin API is unreachable — app routing is not being synced"
	}
	return comp
}

func checkGitHubAppComponent(ctx context.Context) ComponentHealth {
	comp := ComponentHealth{Name: "GitHub App"}
	cfg, err := loadGitHubAppConfig()
	if err != nil {
		comp.Status = componentNotConfigured
		comp.Error = err.Error()
		return comp
	}
	comp.Status = componentHealthy
	comp.Details = map[string]any{"appId": cfg.AppID, "slug": cfg.Slug}
	return comp
}

// ============================================================================
// PROCESS & HOST
// ============================================================================

func collectProcessInfo() ProcessInfo {
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	lastPause := float64(0)
	if mem.NumGC > 0 {
		lastPause = float64(mem.PauseNs[(mem.NumGC+255)%256]) / 1000
	}
	return ProcessInfo{
		PID:           os.Getpid(),
		GoVersion:     runtime.Version(),
		Version:       "1.0.0",
		StartedAt:     processStartedAt.UTC().Format(time.RFC3339),
		UptimeSeconds: time.Since(processStartedAt).Seconds(),
		Goroutines:    runtime.NumGoroutine(),
		HeapAllocB:    mem.HeapAlloc,
		HeapSysB:      mem.HeapSys,
		SysB:          mem.Sys,
		NumGC:         mem.NumGC,
		LastGCPauseUs: lastPause,
	}
}

func collectHostInfo() HostInfo {
	host := HostInfo{
		OS:       runtime.GOOS,
		Arch:     runtime.GOARCH,
		CPUCount: runtime.NumCPU(),
		DiskPath: "/",
	}
	host.Hostname, _ = os.Hostname()

	if data, err := os.ReadFile("/etc/os-release"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if v, ok := strings.CutPrefix(line, "PRETTY_NAME="); ok {
				host.OS = strings.Trim(v, `"`)
				break
			}
		}
	}
	if data, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		host.Kernel = strings.TrimSpace(string(data))
	}
	if data, err := os.ReadFile("/proc/loadavg"); err == nil {
		fields := strings.Fields(string(data))
		for i := 0; i < 3 && i < len(fields); i++ {
			f := fields[i]
			if v, err := strconv.ParseFloat(f, 64); err == nil {
				host.LoadAverage = append(host.LoadAverage, v)
			}
		}
	}
	if data, err := os.ReadFile("/proc/uptime"); err == nil {
		if fields := strings.Fields(string(data)); len(fields) > 0 {
			host.UptimeSeconds, _ = strconv.ParseFloat(fields[0], 64)
		}
	}
	host.MemoryTotalB, host.MemoryAvailableB = readMeminfo()
	host.DiskTotalB, host.DiskFreeB = diskUsage(host.DiskPath)
	if _, err := os.Stat("/.dockerenv"); err == nil {
		host.Containerized = true
	}
	host.CPUUsagePercent = sampleCPUUsage(250 * time.Millisecond)
	return host
}

func readMeminfo() (total, available uint64) {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 2 {
			continue
		}
		kb, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			total = kb * 1024
		case "MemAvailable:":
			available = kb * 1024
		}
	}
	return total, available
}

// readCPUTimes returns (idle, total) jiffies from the aggregate "cpu" line.
func readCPUTimes() (idle, total uint64, ok bool) {
	data, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, 0, false
	}
	line, _, _ := strings.Cut(string(data), "\n")
	fields := strings.Fields(line)
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0, 0, false
	}
	for i, f := range fields[1:] {
		v, err := strconv.ParseUint(f, 10, 64)
		if err != nil {
			return 0, 0, false
		}
		total += v
		if i == 3 || i == 4 { // idle + iowait
			idle += v
		}
	}
	return idle, total, true
}

func sampleCPUUsage(window time.Duration) *float64 {
	idle1, total1, ok := readCPUTimes()
	if !ok {
		return nil
	}
	time.Sleep(window)
	idle2, total2, ok := readCPUTimes()
	if !ok || total2 <= total1 {
		return nil
	}
	usage := 100 * (1 - float64(idle2-idle1)/float64(total2-total1))
	return &usage
}
