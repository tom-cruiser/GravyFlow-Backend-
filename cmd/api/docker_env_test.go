package main

import (
	"strings"
	"testing"
)

func TestWithDockerBuildKitOverridesExistingValue(t *testing.T) {
	env := []string{
		"PATH=/usr/bin",
		"DOCKER_BUILDKIT=1",
		"COMPOSE_DOCKER_CLI_BUILD=1",
	}

	got := withDockerBuildKit(env, false)

	if countEnv(got, "DOCKER_BUILDKIT") != 1 {
		t.Fatalf("expected one DOCKER_BUILDKIT entry, got %d: %v", countEnv(got, "DOCKER_BUILDKIT"), got)
	}
	if !containsEnv(got, "DOCKER_BUILDKIT=0") {
		t.Fatalf("expected DOCKER_BUILDKIT=0 in %v", got)
	}
	if !containsEnv(got, "COMPOSE_DOCKER_CLI_BUILD=0") {
		t.Fatalf("expected COMPOSE_DOCKER_CLI_BUILD=0 in %v", got)
	}
}

func TestIsBuildKitMissingError(t *testing.T) {
	err := fmtError("exit status 1: ERROR: BuildKit is enabled but the buildx component is missing or broken.")
	if !isBuildKitMissingError(err) {
		t.Fatal("expected buildkit missing error to match")
	}
}

type fmtError string

func (e fmtError) Error() string { return string(e) }

func countEnv(env []string, key string) int {
	prefix := key + "="
	count := 0
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			count++
		}
	}
	return count
}

func containsEnv(env []string, want string) bool {
	for _, entry := range env {
		if entry == want {
			return true
		}
	}
	return false
}
