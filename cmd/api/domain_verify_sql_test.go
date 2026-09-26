package main

import (
	"context"
	"testing"
	"time"
)

// Regression test for "could not determine data type of parameter $2"
// (SQLSTATE 42P18) when clicking Verify on a custom domain. The UPDATE in
// VerifyDeploymentDomain was passed record.CustomDomain as $2 but never
// referenced it, so Postgres rejected the statement after the TXT check had
// already passed. markDomainVerified is that exact UPDATE, exercised against
// a throwaway row on the local dev Postgres.
func TestMarkDomainVerifiedNoUntypedParameter(t *testing.T) {
	ctx := context.Background()
	store, err := NewDeploymentStore(ctx)
	if err != nil {
		t.Skipf("skipping: could not reach local postgres: %v", err)
	}
	defer store.Close()

	user, err := store.CreateUser(ctx, "regression-test-domain-verify@example.com", "regression-test", "not-a-real-hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	defer store.pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, user.ID)

	deploymentID, err := store.CreateDeploymentAttemptForUser(ctx, user.ID, "regression-verify-app", "https://example.com/repo.git", "https://example.com/repo.git", "8080", "")
	if err != nil {
		t.Fatalf("CreateDeploymentAttemptForUser: %v", err)
	}

	domain, err := store.UpsertDeploymentDomain(ctx, user.ID, deploymentID, "verify.regression-test.example.com", nil)
	if err != nil {
		t.Fatalf("UpsertDeploymentDomain: %v", err)
	}

	now := time.Now().UTC()
	expiresAt := now.AddDate(1, 0, 0)
	got, err := store.markDomainVerified(ctx, domain.ID, now, &expiresAt, DomainStatusDNSVerified, "dns ok")
	if err != nil {
		t.Fatalf("markDomainVerified: %v (this is the bug — untyped $2)", err)
	}
	if !got.Verified {
		t.Fatalf("Verified = false, want true")
	}
	if got.Status != DomainStatusDNSVerified {
		t.Fatalf("Status = %q, want %q", got.Status, DomainStatusDNSVerified)
	}
	if got.VerifiedAt == nil || got.ExpiresAt == nil {
		t.Fatalf("VerifiedAt/ExpiresAt not set: %+v", got)
	}
}
