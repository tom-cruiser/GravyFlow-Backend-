package main

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// ============================================================================
// UNIT TESTS
// ============================================================================

func TestWithDockerBuildKit_OverridesExistingValue(t *testing.T) {
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

func TestWithDockerBuildKit_NoExistingValues(t *testing.T) {
	env := []string{
		"PATH=/usr/bin",
		"HOME=/root",
	}

	got := withDockerBuildKit(env, false)

	// Should add BuildKit disable flags
	if !containsEnv(got, "DOCKER_BUILDKIT=0") {
		t.Errorf("expected DOCKER_BUILDKIT=0, got %v", got)
	}
	if !containsEnv(got, "COMPOSE_DOCKER_CLI_BUILD=0") {
		t.Errorf("expected COMPOSE_DOCKER_CLI_BUILD=0, got %v", got)
	}
	
	// Should preserve existing variables
	if !containsEnv(got, "PATH=/usr/bin") {
		t.Errorf("expected PATH to be preserved, got %v", got)
	}
	if !containsEnv(got, "HOME=/root") {
		t.Errorf("expected HOME to be preserved, got %v", got)
	}
}

func TestWithDockerBuildKit_EnableBuildKit(t *testing.T) {
	env := []string{
		"PATH=/usr/bin",
		"DOCKER_BUILDKIT=0",
	}

	got := withDockerBuildKit(env, true)

	// Should enable BuildKit
	if !containsEnv(got, "DOCKER_BUILDKIT=1") {
		t.Errorf("expected DOCKER_BUILDKIT=1, got %v", got)
	}
	if !containsEnv(got, "COMPOSE_DOCKER_CLI_BUILD=1") {
		t.Errorf("expected COMPOSE_DOCKER_CLI_BUILD=1, got %v", got)
	}
}

func TestWithDockerBuildKit_DisableBuildKitWithExistingValues(t *testing.T) {
	env := []string{
		"PATH=/usr/bin",
		"DOCKER_BUILDKIT=1",
		"COMPOSE_DOCKER_CLI_BUILD=1",
	}

	got := withDockerBuildKit(env, false)

	// Should override to disable
	if !containsEnv(got, "DOCKER_BUILDKIT=0") {
		t.Errorf("expected DOCKER_BUILDKIT=0, got %v", got)
	}
	if !containsEnv(got, "COMPOSE_DOCKER_CLI_BUILD=0") {
		t.Errorf("expected COMPOSE_DOCKER_CLI_BUILD=0, got %v", got)
	}
	// Should only have one of each
	if countEnv(got, "DOCKER_BUILDKIT") != 1 {
		t.Errorf("expected one DOCKER_BUILDKIT, got %d", countEnv(got, "DOCKER_BUILDKIT"))
	}
}

// ============================================================================
// ERROR DETECTION TESTS
// ============================================================================

func TestIsBuildKitMissingError(t *testing.T) {
	err := fmtError("exit status 1: ERROR: BuildKit is enabled but the buildx component is missing or broken.")
	if !isBuildKitMissingError(err) {
		t.Fatal("expected buildkit missing error to match")
	}
}

func TestIsBuildKitMissingError_EdgeCases(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{
			name:     "nil error",
			err:      nil,
			expected: false,
		},
		{
			name:     "empty error",
			err:      fmtError(""),
			expected: false,
		},
		{
			name:     "generic error",
			err:      fmtError("something went wrong"),
			expected: false,
		},
		{
			name:     "buildkit keyword",
			err:      fmtError("buildkit is not available"),
			expected: true,
		},
		{
			name:     "buildx keyword",
			err:      fmtError("docker buildx command not found"),
			expected: true,
		},
		{
			name:     "case insensitive",
			err:      fmtError("BUILDKIT and BUILDX are missing"),
			expected: true,
		},
		{
			name:     "partial match with context",
			err:      fmtError("Error: Docker build requires BuildKit but buildx is missing"),
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isBuildKitMissingError(tt.err)
			if result != tt.expected {
				t.Errorf("isBuildKitMissingError(%v) = %v, want %v", tt.err, result, tt.expected)
			}
		})
	}
}

func TestIsBuildKitMissingError_WithWrappedErrors(t *testing.T) {
	// Test with wrapped errors
	baseErr := fmtError("buildx not found")
	wrappedErr := fmt.Errorf("docker build failed: %w", baseErr)

	if !isBuildKitMissingError(wrappedErr) {
		t.Errorf("expected wrapped error to be detected")
	}

	// Test with error wrapping chain
	deepErr := fmt.Errorf("outer: %w",
		fmt.Errorf("middle: %w",
			fmtError("BuildKit is missing")))

	if !isBuildKitMissingError(deepErr) {
		t.Errorf("expected deeply wrapped error to be detected")
	}
}

// ============================================================================
// HELPER FUNCTION TESTS
// ============================================================================

func TestCountEnv(t *testing.T) {
	tests := []struct {
		name     string
		env      []string
		key      string
		expected int
	}{
		{
			name:     "single match",
			env:      []string{"DOCKER_BUILDKIT=1", "PATH=/bin"},
			key:      "DOCKER_BUILDKIT",
			expected: 1,
		},
		{
			name:     "multiple matches",
			env:      []string{"DOCKER_BUILDKIT=1", "DOCKER_BUILDKIT=0", "PATH=/bin"},
			key:      "DOCKER_BUILDKIT",
			expected: 2,
		},
		{
			name:     "no matches",
			env:      []string{"PATH=/bin", "HOME=/root"},
			key:      "DOCKER_BUILDKIT",
			expected: 0,
		},
		{
			name:     "partial match should not count",
			env:      []string{"DOCKER_BUILDKIT=1", "DOCKER_BUILDKIT_EXTRA=2"},
			key:      "DOCKER_BUILDKIT",
			expected: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := countEnv(tt.env, tt.key)
			if result != tt.expected {
				t.Errorf("countEnv(%v, %s) = %d, want %d", tt.env, tt.key, result, tt.expected)
			}
		})
	}
}

func TestContainsEnv(t *testing.T) {
	tests := []struct {
		name     string
		env      []string
		want     string
		expected bool
	}{
		{
			name:     "exact match",
			env:      []string{"DOCKER_BUILDKIT=0", "PATH=/bin"},
			want:     "DOCKER_BUILDKIT=0",
			expected: true,
		},
		{
			name:     "different value",
			env:      []string{"DOCKER_BUILDKIT=1", "PATH=/bin"},
			want:     "DOCKER_BUILDKIT=0",
			expected: false,
		},
		{
			name:     "empty env",
			env:      []string{},
			want:     "DOCKER_BUILDKIT=0",
			expected: false,
		},
		{
			name:     "partial match should fail",
			env:      []string{"DOCKER_BUILDKIT=0", "PATH=/bin"},
			want:     "DOCKER_BUILDKIT",
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := containsEnv(tt.env, tt.want)
			if result != tt.expected {
				t.Errorf("containsEnv(%v, %s) = %v, want %v", tt.env, tt.want, result, tt.expected)
			}
		})
	}
}

// ============================================================================
// PROPERTY TESTS
// ============================================================================

func TestProperty_WithDockerBuildKit_Idempotent(t *testing.T) {
	env := []string{
		"PATH=/usr/bin",
		"DOCKER_BUILDKIT=1",
	}

	first := withDockerBuildKit(env, false)
	second := withDockerBuildKit(first, false)

	if len(first) != len(second) {
		t.Errorf("length mismatch: first=%d, second=%d", len(first), len(second))
	}

	for i := range first {
		if first[i] != second[i] {
			t.Errorf("mismatch at index %d: %s vs %s", i, first[i], second[i])
		}
	}
}

func TestProperty_WithDockerBuildKit_PreservesOtherVars(t *testing.T) {
	original := []string{
		"PATH=/usr/bin",
		"HOME=/root",
		"USER=test",
	}

	result := withDockerBuildKit(original, false)

	// All non-BuildKit variables should be preserved
	for _, entry := range original {
		if !strings.HasPrefix(entry, "DOCKER_BUILDKIT=") &&
			!strings.HasPrefix(entry, "COMPOSE_DOCKER_CLI_BUILD=") {
			if !containsEnv(result, entry) {
				t.Errorf("original entry %s not preserved", entry)
			}
		}
	}
}

// ============================================================================
// BENCHMARK TESTS
// ============================================================================

func BenchmarkWithDockerBuildKit(b *testing.B) {
	env := []string{
		"PATH=/usr/bin",
		"DOCKER_BUILDKIT=1",
		"COMPOSE_DOCKER_CLI_BUILD=1",
		"HOME=/root",
		"USER=test",
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = withDockerBuildKit(env, false)
	}
}

func BenchmarkIsBuildKitMissingError(b *testing.B) {
	err := fmtError("ERROR: BuildKit is enabled but the buildx component is missing or broken.")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = isBuildKitMissingError(err)
	}
}

func BenchmarkCountEnv(b *testing.B) {
	env := []string{
		"DOCKER_BUILDKIT=1",
		"DOCKER_BUILDKIT=0",
		"COMPOSE_DOCKER_CLI_BUILD=1",
		"PATH=/usr/bin",
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = countEnv(env, "DOCKER_BUILDKIT")
	}
}

// ============================================================================
// INTEGRATION TESTS (optional, requires Docker)
// ============================================================================

// Note: These tests are skipped by default, run with -tags=integration
// go test -tags=integration -v

func TestIntegration_WithDockerBuildKit_RealEnvironment(t *testing.T) {
	t.Skip("Integration tests require Docker daemon running")
	
	// This would test the actual withDockerBuildKit function
	// against a real environment
	_ = withDockerBuildKit(os.Environ(), false)
	
	// Verify Docker commands work with the modified environment
	// cmd := exec.Command("docker", "build", "--no-cache", ".")
	// cmd.Env = env
	// err := cmd.Run()
	// if err != nil {
	//     t.Errorf("docker build failed: %v", err)
	// }
}

// ============================================================================
// CUSTOM TYPES
// ============================================================================

type fmtError string

func (e fmtError) Error() string { return string(e) }