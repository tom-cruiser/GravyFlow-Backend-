package main

import (
	"os"
	"os/exec"
	"strings"
	"sync"
)

var (
	buildKitAvailableOnce sync.Once
	buildKitAvailable     bool
)

// dockerBuildKitAvailable reports whether the local Docker CLI can use BuildKit
// (requires the buildx plugin). Ubuntu's docker.io package often ships without it.
func dockerBuildKitAvailable() bool {
	buildKitAvailableOnce.Do(func() {
		if strings.EqualFold(strings.TrimSpace(os.Getenv("GRAVYFLOW_FORCE_BUILDKIT")), "1") {
			buildKitAvailable = true
			return
		}
		if strings.EqualFold(strings.TrimSpace(os.Getenv("GRAVYFLOW_DISABLE_BUILDKIT")), "1") {
			buildKitAvailable = false
			return
		}
		cmd := exec.Command("docker", "buildx", "version")
		buildKitAvailable = cmd.Run() == nil
	})
	return buildKitAvailable
}

func withEnvVar(env []string, key, value string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			continue
		}
		out = append(out, entry)
	}
	out = append(out, prefix+value)
	return out
}

func withDockerBuildKit(env []string, enabled bool) []string {
	value := "0"
	if enabled {
		value = "1"
	}
	env = withEnvVar(env, "DOCKER_BUILDKIT", value)
	// Legacy docker-compose knob; keep it aligned so nested docker invocations
	// don't re-enable BuildKit when buildx is missing.
	if enabled {
		return withEnvVar(env, "COMPOSE_DOCKER_CLI_BUILD", "1")
	}
	return withEnvVar(env, "COMPOSE_DOCKER_CLI_BUILD", "0")
}

func dockerCommandEnv() []string {
	return withDockerBuildKit(os.Environ(), dockerBuildKitAvailable())
}

func dockerCommandEnvForceLegacyBuilder() []string {
	return withDockerBuildKit(os.Environ(), false)
}

func isBuildKitMissingError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "buildkit") &&
		(strings.Contains(s, "buildx") || strings.Contains(s, "buildkit is enabled"))
}

func dockerImageTag(appName string) string {
	appName = strings.ToLower(strings.TrimSpace(appName))
	if appName == "" {
		return "gravyflow-app"
	}
	var b strings.Builder
	for _, r := range appName {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-', r == '/':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	tag := strings.Trim(b.String(), "-./")
	if tag == "" {
		return "gravyflow-app"
	}
	return tag
}
