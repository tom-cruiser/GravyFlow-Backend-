package main

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// A private repo host: rejects unauthenticated requests like GitHub does and
// records the Authorization header git sends.
func newPrivateGitServer(t *testing.T) (*httptest.Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("WWW-Authenticate", `Basic realm="GitHub"`)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	t.Cleanup(srv.Close)
	return srv, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), seen...)
	}
}

func TestGitCloneSendsTokenAsScopedHeaderNotInURL(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("GIT_SSL_NO_VERIFY", "true") // self-signed test server

	srv, seen := newPrivateGitServer(t)
	const token = "ghp_testTOKEN1234567890"
	repoURL := srv.URL + "/org/private-repo.git"

	err := runGitClone(context.Background(), repoURL, filepath.Join(t.TempDir(), "checkout"), GitCloneOptions{Token: token})
	if err == nil {
		t.Fatal("clone against a rejecting server succeeded")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("token leaked into the error message: %v", err)
	}

	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))
	found := false
	for _, h := range seen() {
		if h == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("server never received the token header; got %q", seen())
	}
}

func TestGitCloneWithoutTokenFailsFastWithPrivateRepoHint(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	t.Setenv("GIT_SSL_NO_VERIFY", "true")

	srv, _ := newPrivateGitServer(t)
	start := time.Now()
	err := runGitClone(context.Background(), srv.URL+"/org/private-repo.git", filepath.Join(t.TempDir(), "checkout"), GitCloneOptions{})
	if err == nil {
		t.Fatal("clone of a private repo without a token succeeded")
	}
	// GIT_TERMINAL_PROMPT=0: no hang waiting for a username, and no retries
	// with backoff for an auth failure.
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("clone took %s; expected an immediate failure", elapsed)
	}
	if !isGitAuthError(err) {
		t.Fatalf("not recognised as an auth error: %v", err)
	}
	if hinted := withGitAuthHint(err, GitCloneOptions{}); !strings.Contains(hinted.Error(), "private") {
		t.Fatalf("missing private-repo hint: %v", hinted)
	}
}

func TestGitAuthEnvScoping(t *testing.T) {
	env, err := gitAuthEnv("https://github.com/org/repo.git", GitCloneOptions{Token: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "GIT_CONFIG_KEY_0=http.https://github.com/.extraHeader") {
		t.Fatalf("header not scoped to the repo host: %v", env)
	}
	if strings.Contains(joined, "abc") {
		t.Fatalf("raw token present in env (should only appear base64-encoded): %v", env)
	}

	if _, err := gitAuthEnv("http://github.com/org/repo.git", GitCloneOptions{Token: "abc"}); err == nil {
		t.Fatal("token accepted for a plain http:// URL")
	}

	env, err = gitAuthEnv("https://github.com/org/repo.git", GitCloneOptions{})
	if err != nil || len(env) != 1 || env[0] != "GIT_TERMINAL_PROMPT=0" {
		t.Fatalf("no-credential env = %v, %v", env, err)
	}
}

func TestNormalizeGitToken(t *testing.T) {
	if tok, ok := normalizeGitToken("  ghp_abc  "); !ok || tok != "ghp_abc" {
		t.Fatalf("got %q, %v", tok, ok)
	}
	for _, bad := range []string{"has space", "new\nline", strings.Repeat("a", maxGitTokenLength+1)} {
		if _, ok := normalizeGitToken(bad); ok {
			t.Fatalf("accepted %q", bad)
		}
	}
}
