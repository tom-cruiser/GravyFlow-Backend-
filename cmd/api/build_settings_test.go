package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestNormalizeDockerfilePath(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"", "", false},
		{"   ", "", false},
		{"services/auth-tenant/Dockerfile", "services/auth-tenant/Dockerfile", false},
		{"./services//auth-tenant/Dockerfile", "services/auth-tenant/Dockerfile", false},
		{`services\auth-tenant\Dockerfile`, "services/auth-tenant/Dockerfile", false},
		{"Dockerfile", "Dockerfile", false},
		{"/etc/passwd", "", true},
		{"../outside/Dockerfile", "", true},
		{"services/../../outside/Dockerfile", "", true},
		{"..", "", true},
		{".", "", true},
		{"-f/etc/passwd", "", true},
		{"a\x00b", "", true},
		{strings.Repeat("a/", 150) + "Dockerfile", "", true},
	}
	for _, tt := range tests {
		got, err := normalizeDockerfilePath(tt.in)
		if (err != nil) != tt.wantErr {
			t.Errorf("normalizeDockerfilePath(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			continue
		}
		if err == nil && got != tt.want {
			t.Errorf("normalizeDockerfilePath(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestValidateContainerPort(t *testing.T) {
	for _, ok := range []int{0, 1, 3001, 8080, 65535} {
		if err := validateContainerPort(ok); err != nil {
			t.Errorf("port %d rejected: %v", ok, err)
		}
	}
	for _, bad := range []int{-1, 65536, 100000} {
		if err := validateContainerPort(bad); err == nil {
			t.Errorf("port %d accepted", bad)
		}
	}
}

// monorepoFixture lays out a repo shaped like Nerva-backend: no Dockerfile or
// start script at the root, one Dockerfile per service.
func monorepoFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write := func(rel, content string) {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("package.json", `{"name":"root","workspaces":["services/*"]}`)
	write("services/auth-tenant/Dockerfile", "FROM node:20-alpine AS builder\nEXPOSE 9999\nFROM node:20-alpine AS runtime\nEXPOSE 3001\nCMD [\"node\",\"x.js\"]\n")
	write("services/inventory/Dockerfile", "FROM node:20-alpine\nEXPOSE 3002/tcp\n")
	write("services/realtime/Dockerfile", "FROM node:20-alpine\nENV PORT=3008\n")
	write("node_modules/dep/Dockerfile", "FROM scratch\n")
	return root
}

func TestResolveDockerfile(t *testing.T) {
	root := monorepoFixture(t)

	got, err := resolveDockerfile(root, "services/auth-tenant/Dockerfile")
	if err != nil {
		t.Fatalf("resolveDockerfile: %v", err)
	}
	if !strings.HasSuffix(got, filepath.FromSlash("services/auth-tenant/Dockerfile")) {
		t.Fatalf("resolved to %q", got)
	}

	if _, err := resolveDockerfile(root, "services/missing/Dockerfile"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing Dockerfile error = %v", err)
	}
	if _, err := resolveDockerfile(root, "../etc/passwd"); err == nil {
		t.Fatal("path traversal must be rejected")
	}
	if _, err := resolveDockerfile(root, "services/auth-tenant"); err == nil {
		t.Fatal("a directory is not a Dockerfile")
	}

	// A symlink inside the repo that points outside it must not be followed.
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("FROM scratch"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "Dockerfile.evil")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if _, err := resolveDockerfile(root, "Dockerfile.evil"); err == nil || !strings.Contains(err.Error(), "outside") {
		t.Fatalf("symlink escape error = %v, want it rejected", err)
	}
}

func TestDetectDockerfilePort(t *testing.T) {
	root := monorepoFixture(t)
	cases := map[string]int{
		"services/auth-tenant/Dockerfile": 3001, // last stage's EXPOSE wins
		"services/inventory/Dockerfile":   3002, // "/tcp" suffix
		"services/realtime/Dockerfile":    0,    // no EXPOSE
	}
	for rel, want := range cases {
		if got := detectDockerfilePort(filepath.Join(root, filepath.FromSlash(rel))); got != want {
			t.Errorf("detectDockerfilePort(%s) = %d, want %d", rel, got, want)
		}
	}
	if got := detectDockerfilePort(filepath.Join(root, "nope")); got != 0 {
		t.Errorf("missing file = %d, want 0", got)
	}
}

func TestFindDockerfilesAndMonorepoHint(t *testing.T) {
	root := monorepoFixture(t)
	found := findDockerfiles(root, 12)
	if len(found) != 3 {
		t.Fatalf("found %v, want the 3 service Dockerfiles (node_modules is skipped)", found)
	}
	hint := monorepoHint(root)
	for _, want := range []string{"services/auth-tenant/Dockerfile", "Dockerfile path", "monorepo"} {
		if !strings.Contains(hint, want) {
			t.Errorf("hint %q is missing %q", hint, want)
		}
	}
	if monorepoHint(t.TempDir()) != "" {
		t.Error("a repo with no Dockerfiles must produce no hint")
	}
}

func TestBuildWithDockerfileMissingListsAlternatives(t *testing.T) {
	root := monorepoFixture(t)
	_, err := buildWithDockerfile(root, "svc", BuildConfig{DockerfilePath: "services/nope/Dockerfile"})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "not found") || !strings.Contains(err.Error(), "services/inventory/Dockerfile") {
		t.Fatalf("error should name the problem and the real Dockerfiles: %v", err)
	}
}

func TestCheckDockerfileInTree(t *testing.T) {
	entries := []GitHubTreeEntry{
		{Path: "services/auth/Dockerfile", Type: "blob"},
		{Path: "services/shifts/Dockerfile", Type: "blob"},
		{Path: "services", Type: "tree"},
	}
	if err := checkDockerfileInTree("o/r", "services/auth/Dockerfile", entries); err != nil {
		t.Fatalf("existing Dockerfile rejected: %v", err)
	}
	err := checkDockerfileInTree("o/r", "services/auht/Dockerfile", entries)
	if err == nil || !strings.Contains(err.Error(), "services/shifts/Dockerfile") {
		t.Fatalf("typo error = %v, want it to list the real Dockerfiles", err)
	}
	if err := checkDockerfileInTree("o/r", "Dockerfile", nil); err == nil || !strings.Contains(err.Error(), "no Dockerfiles") {
		t.Fatalf("empty repo error = %v", err)
	}
}

func TestSetupRouterRegistersBuildSettingsRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := setupRouter(ServerConfig{Debug: true})
	want := "/api/v1/apps/:id/build-settings"
	for _, method := range []string{"GET", "PUT"} {
		found := false
		for _, route := range router.Routes() {
			if route.Method == method && route.Path == want {
				found = true
			}
		}
		if !found {
			t.Errorf("%s %s not registered", method, want)
		}
	}
}

func TestBuildSettingsStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	store, err := NewDeploymentStore(ctx)
	if err != nil {
		t.Skipf("skipping: could not reach local postgres: %v", err)
	}
	defer store.Close()

	user, err := store.CreateUser(ctx, "regression-test-build-settings@example.com", "regression-test", "not-a-real-hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	defer store.pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, user.ID)

	id, err := store.CreateDeploymentAttemptForUser(ctx, user.ID, "regression-build-settings", "https://example.com/r.git", "https://example.com/r.git", "8080", "")
	if err != nil {
		t.Fatalf("CreateDeploymentAttemptForUser: %v", err)
	}

	got, err := store.GetBuildSettings(ctx, id)
	if err != nil || got != (BuildSettings{}) {
		t.Fatalf("defaults = %+v, %v; want empty settings", got, err)
	}

	want := BuildSettings{DockerfilePath: "services/auth-tenant/Dockerfile", ContainerPort: 3001, MemoryMB: 1536, CPU: 1}
	if err := store.SetBuildSettings(ctx, id, want); err != nil {
		t.Fatalf("SetBuildSettings: %v", err)
	}
	if got, _ := store.GetBuildSettings(ctx, id); got != want {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}

	if err := store.SetBuildSettings(ctx, id, BuildSettings{DockerfilePath: "../evil"}); err == nil {
		t.Fatal("an unsafe path must be rejected before it reaches the database")
	}
	if got, _ := store.GetBuildSettings(ctx, id); got != want {
		t.Fatalf("a rejected update changed the stored settings: %+v", got)
	}

	if err := store.UpdateDeploymentPortMap(ctx, id, "3001"); err != nil {
		t.Fatalf("UpdateDeploymentPortMap: %v", err)
	}
	dep, err := store.GetDeploymentForUser(ctx, user.ID, id)
	if err != nil || dep.PortMap != "3001" {
		t.Fatalf("port map = %q, %v; want 3001", dep.PortMap, err)
	}

	if err := store.SetBuildSettings(ctx, "00000000-0000-0000-0000-000000000000", want); err == nil {
		t.Fatal("updating a non-existent deployment must fail")
	}
}

// End to end through the real docker CLI: a monorepo-shaped project whose
// Dockerfile lives in a subdirectory but COPYs from the repo root — exactly
// what Nerva's services do, and what the root-only builder could not build.
func TestBuildCodeWithConfigMonorepoDockerfile(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not installed")
	}
	if err := exec.Command("docker", "image", "inspect", "busybox:latest").Run(); err != nil {
		t.Skip("busybox:latest is not available locally; skipping to avoid a network pull")
	}

	root := t.TempDir()
	dir := filepath.Join(root, "services", "svc")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "packages", "shared"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "packages", "shared", "lib.txt"), []byte("shared"), 0o644); err != nil {
		t.Fatal(err)
	}
	dockerfile := "FROM busybox:latest\nCOPY packages/shared/lib.txt /lib.txt\nEXPOSE 3001\nCMD [\"cat\",\"/lib.txt\"]\n"
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		t.Fatal(err)
	}

	const name = "gf-monorepo-build-test"
	t.Cleanup(func() { _ = exec.Command("docker", "rmi", "-f", name+":latest").Run() })

	config := DefaultConfig()
	config.DockerfilePath = "services/svc/Dockerfile"
	var logs strings.Builder
	config.LogWriter = &logs

	tag, err := BuildCodeWithConfig(root, name, config)
	if err != nil {
		t.Fatalf("BuildCodeWithConfig: %v\nlogs:\n%s", err, logs.String())
	}
	if tag != name+":latest" {
		t.Fatalf("tag = %q, want %q", tag, name+":latest")
	}
	if !strings.Contains(logs.String(), "Building services/svc/Dockerfile with Docker") {
		t.Fatalf("build log missing the strategy line:\n%s", logs.String())
	}

	out, err := exec.Command("docker", "run", "--rm", tag).CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != "shared" {
		t.Fatalf("running the built image = %q, %v; want the file copied from the repo root", out, err)
	}
}

// Drives the real handlers (auth context, ownership check, validation, DB)
// without the router: valid settings persist, unsafe ones are a 400 and never
// touch the stored value.
func TestBuildSettingsHandlers(t *testing.T) {
	ctx := context.Background()
	store, err := NewDeploymentStore(ctx)
	if err != nil {
		t.Skipf("skipping: could not reach local postgres: %v", err)
	}
	defer store.Close()

	previous := deploymentStore
	deploymentStore = store
	defer func() { deploymentStore = previous }()

	user, err := store.CreateUser(ctx, "regression-test-build-handlers@example.com", "regression-test", "not-a-real-hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	defer store.pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, user.ID)
	id, err := store.CreateDeploymentAttemptForUser(ctx, user.ID, "regression-build-handlers", "https://example.com/r.git", "https://example.com/r.git", "8080", "")
	if err != nil {
		t.Fatalf("CreateDeploymentAttemptForUser: %v", err)
	}

	gin.SetMode(gin.TestMode)
	call := func(handler gin.HandlerFunc, method string, body string) (int, string) {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(method, "/", strings.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		c.Params = gin.Params{{Key: "id", Value: id}}
		c.Set(currentUserContextKey, user)
		handler(c)
		return w.Code, w.Body.String()
	}

	code, body := call(updateBuildSettingsHandler, http.MethodPut, `{"dockerfilePath":"services/auth-tenant/Dockerfile","containerPort":3001}`)
	if code != http.StatusOK {
		t.Fatalf("valid update = %d %s", code, body)
	}
	code, body = call(getBuildSettingsHandler, http.MethodGet, "")
	if code != http.StatusOK || !strings.Contains(body, `"dockerfilePath":"services/auth-tenant/Dockerfile"`) || !strings.Contains(body, `"containerPort":3001`) {
		t.Fatalf("get after update = %d %s", code, body)
	}

	for _, bad := range []string{
		`{"dockerfilePath":"../../etc/passwd"}`,
		`{"dockerfilePath":"/abs/Dockerfile"}`,
		`{"dockerfilePath":"a/Dockerfile","containerPort":70000}`,
		`not json`,
	} {
		if code, body := call(updateBuildSettingsHandler, http.MethodPut, bad); code != http.StatusBadRequest {
			t.Errorf("update %s = %d %s, want 400", bad, code, body)
		}
	}
	if got, _ := store.GetBuildSettings(ctx, id); got.DockerfilePath != "services/auth-tenant/Dockerfile" || got.ContainerPort != 3001 {
		t.Fatalf("rejected updates changed the stored settings: %+v", got)
	}

	// Someone else's deployment is not reachable.
	other, err := store.CreateUser(ctx, "regression-test-build-handlers-other@example.com", "regression-test", "not-a-real-hash")
	if err != nil {
		t.Fatalf("CreateUser(other): %v", err)
	}
	defer store.pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, other.ID)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPut, "/", strings.NewReader(`{"dockerfilePath":"x/Dockerfile"}`))
	c.Params = gin.Params{{Key: "id", Value: id}}
	c.Set(currentUserContextKey, other)
	updateBuildSettingsHandler(c)
	if w.Code == http.StatusOK {
		t.Fatalf("another user could change this service's build settings: %d %s", w.Code, w.Body.String())
	}
}

func TestAppResources(t *testing.T) {
	for _, c := range []struct {
		memoryMB int
		cpu      float64
		ok       bool
	}{
		{0, 0, true},
		{1536, 1, true},
		{minAppMemoryMB, minAppCPU, true},
		{maxAppMemoryMB, maxAppCPU, true},
		{64, 0, false},
		{maxAppMemoryMB + 1, 0, false},
		{0, 0.05, false},
		{0, maxAppCPU + 1, false},
		{-1, 0, false},
	} {
		err := BuildSettings{MemoryMB: c.memoryMB, CPU: c.cpu}.validatedErr()
		if (err == nil) != c.ok {
			t.Errorf("memoryMB=%d cpu=%v: err=%v, want ok=%v", c.memoryMB, c.cpu, err, c.ok)
		}
	}

	if cpu, mem := (BuildSettings{}).resources(); cpu != defaultDeployCPU || mem != defaultDeployMemoryMB {
		t.Errorf("unset resources = %v/%d, want the defaults", cpu, mem)
	}
	if cpu, mem := (BuildSettings{MemoryMB: 1536, CPU: 1}).resources(); cpu != 1 || mem != 1536 {
		t.Errorf("set resources = %v/%d, want 1/1536", cpu, mem)
	}
}

func (b BuildSettings) validatedErr() error {
	_, err := b.validated()
	return err
}
