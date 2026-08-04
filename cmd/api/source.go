package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ============================================================================
// CONSTANTS
// ============================================================================

const (
	gitCloneRetryCount    = 3
	gitCloneRetryDelay    = 5 * time.Second
	gitCommandTimeout     = 10 * time.Minute
	defaultGitDepth       = 1
	maxGitDepth           = 100
)

// ============================================================================
// TYPES
// ============================================================================

type GitCloneOptions struct {
	Branch      string
	Tag         string
	Commit      string
	Depth       int
	Submodules  bool
	SparseCheckout []string
	SSHKeyPath  string
	Username    string
	Password    string
	Token       string
}

type GitStatus struct {
	Branch      string `json:"branch"`
	CommitHash  string `json:"commitHash"`
	CommitDate  string `json:"commitDate"`
	Author      string `json:"author"`
	Message     string `json:"message"`
	IsDirty     bool   `json:"isDirty"`
}

type CloneProgress struct {
	Stage       string `json:"stage"`
	Current     int64  `json:"current"`
	Total       int64  `json:"total"`
	Percentage  int    `json:"percentage"`
	Message     string `json:"message"`
	Timestamp   time.Time `json:"timestamp"`
}

// ============================================================================
// ENVIRONMENT VARIABLES
// ============================================================================

func appBuildRoot() string {
	if value := strings.TrimSpace(os.Getenv("GRAVYFLOW_APPS_DIR")); value != "" {
		return value
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".gravyflow", "apps")
	}
	return "/var/lib/gravyflow/apps"
}

func gitSSHKeyPath() string {
	if value := strings.TrimSpace(os.Getenv("GIT_SSH_KEY_PATH")); value != "" {
		return value
	}
	return filepath.Join(os.Getenv("HOME"), ".ssh", "id_rsa")
}

func gitSSHCommand() string {
	keyPath := gitSSHKeyPath()
	return fmt.Sprintf("ssh -i %s -o StrictHostKeyChecking=accept-new", keyPath)
}

// ============================================================================
// PATH HELPERS
// ============================================================================

func isRemoteRepoSource(source string) bool {
	source = strings.TrimSpace(source)
	return strings.HasPrefix(source, "http://") ||
		strings.HasPrefix(source, "https://") ||
		strings.HasPrefix(source, "git@") ||
		strings.HasPrefix(source, "ssh://")
}

func isGitURL(source string) bool {
	return strings.HasSuffix(source, ".git") || strings.Contains(source, "://")
}

func resolveRepoURL(deployment DeploymentRecord) string {
	if repo := strings.TrimSpace(deployment.SourceRepoURL); repo != "" {
		return repo
	}
	return strings.TrimSpace(deployment.AppPath)
}

// ============================================================================
// MAIN FUNCTION
// ============================================================================

// prepareDeploymentSource ensures deployment.AppPath points at a local checkout.
// Remote repo URLs are shallow-cloned into GRAVYFLOW_APPS_DIR/{deploymentId}.
func prepareDeploymentSource(ctx context.Context, deployment DeploymentRecord) (string, error) {
	return prepareDeploymentSourceWithOptions(ctx, deployment, GitCloneOptions{})
}

func prepareDeploymentSourceWithOptions(ctx context.Context, deployment DeploymentRecord, opts GitCloneOptions) (string, error) {
	appPath := strings.TrimSpace(deployment.AppPath)
	if appPath != "" && !isRemoteRepoSource(appPath) {
		absPath, err := filepath.Abs(appPath)
		if err != nil {
			return "", fmt.Errorf("resolve app path: %w", err)
		}
		info, err := os.Stat(absPath)
		if err != nil {
			return "", fmt.Errorf("app path %q is not accessible: %w", absPath, err)
		}
		if !info.IsDir() {
			return "", fmt.Errorf("app path %q must be a directory", absPath)
		}
		return absPath, nil
	}

	repoURL := resolveRepoURL(deployment)
	if repoURL == "" {
		return "", fmt.Errorf("source repository URL is required")
	}
	if !isRemoteRepoSource(repoURL) {
		return "", fmt.Errorf("source repository %q is not a valid remote URL", repoURL)
	}

	// Check disk space before cloning
	root := appBuildRoot()
	if err := checkDiskSpace(root); err != nil {
		return "", err
	}

	dest := filepath.Join(root, deployment.DeploymentID)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return "", fmt.Errorf("create app build root: %w", err)
	}

	// Check if we need to clone or sync
	if _, err := os.Stat(filepath.Join(dest, ".git")); err == nil {
		// Existing repo - sync
		if err := runGitSync(ctx, dest, repoURL, opts); err != nil {
			return "", err
		}
	} else {
		// New clone - remove any existing directory
		if err := os.RemoveAll(dest); err != nil {
			return "", fmt.Errorf("reset app checkout directory: %w", err)
		}
		if err := runGitClone(ctx, repoURL, dest, opts); err != nil {
			return "", err
		}
	}

	// Update deployment path
	if err := deploymentStore.UpdateDeploymentAppPath(ctx, deployment.DeploymentID, dest); err != nil {
		return "", err
	}

	return dest, nil
}

// ============================================================================
// DISK SPACE CHECK
// ============================================================================

func checkDiskSpace(path string) error {
	// Check if we have enough space
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		// If we can't check, continue anyway
		return nil
	}

	freeBytes := stat.Bavail * uint64(stat.Bsize)
	freeGB := freeBytes / (1024 * 1024 * 1024)

	if freeGB < 1 {
		return fmt.Errorf("insufficient disk space: %dGB available, need at least 1GB", freeGB)
	}
	return nil
}

// ============================================================================
// GIT CLONE
// ============================================================================

func runGitClone(ctx context.Context, repoURL string, dest string, opts GitCloneOptions) error {
	if _, err := exec.LookPath("git"); err != nil {
		return fmt.Errorf("git CLI not found in PATH: %w", err)
	}

	// Set timeout
	ctx, cancel := context.WithTimeout(ctx, gitCommandTimeout)
	defer cancel()

	args := []string{
		"clone",
		"--depth", fmt.Sprintf("%d", getDepth(opts.Depth)),
	}

	// Add filter if not using full depth
	if getDepth(opts.Depth) > 0 && getDepth(opts.Depth) < 100 {
		args = append(args, "--filter", "blob:none")
	}

	args = append(args, "--single-branch")

	// Add branch or tag
	if opts.Branch != "" {
		args = append(args, "--branch", opts.Branch)
	} else if opts.Tag != "" {
		args = append(args, "--branch", opts.Tag)
	}

	// Add sparse checkout
	if len(opts.SparseCheckout) > 0 {
		args = append(args, "--sparse")
	}

	args = append(args, repoURL, dest)

	// Add authentication
	cloneURL, err := addAuthentication(repoURL, opts)
	if err != nil {
		return err
	}

	// Use authenticated URL
	args[1] = cloneURL

	// Setup environment
	env := os.Environ()
	if opts.SSHKeyPath != "" {
		env = append(env, fmt.Sprintf("GIT_SSH_COMMAND=ssh -i %s -o StrictHostKeyChecking=accept-new", opts.SSHKeyPath))
	}

	// Retry on failure
	var lastErr error
	for attempt := 1; attempt <= gitCloneRetryCount; attempt++ {
		if attempt > 1 {
			time.Sleep(gitCloneRetryDelay)
		}

		err := runGitCommand(ctx, env, args...)
		if err == nil {
			// If sparse checkout, add patterns
			if len(opts.SparseCheckout) > 0 {
				if err := setupSparseCheckout(ctx, dest, opts.SparseCheckout); err != nil {
					return err
				}
			}

			// Handle submodules
			if opts.Submodules {
				if err := initSubmodules(ctx, dest, opts); err != nil {
					return err
				}
			}

			return nil
		}
		lastErr = err
	}

	return fmt.Errorf("git clone failed after %d attempts: %w", gitCloneRetryCount, lastErr)
}

// ============================================================================
// GIT SYNC
// ============================================================================

func runGitSync(ctx context.Context, dest string, repoURL string, opts GitCloneOptions) error {
	if _, err := exec.LookPath("git"); err != nil {
		return fmt.Errorf("git CLI not found in PATH: %w", err)
	}

	ctx, cancel := context.WithTimeout(ctx, gitCommandTimeout)
	defer cancel()

	// Setup environment
	env := os.Environ()
	if opts.SSHKeyPath != "" {
		env = append(env, fmt.Sprintf("GIT_SSH_COMMAND=ssh -i %s -o StrictHostKeyChecking=accept-new", opts.SSHKeyPath))
	}

	// Add authentication
	cloneURL, err := addAuthentication(repoURL, opts)
	if err != nil {
		return err
	}

	// Update remote URL
	if err := runGitCommand(ctx, env, "-C", dest, "remote", "set-url", "origin", cloneURL); err != nil {
		return err
	}

	// Fetch latest changes
	fetchArgs := []string{"-C", dest, "fetch", "--depth", fmt.Sprintf("%d", getDepth(opts.Depth))}
	if opts.Branch != "" {
		fetchArgs = append(fetchArgs, "origin", opts.Branch)
	} else {
		fetchArgs = append(fetchArgs, "origin")
	}

	if err := runGitCommand(ctx, env, fetchArgs...); err != nil {
		return err
	}

	// Reset to latest
	if err := runGitCommand(ctx, env, "-C", dest, "reset", "--hard", "FETCH_HEAD"); err != nil {
		return err
	}

	// Clean untracked files
	if err := runGitCommand(ctx, env, "-C", dest, "clean", "-fd"); err != nil {
		return err
	}

	// Handle submodules
	if opts.Submodules {
		if err := initSubmodules(ctx, dest, opts); err != nil {
			return err
		}
	}

	return nil
}

// ============================================================================
// SUBMODULE HANDLING
// ============================================================================

func initSubmodules(ctx context.Context, dest string, opts GitCloneOptions) error {
	// Check if submodules exist
	submoduleFile := filepath.Join(dest, ".gitmodules")
	if _, err := os.Stat(submoduleFile); os.IsNotExist(err) {
		return nil
	}

	env := os.Environ()
	if opts.SSHKeyPath != "" {
		env = append(env, fmt.Sprintf("GIT_SSH_COMMAND=ssh -i %s -o StrictHostKeyChecking=accept-new", opts.SSHKeyPath))
	}

	// Init submodules
	if err := runGitCommand(ctx, env, "-C", dest, "submodule", "update", "--init", "--recursive", "--depth", "1"); err != nil {
		return fmt.Errorf("init submodules: %w", err)
	}

	return nil
}

// ============================================================================
// SPARSE CHECKOUT
// ============================================================================

func setupSparseCheckout(ctx context.Context, dest string, patterns []string) error {
	// Enable sparse checkout
	if err := runGitCommand(ctx, nil, "-C", dest, "sparse-checkout", "init"); err != nil {
		return err
	}

	// Set patterns
	args := []string{"-C", dest, "sparse-checkout", "set"}
	args = append(args, patterns...)

	if err := runGitCommand(ctx, nil, args...); err != nil {
		return err
	}

	return nil
}

// ============================================================================
// AUTHENTICATION
// ============================================================================

func addAuthentication(repoURL string, opts GitCloneOptions) (string, error) {
	if opts.Token != "" {
		return addTokenAuth(repoURL, opts.Token), nil
	}
	if opts.Username != "" && opts.Password != "" {
		return addBasicAuth(repoURL, opts.Username, opts.Password), nil
	}
	return repoURL, nil
}

func addTokenAuth(repoURL, token string) string {
	if strings.HasPrefix(repoURL, "https://") {
		parts := strings.SplitN(repoURL, "://", 2)
		if len(parts) == 2 {
			return fmt.Sprintf("https://x-access-token:%s@%s", token, parts[1])
		}
	}
	return repoURL
}

func addBasicAuth(repoURL, username, password string) string {
	if strings.HasPrefix(repoURL, "https://") {
		parts := strings.SplitN(repoURL, "://", 2)
		if len(parts) == 2 {
			return fmt.Sprintf("https://%s:%s@%s", username, password, parts[1])
		}
	}
	return repoURL
}

// ============================================================================
// GIT COMMAND
// ============================================================================

func runGitCommand(ctx context.Context, env []string, args ...string) error {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = io.MultiWriter(os.Stderr, &stderr)
	
	if env != nil {
		cmd.Env = env
	}

	if err := cmd.Run(); err != nil {
		if trimmed := strings.TrimSpace(stderr.String()); trimmed != "" {
			return fmt.Errorf("git %s failed: %w: %s", strings.Join(args, " "), err, trimmed)
		}
		return fmt.Errorf("git %s failed: %w", strings.Join(args, " "), err)
	}

	return nil
}

// ============================================================================
// GIT STATUS
// ============================================================================

func GetGitStatus(ctx context.Context, path string) (GitStatus, error) {
	var status GitStatus

	// Get branch
	branch, err := getGitOutput(ctx, path, "rev-parse", "--abbrev-ref", "HEAD")
	if err == nil {
		status.Branch = strings.TrimSpace(branch)
	}

	// Get commit hash
	hash, err := getGitOutput(ctx, path, "rev-parse", "HEAD")
	if err == nil {
		status.CommitHash = strings.TrimSpace(hash)
	}

	// Get commit date
	date, err := getGitOutput(ctx, path, "log", "-1", "--format=%ci", "HEAD")
	if err == nil {
		status.CommitDate = strings.TrimSpace(date)
	}

	// Get author
	author, err := getGitOutput(ctx, path, "log", "-1", "--format=%an", "HEAD")
	if err == nil {
		status.Author = strings.TrimSpace(author)
	}

	// Get message
	message, err := getGitOutput(ctx, path, "log", "-1", "--format=%s", "HEAD")
	if err == nil {
		status.Message = strings.TrimSpace(message)
	}

	// Check if dirty
	dirty, err := getGitOutput(ctx, path, "status", "--porcelain")
	if err == nil && strings.TrimSpace(dirty) != "" {
		status.IsDirty = true
	}

	return status, nil
}

func getGitOutput(ctx context.Context, path string, args ...string) (string, error) {
	cmdArgs := []string{"-C", path}
	cmdArgs = append(cmdArgs, args...)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "git", cmdArgs...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s failed: %w: %s", strings.Join(args, " "), err, stderr.String())
	}

	return stdout.String(), nil
}

// ============================================================================
// CLEANUP OLD CHECKOUTS
// ============================================================================

func CleanupOldCheckouts(ctx context.Context, maxAge time.Duration, maxCount int) error {
	root := appBuildRoot()
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}

	var checkouts []os.DirEntry
	for _, entry := range entries {
		if entry.IsDir() {
			checkouts = append(checkouts, entry)
		}
	}

	// Sort by modification time (oldest first)
	// ... sorting logic

	// Remove old checkouts
	for _, entry := range checkouts {
		path := filepath.Join(root, entry.Name())
		info, err := entry.Info()
		if err != nil {
			continue
		}

		if time.Since(info.ModTime()) > maxAge {
			if err := os.RemoveAll(path); err != nil {
				fmt.Printf("[WARN] Failed to remove old checkout %s: %v\n", path, err)
			} else {
				fmt.Printf("[INFO] Removed old checkout: %s\n", path)
			}
		}
	}

	return nil
}

// ============================================================================
// HELPERS
// ============================================================================

func getDepth(requested int) int {
	if requested <= 0 {
		return defaultGitDepth
	}
	if requested > maxGitDepth {
		return maxGitDepth
	}
	return requested
}

// ============================================================================
// USAGE EXAMPLES
// ============================================================================

/*
EXAMPLE USAGE:

1. Basic clone:
   path, err := prepareDeploymentSource(ctx, deployment)

2. Clone with options:
   opts := GitCloneOptions{
       Branch:     "main",
       Depth:      1,
       Submodules: true,
       Token:      "github-token",
   }
   path, err := prepareDeploymentSourceWithOptions(ctx, deployment, opts)

3. Clone specific branch:
   opts := GitCloneOptions{
       Branch: "develop",
   }
   path, err := prepareDeploymentSourceWithOptions(ctx, deployment, opts)

4. Sparse checkout (only specific directories):
   opts := GitCloneOptions{
       SparseCheckout: []string{"src/", "package.json"},
   }
   path, err := prepareDeploymentSourceWithOptions(ctx, deployment, opts)

5. Get git status:
   status, err := GetGitStatus(ctx, "/path/to/repo")
   fmt.Printf("Branch: %s, Commit: %s\n", status.Branch, status.CommitHash)

6. Cleanup old checkouts:
   err := CleanupOldCheckouts(ctx, 7*24*time.Hour, 10)

7. SSH authentication:
   opts := GitCloneOptions{
       SSHKeyPath: "/path/to/private-key",
   }
   path, err := prepareDeploymentSourceWithOptions(ctx, deployment, opts)

8. Private repo with token:
   opts := GitCloneOptions{
       Token: "ghp_xxxxxxxxxxxx",
   }
   path, err := prepareDeploymentSourceWithOptions(ctx, deployment, opts)
*/