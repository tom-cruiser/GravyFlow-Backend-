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
)

func appBuildRoot() string {
	if value := strings.TrimSpace(os.Getenv("GRAVYFLOW_APPS_DIR")); value != "" {
		return value
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".gravyflow", "apps")
	}
	return "/var/lib/gravyflow/apps"
}

func isRemoteRepoSource(source string) bool {
	source = strings.TrimSpace(source)
	return strings.HasPrefix(source, "http://") ||
		strings.HasPrefix(source, "https://") ||
		strings.HasPrefix(source, "git@") ||
		strings.HasPrefix(source, "ssh://")
}

func resolveRepoURL(deployment DeploymentRecord) string {
	if repo := strings.TrimSpace(deployment.SourceRepoURL); repo != "" {
		return repo
	}
	return strings.TrimSpace(deployment.AppPath)
}

// prepareDeploymentSource ensures deployment.AppPath points at a local checkout.
// Remote repo URLs are shallow-cloned into GRAVYFLOW_APPS_DIR/{deploymentId}.
func prepareDeploymentSource(ctx context.Context, deployment DeploymentRecord) (string, error) {
	appPath := strings.TrimSpace(deployment.AppPath)
	if appPath != "" && !isRemoteRepoSource(appPath) {
		absPath, err := filepath.Abs(appPath)
		if err != nil {
			return "", fmt.Errorf("resolve app path: %w", err)
		}
		if info, err := os.Stat(absPath); err != nil {
			return "", fmt.Errorf("app path %q is not accessible: %w", absPath, err)
		} else if !info.IsDir() {
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

	dest := filepath.Join(appBuildRoot(), deployment.DeploymentID)
	if err := os.MkdirAll(appBuildRoot(), 0o755); err != nil {
		return "", fmt.Errorf("create app build root: %w", err)
	}

	if _, err := os.Stat(filepath.Join(dest, ".git")); err == nil {
		if err := runGitSync(ctx, dest, repoURL); err != nil {
			return "", err
		}
	} else {
		if err := os.RemoveAll(dest); err != nil {
			return "", fmt.Errorf("reset app checkout directory: %w", err)
		}
		if err := runGitClone(ctx, repoURL, dest); err != nil {
			return "", err
		}
	}

	if err := deploymentStore.UpdateDeploymentAppPath(ctx, deployment.DeploymentID, dest); err != nil {
		return "", err
	}

	return dest, nil
}

func runGitClone(ctx context.Context, repoURL string, dest string) error {
	if _, err := exec.LookPath("git"); err != nil {
		return fmt.Errorf("git CLI not found in PATH: %w", err)
	}

	args := []string{
		"clone",
		"--depth", "1",
		"--filter", "blob:none",
		"--single-branch",
		repoURL,
		dest,
	}

	return runGitCommand(ctx, args...)
}

func runGitSync(ctx context.Context, dest string, repoURL string) error {
	if _, err := exec.LookPath("git"); err != nil {
		return fmt.Errorf("git CLI not found in PATH: %w", err)
	}

	// Fast refresh of an existing checkout — avoids a full re-clone on redeploy.
	if err := runGitCommand(ctx, "-C", dest, "remote", "set-url", "origin", repoURL); err != nil {
		return err
	}
	if err := runGitCommand(ctx, "-C", dest, "fetch", "--depth", "1", "origin"); err != nil {
		return err
	}
	return runGitCommand(ctx, "-C", dest, "reset", "--hard", "FETCH_HEAD")
}

func runGitCommand(ctx context.Context, args ...string) error {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = io.MultiWriter(os.Stderr, &stderr)

	if err := cmd.Run(); err != nil {
		if trimmed := strings.TrimSpace(stderr.String()); trimmed != "" {
			return fmt.Errorf("git %s failed: %w: %s", strings.Join(args, " "), err, trimmed)
		}
		return fmt.Errorf("git %s failed: %w", strings.Join(args, " "), err)
	}

	return nil
}
