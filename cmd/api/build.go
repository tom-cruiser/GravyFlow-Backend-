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
	"strings"
	"syscall"
	"time"
)

// ============================================================================
// EMBEDDED FILES
// ============================================================================

//go:embed Dockerfile.node
var nodeDockerfileTemplate string

// ============================================================================
// TYPES AND CONSTANTS
// ============================================================================

type ProjectKind int

const (
	projectKindUnknown ProjectKind = iota
	projectKindNode
	projectKindVite
	projectKindNextJS
	projectKindReact
	projectKindAngular
	projectKindPython
	projectKindGo
	projectKindRust
)

const (
	defaultBuildTimeout = 30 * time.Minute
	minDiskSpaceGB      = 5
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
	DockerHost       string
	RegistryURL      string
	PushAfterBuild   bool
	DisableBuildKit  bool
	
	// Platform
	TargetPlatform   string // e.g., "linux/amd64"
	
	// Build arguments
	BuildArgs        map[string]string
	
	// Logging
	Verbose          bool
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

	// Detect project type
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

	if freeGB < minDiskSpaceGB {
		return fmt.Errorf("insufficient disk space: %dGB available, need at least %dGB", freeGB, minDiskSpaceGB)
	}

	logger.Debug("Free disk space: %dGB", freeGB)
	return nil
}

// ============================================================================
// PROJECT DETECTION
// ============================================================================

func detectProjectKind(absPath string) ProjectKind {
	// Check for Node.js projects
	packageJSON := filepath.Join(absPath, "package.json")
	if _, err := os.Stat(packageJSON); err == nil {
		// Read package.json to detect framework
		content, err := os.ReadFile(packageJSON)
		if err == nil {
			// Check for Next.js
			if strings.Contains(string(content), `"next"`) {
				if hasNextConfig(absPath) {
					return projectKindNextJS
				}
			}
			// Check for Vite
			if strings.Contains(string(content), `"vite"`) {
				if hasViteConfig(absPath) {
					return projectKindVite
				}
			}
			// Check for React
			if strings.Contains(string(content), `"react"`) {
				return projectKindReact
			}
			// Check for Angular
			if strings.Contains(string(content), `"@angular/core"`) {
				return projectKindAngular
			}
		}
		return projectKindNode
	}

	// Check for Python
	if _, err := os.Stat(filepath.Join(absPath, "requirements.txt")); err == nil {
		return projectKindPython
	}
	if _, err := os.Stat(filepath.Join(absPath, "pyproject.toml")); err == nil {
		return projectKindPython
	}

	// Check for Go
	if _, err := os.Stat(filepath.Join(absPath, "go.mod")); err == nil {
		return projectKindGo
	}

	// Check for Rust
	if _, err := os.Stat(filepath.Join(absPath, "Cargo.toml")); err == nil {
		return projectKindRust
	}

	return projectKindUnknown
}

func hasNextConfig(absPath string) bool {
	configs := []string{"next.config.js", "next.config.ts", "next.config.mjs", "next.config.cjs"}
	for _, config := range configs {
		if _, err := os.Stat(filepath.Join(absPath, config)); err == nil {
			return true
		}
	}
	return false
}

func hasViteConfig(absPath string) bool {
	configs := []string{"vite.config.js", "vite.config.ts", "vite.config.mjs", "vite.config.cjs"}
	for _, config := range configs {
		if _, err := os.Stat(filepath.Join(absPath, config)); err == nil {
			return true
		}
	}
	return false
}

func projectKindLabel(kind ProjectKind) string {
	switch kind {
	case projectKindNode:
		return "node"
	case projectKindVite:
		return "vite"
	case projectKindNextJS:
		return "nextjs"
	case projectKindReact:
		return "react"
	case projectKindAngular:
		return "angular"
	case projectKindPython:
		return "python"
	case projectKindGo:
		return "go"
	case projectKindRust:
		return "rust"
	default:
		return "unknown"
	}
}

// ============================================================================
// NODE.JS DOCKER BUILDER (FAST PATH)
// ============================================================================

func buildNodeDockerImageWithContext(ctx context.Context, absPath string, appName string, kind ProjectKind, config BuildConfig) error {
	// Create Dockerfile
	dockerfilePath := filepath.Join(absPath, "Dockerfile.build")
	if err := createNodeDockerfile(dockerfilePath, kind); err != nil {
		return fmt.Errorf("create Dockerfile: %w", err)
	}
	defer os.Remove(dockerfilePath)

	// Build Docker image
	args := []string{
		"build",
		"-f", dockerfilePath,
		"-t", dockerImageTag(appName, config.RegistryURL),
		".",
	}

	// Add platform if specified
	if config.TargetPlatform != "" {
		args = append([]string{"build", "--platform", config.TargetPlatform}, args[1:]...)
	}

	// Add build arguments
	for key, value := range config.BuildArgs {
		args = append(args, "--build-arg", fmt.Sprintf("%s=%s", key, value))
	}

	// Disable BuildKit if requested
	if config.DisableBuildKit {
		args = append([]string{"build", "--disable-buildkit"}, args[1:]...)
	}

	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Dir = absPath
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	logger.Debug("Running: docker %s", strings.Join(args, " "))
	
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker build failed: %w", err)
	}

	// Push to registry if requested
	if config.PushAfterBuild && config.RegistryURL != "" {
		if err := pushDockerImage(ctx, appName, config.RegistryURL); err != nil {
			return fmt.Errorf("push to registry failed: %w", err)
		}
	}

	return nil
}

func createNodeDockerfile(path string, kind ProjectKind) error {
	var baseImage, buildCmd, startCmd, exposePort string
	
	switch kind {
	case projectKindNextJS:
		baseImage = "node:20-alpine"
		buildCmd = `RUN npm ci && npm run build`
		startCmd = `CMD ["npm", "start"]`
		exposePort = "EXPOSE 3000"
		
	case projectKindVite:
		baseImage = "node:20-alpine"
		buildCmd = `RUN npm ci && npm run build`
		startCmd = `CMD ["npm", "run", "preview", "--", "--host", "0.0.0.0"]`
		exposePort = "EXPOSE 4173"
		
	case projectKindReact:
		baseImage = "node:20-alpine"
		buildCmd = `RUN npm ci && npm run build`
		startCmd = `CMD ["npx", "serve", "-s", "build", "-l", "3000"]`
		exposePort = "EXPOSE 3000"
		
	case projectKindAngular:
		baseImage = "node:20-alpine"
		buildCmd = `RUN npm ci && npm run build -- --output-path=dist`
		startCmd = `CMD ["npx", "serve", "-s", "dist", "-l", "4200"]`
		exposePort = "EXPOSE 4200"
		
	default: // Node
		baseImage = "node:20-alpine"
		buildCmd = `RUN npm ci --only=production`
		startCmd = `CMD ["npm", "start"]`
		exposePort = "EXPOSE 3000"
	}

	content := fmt.Sprintf(`# Auto-generated Dockerfile for Node.js application
FROM %s AS builder
WORKDIR /app
COPY package*.json ./
%s
COPY . .

FROM %s AS runner
WORKDIR /app
COPY --from=builder /app ./
%s
%s
`, baseImage, buildCmd, baseImage, exposePort, startCmd)

	return os.WriteFile(path, []byte(content), 0644)
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
		
		// Check if it's a retryable error
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
	err := runNixpacksBuildWithEnv(ctx, absPath, appName, cacheDir, dockerCommandEnv(config))
	
	if err != nil && isBuildKitMissingError(err) {
		logger.Warn("BuildKit unavailable, retrying with legacy docker builder")
		if retryErr := runNixpacksBuildWithEnv(ctx, absPath, appName, cacheDir, dockerCommandEnvForceLegacyBuilder()); retryErr == nil {
			return nil
		}
	}

	if err != nil && isUnsupportedCacheDirError(err) {
		logger.Warn("--cache-dir unsupported, retrying with native cache")
		if retryErr := runNixpacksBuildWithEnv(ctx, absPath, appName, "", dockerCommandEnv(config)); retryErr == nil {
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

func dockerCommandEnv(config BuildConfig) []string {
	env := os.Environ()
	
	// Filter out DISABLE_BUILDKIT if not requested
	if !config.DisableBuildKit {
		var filtered []string
		for _, e := range env {
			if !strings.HasPrefix(e, "DISABLE_BUILDKIT=") {
				filtered = append(filtered, e)
			}
		}
		env = filtered
	}
	
	// Set DOCKER_HOST if specified
	if config.DockerHost != "" {
		env = append(env, fmt.Sprintf("DOCKER_HOST=%s", config.DockerHost))
	}
	
	return env
}

func dockerCommandEnvForceLegacyBuilder() []string {
	env := os.Environ()
	return append(env, "DISABLE_BUILDKIT=1")
}

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

func isRetryableError(err error) bool {
	if err == nil {
		return false
	}
	
	s := strings.ToLower(err.Error())
	retryableMarkers := []string{
		"connection refused",
		"timeout",
		"temporary failure",
		"network",
		"dial tcp",
		"unexpected EOF",
	}
	
	for _, marker := range retryableMarkers {
		if strings.Contains(s, marker) {
			return true
		}
	}
	
	return false
}

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

func isBuildKitMissingError(err error) bool {
	if err == nil {
		return false
	}
	
	s := strings.ToLower(err.Error())
	markers := []string{
		"buildkit",
		"docker buildx",
		"docker build requires buildkit",
		"failed to create buildkit client",
		"buildx not found",
	}
	
	for _, marker := range markers {
		if strings.Contains(s, marker) {
			return true
		}
	}
	
	return false
}

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
// USAGE EXAMPLE
// ============================================================================

func ExampleUsage() {
	// Simple usage with defaults
	tag, err := BuildCode("/path/to/my/app", "my-app")
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("Built image: %s", tag)

	// Advanced usage with custom config
	config := DefaultConfig()
	config.Timeout = 45 * time.Minute
	config.RegistryURL = "docker.io/myregistry"
	config.PushAfterBuild = true
	config.Verbose = true
	config.BuildArgs = map[string]string{
		"VERSION": "1.0.0",
		"ENV":     "production",
	}

	tag, err = BuildCodeWithConfig("/path/to/my/app", "my-app", config)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("Built and pushed image: %s", tag)
}