package main

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// BuildCode builds application source into a local Docker image using Nixpacks.
func BuildCode(appPath string, appName string) (string, error) {
	appPath = strings.TrimSpace(appPath)
	appName = strings.TrimSpace(appName)

	if appPath == "" {
		return "", &ValidationError{
			Field:   "appPath",
			Code:    "required",
			Message: "appPath is required",
		}
	}
	if appName == "" {
		return "", &ValidationError{
			Field:   "appName",
			Code:    "required",
			Message: "appName is required",
		}
	}

	absPath, err := filepath.Abs(appPath)
	if err != nil {
		return "", fmt.Errorf("resolve appPath: %w", err)
	}

	info, err := os.Stat(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", &ValidationError{
				Field:   "appPath",
				Code:    "not_found",
				Message: fmt.Sprintf("appPath %q does not exist", appPath),
			}
		}
		return "", fmt.Errorf("check appPath: %w", err)
	}
	if !info.IsDir() {
		return "", &ValidationError{
			Field:   "appPath",
			Code:    "invalid_type",
			Message: "appPath must point to a directory",
		}
	}

	if _, err := exec.LookPath("nixpacks"); err != nil {
		return "", fmt.Errorf("nixpacks CLI not found in PATH: %w", err)
	}

	// Persist Nixpacks' toolchain cache across builds so it doesn't re-download
	// the Nix environment on every deploy. Defaults to /var/cache/nixpacks;
	// override via NIXPACKS_CACHE_DIR, or set it to "off"/"none" to omit the
	// flag entirely (e.g. if a given nixpacks version doesn't support it).
	cacheDir := strings.TrimSpace(os.Getenv("NIXPACKS_CACHE_DIR"))
	if cacheDir == "" {
		cacheDir = "/var/cache/nixpacks"
	}
	useCacheDir := !strings.EqualFold(cacheDir, "off") && !strings.EqualFold(cacheDir, "none")

	if useCacheDir {
		if err := runNixpacksBuild(absPath, appName, cacheDir); err != nil {
			// If the failure is specifically the --cache-dir flag not being
			// supported, fall back to the native env cache rather than letting
			// the (no-retry) worker job crash. Genuine build failures are NOT
			// retried — they return immediately.
			if isUnsupportedCacheDirError(err) {
				log.Printf("nixpacks: --cache-dir unsupported, retrying with native cache: %v", err)
				if fallbackErr := runNixpacksBuild(absPath, appName, ""); fallbackErr != nil {
					return "", fmt.Errorf("nixpacks build failed: %w", fallbackErr)
				}
				return appName, nil
			}
			return "", fmt.Errorf("nixpacks build failed: %w", err)
		}
		return appName, nil
	}

	if err := runNixpacksBuild(absPath, appName, ""); err != nil {
		return "", fmt.Errorf("nixpacks build failed: %w", err)
	}

	return appName, nil
}

// runNixpacksBuild invokes the nixpacks CLI, streaming output to the server logs
// while also capturing stderr so the caller can classify failures. Passing an
// empty cacheDir omits the --cache-dir flag (native cache defaults).
func runNixpacksBuild(absPath string, appName string, cacheDir string) error {
	args := []string{"build", absPath, "--name", appName}
	if cacheDir != "" {
		args = append(args, "--cache-dir", cacheDir)
	}

	var stderr bytes.Buffer
	cmd := exec.Command("nixpacks", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = io.MultiWriter(os.Stderr, &stderr)
	// Enable BuildKit so nixpacks gets content-addressed layer caching and
	// parallel build steps — markedly faster on repeat builds.
	cmd.Env = append(os.Environ(), "DOCKER_BUILDKIT=1")

	if err := cmd.Run(); err != nil {
		if trimmed := strings.TrimSpace(stderr.String()); trimmed != "" {
			return fmt.Errorf("%w: %s", err, trimmed)
		}
		return err
	}

	return nil
}

// isUnsupportedCacheDirError reports whether a build failure was caused by the
// nixpacks CLI rejecting the --cache-dir flag (vs. a real build error). Requires
// both a cache-dir mention and an argument-parsing marker to avoid false
// positives from app build output that merely references the path.
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
