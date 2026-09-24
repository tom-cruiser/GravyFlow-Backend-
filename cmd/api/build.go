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
	// DockerfilePath, when set, builds that Dockerfile (relative to the repo
	// root, which stays the build context) instead of auto-detecting the
	// project — how monorepo services are built. See build_settings.go.
	DockerfilePath string

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

	// LogWriter receives the builder's output (the user-facing build log);
	// nil discards it. Output always also goes to the API's own stdout/stderr.
	LogWriter io.Writer
}

// DefaultConfig returns a sensible default configuration
func DefaultConfig() BuildConfig {
	return BuildConfig{
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

// BuildCodeWithLogs is BuildCode with the build output also written to logs.
func BuildCodeWithLogs(appPath string, appName string, logs io.Writer) (string, error) {
	return BuildCodeWithLogsAndDockerfile(appPath, appName, logs, "")
}

// BuildCodeWithLogsAndDockerfile is BuildCodeWithLogs for a service that names
// its own Dockerfile (dockerfilePath empty = auto-detect, as before).
func BuildCodeWithLogsAndDockerfile(appPath string, appName string, logs io.Writer, dockerfilePath string) (string, error) {
	config := DefaultConfig()
	config.LogWriter = logs
	config.DockerfilePath = dockerfilePath
	return BuildCodeWithConfig(appPath, appName, config)
}

// buildWithDockerfile builds an explicitly chosen Dockerfile with the repo
// root as the context. It skips language detection entirely, so it works for
// anything with a Dockerfile — including monorepo services that have no start
// command at the root.
func buildWithDockerfile(absPath string, appName string, config BuildConfig) (string, error) {
	dockerfile, err := resolveDockerfile(absPath, config.DockerfilePath)
	if err != nil {
		if found := findDockerfiles(absPath, 12); len(found) > 0 {
			return "", fmt.Errorf("%w (Dockerfiles in this repository: %s)", err, strings.Join(found, ", "))
		}
		return "", err
	}

	tag := dockerImageTag(appName, config.RegistryURL)
	fmt.Fprintf(logSink(config.LogWriter), "==> Building %s with Docker\n", config.DockerfilePath)

	opts := BuildOptions{
		Platform:        config.TargetPlatform,
		BuildArgs:       config.BuildArgs,
		Timeout:         config.Timeout,
		DisableBuildKit: config.DisableBuildKit,
		LogWriter:       config.LogWriter,
	}
	if err := runDockerBuildWithOptions(absPath, tag, dockerfile, opts); err != nil {
		return "", fmt.Errorf("docker build failed: %w", err)
	}
	return tag, nil
}

// BuildCodeWithConfig builds with custom configuration
func BuildCodeWithConfig(appPath string, appName string, config BuildConfig) (string, error) {
	// Set global logger verbosity
	if config.Verbose {
		logger = &defaultLogger{verbose: true}
	}

	// Docker/nixpacks require a lowercase tag with no leading/trailing
	// separator; a display name taken straight from a GitHub repo (mixed
	// case, or literally ending in "-" like "GravyFlow-Backend-") can
	// violate that. Sanitize once, up front, so every downstream build path
	// (nixpacks, the Node fast builder) and the tag this function returns
	// all agree on the same valid name — otherwise an image can build
	// successfully under one name and be un-findable under another.
	appName = dockerSafeTag(appName)

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

	if err := ensureDockerignore(absPath); err != nil {
		logger.Warn("could not write default .dockerignore for %q: %v", appName, err)
	}

	// An explicit Dockerfile wins over any detection (and needs no Nixpacks).
	if strings.TrimSpace(config.DockerfilePath) != "" {
		return buildWithDockerfile(absPath, appName, config)
	}

	// Check if Nixpacks is available
	if _, err := exec.LookPath("nixpacks"); err != nil {
		return "", fmt.Errorf(`nixpacks CLI not found in PATH. Please install nixpacks:
    curl -sSL https://nixpacks.com/install.sh | sh
    or visit: https://nixpacks.com/docs/install
    error: %w`, err)
	}

	// Detect project type - uses detectProjectKind from fastbuild.go
	kind := detectProjectKind(absPath)

	// Fast path for Node.js projects. Python/Go/Rust are detected too, but the
	// fast builder only has Node Dockerfiles, so those go through Nixpacks.
	if isNodeProjectKind(kind) {
		logger.Info("Using fast builder for %q (%s)", appName, projectKindLabel(kind))
		fmt.Fprintf(logSink(config.LogWriter), "==> Detected a %s project; building with a generated Dockerfile\n", projectKindLabel(kind))

		ctx, cancel := context.WithTimeout(context.Background(), config.Timeout)
		defer cancel()

		if err := buildNodeDockerImageWithContext(ctx, absPath, appName, kind, config); err != nil {
			return "", fmt.Errorf("docker build failed: %w", err)
		}
		return dockerImageTag(appName, config.RegistryURL), nil
	}

	// Nixpacks path for all other projects
	logger.Info("Using Nixpacks builder for %q", appName)
	fmt.Fprintf(logSink(config.LogWriter), "==> Building with Nixpacks (it detects the language itself)\n")

	return buildWithNixpacks(absPath, appName, config)
}

// defaultDockerignore keeps VCS data and host-built artifacts out of the build
// context. Without it every build uploads .git and any committed node_modules
// (hundreds of MB for some repos), and `COPY . .` then overwrites the
// freshly installed node_modules with the host's copy.
const defaultDockerignore = `.git
node_modules
.next
.nuxt
.svelte-kit
.turbo
.cache
npm-debug.log*
yarn-error.log*
`

// ensureDockerignore writes defaultDockerignore into checkouts GravyFlow owns
// (under appBuildRoot) when the repo doesn't ship its own. It never touches a
// user-supplied local directory; the file is untracked, so the next git sync
// cleans it and the next build writes it again.
func ensureDockerignore(absPath string) error {
	root, err := filepath.Abs(appBuildRoot())
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(root, absPath)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return nil
	}

	path := filepath.Join(absPath, ".dockerignore")
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	return os.WriteFile(path, []byte(defaultDockerignore), 0o644)
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
	// Docker image name regex: [a-z0-9][a-z0-9._-]*[a-z0-9] — note the name
	// must also *end* in an alphanumeric; a trailing separator (e.g. a repo
	// literally named "my-app-") is just as invalid to Docker as a leading
	// one, but was never checked here, letting it slip through to nixpacks
	// (which fails with a much less obvious "invalid reference format").
	if name == "" {
		return false
	}
	runes := []rune(name)
	last := len(runes) - 1
	for i, ch := range runes {
		if i == 0 || i == last {
			if !((ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9')) {
				return false
			}
			continue
		}
		if !((ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') ||
			ch == '.' || ch == '_' || ch == '-') {
			return false
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
		LogWriter:       config.LogWriter,
	}

	return buildNodeDockerImageWithOptions(absPath, appName, kind, opts)
}

// ============================================================================
// NIXPACKS BUILDER
// ============================================================================

func buildWithNixpacks(absPath string, appName string, config BuildConfig) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), config.Timeout)
	defer cancel()

	var lastErr error
	for attempt := 1; attempt <= config.MaxRetries; attempt++ {
		logger.Info("Nixpacks build attempt %d/%d", attempt, config.MaxRetries)

		err := runNixpacksBuild(ctx, absPath, appName, config.LogWriter)

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

// runNixpacksBuild runs `nixpacks build`. Nixpacks has no cache-directory flag
// (only --cache-key/--cache-from/--no-cache); its layer cache lives in the
// Docker daemon and is keyed by the source path, which is stable per
// deployment, so no extra flag is needed.
func runNixpacksBuild(ctx context.Context, absPath string, appName string, logs io.Writer) error {
	// Use dockerCommandEnv from docker_env.go
	err := runNixpacksBuildWithEnv(ctx, absPath, appName, dockerCommandEnv(), logs)

	// isBuildKitMissingError is now in helpers.go
	if err != nil && isBuildKitMissingError(err) {
		logger.Warn("BuildKit unavailable, retrying with legacy docker builder")
		// Report the retry's own error: the first one only says BuildKit is
		// missing, which is no longer the reason the build failed.
		if err = runNixpacksBuildWithEnv(ctx, absPath, appName, dockerCommandEnvForceLegacyBuilder(), logs); err == nil {
			return nil
		}
		if isBuildKitMissingError(err) {
			return fmt.Errorf("%w (install docker-buildx or set DISABLE_BUILDKIT=1)", err)
		}
	}

	return err
}

func runNixpacksBuildWithEnv(ctx context.Context, absPath string, appName string, env []string, logs io.Writer) error {
	args := []string{"build", absPath, "--name", appName}

	// Add verbose flag if configured
	if logger.(*defaultLogger).verbose {
		args = append(args, "--verbose")
	}

	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "nixpacks", args...)
	cmd.Stdout = io.MultiWriter(os.Stdout, logSink(logs))
	cmd.Stderr = io.MultiWriter(os.Stderr, &stderr, logSink(logs))
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

// dockerSafeTag turns an arbitrary display name (e.g. a GitHub repo name,
// which can carry uppercase letters or literally end in "-") into a string
// that is always a valid Docker image tag component: lowercase, alphanumeric
// runs joined by single "-" separators, never starting or ending in one.
// nixpacks and `docker build -t` both reject anything else outright with
// "invalid reference format" — so this must run before the name is ever
// handed to either, and idempotently (safe to call again on its own output).
func dockerSafeTag(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))

	var b strings.Builder
	lastWasSeparator := true // drop a leading separator instead of doubling it
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastWasSeparator = false
		default:
			if !lastWasSeparator {
				b.WriteRune('-')
				lastWasSeparator = true
			}
		}
	}

	tag := strings.Trim(b.String(), "-")
	if tag == "" {
		return "app"
	}
	return tag
}

func dockerImageTag(appName string, registryURL string) string {
	tag := dockerSafeTag(appName)
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

// Note: isBuildKitMissingError is now in helpers.go - DO NOT redeclare here

// ============================================================================
// USAGE EXAMPLE - REMOVED (moved to example_test.go)
// ============================================================================
// Example usage has been moved to example_test.go file