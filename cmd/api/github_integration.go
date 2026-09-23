package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
)

// ============================================================================
// GITHUB APP INTEGRATION
// ============================================================================
//
// Flow:
//  1. GET  /integrations/github/setup     -> install URL carrying a signed state
//  2. user installs / configures the app on GitHub
//  3. GET  /integrations/github/callback  -> state + OAuth code verified, the
//     installation is linked to the user, browser goes back to the dashboard
//  4. POST /webhooks/github               -> keeps linked installations in sync
//  5. GET  /integrations/github/repos     -> repositories for the picker
//  6. POST /apps with {github: {installationId, repositoryId}}
//  7. the deployment worker mints a repo-scoped token for every clone
//     (githubCloneToken, called from prepareDeploymentSource)

const (
	githubStateTTL       = 15 * time.Minute
	githubStateAudience  = "github-app-install"
	githubWebhookMaxBody = 5 << 20
	githubWebhookTimeout = 2 * time.Minute
)

// GitHubRepoRef is what the repositories JSONB column caches.
type GitHubRepoRef struct {
	ID       int64  `json:"id"`
	FullName string `json:"fullName"`
	Private  bool   `json:"private"`
}

type GitHubInstallationRecord struct {
	ID                   string          `json:"id"`
	UserID               string          `json:"userId"`
	GitHubInstallationID int64           `json:"githubInstallationId"`
	AccountLogin         string          `json:"accountLogin"`
	AccountType          string          `json:"accountType"`
	RepositorySelection  string          `json:"repositorySelection"`
	Repositories         []GitHubRepoRef `json:"repositories"`
	SuspendedAt          *time.Time      `json:"suspendedAt,omitempty"`
	CreatedAt            time.Time       `json:"createdAt"`
	UpdatedAt            time.Time       `json:"updatedAt"`
}

// GitHubSourceRequest selects a repository through a linked installation
// when creating an app.
type GitHubSourceRequest struct {
	InstallationID int64 `json:"installationId"`
	RepositoryID   int64 `json:"repositoryId"`
}

func repoRefs(repos []GitHubRepository) []GitHubRepoRef {
	refs := make([]GitHubRepoRef, 0, len(repos))
	for _, r := range repos {
		refs = append(refs, GitHubRepoRef{ID: r.ID, FullName: r.FullName, Private: r.Private})
	}
	return refs
}

// ============================================================================
// STORE
// ============================================================================

func (s *DeploymentStore) UpsertGitHubInstallation(ctx context.Context, userID string, inst GitHubInstallation, repos []GitHubRepoRef) error {
	if s == nil || s.pool == nil {
		return &StoreError{Type: ErrDatabase, Message: "deployment store is not initialized"}
	}
	encoded, err := json.Marshal(repos)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
INSERT INTO github_installations
    (user_id, github_installation_id, account_login, account_type, repository_selection, repositories, suspended_at)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (user_id, github_installation_id) DO UPDATE SET
    account_login = EXCLUDED.account_login,
    account_type = EXCLUDED.account_type,
    repository_selection = EXCLUDED.repository_selection,
    repositories = EXCLUDED.repositories,
    suspended_at = EXCLUDED.suspended_at,
    updated_at = now()
`, userID, inst.ID, inst.Account.Login, inst.Account.Type, inst.RepositorySelection, encoded, inst.SuspendedAt)
	if err != nil {
		return &StoreError{Type: ErrDatabase, Message: "failed to save GitHub installation", Err: err}
	}
	return nil
}

// SyncGitHubInstallation updates every user's link to installationID from
// fresh GitHub data. Returns how many links were updated.
func (s *DeploymentStore) SyncGitHubInstallation(ctx context.Context, inst GitHubInstallation, repos []GitHubRepoRef) (int64, error) {
	if s == nil || s.pool == nil {
		return 0, &StoreError{Type: ErrDatabase, Message: "deployment store is not initialized"}
	}
	encoded, err := json.Marshal(repos)
	if err != nil {
		return 0, err
	}
	tag, err := s.pool.Exec(ctx, `
UPDATE github_installations SET
    account_login = $2, account_type = $3, repository_selection = $4,
    repositories = $5, suspended_at = $6, updated_at = now()
WHERE github_installation_id = $1
`, inst.ID, inst.Account.Login, inst.Account.Type, inst.RepositorySelection, encoded, inst.SuspendedAt)
	if err != nil {
		return 0, &StoreError{Type: ErrDatabase, Message: "failed to sync GitHub installation", Err: err}
	}
	return tag.RowsAffected(), nil
}

func (s *DeploymentStore) HasGitHubInstallationLinks(ctx context.Context, installationID int64) (bool, error) {
	var exists bool
	err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM github_installations WHERE github_installation_id = $1)`, installationID).Scan(&exists)
	if err != nil {
		return false, &StoreError{Type: ErrDatabase, Message: "failed to look up GitHub installation", Err: err}
	}
	return exists, nil
}

func (s *DeploymentStore) DeleteGitHubInstallation(ctx context.Context, installationID int64) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM github_installations WHERE github_installation_id = $1`, installationID); err != nil {
		return &StoreError{Type: ErrDatabase, Message: "failed to delete GitHub installation", Err: err}
	}
	return nil
}

func (s *DeploymentStore) UnlinkGitHubInstallationForUser(ctx context.Context, userID string, installationID int64) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM github_installations WHERE user_id = $1 AND github_installation_id = $2`, userID, installationID)
	if err != nil {
		return &StoreError{Type: ErrDatabase, Message: "failed to unlink GitHub installation", Err: err}
	}
	if tag.RowsAffected() == 0 {
		return &StoreError{Type: ErrNotFound, Message: "GitHub installation not linked to this account"}
	}
	return nil
}

func (s *DeploymentStore) ListGitHubInstallationsForUser(ctx context.Context, userID string) ([]GitHubInstallationRecord, error) {
	if s == nil || s.pool == nil {
		return nil, &StoreError{Type: ErrDatabase, Message: "deployment store is not initialized"}
	}
	rows, err := s.pool.Query(ctx, `
SELECT id::text, user_id::text, github_installation_id, account_login, account_type,
       repository_selection, repositories, suspended_at, created_at, updated_at
FROM github_installations
WHERE user_id = $1
ORDER BY account_login
`, userID)
	if err != nil {
		return nil, &StoreError{Type: ErrDatabase, Message: "failed to list GitHub installations", Err: err}
	}
	defer rows.Close()

	records := []GitHubInstallationRecord{}
	for rows.Next() {
		var rec GitHubInstallationRecord
		var repos []byte
		if err := rows.Scan(&rec.ID, &rec.UserID, &rec.GitHubInstallationID, &rec.AccountLogin, &rec.AccountType,
			&rec.RepositorySelection, &repos, &rec.SuspendedAt, &rec.CreatedAt, &rec.UpdatedAt); err != nil {
			return nil, &StoreError{Type: ErrDatabase, Message: "failed to read GitHub installation", Err: err}
		}
		if err := json.Unmarshal(repos, &rec.Repositories); err != nil {
			rec.Repositories = nil
		}
		records = append(records, rec)
	}
	return records, rows.Err()
}

// UserHasGitHubInstallation reports whether userID has linked installationID
// and it isn't suspended.
func (s *DeploymentStore) UserHasGitHubInstallation(ctx context.Context, userID string, installationID int64) (bool, error) {
	var suspendedAt *time.Time
	err := s.pool.QueryRow(ctx, `
SELECT suspended_at FROM github_installations WHERE user_id = $1 AND github_installation_id = $2
`, userID, installationID).Scan(&suspendedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, &StoreError{Type: ErrDatabase, Message: "failed to look up GitHub installation", Err: err}
	}
	return suspendedAt == nil, nil
}

func (s *DeploymentStore) SetDeploymentGitHubSource(ctx context.Context, deploymentID string, installationID, repositoryID int64) error {
	tag, err := s.pool.Exec(ctx, `
UPDATE deployments
SET github_installation_id = $2, github_repository_id = $3, updated_at = now()
WHERE id = $1 AND deleted_at IS NULL
`, deploymentID, installationID, repositoryID)
	if err != nil {
		return &StoreError{Type: ErrDatabase, Message: "failed to save GitHub source", Err: err}
	}
	if tag.RowsAffected() == 0 {
		return &StoreError{Type: ErrNotFound, Message: "deployment not found"}
	}
	return nil
}

// DeploymentGitHubSource is a deployment's GitHub App link. Linked is false
// for deployments created from a plain repository URL.
type DeploymentGitHubSource struct {
	Linked         bool
	OwnerUserID    string
	InstallationID int64
	RepositoryID   int64
	// InstallationActive is false when the owner has since unlinked the
	// installation, or it was uninstalled or suspended on GitHub.
	InstallationActive bool
}

func (s *DeploymentStore) GetDeploymentGitHubSource(ctx context.Context, deploymentID string) (DeploymentGitHubSource, error) {
	if s == nil || s.pool == nil {
		return DeploymentGitHubSource{}, &StoreError{Type: ErrDatabase, Message: "deployment store is not initialized"}
	}
	var src DeploymentGitHubSource
	var installationID, repositoryID *int64
	err := s.pool.QueryRow(ctx, `
SELECT d.owner_user_id::text, d.github_installation_id, d.github_repository_id,
       EXISTS(
           SELECT 1 FROM github_installations gi
           WHERE gi.user_id = d.owner_user_id
             AND gi.github_installation_id = d.github_installation_id
             AND gi.suspended_at IS NULL
       )
FROM deployments d
WHERE d.id = $1
`, deploymentID).Scan(&src.OwnerUserID, &installationID, &repositoryID, &src.InstallationActive)
	if err != nil {
		return DeploymentGitHubSource{}, &StoreError{Type: ErrDatabase, Message: "failed to load GitHub source", Err: err}
	}
	if installationID != nil && repositoryID != nil {
		src.Linked = true
		src.InstallationID = *installationID
		src.RepositoryID = *repositoryID
	}
	return src, nil
}

// ============================================================================
// CLONE TOKEN (deployment worker)
// ============================================================================

// githubCloneToken mints a fresh installation token that can only read the
// deployment's repository. ok is false for deployments not linked through the
// GitHub App, which keep using their per-app access token (git_token.go).
func githubCloneToken(ctx context.Context, deploymentID string) (token string, ok bool, err error) {
	src, err := deploymentStore.GetDeploymentGitHubSource(ctx, deploymentID)
	if err != nil {
		return "", false, err
	}
	if !src.Linked {
		return "", false, nil
	}
	if !src.InstallationActive {
		return "", true, fmt.Errorf("this service clones through the GitHub App, but its installation was removed, suspended, or unlinked from your account; reconnect GitHub and pick the repository again")
	}
	client, err := githubApp()
	if err != nil {
		return "", true, fmt.Errorf("this service clones through the GitHub App: %w", err)
	}
	minted, err := client.CreateInstallationToken(ctx, src.InstallationID, src.RepositoryID)
	if err != nil {
		if isGitHubStatus(err, http.StatusUnprocessableEntity) || isGitHubStatus(err, http.StatusNotFound) {
			return "", true, fmt.Errorf("the GitHub App no longer has access to this repository; grant it access in the installation's settings on GitHub: %w", err)
		}
		return "", true, err
	}
	return minted.Token, true, nil
}

// ============================================================================
// INSTALL STATE
// ============================================================================
//
// The state parameter binds the GitHub round trip to the user who started it
// (the callback is a browser redirect, so it carries no bearer token) and
// stops another site from completing an installation into someone's account.

type githubStateClaims struct {
	jwt.RegisteredClaims
}

func githubStateSecret() []byte {
	// Derived, so a state can never be replayed as an access token.
	mac := hmac.New(sha256.New, []byte(envOrDefault("AUTH_JWT_SECRET", "dev-auth-secret-change-me-in-production")))
	mac.Write([]byte("gravyflow/github-app-install-state"))
	return mac.Sum(nil)
}

func issueGitHubInstallState(userID string) (string, error) {
	nonce, err := generateRandomToken(16)
	if err != nil {
		return "", err
	}
	now := time.Now()
	claims := githubStateClaims{jwt.RegisteredClaims{
		Subject:   userID,
		Audience:  jwt.ClaimStrings{githubStateAudience},
		ID:        nonce,
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(now.Add(githubStateTTL)),
	}}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(githubStateSecret())
}

func parseGitHubInstallState(state string) (string, error) {
	var claims githubStateClaims
	_, err := jwt.ParseWithClaims(state, &claims, func(*jwt.Token) (any, error) {
		return githubStateSecret(), nil
	}, jwt.WithValidMethods([]string{"HS256"}), jwt.WithAudience(githubStateAudience), jwt.WithExpirationRequired())
	if err != nil {
		return "", err
	}
	if claims.Subject == "" {
		return "", errors.New("state has no subject")
	}
	return claims.Subject, nil
}

// ============================================================================
// HANDLERS
// ============================================================================

func githubDashboardURL(result string) string {
	base := envOrDefault("GRAVYFLOW_DASHBOARD_URL", "http://localhost:3000/dashboard")
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	return base + sep + "github=" + url.QueryEscape(result)
}

func githubAppOrUnavailable(c *gin.Context) (*GitHubAppClient, bool) {
	client, err := githubApp()
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "github_app_not_configured", "details": err.Error()})
		return nil, false
	}
	return client, true
}

// githubSetupHandler returns the GitHub install URL. It's JSON rather than a
// 302 because the dashboard authenticates with a bearer header, which a
// top-level navigation can't send; the frontend sets window.location itself.
func githubSetupHandler(c *gin.Context) {
	user, ok := currentAuthUser(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	if _, impersonating := currentImpersonatorID(c); impersonating {
		c.JSON(http.StatusForbidden, gin.H{"error": "impersonation_read_only", "details": "admin impersonation sessions cannot connect GitHub"})
		return
	}
	client, ok := githubAppOrUnavailable(c)
	if !ok {
		return
	}
	state, err := issueGitHubInstallState(user.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_start_github_setup", "details": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"url": client.InstallURL(state)})
}

// githubCallbackHandler is the app's Setup/Callback URL. GitHub redirects the
// browser here after an install or configuration change with
// ?installation_id=&setup_action=&state=&code=. Always answers with a redirect
// back to the dashboard, carrying the outcome in ?github=.
func githubCallbackHandler(c *gin.Context) {
	fail := func(reason string, err error) {
		if err != nil {
			log.Printf("[github] setup callback rejected (%s): %v", reason, err)
		}
		c.Redirect(http.StatusFound, githubDashboardURL("error:"+reason))
	}

	client, err := githubApp()
	if err != nil {
		fail("not_configured", err)
		return
	}

	userID, err := parseGitHubInstallState(c.Query("state"))
	if err != nil {
		// Also the path for installs started from github.com rather than
		// from the dashboard; there's no way to know which user they're for.
		fail("invalid_state", err)
		return
	}
	installationID, err := strconv.ParseInt(c.Query("installation_id"), 10, 64)
	if err != nil || installationID <= 0 {
		// setup_action=request: an org member asked an owner to approve it.
		if c.Query("setup_action") == "request" {
			c.Redirect(http.StatusFound, githubDashboardURL("requested"))
			return
		}
		fail("missing_installation", err)
		return
	}
	code := c.Query("code")
	if code == "" {
		fail("missing_oauth_code", errors.New(`enable "Request user authorization (OAuth) during installation" on the GitHub App`))
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), time.Minute)
	defer cancel()

	userToken, err := client.ExchangeOAuthCode(ctx, code)
	if err != nil {
		fail("oauth_failed", err)
		return
	}
	allowed, err := client.UserCanAccessInstallation(ctx, userToken, installationID)
	if err != nil {
		fail("oauth_failed", err)
		return
	}
	if !allowed {
		fail("installation_not_accessible", fmt.Errorf("github user cannot access installation %d", installationID))
		return
	}

	inst, err := client.GetInstallation(ctx, installationID)
	if err != nil {
		fail("github_unavailable", err)
		return
	}
	repos, err := client.ListInstallationRepositories(ctx, installationID)
	if err != nil {
		fail("github_unavailable", err)
		return
	}
	if err := deploymentStore.UpsertGitHubInstallation(ctx, userID, inst, repoRefs(repos)); err != nil {
		fail("save_failed", err)
		return
	}
	c.Redirect(http.StatusFound, githubDashboardURL("connected"))
}

func listGitHubInstallationsHandler(c *gin.Context) {
	user, ok := currentAuthUser(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	_, configErr := githubApp()
	records, err := deploymentStore.ListGitHubInstallationsForUser(c.Request.Context(), user.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_list_installations", "details": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"configured": configErr == nil, "installations": records})
}

func unlinkGitHubInstallationHandler(c *gin.Context) {
	user, ok := currentAuthUser(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	installationID, err := strconv.ParseInt(c.Param("installationId"), 10, 64)
	if err != nil {
		sendBadRequest(c, "invalid installation id", err)
		return
	}
	if err := deploymentStore.UnlinkGitHubInstallationForUser(c.Request.Context(), user.ID, installationID); err != nil {
		var storeErr *StoreError
		if errors.As(err, &storeErr) && storeErr.Type == ErrNotFound {
			c.JSON(http.StatusNotFound, gin.H{"error": "installation_not_found", "details": err.Error()})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_unlink_installation", "details": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"unlinked": true})
}

type githubRepoResponse struct {
	ID             int64      `json:"id"`
	InstallationID int64      `json:"installationId"`
	Name           string     `json:"name"`
	FullName       string     `json:"fullName"`
	Owner          string     `json:"owner"`
	Private        bool       `json:"private"`
	DefaultBranch  string     `json:"defaultBranch"`
	CloneURL       string     `json:"cloneUrl"`
	HTMLURL        string     `json:"htmlUrl"`
	PushedAt       *time.Time `json:"pushedAt,omitempty"`
}

// listGitHubReposHandler lists every repository the user's linked
// installations can access, live from GitHub, most recently pushed first.
// The cached repositories column is refreshed along the way.
func listGitHubReposHandler(c *gin.Context) {
	user, ok := currentAuthUser(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	client, ok := githubAppOrUnavailable(c)
	if !ok {
		return
	}
	ctx := c.Request.Context()
	installations, err := deploymentStore.ListGitHubInstallationsForUser(ctx, user.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_list_installations", "details": err.Error()})
		return
	}

	repos := []githubRepoResponse{}
	var failures []string
	for _, inst := range installations {
		if inst.SuspendedAt != nil {
			continue
		}
		list, err := client.ListInstallationRepositories(ctx, inst.GitHubInstallationID)
		if err != nil {
			if isGitHubStatus(err, http.StatusNotFound) {
				// Uninstalled while we missed the webhook.
				client.forgetInstallation(inst.GitHubInstallationID)
				_ = deploymentStore.DeleteGitHubInstallation(ctx, inst.GitHubInstallationID)
				continue
			}
			failures = append(failures, fmt.Sprintf("%s: %v", inst.AccountLogin, err))
			continue
		}
		_, _ = deploymentStore.SyncGitHubInstallation(ctx, GitHubInstallation{
			ID:                  inst.GitHubInstallationID,
			Account:             GitHubAccount{Login: inst.AccountLogin, Type: inst.AccountType},
			RepositorySelection: inst.RepositorySelection,
		}, repoRefs(list))
		for _, r := range list {
			repos = append(repos, githubRepoResponse{
				ID: r.ID, InstallationID: inst.GitHubInstallationID, Name: r.Name, FullName: r.FullName,
				Owner: r.Owner.Login, Private: r.Private, DefaultBranch: r.DefaultBranch,
				CloneURL: r.CloneURL, HTMLURL: r.HTMLURL, PushedAt: r.PushedAt,
			})
		}
	}
	sort.SliceStable(repos, func(i, j int) bool {
		a, b := repos[i].PushedAt, repos[j].PushedAt
		if a == nil || b == nil {
			return a != nil
		}
		return a.After(*b)
	})

	resp := gin.H{"repositories": repos}
	if len(failures) > 0 {
		resp["warnings"] = failures
	}
	c.JSON(http.StatusOK, resp)
}

// resolveGitHubSource checks a create-app request's repository selection and
// returns the repository's canonical clone URL. The client-supplied repo URL
// is never trusted for GitHub App sources.
func resolveGitHubSource(ctx context.Context, userID string, src GitHubSourceRequest) (GitHubRepository, int, error) {
	if src.InstallationID <= 0 || src.RepositoryID <= 0 {
		return GitHubRepository{}, http.StatusBadRequest, errors.New("github.installationId and github.repositoryId are required")
	}
	client, err := githubApp()
	if err != nil {
		return GitHubRepository{}, http.StatusServiceUnavailable, err
	}
	linked, err := deploymentStore.UserHasGitHubInstallation(ctx, userID, src.InstallationID)
	if err != nil {
		return GitHubRepository{}, http.StatusInternalServerError, err
	}
	if !linked {
		return GitHubRepository{}, http.StatusForbidden, errors.New("that GitHub installation isn't connected to your account (or is suspended)")
	}
	repo, err := client.GetRepository(ctx, src.InstallationID, src.RepositoryID)
	if err != nil {
		if isGitHubStatus(err, http.StatusNotFound) || isGitHubStatus(err, http.StatusForbidden) {
			return GitHubRepository{}, http.StatusForbidden, errors.New("the GitHub App can't access that repository")
		}
		return GitHubRepository{}, http.StatusBadGateway, err
	}
	if repo.CloneURL == "" {
		return GitHubRepository{}, http.StatusBadGateway, errors.New("github returned no clone URL for the repository")
	}
	return repo, 0, nil
}

// ============================================================================
// WEBHOOK
// ============================================================================

// verifyGitHubSignature checks X-Hub-Signature-256 in constant time.
func verifyGitHubSignature(secret string, body []byte, header string) bool {
	const prefix = "sha256="
	if secret == "" || !strings.HasPrefix(header, prefix) {
		return false
	}
	got, err := hex.DecodeString(strings.TrimPrefix(header, prefix))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

type githubWebhookPayload struct {
	Action       string             `json:"action"`
	Installation GitHubInstallation `json:"installation"`
}

// githubWebhookHandler keeps linked installations in sync. Events can arrive
// out of order and be redelivered, so rather than applying the added/removed
// diffs in the payload, any change re-reads the installation and its
// repositories from the API; the result is correct whatever order they come in.
func githubWebhookHandler(c *gin.Context) {
	client, err := githubApp()
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "github_app_not_configured"})
		return
	}

	body, err := io.ReadAll(io.LimitReader(c.Request.Body, githubWebhookMaxBody+1))
	if err != nil || len(body) > githubWebhookMaxBody {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "payload_too_large"})
		return
	}
	if !verifyGitHubSignature(client.cfg.WebhookSecret, body, c.GetHeader("X-Hub-Signature-256")) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid_signature"})
		return
	}

	event := c.GetHeader("X-GitHub-Event")
	var payload githubWebhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		sendBadRequest(c, "invalid JSON body", err)
		return
	}
	installationID := payload.Installation.ID

	switch {
	case event == "ping":
		c.JSON(http.StatusOK, gin.H{"ok": true})
		return
	case installationID <= 0:
		c.JSON(http.StatusAccepted, gin.H{"ignored": event})
		return
	case event == "installation" && payload.Action == "deleted":
		client.forgetInstallation(installationID)
		if err := deploymentStore.DeleteGitHubInstallation(c.Request.Context(), installationID); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_delete_installation"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"deleted": installationID})
		return
	case event == "installation", event == "installation_repositories", event == "installation_target":
		// Handled below.
	default:
		c.JSON(http.StatusAccepted, gin.H{"ignored": event})
		return
	}

	if event == "installation" && payload.Action == "suspend" {
		client.forgetInstallation(installationID)
	}

	// GitHub gives up on deliveries after 10 seconds, so the API round trips
	// happen after answering.
	go func(delivery string) {
		ctx, cancel := context.WithTimeout(context.Background(), githubWebhookTimeout)
		defer cancel()
		if err := refreshGitHubInstallation(ctx, client, installationID); err != nil {
			log.Printf("[github] webhook %s (%s.%s): refresh installation %d: %v", delivery, event, payload.Action, installationID, err)
		}
	}(c.GetHeader("X-GitHub-Delivery"))

	c.JSON(http.StatusAccepted, gin.H{"queued": installationID})
}

func refreshGitHubInstallation(ctx context.Context, client *GitHubAppClient, installationID int64) error {
	// Installations nobody here has linked yet are picked up by the setup
	// callback, which reads everything itself.
	linked, err := deploymentStore.HasGitHubInstallationLinks(ctx, installationID)
	if err != nil || !linked {
		return err
	}

	inst, err := client.GetInstallation(ctx, installationID)
	if isGitHubStatus(err, http.StatusNotFound) {
		client.forgetInstallation(installationID)
		return deploymentStore.DeleteGitHubInstallation(ctx, installationID)
	}
	if err != nil {
		return err
	}

	var refs []GitHubRepoRef
	if inst.SuspendedAt == nil {
		repos, err := client.ListInstallationRepositories(ctx, installationID)
		if err != nil {
			return err
		}
		refs = repoRefs(repos)
	} else {
		refs = []GitHubRepoRef{}
	}
	_, err = deploymentStore.SyncGitHubInstallation(ctx, inst, refs)
	return err
}

// githubAppLogStartup reports the integration's state once at boot, so a
// half-filled configuration is noticed before someone clicks Connect.
func githubAppLogStartup() {
	if _, err := githubApp(); err == nil {
		log.Printf("GitHub App integration enabled (app %s)", os.Getenv("GITHUB_APP_ID"))
	}
}
