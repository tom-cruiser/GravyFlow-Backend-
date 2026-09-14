package main

import (
	"context"
	"errors"
	"testing"
)

// Regression test for "inconsistent types deduced for parameter $2"
// (SQLSTATE 42P08) in UpdateDeploymentStatus. $2 was assigned straight to
// the `status` column (deployment_status enum on any schema.sql-provisioned
// database) and, in the same statement, compared against plain string
// literals — two conflicting type inferences for one bind parameter under
// Postgres's extended query protocol. This exercises the real function
// against a throwaway deployment row on the local dev Postgres.
func TestUpdateDeploymentStatusNoParameterTypeConflict(t *testing.T) {
	ctx := context.Background()
	store, err := NewDeploymentStore(ctx)
	if err != nil {
		t.Skipf("skipping: could not reach local postgres: %v", err)
	}
	defer store.Close()

	userID, err := store.CreateUser(ctx, "regression-test-status@example.com", "regression-test", "not-a-real-hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	defer store.pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, userID.ID)

	deploymentID, err := store.CreateDeploymentAttemptForUser(ctx, userID.ID, "regression-test-app", "https://example.com/repo.git", "https://example.com/repo.git", "8080:8080", "")
	if err != nil {
		t.Fatalf("CreateDeploymentAttemptForUser: %v", err)
	}

	// This is the exact call site that produced the live 42P08 error.
	if err := store.MarkDeploymentFailed(ctx, deploymentID, errors.New("regression test failure"), userID.ID); err != nil {
		t.Fatalf("MarkDeploymentFailed: %v (this is the bug — $2 type conflict)", err)
	}

	// MarkDeploymentDeployed shares the same query; exercise the other
	// literal in the finished_at CASE too.
	if err := store.MarkDeploymentDeployed(ctx, deploymentID, "test-container-id", "test-container-name", "test-image:latest", userID.ID); err != nil {
		t.Fatalf("MarkDeploymentDeployed: %v", err)
	}
}
