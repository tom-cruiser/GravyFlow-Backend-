package main

import (
    "bytes"
    "context"
    "encoding/json"
    "fmt"
    "io"
    "log"
    "math"
    "sort"
    "strconv"
    "strings"
    "time"

    "github.com/docker/docker/api/types"
    "github.com/docker/docker/api/types/container"
    "github.com/docker/docker/api/types/filters"
    "github.com/docker/docker/api/types/image"
    "github.com/docker/docker/api/types/events"
    "github.com/docker/docker/api/types/network"
    "github.com/docker/docker/api/types/volume"
    "github.com/docker/docker/client"
    "github.com/docker/docker/errdefs"
    "github.com/docker/go-connections/nat"
)

// ============================================================================
// NEW TYPES
// ============================================================================

type HealthCheckConfig struct {
    Path     string        `json:"path"`
    Interval time.Duration `json:"interval"`
    Timeout  time.Duration `json:"timeout"`
    Retries  int           `json:"retries"`
}

type ContainerLogOptions struct {
    ShowStdout bool      `json:"showStdout"`
    ShowStderr bool      `json:"showStderr"`
    Tail       int       `json:"tail"`
    Since      time.Time `json:"since"`
}

type ContainerStats struct {
    ContainerID   string    `json:"containerId"`
    CPUUsage      float64   `json:"cpuUsage"`
    MemoryUsage   float64   `json:"memoryUsage"`
    MemoryLimit   float64   `json:"memoryLimit"`
    NetworkIn     int64     `json:"networkIn"`
    NetworkOut    int64     `json:"networkOut"`
    BlockRead     int64     `json:"blockRead"`
    BlockWrite    int64     `json:"blockWrite"`
    PIDs          int       `json:"pids"`
    Timestamp     time.Time `json:"timestamp"`
}

type NetworkConfig struct {
    Name    string            `json:"name"`
    Driver  string            `json:"driver"`
    Subnet  string            `json:"subnet"`
    Gateway string            `json:"gateway"`
    IPRange string            `json:"ipRange"`
    Labels  map[string]string `json:"labels"`
}

type CleanupPolicy struct {
    MaxAge        time.Duration `json:"maxAge"`
    MaxCount      int           `json:"maxCount"`
    StopBefore    bool          `json:"stopBefore"`
    RemoveVolumes bool          `json:"removeVolumes"`
}

type ContainerEvent struct {
    Type       string            `json:"type"`
    Action     string            `json:"action"`
    ActorID    string            `json:"actorId"`
    Attributes map[string]string `json:"attributes"`
    Timestamp  time.Time         `json:"timestamp"`
}

type EventHandler func(event ContainerEvent) error

type VolumeConfig struct {
    Name    string            `json:"name"`
    Driver  string            `json:"driver"`
    Labels  map[string]string `json:"labels"`
    Options map[string]string `json:"options"`
}

type BackupConfig struct {
    Source      string   `json:"source"`
    Destination string   `json:"destination"`
    Compress    bool     `json:"compress"`
    Exclude     []string `json:"exclude"`
}

type ImageManager struct {
    client *client.Client
}

const (
    managedByLabelKey   = "gravyflow.managed-by"
    managedByLabelValue = "gravyflow"
)

type ConflictError struct {
    Resource string
    Value    string
}

func (e *ConflictError) Error() string {
    return fmt.Sprintf("%s %q already exists", e.Resource, e.Value)
}

func parsePortMap(portMap string) (string, string, error) {
    portMap = strings.TrimSpace(portMap)
    if portMap == "" {
        return "", "", fmt.Errorf("portMap is required")
    }

    parts := strings.Split(portMap, ":")
    switch len(parts) {
    case 1:
        port := strings.TrimSpace(strings.TrimSuffix(parts[0], "/tcp"))
        if _, err := strconv.Atoi(port); err != nil {
            return "", "", fmt.Errorf("invalid port map %q", portMap)
        }
        return port, port, nil
    case 2:
        hostPort := strings.TrimSpace(strings.TrimSuffix(parts[0], "/tcp"))
        containerPort := strings.TrimSpace(strings.TrimSuffix(parts[1], "/tcp"))
        if _, err := strconv.Atoi(hostPort); err != nil {
            return "", "", fmt.Errorf("invalid host port in %q", portMap)
        }
        if _, err := strconv.Atoi(containerPort); err != nil {
            return "", "", fmt.Errorf("invalid container port in %q", portMap)
        }
        return hostPort, containerPort, nil
    default:
        return "", "", fmt.Errorf("invalid port map %q", portMap)
    }
}

func normalizePortMap(portMap string) string {
    portMap = strings.TrimSpace(portMap)
    if portMap == "" {
        return ""
    }

    hostPort, containerPort, err := parsePortMap(portMap)
    if err != nil {
        return portMap
    }
    if hostPort == "" || hostPort == containerPort {
        return containerPort
    }
    return hostPort + ":" + containerPort
}

// ============================================================================
// ENHANCED CONTAINER CREATION WITH HEALTH CHECK
// ============================================================================

func CreateAndStartContainerWithHealthCheck(
    imageName string,
    containerName string,
    deploymentID string,
    portMap string,
    envVars []string,
    memoryMB int64,
    cpus float64,
    healthCheck HealthCheckConfig,
) (string, error) {
    imageName = strings.TrimSpace(imageName)
    containerName = strings.TrimSpace(containerName)
    deploymentID = strings.TrimSpace(deploymentID)
    portMap = strings.TrimSpace(portMap)

    if imageName == "" {
        return "", fmt.Errorf("imageName is required")
    }
    if containerName == "" {
        return "", fmt.Errorf("containerName is required")
    }
    if deploymentID == "" {
        return "", fmt.Errorf("deploymentID is required")
    }
    if portMap == "" {
        return "", fmt.Errorf("portMap is required")
    }

    hostPort, containerPort, err := parsePortMap(portMap)
    if err != nil {
        return "", err
    }
    _ = hostPort // routing uses Caddy + container internal IP

    ctx := context.Background()
    dockerClient, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
    if err != nil {
        return "", fmt.Errorf("create docker client: %w", err)
    }
    defer dockerClient.Close()

    // Pull image if not present
    if _, err := dockerClient.ImageInspect(ctx, imageName); err != nil {
        pullReader, pullErr := dockerClient.ImagePull(ctx, imageName, image.PullOptions{})
        if pullErr != nil {
            return "", fmt.Errorf("pull image %q: %w", imageName, pullErr)
        }
        defer pullReader.Close()

        if _, err := io.Copy(io.Discard, pullReader); err != nil {
            return "", fmt.Errorf("read image pull stream: %w", err)
        }
    }

    if err := removeContainerByName(ctx, dockerClient, containerName); err != nil {
        return "", err
    }

    exposedPort, err := nat.NewPort("tcp", containerPort)
    if err != nil {
        return "", fmt.Errorf("invalid container port %q: %w", containerPort, err)
    }

    containerCfg := &container.Config{
        Image: imageName,
        Env:   envVars,
        Labels: map[string]string{
            managedByLabelKey:         managedByLabelValue,
            "gravyflow.deployment-id": deploymentID,
            "gravyflow.app-name":      containerName,
            "gravyflow.internal-port": containerPort,
        },
        ExposedPorts: nat.PortSet{
            exposedPort: struct{}{},
        },
    }

    // Configure health check if provided
    if healthCheck.Path != "" {
        if healthCheck.Interval == 0 {
            healthCheck.Interval = 30 * time.Second
        }
        if healthCheck.Timeout == 0 {
            healthCheck.Timeout = 5 * time.Second
        }
        if healthCheck.Retries == 0 {
            healthCheck.Retries = 3
        }

        containerCfg.Healthcheck = &container.HealthConfig{
            Test:     []string{"CMD", "curl", "-f", fmt.Sprintf("http://localhost:%s%s", containerPort, healthCheck.Path)},
            Interval: healthCheck.Interval,
            Timeout:  healthCheck.Timeout,
            Retries:  healthCheck.Retries,
        }
    }

    hostCfg := &container.HostConfig{
        Resources: container.Resources{
            Memory:   memoryMB * 1024 * 1024,
            NanoCPUs: int64(math.Round(cpus * 1_000_000_000)),
        },
    }

    resp, err := dockerClient.ContainerCreate(ctx, containerCfg, hostCfg, &network.NetworkingConfig{}, nil, containerName)
    if err != nil {
        if errdefs.IsConflict(err) || strings.Contains(strings.ToLower(err.Error()), "is already in use") {
            return "", &ConflictError{Resource: "containerName", Value: containerName}
        }
        return "", fmt.Errorf("create container %q: %w", containerName, err)
    }

    if err := dockerClient.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
        return "", fmt.Errorf("start container %q: %w", containerName, err)
    }

    if err := SyncCaddyRoutesFromRunningContainers(); err != nil {
        log.Printf("warning: caddy sync after start failed for %q: %v (container kept running)", containerName, err)
    }

    return resp.ID, nil
}

func CreateAndStartContainer(
    imageName string,
    containerName string,
    deploymentID string,
    portMap string,
    envVars []string,
    memoryMB int64,
    cpus float64,
) (string, error) {
    return CreateAndStartContainerWithHealthCheck(imageName, containerName, deploymentID, portMap, envVars, memoryMB, cpus, HealthCheckConfig{})
}

func StopAndRemoveContainer(containerID string) error {
    containerID = strings.TrimSpace(containerID)
    if containerID == "" {
        return fmt.Errorf("containerID is required")
    }

    dockerClient, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
    if err != nil {
        return fmt.Errorf("create docker client: %w", err)
    }
    defer dockerClient.Close()

    ctx := context.Background()
    if err := dockerClient.ContainerStop(ctx, containerID, container.StopOptions{}); err != nil {
        if !strings.Contains(strings.ToLower(err.Error()), "not running") {
            return fmt.Errorf("stop container %q: %w", containerID, err)
        }
    }

    if err := dockerClient.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true}); err != nil {
        return fmt.Errorf("remove container %q: %w", containerID, err)
    }

    return nil
}

func RestartContainer(
    containerID string,
    imageName string,
    containerName string,
    deploymentID string,
    portMap string,
    envVars []string,
    memoryMB int64,
    cpus float64,
) (string, error) {
    if err := StopAndRemoveContainer(containerID); err != nil {
        return "", err
    }
    return CreateAndStartContainer(imageName, containerName, deploymentID, portMap, envVars, memoryMB, cpus)
}

func removeContainerByName(ctx context.Context, dockerClient *client.Client, containerName string) error {
    containerName = strings.TrimSpace(containerName)
    if containerName == "" {
        return nil
    }

    containers, err := dockerClient.ContainerList(ctx, container.ListOptions{All: true, Filters: filters.NewArgs(filters.Arg("name", containerName))})
    if err != nil {
        return fmt.Errorf("list containers by name %q: %w", containerName, err)
    }

    for _, c := range containers {
        if err := dockerClient.ContainerRemove(ctx, c.ID, container.RemoveOptions{Force: true}); err != nil {
            return fmt.Errorf("remove container %q: %w", containerName, err)
        }
    }

    return nil
}

// ============================================================================
// CONTAINER LOGGING
// ============================================================================

func GetContainerLogs(ctx context.Context, containerID string, opts ContainerLogOptions) (string, error) {
    dockerClient, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
    if err != nil {
        return "", fmt.Errorf("create docker client: %w", err)
    }
    defer dockerClient.Close()

    options := container.LogsOptions{
        ShowStdout: opts.ShowStdout,
        ShowStderr: opts.ShowStderr,
        Tail:       fmt.Sprintf("%d", opts.Tail),
        Timestamps: true,
    }

    if !opts.Since.IsZero() {
        options.Since = opts.Since.Format(time.RFC3339)
    }

    logs, err := dockerClient.ContainerLogs(ctx, containerID, options)
    if err != nil {
        return "", fmt.Errorf("get container logs: %w", err)
    }
    defer logs.Close()

    var buf bytes.Buffer
    if _, err := io.Copy(&buf, logs); err != nil {
        return "", fmt.Errorf("read container logs: %w", err)
    }

    return buf.String(), nil
}

// ============================================================================
// CONTAINER STATS
// ============================================================================

func GetContainerStats(ctx context.Context, containerID string) (*ContainerStats, error) {
    dockerClient, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
    if err != nil {
        return nil, fmt.Errorf("create docker client: %w", err)
    }
    defer dockerClient.Close()

    stats, err := dockerClient.ContainerStats(ctx, containerID, false)
    if err != nil {
        return nil, fmt.Errorf("get container stats: %w", err)
    }
    defer stats.Body.Close()

    var v container.StatsResponse
    if err := json.NewDecoder(stats.Body).Decode(&v); err != nil {
        return nil, fmt.Errorf("decode stats: %w", err)
    }

    // Calculate CPU usage
    cpuDelta := v.CPUStats.CPUUsage.TotalUsage - v.PreCPUStats.CPUUsage.TotalUsage
    systemDelta := v.CPUStats.SystemUsage - v.PreCPUStats.SystemUsage
    cpuPercent := 0.0
    if systemDelta > 0 && cpuDelta > 0 {
        cpuPercent = float64(cpuDelta) / float64(systemDelta) * 100.0
    }

    // Memory usage
    memoryUsage := float64(v.MemoryStats.Usage)
    memoryLimit := float64(v.MemoryStats.Limit)

    var networkIn uint64
    var networkOut uint64
    for _, networkStats := range v.Networks {
        networkIn = networkStats.RxBytes
        networkOut = networkStats.TxBytes
        break
    }

    blockRead, blockWrite := blockIOReadWrite(v.BlkioStats.IoServiceBytesRecursive)

    return &ContainerStats{
        ContainerID: containerID,
        CPUUsage:    cpuPercent,
        MemoryUsage: memoryUsage,
        MemoryLimit: memoryLimit,
        NetworkIn:   int64(networkIn),
        NetworkOut:  int64(networkOut),
        BlockRead:   blockRead,
        BlockWrite:  blockWrite,
        PIDs:        int(v.PidsStats.Current),
        Timestamp:   time.Now(),
    }, nil
}

// blockIOReadWrite sums block I/O bytes by operation type. On cgroups v2
// hosts (and in some other configurations) IoServiceBytesRecursive can be
// empty, so entries are matched by their "Op" field rather than assumed to
// be present at fixed indices; missing values default to zero.
func blockIOReadWrite(entries []container.BlkioStatEntry) (read int64, write int64) {
    for _, entry := range entries {
        switch strings.ToLower(entry.Op) {
        case "read":
            read += int64(entry.Value)
        case "write":
            write += int64(entry.Value)
        }
    }
    return read, write
}

// ============================================================================
// NETWORK MANAGEMENT
// ============================================================================

func EnsureNetwork(ctx context.Context, config NetworkConfig) (string, error) {
    dockerClient, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
    if err != nil {
        return "", fmt.Errorf("create docker client: %w", err)
    }
    defer dockerClient.Close()

    networks, err := dockerClient.NetworkList(ctx, network.ListOptions{
        Filters: filters.NewArgs(filters.Arg("name", config.Name)),
    })
    if err != nil {
        return "", fmt.Errorf("list networks: %w", err)
    }

    if len(networks) > 0 {
        return networks[0].ID, nil
    }

    createOpts := network.CreateOptions{
        Driver: config.Driver,
        Options: map[string]string{},
        Labels: config.Labels,
    }

    if config.Subnet != "" {
        createOpts.IPAM = &network.IPAM{
            Config: []network.IPAMConfig{
                {
                    Subnet:  config.Subnet,
                    Gateway: config.Gateway,
                    IPRange: config.IPRange,
                },
            },
        }
    }

    resp, err := dockerClient.NetworkCreate(ctx, config.Name, createOpts)
    if err != nil {
        return "", fmt.Errorf("create network: %w", err)
    }

    return resp.ID, nil
}

// ============================================================================
// CONTAINER CLEANUP
// ============================================================================

func CleanupOldContainers(ctx context.Context, policy CleanupPolicy) error {
    dockerClient, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
    if err != nil {
        return fmt.Errorf("create docker client: %w", err)
    }
    defer dockerClient.Close()

    args := filters.NewArgs(
        filters.Arg("label", managedByLabelKey+"="+managedByLabelValue),
        filters.Arg("status", "exited"),
    )
    containers, err := dockerClient.ContainerList(ctx, container.ListOptions{
        Filters: args,
        All:     true,
    })
    if err != nil {
        return fmt.Errorf("list containers: %w", err)
    }

    sort.Slice(containers, func(i, j int) bool {
        return containers[i].Created < containers[j].Created
    })

    var toRemove []types.Container
    now := time.Now()

    for _, c := range containers {
        created := time.Unix(c.Created, 0)
        shouldRemove := false

        if policy.MaxAge > 0 && now.Sub(created) > policy.MaxAge {
            shouldRemove = true
        }

        if policy.MaxCount > 0 && len(toRemove) >= policy.MaxCount {
            continue
        }

        if shouldRemove {
            toRemove = append(toRemove, c)
        }
    }

    for _, c := range toRemove {
        if err := dockerClient.ContainerRemove(ctx, c.ID, container.RemoveOptions{
            Force:         policy.StopBefore,
            RemoveVolumes: policy.RemoveVolumes,
        }); err != nil {
            log.Printf("Failed to remove container %s: %v", c.ID[:12], err)
        } else {
            log.Printf("Cleaned up container %s", c.ID[:12])
        }
    }

    return nil
}

// ============================================================================
// IMAGE MANAGEMENT
// ============================================================================

func NewImageManager() (*ImageManager, error) {
    dockerClient, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
    if err != nil {
        return nil, err
    }
    return &ImageManager{client: dockerClient}, nil
}

func (im *ImageManager) PruneImages(ctx context.Context, keepLatest int) error {
    images, err := im.client.ImageList(ctx, image.ListOptions{
        Filters: filters.NewArgs(
            filters.Arg("label", managedByLabelKey+"="+managedByLabelValue),
        ),
    })
    if err != nil {
        return err
    }

    imageMap := make(map[string][]image.Summary)
    for _, img := range images {
        for _, repoTag := range img.RepoTags {
            parts := strings.Split(repoTag, ":")
            if len(parts) > 1 {
                imageMap[parts[0]] = append(imageMap[parts[0]], img)
            }
        }
    }

    for _, imgs := range imageMap {
        if len(imgs) <= keepLatest {
            continue
        }

        sort.Slice(imgs, func(i, j int) bool {
            return imgs[i].Created < imgs[j].Created
        })

        for i := 0; i < len(imgs)-keepLatest; i++ {
            if _, err := im.client.ImageRemove(ctx, imgs[i].ID, image.RemoveOptions{
                Force: true,
            }); err != nil {
                log.Printf("Failed to remove image %s: %v", imgs[i].ID[:12], err)
            }
        }
    }

    return nil
}

// ============================================================================
// EVENT MONITORING
// ============================================================================

func MonitorContainerEvents(ctx context.Context, handler EventHandler) error {
    dockerClient, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
    if err != nil {
        return fmt.Errorf("create docker client: %w", err)
    }
    defer dockerClient.Close()

    eventStream, errs := dockerClient.Events(ctx, events.ListOptions{
        Filters: filters.NewArgs(
            filters.Arg("type", "container"),
            filters.Arg("label", managedByLabelKey+"="+managedByLabelValue),
        ),
    })

    for {
        select {
        case <-ctx.Done():
            return ctx.Err()
        case err := <-errs:
            return fmt.Errorf("events error: %w", err)
        case event := <-eventStream:
            containerEvent := ContainerEvent{
                Type:       string(event.Type),
                Action:     string(event.Action),
                ActorID:    event.Actor.ID,
                Attributes: event.Actor.Attributes,
                Timestamp:  time.Unix(event.Time, 0),
            }

            if err := handler(containerEvent); err != nil {
                log.Printf("Event handler error: %v", err)
            }
        }
    }
}

// ============================================================================
// VOLUME MANAGEMENT
// ============================================================================

func CreateVolume(ctx context.Context, config VolumeConfig) (string, error) {
    dockerClient, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
    if err != nil {
        return "", fmt.Errorf("create docker client: %w", err)
    }
    defer dockerClient.Close()

    volume, err := dockerClient.VolumeCreate(ctx, volume.CreateOptions{
        Name:       config.Name,
        Driver:     config.Driver,
        Labels:     config.Labels,
        DriverOpts: config.Options,
    })
    if err != nil {
        return "", fmt.Errorf("create volume: %w", err)
    }

    return volume.Name, nil
}

func ListContainerVolumes(ctx context.Context, containerID string) ([]types.MountPoint, error) {
    dockerClient, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
    if err != nil {
        return nil, fmt.Errorf("create docker client: %w", err)
    }
    defer dockerClient.Close()

    inspect, err := dockerClient.ContainerInspect(ctx, containerID)
    if err != nil {
        return nil, fmt.Errorf("inspect container: %w", err)
    }

    return inspect.Mounts, nil
}

// ============================================================================
// BACKUP SUPPORT
// ============================================================================

func BackupContainerVolume(ctx context.Context, containerID string, config BackupConfig) error {
    dockerClient, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
    if err != nil {
        return fmt.Errorf("create docker client: %w", err)
    }
    defer dockerClient.Close()

    backupCmd := []string{"tar", "-czf", config.Destination}
    if config.Compress {
        backupCmd = []string{"tar", "-czf", config.Destination}
    } else {
        backupCmd = []string{"tar", "-cf", config.Destination}
    }

    for _, exclude := range config.Exclude {
        backupCmd = append(backupCmd, "--exclude", exclude)
    }

    backupCmd = append(backupCmd, "-C", config.Source, ".")

    execConfig := container.ExecOptions{
        Cmd:          backupCmd,
        AttachStdout: true,
        AttachStderr: true,
        WorkingDir:   "/",
    }

    execID, err := dockerClient.ContainerExecCreate(ctx, containerID, execConfig)
    if err != nil {
        return fmt.Errorf("create backup exec: %w", err)
    }

    attach, err := dockerClient.ContainerExecAttach(ctx, execID.ID, container.ExecStartOptions{})
    if err != nil {
        return fmt.Errorf("attach backup exec: %w", err)
    }
    defer attach.Close()

    inspect, err := dockerClient.ContainerExecInspect(ctx, execID.ID)
    if err != nil {
        return fmt.Errorf("inspect backup exec: %w", err)
    }

    for inspect.Running {
        time.Sleep(1 * time.Second)
        inspect, err = dockerClient.ContainerExecInspect(ctx, execID.ID)
        if err != nil {
            return fmt.Errorf("inspect backup exec: %w", err)
        }
    }

    if inspect.ExitCode != 0 {
        return fmt.Errorf("backup failed with exit code %d", inspect.ExitCode)
    }

    return nil
}

// ============================================================================
// USAGE EXAMPLES
// ============================================================================

/*
EXAMPLE USAGE:

1. Create container with health check:
   healthCheck := HealthCheckConfig{
       Path:     "/health",
       Interval: 30 * time.Second,
       Timeout:  5 * time.Second,
       Retries:  3,
   }
   id, err := CreateAndStartContainerWithHealthCheck(
       "my-app:latest",
       "my-app-1",
       "deploy-123",
       "8080:80",
       []string{"NODE_ENV=production"},
       1024, 0.5,
       healthCheck,
   )

2. Get container logs:
   logs, err := GetContainerLogs(ctx, containerID, ContainerLogOptions{
       ShowStdout: true,
       ShowStderr: true,
       Tail: 100,
   })

3. Get container stats:
   stats, err := GetContainerStats(ctx, containerID)
   fmt.Printf("CPU: %.2f%%, Memory: %.2f MB\n", stats.CPUUsage, stats.MemoryUsage/1024/1024)

4. Cleanup old containers:
   err := CleanupOldContainers(ctx, CleanupPolicy{
       MaxAge: 24 * time.Hour,
       MaxCount: 5,
       RemoveVolumes: true,
   })

5. Monitor container events:
   handler := func(event ContainerEvent) error {
       fmt.Printf("Event: %s %s %s\n", event.Type, event.Action, event.ActorID)
       return nil
   }
   err := MonitorContainerEvents(ctx, handler)

6. Prune old images:
   imgManager, _ := NewImageManager()
   err := imgManager.PruneImages(ctx, 3) // Keep latest 3 images

7. Create and manage volumes:
   volumeName, err := CreateVolume(ctx, VolumeConfig{
       Name: "app-data",
       Driver: "local",
       Labels: map[string]string{"app": "my-app"},
   })
*/