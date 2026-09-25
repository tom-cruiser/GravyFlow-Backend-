package main

import (
	"crypto/rand"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
)

// ============================================================================
// SECRETS
// ============================================================================
//
// Every secret comes from the environment (.env via docker-compose); none has
// a usable built-in default. validateRequiredSecrets runs at startup so a
// missing or placeholder secret stops the API instead of silently weakening
// it — the old fallback JWT key was a constant anyone could read in the repo,
// which would have let them forge admin tokens.

const minSecretLength = 32

// placeholderMarkers are fragments of the example values shipped in
// .env.example / docs. A real secret is random and contains none of them.
var placeholderMarkers = []string{"change-me", "changeme", "change_me", "your-", "your_", "example", "placeholder", "generate-"}

// knownPublicSecrets are values that have appeared in this repository.
var knownPublicSecrets = []string{"dev-auth-secret-change-me-in-production"}

func validateSecret(name, value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf("%s is not set (generate one with: openssl rand -base64 48)", name)
	}
	for _, known := range knownPublicSecrets {
		if value == known {
			return fmt.Errorf("%s is a value published in the repository; generate a new one", name)
		}
	}
	if len(value) < minSecretLength {
		return fmt.Errorf("%s is too short (%d chars, need at least %d)", name, len(value), minSecretLength)
	}
	return nil
}

func looksLikePlaceholder(value string) bool {
	lower := strings.ToLower(value)
	for _, marker := range placeholderMarkers {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// validateRequiredSecrets fails startup on a missing/short/published secret and
// warns about human-written ones (guessable, but not worth an outage — rotate
// them; see .env.example for how).
func validateRequiredSecrets() error {
	var problems []string
	for _, name := range []string{"AUTH_JWT_SECRET", "APP_ENV_ENCRYPTION_KEY"} {
		value := os.Getenv(name)
		if name == "APP_ENV_ENCRYPTION_KEY" && strings.TrimSpace(value) == "" {
			value = os.Getenv("ENV_ENCRYPTION_KEY") // legacy name, still honoured
		}
		if err := validateSecret(name, value); err != nil {
			problems = append(problems, err.Error())
			continue
		}
		if looksLikePlaceholder(value) {
			log.Printf("[WARN] %s looks human-written (contains placeholder words); rotate it for a random value", name)
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("refusing to start with unsafe secrets:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return nil
}

var (
	ephemeralJWTSecretOnce sync.Once
	ephemeralJWTSecret     []byte
)

// authJWTSecret is the HMAC key for access/refresh tokens and the GitHub
// install-state signature. Startup guarantees it's set in the server; the
// per-process random fallback only exists so unit tests that never run
// startup can still sign and verify tokens — it's never a known value.
func authJWTSecret() []byte {
	if secret := strings.TrimSpace(os.Getenv("AUTH_JWT_SECRET")); secret != "" {
		return []byte(secret)
	}
	ephemeralJWTSecretOnce.Do(func() {
		ephemeralJWTSecret = make([]byte, 48)
		if _, err := rand.Read(ephemeralJWTSecret); err != nil {
			panic(fmt.Sprintf("generate ephemeral JWT secret: %v", err))
		}
	})
	return ephemeralJWTSecret
}
