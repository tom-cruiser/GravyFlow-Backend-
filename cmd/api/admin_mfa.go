package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// ============================================================================
// TOTP (RFC 6238 / RFC 4226) — stdlib only, no external dependency.
// ============================================================================

const (
	totpPeriodSeconds = 30
	totpDigits        = 6
	totpSkewPeriods   = 1 // tolerate ±1 period of clock drift
)

func generateTOTPSecret() (string, error) {
	buf := make([]byte, 20)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate totp secret: %w", err)
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf), nil
}

func totpCodeAt(secret string, t time.Time) (string, error) {
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(strings.TrimSpace(secret)))
	if err != nil {
		return "", fmt.Errorf("decode totp secret: %w", err)
	}

	counter := uint64(t.Unix()) / totpPeriodSeconds
	counterBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(counterBytes, counter)

	mac := hmac.New(sha1.New, key)
	mac.Write(counterBytes)
	sum := mac.Sum(nil)

	offset := sum[len(sum)-1] & 0x0f
	truncated := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff
	code := truncated % uint32(math.Pow10(totpDigits))

	return fmt.Sprintf("%0*d", totpDigits, code), nil
}

func verifyTOTPCode(secret string, code string) bool {
	code = strings.TrimSpace(code)
	if secret == "" || code == "" {
		return false
	}
	now := time.Now().UTC()
	for skew := -totpSkewPeriods; skew <= totpSkewPeriods; skew++ {
		expected, err := totpCodeAt(secret, now.Add(time.Duration(skew*totpPeriodSeconds)*time.Second))
		if err != nil {
			return false
		}
		if hmacEqual(expected, code) {
			return true
		}
	}
	return false
}

func totpProvisioningURI(email string, secret string) string {
	const issuer = "GravyFlow"
	label := fmt.Sprintf("%s:%s", issuer, email)
	return fmt.Sprintf(
		"otpauth://totp/%s?secret=%s&issuer=%s&digits=%d&period=%d",
		url.PathEscape(label), secret, url.QueryEscape(issuer), totpDigits, totpPeriodSeconds,
	)
}

// ============================================================================
// STORE METHODS
// ============================================================================

// SetPendingMFASecret stores a freshly-generated TOTP secret without enabling
// it yet — the admin must prove possession via mfaEnableHandler first.
func (s *DeploymentStore) SetPendingMFASecret(ctx context.Context, userID string, secret string) error {
	if s == nil || s.pool == nil {
		return &StoreError{Type: ErrDatabase, Message: "deployment store is not initialized"}
	}
	_, err := s.pool.Exec(ctx, `UPDATE users SET mfa_totp_secret = $1, mfa_enabled = FALSE WHERE id = $2`, secret, userID)
	return err
}

// EnableMFA flips mfa_enabled once the enrollment code has been verified.
func (s *DeploymentStore) EnableMFA(ctx context.Context, userID string) error {
	if s == nil || s.pool == nil {
		return &StoreError{Type: ErrDatabase, Message: "deployment store is not initialized"}
	}
	_, err := s.pool.Exec(ctx, `UPDATE users SET mfa_enabled = TRUE WHERE id = $1`, userID)
	return err
}

// DisableMFA turns MFA off and clears the stored secret.
func (s *DeploymentStore) DisableMFA(ctx context.Context, userID string) error {
	if s == nil || s.pool == nil {
		return &StoreError{Type: ErrDatabase, Message: "deployment store is not initialized"}
	}
	_, err := s.pool.Exec(ctx, `UPDATE users SET mfa_enabled = FALSE, mfa_totp_secret = NULL WHERE id = $1`, userID)
	return err
}

// ============================================================================
// HANDLERS
// ============================================================================

type MFAEnrollResponse struct {
	Secret          string `json:"secret"`
	ProvisioningURI string `json:"provisioningUri"`
}

// mfaEnrollHandler generates a new TOTP secret for the authenticated admin.
// The secret is not active until confirmed via mfaEnableHandler.
func mfaEnrollHandler(c *gin.Context) {
	user, ok := currentAuthUser(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}
	if !user.IsAdmin {
		c.JSON(http.StatusForbidden, gin.H{"error": "forbidden", "details": "MFA enrollment is limited to admin accounts"})
		return
	}

	secret, err := generateTOTPSecret()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_generate_secret", "details": err.Error()})
		return
	}

	if err := deploymentStore.SetPendingMFASecret(c.Request.Context(), user.ID, secret); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_store_secret", "details": err.Error()})
		return
	}

	c.JSON(http.StatusOK, MFAEnrollResponse{
		Secret:          secret,
		ProvisioningURI: totpProvisioningURI(user.Email, secret),
	})
}

type MFAEnableRequest struct {
	Code string `json:"code" binding:"required"`
}

// mfaEnableHandler confirms enrollment by checking one live TOTP code.
func mfaEnableHandler(c *gin.Context) {
	user, ok := currentAuthUser(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	var req MFAEnableRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		sendBadRequest(c, "invalid JSON body", err)
		return
	}

	// Re-fetch: currentAuthUser may be stale if enroll happened moments ago in
	// the same session.
	fresh, err := deploymentStore.GetUserByID(c.Request.Context(), user.ID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_load_user", "details": err.Error()})
		return
	}

	if fresh.MFATOTPSecret == "" || !verifyTOTPCode(fresh.MFATOTPSecret, req.Code) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid_mfa_code"})
		return
	}

	if err := deploymentStore.EnableMFA(c.Request.Context(), user.ID); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_enable_mfa", "details": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "mfa enabled"})
}

type MFAVerifyRequest struct {
	MFAToken string `json:"mfaToken" binding:"required"`
	Code     string `json:"code" binding:"required"`
}

// mfaVerifyHandler exchanges a password-only mfaToken plus a live TOTP code
// for real access/refresh tokens, completing the login flow started in
// loginHandler when the account has MFA enabled.
func mfaVerifyHandler(c *gin.Context) {
	var req MFAVerifyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		sendBadRequest(c, "invalid JSON body", err)
		return
	}

	claims, err := parseAndValidateToken(req.MFAToken, tokenTypeMFA)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid_mfa_token"})
		return
	}

	user, err := deploymentStore.GetUserByID(c.Request.Context(), claims.Subject)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid_mfa_token"})
		return
	}

	if !user.MFAEnabled || !verifyTOTPCode(user.MFATOTPSecret, req.Code) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid_mfa_code"})
		return
	}

	if err := respondWithIssuedTokens(c, user); err != nil {
		return
	}

	go func() {
		if err := deploymentStore.UpdateLastLogin(context.Background(), user.ID); err != nil {
			fmt.Printf("[WARN] failed to update last_login_at for %s: %v\n", user.ID, err)
		}
	}()
}
