package main

import (
	"strings"
	"testing"
)

func TestValidateRequiredSecrets(t *testing.T) {
	strong := "k3J9vQ2xLm8PzR4tY7wB1nC6dF0gH5sA9eU2iO"
	cases := []struct {
		name, jwt, enc, wantErr string
	}{
		{"both strong", strong, strong, ""},
		{"jwt missing", "", strong, "AUTH_JWT_SECRET is not set"},
		{"enc missing", strong, "", "APP_ENV_ENCRYPTION_KEY is not set"},
		{"jwt too short", "short-secret", strong, "AUTH_JWT_SECRET is too short"},
		{"published default", "dev-auth-secret-change-me-in-production", strong, "published in the repository"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("AUTH_JWT_SECRET", c.jwt)
			t.Setenv("APP_ENV_ENCRYPTION_KEY", c.enc)
			t.Setenv("ENV_ENCRYPTION_KEY", "")
			err := validateRequiredSecrets()
			if c.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("err = %v, want it to mention %q", err, c.wantErr)
			}
		})
	}

	t.Run("legacy ENV_ENCRYPTION_KEY accepted", func(t *testing.T) {
		t.Setenv("AUTH_JWT_SECRET", strong)
		t.Setenv("APP_ENV_ENCRYPTION_KEY", "")
		t.Setenv("ENV_ENCRYPTION_KEY", strong)
		if err := validateRequiredSecrets(); err != nil {
			t.Fatalf("legacy name rejected: %v", err)
		}
	})
}

func TestAuthJWTSecretNeverFallsBackToAKnownValue(t *testing.T) {
	t.Setenv("AUTH_JWT_SECRET", "")
	a, b := authJWTSecret(), authJWTSecret()
	if string(a) != string(b) {
		t.Fatal("fallback secret must be stable within a process")
	}
	for _, known := range knownPublicSecrets {
		if string(a) == known {
			t.Fatalf("fallback secret is the published value %q", known)
		}
	}
	if len(a) < minSecretLength {
		t.Fatalf("fallback secret is only %d bytes", len(a))
	}

	t.Setenv("AUTH_JWT_SECRET", "configured-value-configured-value-xx")
	if got := string(authJWTSecret()); got != "configured-value-configured-value-xx" {
		t.Fatalf("configured secret ignored: %q", got)
	}
}
