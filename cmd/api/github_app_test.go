package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func testGitHubApp(t *testing.T, apiURL string) (*GitHubAppClient, *rsa.PrivateKey) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return newGitHubAppClient(GitHubAppConfig{
		AppID:         "12345",
		Slug:          "gravyflow-test",
		PrivateKey:    key,
		WebhookSecret: "whsec",
		APIBaseURL:    apiURL,
		WebBaseURL:    "https://github.com",
	}), key
}

func TestGitHubAppJWT(t *testing.T) {
	client, key := testGitHubApp(t, "")
	signed, err := client.AppJWT()
	if err != nil {
		t.Fatal(err)
	}

	var claims jwt.RegisteredClaims
	if _, err := jwt.ParseWithClaims(signed, &claims, func(*jwt.Token) (any, error) {
		return &key.PublicKey, nil
	}, jwt.WithValidMethods([]string{"RS256"})); err != nil {
		t.Fatalf("JWT does not verify with the app's public key: %v", err)
	}
	if claims.Issuer != "12345" {
		t.Fatalf("iss = %q", claims.Issuer)
	}
	if lifetime := claims.ExpiresAt.Sub(claims.IssuedAt.Time); lifetime > 10*time.Minute {
		t.Fatalf("GitHub rejects JWTs valid for more than 10 minutes; got %s", lifetime)
	}
	if !claims.IssuedAt.Before(time.Now()) {
		t.Fatal("iat should be backdated for clock drift")
	}
}

func TestLoadGitHubAppConfigAcceptsEscapedNewlinePEM(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	t.Setenv("GITHUB_APP_ID", "1")
	t.Setenv("GITHUB_APP_PRIVATE_KEY", strings.ReplaceAll(string(pemBytes), "\n", `\n`))
	t.Setenv("GITHUB_APP_SLUG", "s")
	t.Setenv("GITHUB_APP_CLIENT_ID", "c")
	t.Setenv("GITHUB_APP_CLIENT_SECRET", "cs")
	t.Setenv("GITHUB_APP_WEBHOOK_SECRET", "w")
	if _, err := loadGitHubAppConfig(); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GITHUB_APP_WEBHOOK_SECRET", "")
	if _, err := loadGitHubAppConfig(); err == nil {
		t.Fatal("accepted a config without a webhook secret")
	}
	t.Setenv("GITHUB_APP_ID", "")
	if _, err := loadGitHubAppConfig(); err != errGitHubAppNotConfigured {
		t.Fatalf("unset GITHUB_APP_ID: got %v", err)
	}
}

func TestCreateInstallationTokenScopedToRepository(t *testing.T) {
	var client *GitHubAppClient
	var key *rsa.PrivateKey
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/app/installations/77/access_tokens" {
			http.Error(w, "unexpected "+r.Method+" "+r.URL.Path, http.StatusNotFound)
			return
		}
		appJWT := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if _, err := jwt.Parse(appJWT, func(*jwt.Token) (any, error) { return &key.PublicKey, nil }); err != nil {
			http.Error(w, "bad jwt", http.StatusUnauthorized)
			return
		}
		var body struct {
			RepositoryIDs []int64           `json:"repository_ids"`
			Permissions   map[string]string `json:"permissions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if len(body.RepositoryIDs) != 1 || body.RepositoryIDs[0] != 555 || body.Permissions["contents"] != "read" {
			http.Error(w, fmt.Sprintf("not scoped: %+v", body), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"token":"ghs_scoped","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
	}))
	defer srv.Close()
	client, key = testGitHubApp(t, srv.URL)

	token, err := client.CreateInstallationToken(t.Context(), 77, 555)
	if err != nil {
		t.Fatal(err)
	}
	if token.Token != "ghs_scoped" {
		t.Fatalf("token = %q", token.Token)
	}
}

func TestInstallationTokenCachedUntilNearExpiry(t *testing.T) {
	var minted atomic.Int32
	expiry := time.Now().Add(time.Hour)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := minted.Add(1)
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"token":"ghs_%d","expires_at":%q}`, n, expiry.Format(time.RFC3339))
	}))
	defer srv.Close()
	client, _ := testGitHubApp(t, srv.URL)

	first, err := client.installationToken(t.Context(), 1)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := client.installationToken(t.Context(), 1)
	if first != second || minted.Load() != 1 {
		t.Fatalf("token not reused: %q %q (%d minted)", first, second, minted.Load())
	}

	client.now = func() time.Time { return expiry.Add(-githubTokenRefreshMargin + time.Second) }
	third, _ := client.installationToken(t.Context(), 1)
	if third == first {
		t.Fatal("token about to expire was reused")
	}
}

func TestListInstallationRepositoriesPaginates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/access_tokens") {
			w.WriteHeader(http.StatusCreated)
			fmt.Fprintf(w, `{"token":"ghs_x","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
			return
		}
		if r.Header.Get("Authorization") != "token ghs_x" {
			http.Error(w, "bad auth", http.StatusUnauthorized)
			return
		}
		count := 100
		if r.URL.Query().Get("page") == "2" {
			count = 3
		}
		repos := make([]GitHubRepository, count)
		for i := range repos {
			repos[i] = GitHubRepository{ID: int64(i + 1), FullName: "org/r"}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 103, "repositories": repos})
	}))
	defer srv.Close()
	client, _ := testGitHubApp(t, srv.URL)

	repos, err := client.ListInstallationRepositories(t.Context(), 9)
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 103 {
		t.Fatalf("got %d repos, want 103", len(repos))
	}
}

func TestGitHubAPIErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(`{"message":"There is at least one repository that does not exist or is not accessible"}`))
	}))
	defer srv.Close()
	client, _ := testGitHubApp(t, srv.URL)

	_, err := client.CreateInstallationToken(t.Context(), 1, 2)
	if !isGitHubStatus(err, http.StatusUnprocessableEntity) {
		t.Fatalf("status not preserved: %v", err)
	}
	if !strings.Contains(err.Error(), "not accessible") {
		t.Fatalf("message not preserved: %v", err)
	}
}

func TestVerifyGitHubSignature(t *testing.T) {
	body := []byte(`{"action":"created"}`)
	mac := hmac.New(sha256.New, []byte("whsec"))
	mac.Write(body)
	good := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	if !verifyGitHubSignature("whsec", body, good) {
		t.Fatal("valid signature rejected")
	}
	for name, tc := range map[string]struct {
		secret, header string
		body           []byte
	}{
		"tampered body": {"whsec", good, []byte(`{"action":"deleted"}`)},
		"wrong secret":  {"other", good, body},
		"sha1 header":   {"whsec", "sha1=" + strings.TrimPrefix(good, "sha256="), body},
		"missing":       {"whsec", "", body},
		"empty secret":  {"", good, body},
	} {
		if verifyGitHubSignature(tc.secret, tc.body, tc.header) {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestGitHubInstallState(t *testing.T) {
	t.Setenv("AUTH_JWT_SECRET", "test-secret")
	state, err := issueGitHubInstallState("user-1")
	if err != nil {
		t.Fatal(err)
	}
	if userID, err := parseGitHubInstallState(state); err != nil || userID != "user-1" {
		t.Fatalf("round trip: %q, %v", userID, err)
	}

	if _, err := parseGitHubInstallState(state[:len(state)-2] + "xx"); err == nil {
		t.Fatal("tampered state accepted")
	}

	// A regular access token (same base secret) must not pass as a state.
	access, _, err := issueToken(UserRecord{ID: "user-1"}, tokenTypeAccess, time.Minute, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseGitHubInstallState(access); err == nil {
		t.Fatal("access token accepted as install state")
	}

	t.Setenv("AUTH_JWT_SECRET", "rotated")
	if _, err := parseGitHubInstallState(state); err == nil {
		t.Fatal("state accepted after secret rotation")
	}
}

func TestInstallURLEscapesState(t *testing.T) {
	client, _ := testGitHubApp(t, "")
	got := client.InstallURL("a.b+c")
	if got != "https://github.com/apps/gravyflow-test/installations/new?state=a.b%2Bc" {
		t.Fatalf("got %q", got)
	}
}
