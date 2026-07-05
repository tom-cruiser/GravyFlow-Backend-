package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"sort"
	"strings"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
	"github.com/docker/go-connections/nat"
)

const (
	managedByLabelKey   = "gravyflow.managed-by"
	managedByLabelValue = "control-plane"
)

type ValidationError struct {
	Field   string
	Code    string
	Message string
}

func (e *ValidationError) Error() string {
	return e.Message
}

type ConflictError struct {
	Resource string
	Value    string
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("%s %q already exists", e.Resource, e.Value)
}

type ManagedContainer struct {
	ContainerID   string `json:"containerId"`
	ContainerName string `json:"containerName"`
	DeploymentID  string `json:"deploymentId"`
	AppName       string `json:"appName"`
	ImageName     string `json:"imageName"`
	InternalIP    string `json:"internalIP"`
	InternalPort  string `json:"internalPort"`
	Status        string `json:"status"`
	PortMap       string `json:"portMap"`
}

// CreateAndStartContainer pulls an image, creates a container with port mapping, and starts it.
func CreateAndStartContainer(imageName string, containerName string, deploymentID string, portMap string, envVars []string, memoryMB int64, cpus float64) (string, error) {
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
	_ = hostPort // routing uses Caddy + container internal IP, not host port publish

	ctx := context.Background()
	dockerClient, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return "", fmt.Errorf("create docker client: %w", err)
	}
	defer dockerClient.Close()

	// Images built locally by nixpacks (tagged with the app name) do not exist in
	// any registry. Pulling them would resolve to docker.io/library/<name> and
	// fail or stall, so only pull when the image isn't already present locally.
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

func RestartContainer(containerID string, imageName string, containerName string, deploymentID string, portMap string, envVars []string, memoryMB int64, cpus float64) (string, error) {
	if strings.TrimSpace(containerID) != "" {
		if err := StopAndRemoveContainer(containerID); err != nil {
			return "", err
		}
	}

	return CreateAndStartContainer(imageName, containerName, deploymentID, portMap, envVars, memoryMB, cpus)
}

// StopAndRemoveContainer stops and removes a managed container, then rebuilds the Caddy config from live containers.
// This is the safe deletion path: once the container disappears from Docker, the next sync removes its route.
func StopAndRemoveContainer(containerID string) error {
	containerID = strings.TrimSpace(containerID)
	if containerID == "" {
		return &ValidationError{
			Field:   "containerId",
			Code:    "required",
			Message: "containerId is required",
		}
	}

	ctx := context.Background()
	dockerClient, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return fmt.Errorf("create docker client: %w", err)
	}
	defer dockerClient.Close()

	if err := dockerClient.ContainerStop(ctx, containerID, container.StopOptions{}); err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("stop container %q: %w", containerID, err)
	}
	if err := dockerClient.ContainerRemove(ctx, containerID, container.RemoveOptions{Force: true}); err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("remove container %q: %w", containerID, err)
	}

	return SyncCaddyRoutesFromRunningContainers()
}

func ListRunningManagedContainers() ([]ManagedContainer, error) {
	ctx := context.Background()
	dockerClient, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("create docker client: %w", err)
	}
	defer dockerClient.Close()

	args := filters.NewArgs(filters.Arg("label", managedByLabelKey+"="+managedByLabelValue))
	containers, err := dockerClient.ContainerList(ctx, container.ListOptions{Filters: args})
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}

	out := make([]ManagedContainer, 0, len(containers))
	for _, c := range containers {
		name := ""
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}

		internalIP, internalPort, err := resolveManagedContainerEndpoint(ctx, dockerClient, c.ID)
		if err != nil {
			return nil, err
		}

		out = append(out, ManagedContainer{
			ContainerID:   c.ID,
			ContainerName: name,
			DeploymentID:  c.Labels["gravyflow.deployment-id"],
			AppName:       c.Labels["gravyflow.app-name"],
			ImageName:     c.Image,
			InternalIP:    internalIP,
			InternalPort:  internalPort,
			Status:        c.Status,
			PortMap:       formatPortMap(c.Ports),
		})
	}

	return out, nil
}

func parsePortMap(portMap string) (string, string, error) {
	parts := strings.Split(portMap, ":")
	if len(parts) != 2 {
		return "", "", &ValidationError{
			Field:   "portMap",
			Code:    "invalid_format",
			Message: "portMap must be in host:container format, e.g. 8080:80",
		}
	}

	hostPort := strings.TrimSpace(parts[0])
	containerPort := strings.TrimSpace(parts[1])
	if hostPort == "" || containerPort == "" {
		return "", "", &ValidationError{
			Field:   "portMap",
			Code:    "missing_port",
			Message: "both host and container ports are required",
		}
	}

	if _, err := nat.NewPort("tcp", hostPort); err != nil {
		return "", "", &ValidationError{
			Field:   "portMap",
			Code:    "invalid_host_port",
			Message: fmt.Sprintf("host port %q is invalid", hostPort),
		}
	}
	if _, err := nat.NewPort("tcp", containerPort); err != nil {
		return "", "", &ValidationError{
			Field:   "portMap",
			Code:    "invalid_container_port",
			Message: fmt.Sprintf("container port %q is invalid", containerPort),
		}
	}

	return hostPort, containerPort, nil
}

// allocatePortMap returns host:container with host port 0 so Docker assigns a free port.
func allocatePortMap(containerPort string) string {
	containerPort = strings.TrimSpace(containerPort)
	if containerPort == "" {
		containerPort = "8080"
	}
	return "0:" + containerPort
}

// normalizePortMap upgrades legacy 8080:8080 maps that conflict with the API listener.
func normalizePortMap(portMap string) string {
	portMap = strings.TrimSpace(portMap)
	if portMap == "" || portMap == "8080:8080" {
		return allocatePortMap("8080")
	}
	return portMap
}

func asValidationError(err error) *ValidationError {
	if err == nil {
		return nil
	}

	var validationErr *ValidationError
	if errors.As(err, &validationErr) {
		return validationErr
	}

	return nil
}

func formatPortMap(ports []types.Port) string {
	if len(ports) == 0 {
		return ""
	}

	mappings := make([]string, 0, len(ports))
	for _, p := range ports {
		protocol := p.Type
		if protocol == "" {
			protocol = "tcp"
		}

		if p.PublicPort > 0 {
			mappings = append(mappings, fmt.Sprintf("%d:%d/%s", p.PublicPort, p.PrivatePort, protocol))
			continue
		}

		mappings = append(mappings, fmt.Sprintf("%d/%s", p.PrivatePort, protocol))
	}

	return strings.Join(mappings, ",")
}

func resolveManagedContainerEndpoint(ctx context.Context, dockerClient *client.Client, containerID string) (string, string, error) {
	inspect, err := dockerClient.ContainerInspect(ctx, containerID)
	if err != nil {
		return "", "", fmt.Errorf("inspect container %q: %w", containerID, err)
	}

	internalIP := ""
	for _, network := range inspect.NetworkSettings.Networks {
		if network.IPAddress != "" {
			internalIP = network.IPAddress
			break
		}
	}
	if internalIP == "" {
		return "", "", fmt.Errorf("container %q has no assigned internal IP", containerID)
	}

	internalPort := strings.TrimSpace(inspect.Config.Labels["gravyflow.internal-port"])
	if internalPort == "" {
		exposed := make([]string, 0, len(inspect.Config.ExposedPorts))
		for port := range inspect.Config.ExposedPorts {
			exposed = append(exposed, port.Port())
		}
		sort.Strings(exposed)
		for _, preferred := range []string{"8080", "3000", "5000", "80"} {
			for _, candidate := range exposed {
				if candidate == preferred {
					internalPort = candidate
					break
				}
			}
			if internalPort != "" {
				break
			}
		}
		if internalPort == "" && len(exposed) > 0 {
			internalPort = exposed[len(exposed)-1]
		}
	}
	if internalPort == "" {
		for _, binding := range inspect.NetworkSettings.Ports {
			if len(binding) == 0 {
				continue
			}
			internalPort = binding[0].HostPort
			break
		}
	}
	if internalPort == "" {
		return "", "", fmt.Errorf("container %q has no exposed internal port", containerID)
	}

	return internalIP, internalPort, nil
}

func removeContainerByName(ctx context.Context, dockerClient *client.Client, containerName string) error {
	containerName = strings.TrimSpace(containerName)
	if containerName == "" {
		return nil
	}

	existing, err := dockerClient.ContainerInspect(ctx, containerName)
	if err != nil {
		if errdefs.IsNotFound(err) || client.IsErrNotFound(err) {
			return nil
		}
		return fmt.Errorf("inspect container %q: %w", containerName, err)
	}

	if err := dockerClient.ContainerStop(ctx, existing.ID, container.StopOptions{}); err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("stop container %q: %w", containerName, err)
	}
	if err := dockerClient.ContainerRemove(ctx, existing.ID, container.RemoveOptions{Force: true}); err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("remove container %q: %w", containerName, err)
	}

	return SyncCaddyRoutesFromRunningContainers()
}
