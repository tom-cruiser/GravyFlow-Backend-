package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"time"
)

// ============================================================================
// CONSTANTS
// ============================================================================

const (
	MinDiskSpaceGB = 1
)

// ============================================================================
// ENVIRONMENT HELPERS
// ============================================================================

// envOrDefault returns environment variable value or default
func envOrDefault(key string, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}

// durationFromEnv parses duration from environment variable
func durationFromEnv(key string, fallback time.Duration) (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", key, err)
	}
	if parsed < 0 {
		return 0, fmt.Errorf("%s must be non-negative", key)
	}
	return parsed, nil
}

// ============================================================================
// CRYPTO HELPERS
// ============================================================================

// hashToken creates a SHA256 hash of a token
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// generateRandomToken generates a cryptographically secure random token
func generateRandomToken(numBytes int) (string, error) {
	buf := make([]byte, numBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// ============================================================================
// RETRY HELPERS
// ============================================================================

// isRetryableError checks if an error is retryable
func isRetryableError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	retryableMarkers := []string{
		"connection refused",
		"connection reset",
		"timeout",
		"temporary failure",
		"network",
		"dial tcp",
		"unexpected eof",
		"i/o timeout",
		"context deadline exceeded",
	}
	for _, marker := range retryableMarkers {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

// ============================================================================
// EXAMPLE USAGE (Removed - this should be in a separate test file)
// ============================================================================
// Note: ExampleUsage has been removed from here.
// If you need examples, create a separate file like example_test.go