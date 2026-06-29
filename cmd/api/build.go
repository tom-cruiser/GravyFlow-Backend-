package main

import (
	"fmt"
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

	cmd := exec.Command("nixpacks", "build", absPath, "--name", appName)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("nixpacks build failed: %w", err)
	}

	return appName, nil
}
