package main

import (
	"context"
	"testing"
)

// computeDeploymentURL backs the "give me a link to my deployed app" ask:
// GET /apps (and /apps/:id) should return a reachable URL once a deployment
// has a running container. This exercises the real read paths, not just the
// pure helper, so a future column/field rename in the scan sites is caught.
func TestDeploymentURLPopulatedOnceRunning(t *testing.T) {
	ctx := context.Background()
	store, err := NewDeploymentStore(ctx)
	if err != nil {
		t.Skipf("skipping: could not reach local postgres: %v", err)
	}
	defer store.Close()

	user, err := store.CreateUser(ctx, "regression-test-url@example.com", "regression-test", "not-a-real-hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	defer store.pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, user.ID)

	deploymentID, err := store.CreateDeploymentAttemptForUser(ctx, user.ID, "regression-url-app", "https://example.com/repo.git", "https://example.com/repo.git", "8080", "")
	if err != nil {
		t.Fatalf("CreateDeploymentAttemptForUser: %v", err)
	}

	// Freshly created: BUILDING, no container yet — no URL to show.
	fresh, err := store.GetDeploymentForUser(ctx, user.ID, deploymentID)
	if err != nil {
		t.Fatalf("GetDeploymentForUser (fresh): %v", err)
	}
	if fresh.URL != "" {
		t.Fatalf("URL = %q before any container exists, want empty", fresh.URL)
	}

	if err := store.MarkDeploymentDeployed(ctx, deploymentID, "test-container-id", "regression-url-app", "test-image:latest", user.ID); err != nil {
		t.Fatalf("MarkDeploymentDeployed: %v", err)
	}

	running, err := store.GetDeploymentForUser(ctx, user.ID, deploymentID)
	if err != nil {
		t.Fatalf("GetDeploymentForUser (running): %v", err)
	}
	wantURL := "http://regression-url-app.localhost"
	if running.URL != wantURL {
		t.Fatalf("URL = %q, want %q", running.URL, wantURL)
	}

	// Same field, same value, via the list endpoint listAppsHandler serves.
	list, err := store.ListDeploymentsForUser(ctx, user.ID)
	if err != nil {
		t.Fatalf("ListDeploymentsForUser: %v", err)
	}
	if len(list) != 1 || list[0].URL != wantURL {
		t.Fatalf("ListDeploymentsForUser URL = %+v, want one item with URL %q", list, wantURL)
	}
}
