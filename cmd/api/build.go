package main

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"strings"
	"time"
)

// ============================================================================
// CUSTOM ERROR TYPES
// ============================================================================

type ValidationError struct {
	Field   string
	Code    string
	Message string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("validation error [%s.%s]: %s", e.Field, e.Code, e.Message)
}

// ============================================================================
// EMBEDDED FILES
// ============================================================================

//go:embed Dockerfile.node
var nodeDockerfileTemplate string

// ============================================================================
// TYPES AND CONSTANTS - MOVED TO fastbuild.go
// ============================================================================
// Note: ProjectKind, projectKind constants, and related types are now in fastbuild.go
// DO NOT redeclare them here

const (
	defaultBuildTimeout = 30 * time.Minute
	// Note: minDiskSpaceGB is now in helpers.go - DO NOT redeclare here
)

// ============================================================================
// CONFIGURATION
// ============================================================================

type BuildConfig struct {
	// Cache directory for Nixpacks (empty = default)
	NixpacksCacheDir string

	// Build timeout
	Timeout time.Duration

	// Maximum retries on recoverable errors
	MaxRetries int

	// Docker options
	DockerHost      string
	RegistryURL     string
	PushAfterBuild  bool
	DisableBuildKit bool

	// Platform
	TargetPlatform string // e.g., "linux/amd64"

	// Build arguments
	BuildArgs map[string]string

	// Logging
	Verbose bool
}

// DefaultConfig returns a sensible default configuration
func DefaultConfig() BuildConfig {
	return BuildConfig{
		NixpacksCacheDir: "/var/cache/nixpacks",
		Timeout:          defaultBuildTimeout,
		MaxRetries:       2,
		DisableBuildKit:  false,
		Verbose:          false,
	}
}

// ============================================================================
// LOGGING
// ============================================================================

type BuildLogger interface {
	Info(format string, args ...interface{})
	Warn(format string, args ...interface{})
	Error(format string, args ...interface{})
	Debug(format string, args ...interface{})
}

type defaultLogger struct {
	verbose bool
}

func (l *defaultLogger) Info(format string, args ...interface{}) {
	log.Printf("[INFO] "+format, args...)
}

func (l *defaultLogger) Warn(format string, args ...interface{}) {
	log.Printf("[WARN] "+format, args...)
}

func (l *defaultLogger) Error(format string, args ...interface{}) {
	log.Printf("[ERROR] "+format, args...)
}

func (l *defaultLogger) Debug(format string, args ...interface{}) {
	if l.verbose {
		log.Printf("[DEBUG] "+format, args...)
	}
}

var logger BuildLogger = &defaultLogger{verbose: false}

// ============================================================================
// MAIN BUILD FUNCTION
// ============================================================================

// BuildCode builds application source into a local Docker image using Nixpacks.
func BuildCode(appPath string, appName string) (string, error) {
	return BuildCodeWithConfig(appPath, appName, DefaultConfig())
}

// BuildCodeWithConfig builds with custom configuration
func BuildCodeWithConfig(appPath string, appName string, config BuildConfig) (string, error) {
	// Set global logger verbosity
	if config.Verbose {
		logger = &defaultLogger{verbose: true}
	}

	// Validate inputs
	if err := validateInputs(appPath, appName); err != nil {
		return "", err
	}

	// Check system resources
	if err := checkSystemResources(); err != nil {
		return "", fmt.Errorf("system resource check failed: %w", err)
	}

	// Resolve absolute path
	absPath, err := filepath.Abs(appPath)
	if err != nil {
		return "", fmt.Errorf("resolve appPath: %w", err)
	}

	logger.Info("Building application: %s from %s", appName, absPath)

	// Check if Nixpacks is available
	if _, err := exec.LookPath("nixpacks"); err != nil {
		return "", fmt.Errorf(`nixpacks CLI not found in PATH. Please install nixpacks:
    curl -sSL https://nixpacks.com/install.sh | sh
    or visit: https://nixpacks.com/docs/install
    error: %w`, err)
	}

	// Detect project type - uses detectProjectKind from fastbuild.go
	kind := detectProjectKind(absPath)

	// Fast path for Node.js projects
	if kind != projectKindUnknown {
		logger.Info("Using fast builder for %q (%s)", appName, projectKindLabel(kind))

		ctx, cancel := context.WithTimeout(context.Background(), config.Timeout)
		defer cancel()

		if err := buildNodeDockerImageWithContext(ctx, absPath, appName, kind, config); err != nil {
			return "", fmt.Errorf("docker build failed: %w", err)
		}
		return dockerImageTag(appName, config.RegistryURL), nil
	}

	// Nixpacks path for all other projects
	logger.Info("Using Nixpacks builder for %q", appName)

	return buildWithNixpacks(absPath, appName, config)
}

// ============================================================================
// VALIDATION
// ============================================================================

func validateInputs(appPath, appName string) error {
	appPath = strings.TrimSpace(appPath)
	appName = strings.TrimSpace(appName)

	if appPath == "" {
		return &ValidationError{
			Field:   "appPath",
			Code:    "required",
			Message: "appPath is required",
		}
	}
	if appName == "" {
		return &ValidationError{
			Field:   "appName",
			Code:    "required",
			Message: "appName is required",
		}
	}

	// Validate appName format (docker image naming rules)
	if !isValidDockerImageName(appName) {
		return &ValidationError{
			Field:   "appName",
			Code:    "invalid_format",
			Message: "appName must be a valid Docker image name (lowercase, alphanumeric, underscores, hyphens, dots)",
		}
	}

	absPath, err := filepath.Abs(appPath)
	if err != nil {
		return fmt.Errorf("resolve appPath: %w", err)
	}

	info, err := os.Stat(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			return &ValidationError{
				Field:   "appPath",
				Code:    "not_found",
				Message: fmt.Sprintf("appPath %q does not exist", appPath),
			}
		}
		return fmt.Errorf("check appPath: %w", err)
	}
	if !info.IsDir() {
		return &ValidationError{
			Field:   "appPath",
			Code:    "invalid_type",
			Message: "appPath must point to a directory",
		}
	}

	return nil
}

func isValidDockerImageName(name string) bool {
	// Docker image name regex: [a-z0-9][a-z0-9._-]*
	if name == "" {
		return false
	}
	for i, ch := range name {
		if i == 0 {
			if !((ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9')) {
				return false
			}
		} else {
			if !((ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') ||
				ch == '.' || ch == '_' || ch == '-') {
				return false
			}
		}
	}
	return true
}

// ============================================================================
// SYSTEM RESOURCE CHECKS
// ============================================================================

func checkSystemResources() error {
	// Check available disk space
	var stat syscall.Statfs_t
	if err := syscall.Statfs("/", &stat); err != nil {
		return fmt.Errorf("failed to check disk space: %w", err)
	}

	freeBytes := stat.Bavail * uint64(stat.Bsize)
	freeGB := freeBytes / (1024 * 1024 * 1024)

	// minDiskSpaceGB is now in helpers.go
	if freeGB < minDiskSpaceGB {
		return fmt.Errorf("insufficient disk space: %dGB available, need at least %dGB", freeGB, minDiskSpaceGB)
	}

	logger.Debug("Free disk space: %dGB", freeGB)
	return nil
}

// ============================================================================
// PROJECT DETECTION - DEPRECATED (use fastbuild.go version)
// ============================================================================
// Note: detectProjectKind, hasNextConfig, hasViteConfig, and projectKindLabel
// are now in fastbuild.go with more comprehensive framework support.
// DO NOT redeclare them here.

// ============================================================================
// NODE.JS DOCKER BUILDER (FAST PATH)
// ============================================================================

func buildNodeDockerImageWithContext(ctx context.Context, absPath string, appName string, kind projectKind, config BuildConfig) error {
	// Use buildNodeDockerImageWithOptions from fastbuild.go
	opts := BuildOptions{
		Platform:        config.TargetPlatform,
		BuildArgs:       config.BuildArgs,
		Registry:        config.RegistryURL,
		Push:            config.PushAfterBuild,
		DisableBuildKit: config.DisableBuildKit,
		Timeout:         config.Timeout,
	}

	return buildNodeDockerImageWithOptions(absPath, appName, kind, opts)
}

// ============================================================================
// NIXPACKS BUILDER
// ============================================================================

func buildWithNixpacks(absPath string, appName string, config BuildConfig) (string, error) {
	// Determine cache strategy
	cacheDir := config.NixpacksCacheDir
	if cacheDir == "" {
		cacheDir = os.Getenv("NIXPACKS_CACHE_DIR")
	}
	if cacheDir == "" {
		cacheDir = "/var/cache/nixpacks"
	}

	useCacheDir := !strings.EqualFold(cacheDir, "off") && !strings.EqualFold(cacheDir, "none")

	ctx, cancel := context.WithTimeout(context.Background(), config.Timeout)
	defer cancel()

	var lastErr error
	for attempt := 1; attempt <= config.MaxRetries; attempt++ {
		logger.Info("Nixpacks build attempt %d/%d", attempt, config.MaxRetries)

		var err error
		if useCacheDir {
			err = runNixpacksBuild(ctx, absPath, appName, cacheDir, config)
		} else {
			err = runNixpacksBuild(ctx, absPath, appName, "", config)
		}

		if err == nil {
			logger.Info("Nixpacks build completed successfully")
			return dockerImageTag(appName, config.RegistryURL), nil
		}

		lastErr = err

		// Check if it's a retryable error - isRetryableError is now in helpers.go
		if !isRetryableError(err) {
			logger.Warn("Non-retryable error, stopping")
			break
		}

		if attempt < config.MaxRetries {
			logger.Warn("Build failed, retrying in 5 seconds...")
			select {
			case <-ctx.Done():
				return "", fmt.Errorf("build cancelled: %w", ctx.Err())
			case <-time.After(5 * time.Second):
				continue
			}
		}
	}

	return "", fmt.Errorf("nixpacks build failed after %d attempts: %w", config.MaxRetries, lastErr)
}

func runNixpacksBuild(ctx context.Context, absPath string, appName string, cacheDir string, config BuildConfig) error {
	// Use dockerCommandEnv from docker_env.go
	err := runNixpacksBuildWithEnv(ctx, absPath, appName, cacheDir, dockerCommandEnv())

	// isBuildKitMissingError is now in helpers.go
	if err != nil && isBuildKitMissingError(err) {
		logger.Warn("BuildKit unavailable, retrying with legacy docker builder")
		if retryErr := runNixpacksBuildWithEnv(ctx, absPath, appName, cacheDir, dockerCommandEnvForceLegacyBuilder()); retryErr == nil {
			return nil
		}
	}

	if err != nil && isUnsupportedCacheDirError(err) {
		logger.Warn("--cache-dir unsupported, retrying with native cache")
		if retryErr := runNixpacksBuildWithEnv(ctx, absPath, appName, "", dockerCommandEnv()); retryErr == nil {
			return nil
		}
	}

	if err != nil && isBuildKitMissingError(err) {
		return fmt.Errorf("%w (install docker-buildx or set DISABLE_BUILDKIT=1)", err)
	}

	return err
}

func runNixpacksBuildWithEnv(ctx context.Context, absPath string, appName string, cacheDir string, env []string) error {
	args := []string{"build", absPath, "--name", appName}

	if cacheDir != "" {
		args = append(args, "--cache-dir", cacheDir)
	}

	// Add verbose flag if configured
	if logger.(*defaultLogger).verbose {
		args = append(args, "--verbose")
	}

	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "nixpacks", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = io.MultiWriter(os.Stderr, &stderr)
	cmd.Env = env

	logger.Debug("Running: nixpacks %s", strings.Join(args, " "))

	if err := cmd.Run(); err != nil {
		if trimmed := strings.TrimSpace(stderr.String()); trimmed != "" {
			return fmt.Errorf("%w: %s", err, trimmed)
		}
		return err
	}

	return nil
}

// ============================================================================
// DOCKER HELPERS
// ============================================================================

func dockerImageTag(appName string, registryURL string) string {
	tag := strings.ToLower(appName)
	if registryURL != "" {
		return fmt.Sprintf("%s/%s:latest", strings.TrimSuffix(registryURL, "/"), tag)
	}
	return fmt.Sprintf("%s:latest", tag)
}

// Note: dockerCommandEnv and dockerCommandEnvForceLegacyBuilder are now in docker_env.go
// DO NOT redeclare them here

func pushDockerImage(ctx context.Context, appName string, registryURL string) error {
	tag := dockerImageTag(appName, registryURL)
	logger.Info("Pushing image: %s", tag)

	cmd := exec.CommandContext(ctx, "docker", "push", tag)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	return cmd.Run()
}

// ============================================================================
// ERROR DETECTION HELPERS
// ============================================================================

// Note: isRetryableError is now in helpers.go - DO NOT redeclare here

func isUnsupportedCacheDirError(err error) bool {
	if err == nil {
		return false
	}

	s := strings.ToLower(err.Error())
	if !strings.Contains(s, "cache-dir") {
		return false
	}

	for _, marker := range []string{
		"unexpected argument",
		"wasn't expected",
		"isn't expected",
		"unrecognized",
		"unknown flag",
		"unknown option",
		"invalid option",
		"found argument",
	} {
		if strings.Contains(s, marker) {
			return true
		}
	}

	return false
}

// Note: isBuildKitMissingError is now in helpers.go - DO NOT redeclare here

// ============================================================================
// USAGE EXAMPLE - REMOVED (moved to example_test.go)
// ============================================================================
// Example usage has been moved to example_test.go file