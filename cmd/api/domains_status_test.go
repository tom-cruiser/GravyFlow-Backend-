package main

import (
	"context"
	"testing"

	"github.com/gin-gonic/gin"
)

// Route-registration regression tests (see delete_app_route_test.go/
// env_bulk_route_test.go for the established pattern) for the two new
// production-domains endpoints.
func TestSetupRouterRegistersMakePrimaryDomainRoute(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := setupRouter(ServerConfig{Debug: true})

	want := "/api/v1/apps/:id/domains/:domain/primary"
	for _, route := range router.Routes() {
		if route.Method == "PATCH" && route.Path == want {
			return
		}
	}
	t.Fatalf("PATCH %s not registered", want)
}

func TestSetupRouterRegistersEdgeSettingsRoute(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := setupRouter(ServerConfig{Debug: true})

	want := "/api/v1/apps/:id/edge-settings"
	for _, method := range []string{"GET", "PATCH"} {
		found := false
		for _, route := range router.Routes() {
			if route.Method == method && route.Path == want {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("%s %s not registered", method, want)
		}
	}
}

// checkDNSTarget must report an honest, distinct failure when the platform
// hasn't been told what its own edge is reachable at, rather than silently
// treating an unconfigured server as either verified or a DNS error.
func TestCheckDNSTargetReportsUnconfiguredProxyTarget(t *testing.T) {
	t.Setenv("GRAVYFLOW_PROXY_TARGET", "")

	ok, message := checkDNSTarget(context.Background(), "example.com")
	if ok {
		t.Fatalf("checkDNSTarget() ok = true, want false when GRAVYFLOW_PROXY_TARGET is unset")
	}
	if message == "" {
		t.Fatalf("checkDNSTarget() message is empty, want a message explaining why")
	}
}

// MakeDomainPrimary must never leave a deployment with zero or two primary
// domains — this is the single UPDATE ... SET is_primary = (custom_domain =
// $2) statement's whole reason for existing instead of two separate writes.
func TestMakeDomainPrimarySingleInvariant(t *testing.T) {
	ctx := context.Background()
	store, err := NewDeploymentStore(ctx)
	if err != nil {
		t.Skipf("skipping: could not reach local postgres: %v", err)
	}
	defer store.Close()

	user, err := store.CreateUser(ctx, "regression-test-primary-domain@example.com", "regression-test", "not-a-real-hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	defer store.pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, user.ID)

	deploymentID, err := store.CreateDeploymentAttemptForUser(ctx, user.ID, "regression-primary-app", "https://example.com/repo.git", "https://example.com/repo.git", "8080", "")
	if err != nil {
		t.Fatalf("CreateDeploymentAttemptForUser: %v", err)
	}

	const domainA = "a.regression-primary.example.com"
	const domainB = "b.regression-primary.example.com"
	if _, err := store.UpsertDeploymentDomain(ctx, user.ID, deploymentID, domainA, nil); err != nil {
		t.Fatalf("UpsertDeploymentDomain(A): %v", err)
	}
	if _, err := store.UpsertDeploymentDomain(ctx, user.ID, deploymentID, domainB, nil); err != nil {
		t.Fatalf("UpsertDeploymentDomain(B): %v", err)
	}

	assertExactlyOnePrimary := func(want string) {
		t.Helper()
		domains, err := store.ListDeploymentDomains(ctx, user.ID, deploymentID)
		if err != nil {
			t.Fatalf("ListDeploymentDomains: %v", err)
		}
		var primaryCount int
		var primaryDomain string
		for _, d := range domains {
			if d.IsPrimary {
				primaryCount++
				primaryDomain = d.CustomDomain
			}
		}
		if primaryCount != 1 {
			t.Fatalf("primary count = %d, want exactly 1 (domains: %+v)", primaryCount, domains)
		}
		if primaryDomain != want {
			t.Fatalf("primary domain = %q, want %q", primaryDomain, want)
		}
	}

	if _, err := store.MakeDomainPrimary(ctx, user.ID, deploymentID, domainA); err != nil {
		t.Fatalf("MakeDomainPrimary(A): %v", err)
	}
	assertExactlyOnePrimary(domainA)

	if _, err := store.MakeDomainPrimary(ctx, user.ID, deploymentID, domainB); err != nil {
		t.Fatalf("MakeDomainPrimary(B): %v", err)
	}
	assertExactlyOnePrimary(domainB)
}
