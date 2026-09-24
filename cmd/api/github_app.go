package main

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// ============================================================================
// GITHUB APP CLIENT
// ============================================================================
//
// Repositories are cloned through a GitHub App instead of user-supplied
// personal access tokens. The app authenticates as itself with a short-lived
// RS256 JWT signed by its private key, and exchanges that JWT for an
// installation access token (valid for one hour) whenever it needs to read an
// installation's repositories. Installation tokens are never stored: the
// deployment worker mints a fresh one, scoped to the single repository being
// cloned with contents:read, right before each clone.
//
// Configuration (all required unless noted):
//   GITHUB_APP_ID                  numeric App ID
//   GITHUB_APP_SLUG                the app's URL name (github.com/apps/<slug>)
//   GITHUB_APP_PRIVATE_KEY         PEM contents, or GITHUB_APP_PRIVATE_KEY_PATH
//   GITHUB_APP_CLIENT_ID           OAuth client ID, used to verify who installed
//   GITHUB_APP_CLIENT_SECRET       OAuth client secret
//   GITHUB_APP_WEBHOOK_SECRET      webhook HMAC secret
//   GITHUB_API_URL                 optional, for GitHub Enterprise Server
//   GITHUB_WEB_URL                 optional, for GitHub Enterprise Server

const (
	githubAppJWTTTL = 9 * time.Minute // GitHub rejects app JWTs valid for > 10 minutes
	// Backdated to tolerate clock drift between us and GitHub.
	githubAppJWTClockSkew = 60 * time.Second
	// Cached installation tokens are dropped this long before they expire, so
	// a caller never receives one that dies mid-request.
	githubTokenRefreshMargin = 5 * time.Minute
	githubHTTPTimeout        = 20 * time.Second
	githubMaxRepoPages       = 30 // 100 per page
	githubMaxErrorBody       = 4 << 10
)

var errGitHubAppNotConfigured = errors.New("the GitHub App integration is not configured on this server")

type GitHubAppConfig struct {
	AppID         string
	Slug          string
	PrivateKey    *rsa.PrivateKey
	ClientID      string
	ClientSecret  string
	WebhookSecret string
	APIBaseURL    string
	WebBaseURL    string
}

type GitHubAppClient struct {
	cfg  GitHubAppConfig
	http *http.Client
	now  func() time.Time

	mu     sync.Mutex
	tokens map[int64]GitHubInstallationToken // unscoped tokens, by installation
}

type GitHubInstallationToken struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

type GitHubAccount struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
	Type  string `json:"type"` // "User" or "Organization"
}

type GitHubInstallation struct {
	ID                  int64         `json:"id"`
	Account             GitHubAccount `json:"account"`
	RepositorySelection string        `json:"repository_selection"` // "all" or "selected"
	SuspendedAt         *time.Time    `json:"suspended_at"`
}

type GitHubRepository struct {
	ID            int64         `json:"id"`
	Name          string        `json:"name"`
	FullName      string        `json:"full_name"`
	Private       bool          `json:"private"`
	DefaultBranch string        `json:"default_branch"`
	CloneURL      string        `json:"clone_url"`
	HTMLURL       string        `json:"html_url"`
	PushedAt      *time.Time    `json:"pushed_at"`
	Owner         GitHubAccount `json:"owner"`
}

// GitHubAPIError is a non-2xx response from the GitHub API.
type GitHubAPIError struct {
	StatusCode int
	Message    string
}

func (e *GitHubAPIError) Error() string {
	return fmt.Sprintf("github api: %d %s", e.StatusCode, e.Message)
}

func isGitHubStatus(err error, status int) bool {
	var apiErr *GitHubAPIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == status
}

var (
	githubAppOnce   sync.Once
	githubAppClient *GitHubAppClient
	githubAppErr    error
)

// githubApp returns the process-wide client, loading its configuration from
// the environment on first use. errGitHubAppNotConfigured means the
// integration is simply turned off.
func githubApp() (*GitHubAppClient, error) {
	githubAppOnce.Do(func() {
		cfg, err := loadGitHubAppConfig()
		if err != nil {
			githubAppErr = err
			if !errors.Is(err, errGitHubAppNotConfigured) {
				fmt.Printf("[WARN] GitHub App integration disabled: %v\n", err)
			}
			return
		}
		githubAppClient = newGitHubAppClient(cfg)
	})
	return githubAppClient, githubAppErr
}

func loadGitHubAppConfig() (GitHubAppConfig, error) {
	cfg := GitHubAppConfig{
		AppID:         strings.TrimSpace(os.Getenv("GITHUB_APP_ID")),
		Slug:          strings.TrimSpace(os.Getenv("GITHUB_APP_SLUG")),
		ClientID:      strings.TrimSpace(os.Getenv("GITHUB_APP_CLIENT_ID")),
		ClientSecret:  strings.TrimSpace(os.Getenv("GITHUB_APP_CLIENT_SECRET")),
		WebhookSecret: strings.TrimSpace(os.Getenv("GITHUB_APP_WEBHOOK_SECRET")),
		APIBaseURL:    strings.TrimRight(envOrDefault("GITHUB_API_URL", "https://api.github.com"), "/"),
		WebBaseURL:    strings.TrimRight(envOrDefault("GITHUB_WEB_URL", "https://github.com"), "/"),
	}
	if cfg.AppID == "" {
		return GitHubAppConfig{}, errGitHubAppNotConfigured
	}

	pemData := os.Getenv("GITHUB_APP_PRIVATE_KEY")
	if strings.TrimSpace(pemData) == "" {
		if path := strings.TrimSpace(os.Getenv("GITHUB_APP_PRIVATE_KEY_PATH")); path != "" {
			data, err := os.ReadFile(path)
			if err != nil {
				return GitHubAppConfig{}, fmt.Errorf("read GITHUB_APP_PRIVATE_KEY_PATH: %w", err)
			}
			pemData = string(data)
		}
	}
	// .env files often carry the PEM on one line with literal \n.
	pemData = strings.ReplaceAll(pemData, `\n`, "\n")
	if strings.TrimSpace(pemData) == "" {
		return GitHubAppConfig{}, fmt.Errorf("GITHUB_APP_ID is set but neither GITHUB_APP_PRIVATE_KEY nor GITHUB_APP_PRIVATE_KEY_PATH is")
	}
	key, err := jwt.ParseRSAPrivateKeyFromPEM([]byte(pemData))
	if err != nil {
		return GitHubAppConfig{}, fmt.Errorf("parse GitHub App private key: %w", err)
	}
	cfg.PrivateKey = key

	for name, value := range map[string]string{
		"GITHUB_APP_SLUG":           cfg.Slug,
		"GITHUB_APP_CLIENT_ID":      cfg.ClientID,
		"GITHUB_APP_CLIENT_SECRET":  cfg.ClientSecret,
		"GITHUB_APP_WEBHOOK_SECRET": cfg.WebhookSecret,
	} {
		if value == "" {
			return GitHubAppConfig{}, fmt.Errorf("%s is required when GITHUB_APP_ID is set", name)
		}
	}
	return cfg, nil
}

func newGitHubAppClient(cfg GitHubAppConfig) *GitHubAppClient {
	return &GitHubAppClient{
		cfg:    cfg,
		http:   &http.Client{Timeout: githubHTTPTimeout},
		now:    time.Now,
		tokens: make(map[int64]GitHubInstallationToken),
	}
}

// InstallURL is where a user installs the app, or edits which repositories
// an existing installation can see. state comes back on the setup callback.
func (g *GitHubAppClient) InstallURL(state string) string {
	return fmt.Sprintf("%s/apps/%s/installations/new?state=%s", g.cfg.WebBaseURL, url.PathEscape(g.cfg.Slug), url.QueryEscape(state))
}

// ============================================================================
// APP AUTHENTICATION
// ============================================================================

// AppJWT signs a JWT that authenticates as the app itself (not as any
// installation). It's only accepted by the /app/* endpoints.
func (g *GitHubAppClient) AppJWT() (string, error) {
	now := g.now()
	claims := jwt.RegisteredClaims{
		Issuer:    g.cfg.AppID,
		IssuedAt:  jwt.NewNumericDate(now.Add(-githubAppJWTClockSkew)),
		ExpiresAt: jwt.NewNumericDate(now.Add(githubAppJWTTTL)),
	}
	signed, err := jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(g.cfg.PrivateKey)
	if err != nil {
		return "", fmt.Errorf("sign GitHub App JWT: %w", err)
	}
	return signed, nil
}

// CreateInstallationToken mints a new installation access token. With a
// repositoryID it's restricted to that one repository and to read-only
// contents, which is all a clone needs; GitHub answers 422 if the
// installation can't access the repository.
func (g *GitHubAppClient) CreateInstallationToken(ctx context.Context, installationID int64, repositoryID int64) (GitHubInstallationToken, error) {
	appJWT, err := g.AppJWT()
	if err != nil {
		return GitHubInstallationToken{}, err
	}

	var body any
	if repositoryID > 0 {
		body = map[string]any{
			"repository_ids": []int64{repositoryID},
			"permissions":    map[string]string{"contents": "read", "metadata": "read"},
		}
	}

	var token GitHubInstallationToken
	path := fmt.Sprintf("/app/installations/%d/access_tokens", installationID)
	if err := g.do(ctx, http.MethodPost, g.cfg.APIBaseURL+path, "Bearer "+appJWT, body, &token); err != nil {
		return GitHubInstallationToken{}, fmt.Errorf("create installation token: %w", err)
	}
	if token.Token == "" {
		return GitHubInstallationToken{}, fmt.Errorf("create installation token: empty token in response")
	}
	return token, nil
}

// installationToken returns an unscoped installation token for API reads,
// reusing a cached one while it has more than githubTokenRefreshMargin left.
func (g *GitHubAppClient) installationToken(ctx context.Context, installationID int64) (string, error) {
	g.mu.Lock()
	cached, ok := g.tokens[installationID]
	g.mu.Unlock()
	if ok && g.now().Add(githubTokenRefreshMargin).Before(cached.ExpiresAt) {
		return cached.Token, nil
	}

	token, err := g.CreateInstallationToken(ctx, installationID, 0)
	if err != nil {
		return "", err
	}
	g.mu.Lock()
	g.tokens[installationID] = token
	g.mu.Unlock()
	return token.Token, nil
}

// forgetInstallation drops the cached token, e.g. after the installation was
// deleted or suspended.
func (g *GitHubAppClient) forgetInstallation(installationID int64) {
	g.mu.Lock()
	delete(g.tokens, installationID)
	g.mu.Unlock()
}

// ============================================================================
// APP-LEVEL READS
// ============================================================================

func (g *GitHubAppClient) GetInstallation(ctx context.Context, installationID int64) (GitHubInstallation, error) {
	appJWT, err := g.AppJWT()
	if err != nil {
		return GitHubInstallation{}, err
	}
	var inst GitHubInstallation
	if err := g.do(ctx, http.MethodGet, fmt.Sprintf("%s/app/installations/%d", g.cfg.APIBaseURL, installationID), "Bearer "+appJWT, nil, &inst); err != nil {
		return GitHubInstallation{}, fmt.Errorf("get installation: %w", err)
	}
	return inst, nil
}

// ListInstallationRepositories returns every repository the installation
// can access.
func (g *GitHubAppClient) ListInstallationRepositories(ctx context.Context, installationID int64) ([]GitHubRepository, error) {
	token, err := g.installationToken(ctx, installationID)
	if err != nil {
		return nil, err
	}

	var repos []GitHubRepository
	for page := 1; page <= githubMaxRepoPages; page++ {
		var resp struct {
			TotalCount   int                `json:"total_count"`
			Repositories []GitHubRepository `json:"repositories"`
		}
		endpoint := fmt.Sprintf("%s/installation/repositories?per_page=100&page=%d", g.cfg.APIBaseURL, page)
		if err := g.do(ctx, http.MethodGet, endpoint, "token "+token, nil, &resp); err != nil {
			return nil, fmt.Errorf("list installation repositories: %w", err)
		}
		repos = append(repos, resp.Repositories...)
		if len(resp.Repositories) < 100 || len(repos) >= resp.TotalCount {
			break
		}
	}
	return repos, nil
}

// GetRepository reads one repository through the installation, which also
// proves the installation can access it.
func (g *GitHubAppClient) GetRepository(ctx context.Context, installationID int64, repositoryID int64) (GitHubRepository, error) {
	token, err := g.installationToken(ctx, installationID)
	if err != nil {
		return GitHubRepository{}, err
	}
	var repo GitHubRepository
	if err := g.do(ctx, http.MethodGet, fmt.Sprintf("%s/repositories/%d", g.cfg.APIBaseURL, repositoryID), "token "+token, nil, &repo); err != nil {
		return GitHubRepository{}, fmt.Errorf("get repository: %w", err)
	}
	return repo, nil
}

// GitHubTreeEntry is one path in a repository tree.
type GitHubTreeEntry struct {
	Path string `json:"path"`
	Type string `json:"type"` // "blob", "tree" or "commit" (submodule)
	Size int64  `json:"size"` // blobs only
}

// GetRepositoryTree lists every path at ref, with blob sizes, in a single
// call. It's how a clone's real size is known up front: the repository
// "size" GitHub reports covers all history, which a --depth 1 clone never
// downloads, so it would wrongly reject repos that are large only in the
// past. truncated is true when GitHub cut the listing short (100k entries),
// in which case the sizes are a lower bound.
func (g *GitHubAppClient) GetRepositoryTree(ctx context.Context, installationID int64, fullName string, ref string) (entries []GitHubTreeEntry, truncated bool, err error) {
	token, err := g.installationToken(ctx, installationID)
	if err != nil {
		return nil, false, err
	}
	var resp struct {
		Truncated bool              `json:"truncated"`
		Tree      []GitHubTreeEntry `json:"tree"`
	}
	endpoint := fmt.Sprintf("%s/repos/%s/git/trees/%s?recursive=1", g.cfg.APIBaseURL, fullName, url.PathEscape(ref))
	if err := g.do(ctx, http.MethodGet, endpoint, "token "+token, nil, &resp); err != nil {
		return nil, false, fmt.Errorf("get repository tree: %w", err)
	}
	return resp.Tree, resp.Truncated, nil
}

// ============================================================================
// USER VERIFICATION (OAuth during installation)
// ============================================================================
//
// The installation_id on the setup callback is just a query parameter, so on
// its own it proves nothing: anyone could send someone else's. With "Request
// user authorization (OAuth) during installation" enabled on the app, the
// callback also carries an OAuth code. Exchanging it gives a user-to-server
// token, and GET /user/installations lists only installations that GitHub
// user can access, which is what we check before linking.

func (g *GitHubAppClient) ExchangeOAuthCode(ctx context.Context, code string) (string, error) {
	form := url.Values{
		"client_id":     {g.cfg.ClientID},
		"client_secret": {g.cfg.ClientSecret},
		"code":          {code},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.cfg.WebBaseURL+"/login/oauth/access_token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := g.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("exchange oauth code: %w", err)
	}
	defer resp.Body.Close()

	// Errors come back as 200 with an "error" field.
	var out struct {
		AccessToken      string `json:"access_token"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return "", fmt.Errorf("exchange oauth code: decode response (status %d): %w", resp.StatusCode, err)
	}
	if out.Error != "" || out.AccessToken == "" {
		return "", fmt.Errorf("exchange oauth code: %s %s", out.Error, out.ErrorDescription)
	}
	return out.AccessToken, nil
}

// UserCanAccessInstallation reports whether the GitHub user behind
// userToken can access installationID.
func (g *GitHubAppClient) UserCanAccessInstallation(ctx context.Context, userToken string, installationID int64) (bool, error) {
	for page := 1; page <= githubMaxRepoPages; page++ {
		var resp struct {
			Installations []GitHubInstallation `json:"installations"`
		}
		endpoint := fmt.Sprintf("%s/user/installations?per_page=100&page=%d", g.cfg.APIBaseURL, page)
		if err := g.do(ctx, http.MethodGet, endpoint, "Bearer "+userToken, nil, &resp); err != nil {
			return false, fmt.Errorf("list user installations: %w", err)
		}
		for _, inst := range resp.Installations {
			if inst.ID == installationID {
				return true, nil
			}
		}
		if len(resp.Installations) < 100 {
			break
		}
	}
	return false, nil
}

// ============================================================================
// HTTP
// ============================================================================

func (g *GitHubAppClient) do(ctx context.Context, method, endpoint, authorization string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "GravyFlow")
	req.Header.Set("Authorization", authorization)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := g.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, githubMaxErrorBody))
		var parsed struct {
			Message string `json:"message"`
		}
		message := strings.TrimSpace(string(raw))
		if json.Unmarshal(raw, &parsed) == nil && parsed.Message != "" {
			message = parsed.Message
		}
		return &GitHubAPIError{StatusCode: resp.StatusCode, Message: message}
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode %s response: %w", strconv.Quote(req.URL.Path), err)
	}
	return nil
}
