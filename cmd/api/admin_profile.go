package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
)

// ============================================================================
// Admin Profile & Credentials Management (Module E)
//
// Self-service settings for the authenticated admin/SRE's OWN account:
// password rotation (with current-password verification and an optional
// "log out everywhere else" step), MFA disable, and MFA recovery-code
// regeneration. Enrollment/enable already exist in admin_mfa.go — this file
// only adds what an admin does to an account they already control.
//
// Every handler here re-fetches the caller's UserRecord via GetUserByID
// instead of trusting the copy AuthMiddleware attached to the context: that
// copy can be stale by the time the request lands (e.g. a previous password
// change in the same session), and a stale password hash would make the
// "current password" check meaningless.
// ============================================================================

const (
	// minAdminPasswordLength is deliberately higher than the 8-character
	// floor registerHandler (auth.go) applies to ordinary users — these are
	// credentials for accounts with cluster-wide admin access.
	minAdminPasswordLength = 12
	recoveryCodeCount      = 10
)

// ----------------------------------------------------------------------------
// Password strength
// ----------------------------------------------------------------------------

// commonWeakPasswords is a tiny denylist, not a substitute for a real
// breached-password check (e.g. the HaveIBeenPwned k-anonymity API) — that's
// the right long-term fix and is intentionally out of scope here.
var commonWeakPasswords = map[string]bool{
	"password123!": true, "qwerty123456": true, "letmein12345": true,
	"admin1234567!": true, "changeme12345": true, "welcome123456!": true,
}

// validatePasswordStrength returns one violation code per failed rule (empty
// slice = passes) so the UI can render a live checklist instead of a single
// opaque error. It's a deliberately lightweight, dependency-free proxy for
// real entropy estimation — swap in a library such as zxcvbn if this ever
// needs to catch more than the obvious cases.
func validatePasswordStrength(password string, email string) []string {
	var violations []string

	if len(password) < minAdminPasswordLength {
		violations = append(violations, "min_length")
	}

	var hasUpper, hasLower, hasDigit, hasSpecial bool
	unique := make(map[rune]bool)
	for _, r := range password {
		unique[r] = true
		switch {
		case unicode.IsUpper(r):
			hasUpper = true
		case unicode.IsLower(r):
			hasLower = true
		case unicode.IsDigit(r):
			hasDigit = true
		case unicode.IsPunct(r) || unicode.IsSymbol(r):
			hasSpecial = true
		}
	}
	if !hasUpper {
		violations = append(violations, "uppercase")
	}
	if !hasLower {
		violations = append(violations, "lowercase")
	}
	if !hasDigit {
		violations = append(violations, "digit")
	}
	if !hasSpecial {
		violations = append(violations, "special_char")
	}

	// Crude entropy floor: fewer than 6 distinct characters means the
	// password is dominated by repeats/sequences ("aaaaaaaaaaaa",
	// "abababababab") even if it happens to clear the length bar above.
	if len(unique) < 6 {
		violations = append(violations, "low_entropy")
	}

	if localPart := strings.ToLower(strings.SplitN(email, "@", 2)[0]); len(localPart) >= 4 && strings.Contains(strings.ToLower(password), localPart) {
		violations = append(violations, "contains_email")
	}

	if commonWeakPasswords[strings.ToLower(password)] {
		violations = append(violations, "common_password")
	}

	return violations
}

// ----------------------------------------------------------------------------
// Store methods
// ----------------------------------------------------------------------------

// UpdateUserPassword rotates password_hash and stamps password_changed_at.
// It deliberately does not touch refresh_tokens — whether other sessions get
// revoked is an independent choice the caller makes (see
// adminChangePasswordHandler's logoutOtherDevices branch), not something
// baked into "change the password."
func (s *DeploymentStore) UpdateUserPassword(ctx context.Context, userID string, newPasswordHash string) error {
	if s == nil || s.pool == nil {
		return &StoreError{Type: ErrDatabase, Message: "deployment store is not initialized"}
	}
	_, err := s.pool.Exec(ctx, `UPDATE users SET password_hash = $1, password_changed_at = now() WHERE id = $2`, newPasswordHash, userID)
	return err
}

// SetMFARecoveryCodes overwrites the stored recovery codes with a new hashed
// set. Codes are hashed with hashToken (SHA-256, helpers.go) — the same
// scheme used for refresh tokens and API keys — because they're bearer
// secrets redeemed by exact match, not passwords a human types repeatedly;
// bcrypt's deliberate slowness buys nothing here.
func (s *DeploymentStore) SetMFARecoveryCodes(ctx context.Context, userID string, hashedCodes []string) error {
	if s == nil || s.pool == nil {
		return &StoreError{Type: ErrDatabase, Message: "deployment store is not initialized"}
	}
	_, err := s.pool.Exec(ctx, `UPDATE users SET mfa_recovery_codes = $1 WHERE id = $2`, hashedCodes, userID)
	return err
}

// ----------------------------------------------------------------------------
// POST /admin/profile/password
// ----------------------------------------------------------------------------

type AdminChangePasswordRequest struct {
	CurrentPassword    string `json:"currentPassword" binding:"required"`
	NewPassword        string `json:"newPassword" binding:"required"`
	LogoutOtherDevices bool   `json:"logoutOtherDevices"`
}

type AdminChangePasswordResponse struct {
	Message         string `json:"message"`
	SessionsRevoked bool   `json:"sessionsRevoked"`
	// Populated only when LogoutOtherDevices is true: revoking *every*
	// refresh token for the user would also log this very request's session
	// out, so a fresh pair is issued in the same response instead of leaving
	// the caller locked out by their own action.
	AccessToken  string `json:"accessToken,omitempty"`
	RefreshToken string `json:"refreshToken,omitempty"`
	ExpiresIn    int64  `json:"expiresIn,omitempty"`
}

// adminChangePasswordHandler verifies the caller's current password, applies
// strength validation to the replacement, rotates the hash, optionally
// revokes every other session's refresh token, and always writes an audit
// log entry — including on a failed current-password check, since repeated
// failures against this endpoint are themselves a signal Module D should
// capture (e.g. a stolen access token being used to probe for the password).
//
// This is intentionally decoupled from MFA: a password rotation never reads,
// requires, or mutates mfa_enabled / mfa_totp_secret / mfa_recovery_codes.
// Enrolling, disabling, or regenerating recovery codes are separate,
// deliberate actions (mfaEnrollHandler/mfaEnableHandler in admin_mfa.go,
// adminMFADisableHandler/adminRegenerateRecoveryCodesHandler below) — a
// password change must not have side effects on a factor it didn't touch.
func adminChangePasswordHandler(c *gin.Context) {
	caller, ok := currentAuthUser(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	var req AdminChangePasswordRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		sendBadRequest(c, "invalid JSON body", err)
		return
	}

	fresh, err := deploymentStore.GetUserByID(c.Request.Context(), caller.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_load_user", "details": err.Error()})
		return
	}

	actorID, actorEmail := auditActorFromContext(c)

	if err := bcrypt.CompareHashAndPassword([]byte(fresh.PasswordHash), []byte(req.CurrentPassword)); err != nil {
		if logErr := RecordAuditLog(c.Request.Context(), actorID, actorEmail, "admin.password.change_failed", "user", caller.ID, map[string]any{"reason": "invalid_current_password"}, c.ClientIP()); logErr != nil {
			fmt.Printf("[WARN] failed to record audit log for admin.password.change_failed on %q: %v\n", caller.ID, logErr)
		}
		// 403, not 401: the bearer token itself is valid — this is an
		// authorization failure on the *action*. Returning 401 would make
		// the frontend's axios interceptor (lib/api.ts) mistake a wrong
		// current-password attempt for an expired token and burn a refresh
		// call before the real error ever reaches the form.
		c.JSON(http.StatusForbidden, gin.H{"error": "invalid_current_password"})
		return
	}

	if violations := validatePasswordStrength(req.NewPassword, fresh.Email); len(violations) > 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "weak_password", "violations": violations})
		return
	}

	if bcrypt.CompareHashAndPassword([]byte(fresh.PasswordHash), []byte(req.NewPassword)) == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "password_reuse", "details": "new password must differ from the current one"})
		return
	}

	// bcrypt, not Argon2id: every other password path in this codebase
	// (registerHandler in auth.go, and loginHandler's verification of
	// whatever hash is on the row) is bcrypt.CompareHashAndPassword.
	// Hashing this one path with a different algorithm would leave the
	// users.password_hash column holding two incompatible formats with no
	// way to tell them apart on login without a scheme prefix — a bigger,
	// deliberate migration, not a side effect of this feature.
	newHash, err := bcrypt.GenerateFromPassword([]byte(req.NewPassword), bcrypt.DefaultCost)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_hash_password", "details": err.Error()})
		return
	}

	if err := deploymentStore.UpdateUserPassword(c.Request.Context(), caller.ID, string(newHash)); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_update_password", "details": err.Error()})
		return
	}

	resp := AdminChangePasswordResponse{Message: "password updated", SessionsRevoked: req.LogoutOtherDevices}

	if req.LogoutOtherDevices {
		// Revoke every refresh token for this user — including this
		// session's — then immediately issue a fresh pair so the caller
		// isn't caught in their own blast radius. Every *other* device's
		// refresh token is now dead, and its access token expires on its
		// own within AUTH_ACCESS_TOKEN_TTL (15 min by default): access
		// tokens are stateless HS256 JWTs with no server-side revocation
		// list (parseAndValidateToken only checks signature + exp), so that
		// TTL — not this call — is the real upper bound on "logged out
		// everywhere." Shortening AUTH_ACCESS_TOKEN_TTL tightens that bound;
		// a token-blocklist/session-version claim would close it entirely,
		// but that's a larger change than this endpoint should make alone.
		if err := deploymentStore.RevokeAllUserTokens(c.Request.Context(), caller.ID); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_revoke_sessions", "details": err.Error()})
			return
		}

		accessToken, accessExpiry, err := issueAccessToken(fresh)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_issue_access_token", "details": err.Error()})
			return
		}
		refreshToken, refreshExpiry, err := issueRefreshToken(fresh)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_issue_refresh_token", "details": err.Error()})
			return
		}
		if err := deploymentStore.StoreRefreshToken(c.Request.Context(), fresh.ID, hashToken(refreshToken), refreshExpiry); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_store_refresh_token", "details": err.Error()})
			return
		}

		resp.AccessToken = accessToken
		resp.RefreshToken = refreshToken
		resp.ExpiresIn = int64(accessExpiry.Sub(time.Now().UTC()).Seconds())
	}

	if logErr := RecordAuditLog(c.Request.Context(), actorID, actorEmail, "admin.password.changed", "user", caller.ID, map[string]any{"logoutOtherDevices": req.LogoutOtherDevices}, c.ClientIP()); logErr != nil {
		fmt.Printf("[WARN] failed to record audit log for admin.password.changed on %q: %v\n", caller.ID, logErr)
	}

	c.JSON(http.StatusOK, resp)
}

// ----------------------------------------------------------------------------
// POST /admin/profile/mfa/disable
// ----------------------------------------------------------------------------

type AdminMFADisableRequest struct {
	CurrentPassword string `json:"currentPassword" binding:"required"`
}

// adminMFADisableHandler turns MFA off for the caller's own account. Like
// mfaEnableHandler (admin_mfa.go) requires a live TOTP code to turn MFA *on*,
// this requires the current password to turn it *off* — a stolen bearer
// token alone must never be enough to strip an account's own defenses.
func adminMFADisableHandler(c *gin.Context) {
	caller, ok := currentAuthUser(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	var req AdminMFADisableRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		sendBadRequest(c, "invalid JSON body", err)
		return
	}

	fresh, err := deploymentStore.GetUserByID(c.Request.Context(), caller.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_load_user", "details": err.Error()})
		return
	}

	actorID, actorEmail := auditActorFromContext(c)

	if err := bcrypt.CompareHashAndPassword([]byte(fresh.PasswordHash), []byte(req.CurrentPassword)); err != nil {
		if logErr := RecordAuditLog(c.Request.Context(), actorID, actorEmail, "admin.mfa.disable_failed", "user", caller.ID, map[string]any{"reason": "invalid_current_password"}, c.ClientIP()); logErr != nil {
			fmt.Printf("[WARN] failed to record audit log for admin.mfa.disable_failed on %q: %v\n", caller.ID, logErr)
		}
		c.JSON(http.StatusForbidden, gin.H{"error": "invalid_current_password"})
		return
	}

	if !fresh.MFAEnabled {
		c.JSON(http.StatusConflict, gin.H{"error": "mfa_not_enabled"})
		return
	}

	if err := deploymentStore.DisableMFA(c.Request.Context(), caller.ID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_disable_mfa", "details": err.Error()})
		return
	}

	if logErr := RecordAuditLog(c.Request.Context(), actorID, actorEmail, "admin.mfa.disabled", "user", caller.ID, nil, c.ClientIP()); logErr != nil {
		fmt.Printf("[WARN] failed to record audit log for admin.mfa.disabled on %q: %v\n", caller.ID, logErr)
	}

	c.JSON(http.StatusOK, gin.H{"message": "mfa disabled"})
}

// ----------------------------------------------------------------------------
// POST /admin/profile/mfa/recovery-codes/regenerate
// ----------------------------------------------------------------------------

type AdminRegenerateRecoveryCodesResponse struct {
	Codes []string `json:"codes"`
}

// generateRecoveryCode returns a display form ("A1B2C-D3E4F", shown to the
// admin exactly once) and the hash of its canonical (uppercase, no hyphen)
// form, which is what actually gets persisted and later compared against.
func generateRecoveryCode() (display string, hashed string, err error) {
	buf := make([]byte, 5) // 10 hex chars
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("generate recovery code: %w", err)
	}
	canonical := strings.ToUpper(hex.EncodeToString(buf))
	return canonical[:5] + "-" + canonical[5:], hashToken(canonical), nil
}

// adminRegenerateRecoveryCodesHandler issues a fresh set of one-time MFA
// fallback codes, overwriting whatever set existed before — so a leaked or
// partially-used set stops working the instant a new one is generated. The
// plaintext codes are returned exactly once in this response; only their
// SHA-256 hashes are persisted, so the caller must save them immediately.
//
// Note: this endpoint generates and stores codes; it does not add a
// code-redemption step to the login flow (mfaVerifyHandler in admin_mfa.go
// still only accepts a live TOTP code). Wiring recovery-code login is a
// separate change to that handler and is out of scope here.
func adminRegenerateRecoveryCodesHandler(c *gin.Context) {
	caller, ok := currentAuthUser(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	fresh, err := deploymentStore.GetUserByID(c.Request.Context(), caller.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_load_user", "details": err.Error()})
		return
	}
	if !fresh.MFAEnabled {
		c.JSON(http.StatusConflict, gin.H{"error": "mfa_not_enabled", "details": "enable MFA before generating recovery codes"})
		return
	}

	codes := make([]string, recoveryCodeCount)
	hashed := make([]string, recoveryCodeCount)
	for i := range codes {
		display, hash, err := generateRecoveryCode()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_generate_recovery_codes", "details": err.Error()})
			return
		}
		codes[i] = display
		hashed[i] = hash
	}

	if err := deploymentStore.SetMFARecoveryCodes(c.Request.Context(), caller.ID, hashed); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_store_recovery_codes", "details": err.Error()})
		return
	}

	actorID, actorEmail := auditActorFromContext(c)
	if logErr := RecordAuditLog(c.Request.Context(), actorID, actorEmail, "admin.mfa.recovery_codes.regenerated", "user", caller.ID, map[string]any{"count": recoveryCodeCount}, c.ClientIP()); logErr != nil {
		fmt.Printf("[WARN] failed to record audit log for admin.mfa.recovery_codes.regenerated on %q: %v\n", caller.ID, logErr)
	}

	c.JSON(http.StatusOK, AdminRegenerateRecoveryCodesResponse{Codes: codes})
}
