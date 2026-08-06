package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// ============================================================================
// CONSTANTS
// ============================================================================

const (
	gravyflowAppTag = "gravyflow-app"
	// Note: minDiskSpaceGB is now in helpers.go - DO NOT redeclare here
)

// ============================================================================
// TYPES
// ============================================================================

type BuildKitInfo struct {
	Available bool
	Version   string
	Error     error
}

type BuildKitFeatures struct {
	CacheTo    bool
	CacheFrom  bool
	Export     bool
	Attest     bool
	SBOM       bool
	Provenance bool
}

type BuildKitConfig struct {
	Enabled        bool
	Version        string
	Features       BuildKitFeatures
	Insecure       bool
	CacheDir       string
	MaxParallelism int
}

type EnvValidation struct {
	Valid    bool
	Issues   []string
	Warnings []string
}

type BuildKitStrategy struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Available   bool     `json:"available"`
	Features    []string `json:"features"`
}

// ============================================================================
// GLOBAL STATE
// ============================================================================

var (
	buildKitInfoOnce sync.Once
	buildKitInfo     BuildKitInfo
	configOnce       sync.Once
	globalConfig     BuildKitConfig
)

// ============================================================================
// BUILDKIT DETECTION
// ============================================================================

// dockerBuildKitAvailable reports whether the local Docker CLI can use BuildKit
func dockerBuildKitAvailable() bool {
	info := getBuildKitInfo()
	return info.Available
}

func getBuildKitInfo() BuildKitInfo {
	buildKitInfoOnce.Do(func() {
		// Check environment overrides first
		if strings.EqualFold(strings.TrimSpace(os.Getenv("GRAVYFLOW_FORCE_BUILDKIT")), "1") {
			buildKitInfo = BuildKitInfo{Available: true, Version: "forced"}
			log.Printf("[BUILDKIT] Forced enabled by environment variable")
			return
		}
		if strings.EqualFold(strings.TrimSpace(os.Getenv("GRAVYFLOW_DISABLE_BUILDKIT")), "1") {
			buildKitInfo = BuildKitInfo{Available: false, Version: "disabled"}
			log.Printf("[BUILDKIT] Disabled by environment variable")
			return
		}

		// Check if docker is available
		if _, err := exec.LookPath("docker"); err != nil {
			buildKitInfo = BuildKitInfo{Available: false, Error: fmt.Errorf("docker not found")}
			log.Printf("[BUILDKIT] Docker not found in PATH")
			return
		}

		// Check buildx version
		cmd := exec.Command("docker", "buildx", "version")
		output, err := cmd.Output()
		if err != nil {
			buildKitInfo = BuildKitInfo{Available: false, Error: err}
			log.Printf("[BUILDKIT] buildx not available: %v", err)
			return
		}

		// Parse version from output
		version := parseBuildKitVersion(string(output))
		buildKitInfo = BuildKitInfo{
			Available: true,
			Version:   version,
		}
		log.Printf("[BUILDKIT] Available with version: %s", version)
	})
	return buildKitInfo
}

func parseBuildKitVersion(output string) string {
	lines := strings.Split(output, "\n")
	for _, line := range lines {
		if strings.Contains(line, "version") {
			parts := strings.Fields(line)
			for i, part := range parts {
				if part == "version" && i+1 < len(parts) {
					version := strings.TrimSpace(parts[i+1])
					// Remove trailing comma if present
					version = strings.TrimSuffix(version, ",")
					return version
				}
			}
		}
	}
	return "unknown"
}

// ============================================================================
// BUILDKIT FEATURES
// ============================================================================

func getBuildKitFeatures() BuildKitFeatures {
	if !dockerBuildKitAvailable() {
		return BuildKitFeatures{}
	}

	features := BuildKitFeatures{
		CacheTo:   true,
		CacheFrom: true,
		Export:    true,
	}

	// Check version for newer features
	info := getBuildKitInfo()
	if info.Version != "" && info.Version != "forced" && info.Version != "disabled" {
		versionParts := strings.Split(info.Version, ".")
		if len(versionParts) >= 2 {
			major, _ := strconv.Atoi(versionParts[0])
			minor := 0
			if len(versionParts) >= 2 {
				minor, _ = strconv.Atoi(versionParts[1])
			}

			// Attest, SBOM, Provenance available in v0.10+
			if major > 0 || (major == 0 && minor >= 10) {
				features.Attest = true
				features.SBOM = true
				features.Provenance = true
			}
		}
	}

	return features
}

// ============================================================================
// BUILDKIT CONFIG
// ============================================================================

func getBuildKitConfig() BuildKitConfig {
	configOnce.Do(func() {
		info := getBuildKitInfo()
		features := getBuildKitFeatures()

		config := BuildKitConfig{
			Enabled:        info.Available,
			Version:        info.Version,
			Features:       features,
			MaxParallelism: 4,
		}

		// Allow override from environment
		if cacheDir := os.Getenv("BUILDKIT_CACHE_DIR"); cacheDir != "" {
			config.CacheDir = cacheDir
			log.Printf("[BUILDKIT] Cache directory: %s", cacheDir)
		}
		if maxPar := os.Getenv("BUILDKIT_MAX_PARALLELISM"); maxPar != "" {
			if val, err := strconv.Atoi(maxPar); err == nil && val > 0 {
				config.MaxParallelism = val
				log.Printf("[BUILDKIT] Max parallelism: %d", val)
			}
		}
		if strings.EqualFold(os.Getenv("BUILDKIT_INSECURE"), "1") {
			config.Insecure = true
			log.Printf("[BUILDKIT] Insecure mode enabled")
		}

		globalConfig = config
	})
	return globalConfig
}

// ============================================================================
// ENVIRONMENT VALIDATION
// ============================================================================

func validateDockerEnvironment() EnvValidation {
	validation := EnvValidation{Valid: true}

	// Check if docker is installed
	if _, err := exec.LookPath("docker"); err != nil {
		validation.Valid = false
		validation.Issues = append(validation.Issues, "docker not found in PATH")
		log.Printf("[DOCKER] Error: docker not found in PATH")
		return validation
	}

	// Check docker daemon is running
	cmd := exec.Command("docker", "info")
	if err := cmd.Run(); err != nil {
		validation.Valid = false
		validation.Issues = append(validation.Issues, "docker daemon not running")
		log.Printf("[DOCKER] Error: docker daemon not running")
		return validation
	}

	// Check docker version compatibility
	cmd = exec.Command("docker", "version", "--format", "{{.Server.Version}}")
	output, err := cmd.Output()
	if err == nil {
		version := strings.TrimSpace(string(output))
		if !isDockerVersionCompatible(version) {
			validation.Warnings = append(validation.Warnings,
				fmt.Sprintf("docker version %s may not be compatible with BuildKit", version))
			log.Printf("[DOCKER] Warning: version %s may be incompatible", version)
		} else {
			log.Printf("[DOCKER] Version: %s", version)
		}
	}

	// Check disk space - minDiskSpaceGB is now in helpers.go
	if !hasEnoughDiskSpace() {
		validation.Warnings = append(validation.Warnings,
			"low disk space available for Docker builds (< 1GB)")
		log.Printf("[DOCKER] Warning: low disk space available")
	}

	// Check BuildKit availability
	info := getBuildKitInfo()
	if info.Available {
		log.Printf("[DOCKER] BuildKit available: %s", info.Version)
		validation.Warnings = append(validation.Warnings,
			fmt.Sprintf("BuildKit available: %s", info.Version))
	} else {
		if info.Error != nil {
			log.Printf("[DOCKER] BuildKit not available: %v", info.Error)
		}
		validation.Warnings = append(validation.Warnings,
			"BuildKit not available, using legacy builder")
	}

	return validation
}

func isDockerVersionCompatible(version string) bool {
	parts := strings.Split(version, ".")
	if len(parts) >= 2 {
		major, _ := strconv.Atoi(parts[0])
		minor := 0
		if len(parts) >= 2 {
			minor, _ = strconv.Atoi(parts[1])
		}
		// Require Docker 19.03+ for BuildKit
		return major > 19 || (major == 19 && minor >= 3)
	}
	return true
}

func hasEnoughDiskSpace() bool {
	// Check available disk space in Docker directory
	dockerDir := "/var/lib/docker"
	var stat syscall.Statfs_t
	if err := syscall.Statfs(dockerDir, &stat); err != nil {
		// Try current directory if Docker dir not accessible
		if err := syscall.Statfs(".", &stat); err != nil {
			return true // assume ok if can't check
		}
	}
	freeBytes := stat.Bavail * uint64(stat.Bsize)
	freeGB := freeBytes / (1024 * 1024 * 1024)
	// minDiskSpaceGB is now in helpers.go
	return freeGB >= minDiskSpaceGB
}

// ============================================================================
// ENVIRONMENT MANAGEMENT
// ============================================================================

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
	env := withDockerBuildKit(os.Environ(), dockerBuildKitAvailable())
	logDockerEnvironment(env)
	return env
}

func dockerCommandEnvForceLegacyBuilder() []string {
	env := withDockerBuildKit(os.Environ(), false)
	logDockerEnvironment(env)
	return env
}

func dockerCommandEnvWithConfig(config BuildKitConfig) []string {
	env := withDockerBuildKit(os.Environ(), config.Enabled)
	
	// Add additional environment variables based on config
	if config.CacheDir != "" {
		env = withEnvVar(env, "BUILDKIT_CACHE_DIR", config.CacheDir)
	}
	if config.MaxParallelism > 0 {
		env = withEnvVar(env, "BUILDKIT_MAX_PARALLELISM", strconv.Itoa(config.MaxParallelism))
	}
	if config.Insecure {
		env = withEnvVar(env, "BUILDKIT_INSECURE", "1")
	}
	
	logDockerEnvironment(env)
	return env
}

// ============================================================================
// ENVIRONMENT LOGGING
// ============================================================================

func logDockerEnvironment(env []string) {
	if os.Getenv("GRAVYFLOW_DEBUG") != "1" {
		return
	}
	
	var logBuilder strings.Builder
	logBuilder.WriteString("[DOCKER] Environment variables:\n")
	
	importantVars := []string{
		"DOCKER_BUILDKIT",
		"COMPOSE_DOCKER_CLI_BUILD",
		"DOCKER_HOST",
		"DOCKER_TLS_VERIFY",
		"DOCKER_CERT_PATH",
		"BUILDKIT_CACHE_DIR",
		"BUILDKIT_MAX_PARALLELISM",
		"BUILDKIT_INSECURE",
	}
	
	for _, key := range importantVars {
		for _, entry := range env {
			if strings.HasPrefix(entry, key+"=") {
				logBuilder.WriteString(fmt.Sprintf("  %s\n", entry))
				break
			}
		}
	}
	
	log.Print(logBuilder.String())
}

// ============================================================================
// ERROR DETECTION
// ============================================================================

// Note: isBuildKitMissingError is now in helpers.go - DO NOT redeclare here
// The function is used from helpers.go

func isBuildKitIncompatibleError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "buildkit") &&
		(strings.Contains(s, "incompatible") || strings.Contains(s, "unsupported"))
}

// ============================================================================
// BUILDKIT STRATEGIES
// ============================================================================

func getBuildKitStrategies() []BuildKitStrategy {
	strategies := []BuildKitStrategy{
		{
			Name:        "auto",
			Description: "Automatically detect and use BuildKit if available",
			Available:   true,
			Features:    []string{"automatic", "default"},
		},
		{
			Name:        "force",
			Description: "Force BuildKit usage, fail if not available",
			Available:   dockerBuildKitAvailable(),
			Features:    []string{"enforced", "fast"},
		},
		{
			Name:        "legacy",
			Description: "Use legacy Docker builder",
			Available:   true,
			Features:    []string{"compatible", "stable"},
		},
		{
			Name:        "hybrid",
			Description: "Try BuildKit, fallback to legacy on failure",
			Available:   true,
			Features:    []string{"resilient", "adaptive"},
		},
	}
	return strategies
}

// ============================================================================
// IMAGE TAG SANITIZATION - REMOVED (now in build.go)
// ============================================================================
// Note: dockerImageTag is now in build.go - DO NOT redeclare here

// ============================================================================
// INITIALIZATION
// ============================================================================

func init() {
	// Log BuildKit status on startup
	info := getBuildKitInfo()
	if info.Available {
		log.Printf("[BUILDKIT] Initialized with version: %s", info.Version)
	} else {
		if info.Error != nil {
			log.Printf("[BUILDKIT] Not available: %v", info.Error)
		} else {
			log.Printf("[BUILDKIT] Not available")
		}
	}
	
	// Validate Docker environment
	validation := validateDockerEnvironment()
	if !validation.Valid {
		log.Printf("[DOCKER] Environment validation failed: %v", validation.Issues)
	}
	if len(validation.Warnings) > 0 {
		log.Printf("[DOCKER] Environment warnings: %v", validation.Warnings)
	}
}

// ============================================================================
// USAGE EXAMPLES
// ============================================================================

/*
EXAMPLE USAGE:

1. Basic build with automatic BuildKit detection:
   env := dockerCommandEnv()
   cmd := exec.Command("nixpacks", "build", path)
   cmd.Env = env

2. Force legacy builder:
   env := dockerCommandEnvForceLegacyBuilder()
   cmd.Env = env

3. Use BuildKit with custom configuration:
   config := getBuildKitConfig()
   env := dockerCommandEnvWithConfig(config)
   cmd.Env = env

4. Check BuildKit availability:
   if dockerBuildKitAvailable() {
       fmt.Println("BuildKit is available")
       info := getBuildKitInfo()
       fmt.Printf("Version: %s\n", info.Version)
       features := getBuildKitFeatures()
       fmt.Printf("Features: %+v\n", features)
   }

5. Validate Docker environment:
   validation := validateDockerEnvironment()
   if !validation.Valid {
       fmt.Printf("Environment issues: %v\n", validation.Issues)
   }

6. Get BuildKit strategies:
   strategies := getBuildKitStrategies()
   for _, s := range strategies {
       fmt.Printf("%s: %s (available: %v)\n", s.Name, s.Description, s.Available)
   }

7. Sanitize image tag (now in build.go):
   tag := dockerImageTag("My App with Spaces")
   // tag = "my-app-with-spaces"
*/