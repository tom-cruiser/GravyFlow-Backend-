package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

// ============================================================================
// CONSTANTS
// ============================================================================

const (
	currentUserContextKey  = "currentUser"
	impersonatorContextKey = "impersonatorId"
	tokenTypeAccess        = "access"
	tokenTypeRefresh       = "refresh"
	tokenTypeMFA           = "mfa"
	apiKeyPrefix           = "gfy"
)

// ============================================================================
// REQUEST/RESPONSE TYPES
// ============================================================================

type AuthRegisterRequest struct {
	Email       string `json:"email" binding:"required"`
	Password    string `json:"password" binding:"required"`
	DisplayName string `json:"displayName"`
	APIKeyName  string `json:"apiKeyName"`
}

type AuthLoginRequest struct {
	Email    string `json:"email" binding:"required"`
	Password string `json:"password" binding:"required"`
}

type AuthRefreshRequest struct {
	RefreshToken string `json:"refreshToken" binding:"required"`
}

type APIKeyCreateRequest struct {
	Name string `json:"name" binding:"required"`
}

type AuthUserResponse struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	DisplayName string `json:"displayName"`
	// IsAdmin/MFAEnabled let the frontend gate the admin panel and the MFA
	// enrollment prompt without decoding the JWT itself.
	IsAdmin    bool `json:"isAdmin"`
	MFAEnabled bool `json:"mfaEnabled"`
}

type AuthTokenResponse struct {
	User         AuthUserResponse `json:"user"`
	AccessToken  string           `json:"accessToken"`
	RefreshToken string           `json:"refreshToken,omitempty"`
	APIKey       string           `json:"apiKey,omitempty"`
	TokenType    string           `json:"tokenType"`
	ExpiresIn    int64            `json:"expiresIn"`
}

type authClaims struct {
	TokenType    string `json:"typ"`
	DisplayName  string `json:"displayName,omitempty"`
	Impersonator string `json:"imp,omitempty"` // admin user ID, set only on impersonation-mode tokens
	jwt.RegisteredClaims
}

// MFALoginResponse is returned by loginHandler instead of AuthTokenResponse
// when the account has MFA enabled: the caller must complete POST
// /auth/mfa/verify with the returned mfaToken before receiving real tokens.
type MFALoginResponse struct {
	MFARequired bool   `json:"mfaRequired"`
	MFAToken    string `json:"mfaToken"`
	ExpiresIn   int64  `json:"expiresIn"`
}

// ============================================================================
// HELPER FUNCTIONS (REMOVED - Now in helpers.go)
// ============================================================================
// Removed: generateRandomToken, hashToken, envOrDefault, durationFromEnv, sendBadRequest
// These are now imported from helpers.go

// fingerprintAPIKey creates a short fingerprint of an API key for logging
func fingerprintAPIKey(apiKey string) string {
	sum := sha256.Sum256([]byte(apiKey))
	return hex.EncodeToString(sum[:8])
}

// tokenTTLFromEnv gets TTL from environment or returns fallback
func tokenTTLFromEnv(key string, fallback time.Duration) time.Duration {
	ttl, err := durationFromEnv(key, fallback)
	if err != nil || ttl <= 0 {
		return fallback
	}
	return ttl
}

// hmacEqual performs constant-time comparison
func hmacEqual(a string, b string) bool {
	return hmac.Equal([]byte(a), []byte(b))
}

// ============================================================================
// JWT TOKEN FUNCTIONS
// ============================================================================

// issueAccessToken issues a new access token
func issueAccessToken(user UserRecord) (string, time.Time, error) {
	ttl := tokenTTLFromEnv("AUTH_ACCESS_TOKEN_TTL", 15*time.Minute)
	return issueToken(user, tokenTypeAccess, ttl, "")
}

// issueRefreshToken issues a new refresh token
func issueRefreshToken(user UserRecord) (string, time.Time, error) {
	ttl := tokenTTLFromEnv("AUTH_REFRESH_TOKEN_TTL", 30*24*time.Hour)
	return issueToken(user, tokenTypeRefresh, ttl, "")
}

// issueMFAToken issues a short-lived token that only proves the password step
// passed; it must be exchanged via POST /auth/mfa/verify for real tokens.
func issueMFAToken(user UserRecord) (string, time.Time, error) {
	ttl := tokenTTLFromEnv("AUTH_MFA_TOKEN_TTL", 5*time.Minute)
	return issueToken(user, tokenTypeMFA, ttl, "")
}

// issueImpersonationToken issues a normal access token scoped to targetUser,
// but tagged with the acting admin's ID. AuthMiddleware surfaces that tag so
// ImpersonationReadOnlyMiddleware can block any non-GET request made with it,
// giving admins a read-only view into a user's workspace (Module A).
func issueImpersonationToken(admin UserRecord, target UserRecord) (string, time.Time, error) {
	ttl := tokenTTLFromEnv("AUTH_IMPERSONATION_TOKEN_TTL", 15*time.Minute)
	return issueToken(target, tokenTypeAccess, ttl, admin.ID)
}

// issueToken is the core token issuance function
func issueToken(user UserRecord, tokenType string, ttl time.Duration, impersonator string) (string, time.Time, error) {
	secret := []byte(envOrDefault("AUTH_JWT_SECRET", "dev-auth-secret-change-me-in-production"))
	now := time.Now().UTC()
	expiresAt := now.Add(ttl)

	jti, err := generateRandomToken(24)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("failed to generate token ID: %w", err)
	}

	claims := authClaims{
		TokenType:    tokenType,
		DisplayName:  user.DisplayName,
		Impersonator: impersonator,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   user.ID,
			ID:        jti,
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(expiresAt),
			NotBefore: jwt.NewNumericDate(now),
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString(secret)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign %s token: %w", tokenType, err)
	}

	return signed, expiresAt, nil
}

// parseAndValidateToken parses and validates a JWT token
func parseAndValidateToken(tokenString string, expectedType string) (*authClaims, error) {
	secret := []byte(envOrDefault("AUTH_JWT_SECRET", "dev-auth-secret-change-me-in-production"))

	parsed, err := jwt.ParseWithClaims(tokenString, &authClaims{}, func(token *jwt.Token) (any, error) {
		if token.Method != jwt.SigningMethodHS256 {
			return nil, fmt.Errorf("unexpected signing method: %s", token.Method.Alg())
		}
		return secret, nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}))

	if err != nil {
		return nil, fmt.Errorf("token validation failed: %w", err)
	}

	claims, ok := parsed.Claims.(*authClaims)
	if !ok || !parsed.Valid {
		return nil, errors.New("invalid token claims")
	}

	if claims.TokenType != expectedType {
		return nil, fmt.Errorf("unexpected token type: expected %s, got %s", expectedType, claims.TokenType)
	}

	return claims, nil
}

// ============================================================================
// API KEY FUNCTIONS
// ============================================================================

// createAPIKeyForUser creates a new API key for a user
func createAPIKeyForUser(ctx context.Context, userID string, name string) (string, error) {
	// Generate key components
	prefix, err := generateRandomToken(8)
	if err != nil {
		return "", fmt.Errorf("failed to generate prefix: %w", err)
	}

	secret, err := generateRandomToken(32)
	if err != nil {
		return "", fmt.Errorf("failed to generate secret: %w", err)
	}

	// Construct the full API key
	apiKey := fmt.Sprintf("%s_%s_%s", apiKeyPrefix, prefix, secret)

	// Store the hashed version
	hashedKey := hashToken(apiKey)
	if err := deploymentStore.StoreAPIKey(ctx, userID, name, prefix, hashedKey, nil); err != nil {
		return "", fmt.Errorf("failed to store API key: %w", err)
	}

	return apiKey, nil
}

// ============================================================================
// AUTHENTICATION HANDLERS
// ============================================================================

// registerHandler handles user registration
func registerHandler(c *gin.Context) {
	var req AuthRegisterRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		sendBadRequest(c, "invalid JSON body", err)
		return
	}

	// Validate and sanitize inputs
	email := strings.ToLower(strings.TrimSpace(req.Email))
	password := strings.TrimSpace(req.Password)
	displayName := strings.TrimSpace(req.DisplayName)

	if email == "" || password == "" {
		sendBadRequest(c, "email and password are required", nil)
		return
	}

	// Password strength validation
	if len(password) < 8 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "password_must_be_at_least_8_characters"})
		return
	}

	if displayName == "" {
		displayName = strings.Split(email, "@")[0]
	}

	// Hash password
	passwordHash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_hash_password", "details": err.Error()})
		return
	}

	// Create user
	user, err := deploymentStore.CreateUser(c.Request.Context(), email, displayName, string(passwordHash))
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "duplicate") {
			c.JSON(http.StatusConflict, gin.H{"error": "email_already_registered"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_create_user", "details": err.Error()})
		return
	}

	// Issue access token
	accessToken, accessExpiry, err := issueAccessToken(user)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_issue_access_token", "details": err.Error()})
		return
	}

	// Issue and store refresh token
	refreshToken, refreshExpiry, err := issueRefreshToken(user)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_issue_refresh_token", "details": err.Error()})
		return
	}

	if err := deploymentStore.StoreRefreshToken(c.Request.Context(), user.ID, hashToken(refreshToken), refreshExpiry); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_store_refresh_token", "details": err.Error()})
		return
	}

	// Create initial API key
	apiKeyName := strings.TrimSpace(req.APIKeyName)
	if apiKeyName == "" {
		apiKeyName = "initial-access"
	}

	apiKey, err := createAPIKeyForUser(c.Request.Context(), user.ID, apiKeyName)
	if err != nil {
		// Log error but don't fail registration if API key creation fails
		fmt.Printf("Warning: failed to create API key: %v\n", err)
	}

	c.JSON(http.StatusCreated, AuthTokenResponse{
		User: AuthUserResponse{
			ID:          user.ID,
			Email:       user.Email,
			DisplayName: user.DisplayName,
			IsAdmin:     user.IsAdmin,
			MFAEnabled:  user.MFAEnabled,
		},
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		APIKey:       apiKey,
		TokenType:    tokenTypeAccess,
		ExpiresIn:    int64(accessExpiry.Sub(time.Now().UTC()).Seconds()),
	})
}

// loginHandler handles user login
func loginHandler(c *gin.Context) {
	var req AuthLoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		sendBadRequest(c, "invalid JSON body", err)
		return
	}

	email := strings.ToLower(strings.TrimSpace(req.Email))
	password := strings.TrimSpace(req.Password)

	if email == "" || password == "" {
		sendBadRequest(c, "email and password are required", nil)
		return
	}

	// Get user
	user, err := deploymentStore.GetUserByEmail(c.Request.Context(), email)
	if err != nil {
		// Use generic error to prevent user enumeration
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid_credentials"})
		return
	}

	// Verify password
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid_credentials"})
		return
	}

	// Account Status Management (Module A): a suspended/flagged/deleted account
	// cannot start a new session, regardless of password validity. Admins can
	// still reach the account via impersonation.
	if user.Status != "" && user.Status != UserStatusActive {
		c.JSON(http.StatusForbidden, gin.H{
			"error":   "account_" + user.Status,
			"details": fmt.Sprintf("this account is %s", user.Status),
		})
		return
	}

	// Security Constraints: MFA is required for admin logins once enrolled.
	// Hold the session at a password-only checkpoint until the TOTP code is
	// verified via /auth/mfa/verify.
	if user.IsAdmin && user.MFAEnabled {
		mfaToken, mfaExpiry, err := issueMFAToken(user)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_issue_mfa_token", "details": err.Error()})
			return
		}
		c.JSON(http.StatusOK, MFALoginResponse{
			MFARequired: true,
			MFAToken:    mfaToken,
			ExpiresIn:   int64(mfaExpiry.Sub(time.Now().UTC()).Seconds()),
		})
		return
	}

	if err := respondWithIssuedTokens(c, user); err != nil {
		return
	}

	go func() {
		if err := deploymentStore.UpdateLastLogin(context.Background(), user.ID); err != nil {
			log.Printf("[WARN] failed to update last_login_at for %s: %v", user.ID, err)
		}
	}()
}

// respondWithIssuedTokens issues an access/refresh token pair for user,
// persists the refresh token, and writes the AuthTokenResponse. Shared by
// loginHandler and mfaVerifyHandler so both paths end a session the same way.
func respondWithIssuedTokens(c *gin.Context, user UserRecord) error {
	accessToken, accessExpiry, err := issueAccessToken(user)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_issue_access_token", "details": err.Error()})
		return err
	}

	refreshToken, refreshExpiry, err := issueRefreshToken(user)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_issue_refresh_token", "details": err.Error()})
		return err
	}

	if err := deploymentStore.StoreRefreshToken(c.Request.Context(), user.ID, hashToken(refreshToken), refreshExpiry); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_store_refresh_token", "details": err.Error()})
		return err
	}

	c.JSON(http.StatusOK, AuthTokenResponse{
		User: AuthUserResponse{
			ID:          user.ID,
			Email:       user.Email,
			DisplayName: user.DisplayName,
			IsAdmin:     user.IsAdmin,
			MFAEnabled:  user.MFAEnabled,
		},
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		TokenType:    tokenTypeAccess,
		ExpiresIn:    int64(accessExpiry.Sub(time.Now().UTC()).Seconds()),
	})
	return nil
}

// refreshHandler handles token refresh with rotation
func refreshHandler(c *gin.Context) {
	var req AuthRefreshRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		sendBadRequest(c, "invalid JSON body", err)
		return
	}

	// Parse and validate the refresh token
	claims, err := parseAndValidateToken(req.RefreshToken, tokenTypeRefresh)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid_refresh_token"})
		return
	}

	// Get user
	user, err := deploymentStore.GetUserByID(c.Request.Context(), claims.Subject)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid_refresh_token"})
		return
	}

	// ATOMIC: Consume old refresh token first
	// This prevents reuse attacks
	_, err = deploymentStore.ConsumeRefreshToken(c.Request.Context(), req.RefreshToken)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid_refresh_token", "details": err.Error()})
		return
	}

	// Issue new tokens
	newAccessToken, accessExpiry, err := issueAccessToken(user)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_issue_access_token", "details": err.Error()})
		return
	}

	newRefreshToken, refreshExpiry, err := issueRefreshToken(user)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_issue_refresh_token", "details": err.Error()})
		return
	}

	// Store the new refresh token
	if err := deploymentStore.StoreRefreshToken(c.Request.Context(), user.ID, hashToken(newRefreshToken), refreshExpiry); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_store_refresh_token", "details": err.Error()})
		return
	}

	c.JSON(http.StatusOK, AuthTokenResponse{
		User: AuthUserResponse{
			ID:          user.ID,
			Email:       user.Email,
			DisplayName: user.DisplayName,
			IsAdmin:     user.IsAdmin,
			MFAEnabled:  user.MFAEnabled,
		},
		AccessToken:  newAccessToken,
		RefreshToken: newRefreshToken,
		TokenType:    tokenTypeAccess,
		ExpiresIn:    int64(accessExpiry.Sub(time.Now().UTC()).Seconds()),
	})
}

// createAPIKeyHandler creates a new API key for the authenticated user
func createAPIKeyHandler(c *gin.Context) {
	user, ok := currentAuthUser(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return
	}

	var req APIKeyCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		sendBadRequest(c, "invalid JSON body", err)
		return
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		sendBadRequest(c, "API key name is required", nil)
		return
	}

	apiKey, err := createAPIKeyForUser(c.Request.Context(), user.ID, name)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_create_api_key", "details": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"name":    name,
		"apiKey":  apiKey,
		"userId":  user.ID,
		"message": "api key created successfully",
	})
}

// ============================================================================
// MIDDLEWARE
// ============================================================================

// AuthMiddleware creates authentication middleware
func AuthMiddleware(allowAPIKey bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		user, impersonatorID, err := authenticateRequest(c, allowAPIKey)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized", "details": err.Error()})
			return
		}

		c.Set(currentUserContextKey, user)
		if impersonatorID != "" {
			c.Set(impersonatorContextKey, impersonatorID)
		}
		c.Next()
	}
}

// AdminMiddleware restricts a route group to internal System Administrators.
// It must run after AuthMiddleware(false) so currentAuthUser(c) is populated.
//
// MFA is opt-in, not enforced here: an admin can enroll voluntarily via
// POST /auth/mfa/enroll + /enable (both under plain AuthMiddleware, not this
// one), and once enrolled, loginHandler holds their session at the TOTP
// checkpoint (see "Security Constraints" comment there). This middleware only
// checks IsAdmin — it does not require MFAEnabled — so admin panel access
// never depends on enrollment status.
func AdminMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		user, ok := currentAuthUser(c)
		if !ok {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}
		if !user.IsAdmin {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "forbidden", "details": "admin access required"})
			return
		}
		c.Next()
	}
}

// currentImpersonatorID returns the acting admin's user ID when the current
// request is authenticated with an impersonation-mode token, per
// issueImpersonationToken.
func currentImpersonatorID(c *gin.Context) (string, bool) {
	value, ok := c.Get(impersonatorContextKey)
	if !ok {
		return "", false
	}
	id, ok := value.(string)
	return id, ok && id != ""
}

// ImpersonationReadOnlyMiddleware blocks any mutating request made under an
// admin impersonation session, so "Inspect a user's workspace" (Module A)
// stays read-only no matter which downstream route is hit.
func ImpersonationReadOnlyMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if _, impersonating := currentImpersonatorID(c); impersonating {
			switch c.Request.Method {
			case http.MethodGet, http.MethodHead, http.MethodOptions:
				// read-only, allowed
			default:
				c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
					"error":   "impersonation_read_only",
					"details": "admin impersonation sessions cannot modify data",
				})
				return
			}
		}
		c.Next()
	}
}

// currentAuthUser extracts the authenticated user from context
func currentAuthUser(c *gin.Context) (UserRecord, bool) {
	value, ok := c.Get(currentUserContextKey)
	if !ok {
		return UserRecord{}, false
	}

	user, ok := value.(UserRecord)
	return user, ok
}

// currentUserDeployment validates user has access to a deployment
func currentUserDeployment(c *gin.Context) (UserRecord, DeploymentRecord, bool) {
	user, ok := currentAuthUser(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
		return UserRecord{}, DeploymentRecord{}, false
	}

	deploymentID := strings.TrimSpace(c.Param("id"))
	if deploymentID == "" {
		sendBadRequest(c, "deployment id is required", nil)
		return UserRecord{}, DeploymentRecord{}, false
	}

	deployment, err := deploymentStore.GetDeploymentForUser(c.Request.Context(), user.ID, deploymentID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "deployment_not_found", "details": err.Error()})
		return UserRecord{}, DeploymentRecord{}, false
	}

	return user, deployment, true
}

// authenticateRequest authenticates the request using various methods. The
// second return value is the acting admin's user ID when the token was
// issued by issueImpersonationToken, empty otherwise.
func authenticateRequest(c *gin.Context, allowAPIKey bool) (UserRecord, string, error) {
	// Try Bearer token first
	if bearerToken := extractBearerToken(c.GetHeader("Authorization")); bearerToken != "" {
		claims, err := parseAndValidateToken(bearerToken, tokenTypeAccess)
		if err != nil {
			return UserRecord{}, "", fmt.Errorf("invalid bearer token: %w", err)
		}
		user, err := deploymentStore.GetUserByID(c.Request.Context(), claims.Subject)
		return user, claims.Impersonator, err
	}

	// Try query token (for WebSocket support)
	if queryToken := strings.TrimSpace(c.Query("token")); queryToken != "" {
		claims, err := parseAndValidateToken(queryToken, tokenTypeAccess)
		if err != nil {
			return UserRecord{}, "", fmt.Errorf("invalid query token: %w", err)
		}
		user, err := deploymentStore.GetUserByID(c.Request.Context(), claims.Subject)
		return user, claims.Impersonator, err
	}

	// Try API key if allowed
	if allowAPIKey {
		if apiKey := extractAPIKey(c); apiKey != "" {
			user, err := deploymentStore.GetUserByAPIKey(c.Request.Context(), apiKey)
			if err != nil {
				return UserRecord{}, "", fmt.Errorf("invalid API key: %w", err)
			}
			return user, "", nil
		}
	}

	return UserRecord{}, "", fmt.Errorf("no valid authentication credentials provided")
}

// extractBearerToken extracts Bearer token from Authorization header
func extractBearerToken(header string) string {
	header = strings.TrimSpace(header)
	if header == "" {
		return ""
	}

	if len(header) > 7 && strings.EqualFold(header[:7], "Bearer ") {
		return strings.TrimSpace(header[7:])
	}

	return ""
}

// extractAPIKey extracts API key from headers
func extractAPIKey(c *gin.Context) string {
	// Check X-API-Key header first
	if apiKey := strings.TrimSpace(c.GetHeader("X-API-Key")); apiKey != "" {
		return apiKey
	}

	// Check Authorization header with ApiKey scheme
	authorization := strings.TrimSpace(c.GetHeader("Authorization"))
	if len(authorization) > 7 && strings.EqualFold(authorization[:7], "ApiKey ") {
		return strings.TrimSpace(authorization[7:])
	}

	// Check query parameter (optional)
	if apiKey := strings.TrimSpace(c.Query("apiKey")); apiKey != "" {
		return apiKey
	}

	return ""
}

// ============================================================================
// OPTIONAL: LOGOUT HANDLER
// ============================================================================

// logoutHandler invalidates the current refresh token
func logoutHandler(c *gin.Context) {
	var req AuthRefreshRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		sendBadRequest(c, "invalid JSON body", err)
		return
	}

	// Parse token to get user ID
	_, err := parseAndValidateToken(req.RefreshToken, tokenTypeRefresh)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid_refresh_token"})
		return
	}

	// Invalidate the refresh token
	if _, err := deploymentStore.ConsumeRefreshToken(c.Request.Context(), req.RefreshToken); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_logout", "details": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "logged out successfully"})
}

// ============================================================================
// ROUTE SETUP
// ============================================================================

// setupAuthRoutes configures all authentication routes
func setupAuthRoutes(router *gin.Engine) {
	auth := router.Group("/api/auth")
	{
		auth.POST("/register", registerHandler)
		auth.POST("/login", loginHandler)
		auth.POST("/refresh", refreshHandler)
		auth.POST("/logout", AuthMiddleware(false), logoutHandler)

		// Protected routes
		protected := auth.Group("/")
		protected.Use(AuthMiddleware(false))
		{
			protected.POST("/api-keys", createAPIKeyHandler)
			// Add other protected routes here
		}
	}
}