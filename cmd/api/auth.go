package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

const (
	currentUserContextKey = "currentUser"
	tokenTypeAccess       = "access"
	tokenTypeRefresh      = "refresh"
)

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
	TokenType   string `json:"typ"`
	DisplayName string `json:"displayName,omitempty"`
	jwt.RegisteredClaims
}

func registerHandler(c *gin.Context) {
	var req AuthRegisterRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		sendBadRequest(c, "invalid JSON body", err)
		return
	}

	email := strings.ToLower(strings.TrimSpace(req.Email))
	password := strings.TrimSpace(req.Password)
	displayName := strings.TrimSpace(req.DisplayName)
	if email == "" || password == "" {
		sendBadRequest(c, "email and password are required", nil)
		return
	}
	if displayName == "" {
		displayName = strings.Split(email, "@")[0]
	}

	passwordHash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_hash_password", "details": err.Error()})
		return
	}

	user, err := deploymentStore.CreateUser(c.Request.Context(), email, displayName, string(passwordHash))
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "duplicate") {
			c.JSON(http.StatusConflict, gin.H{"error": "email_already_registered"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_create_user", "details": err.Error()})
		return
	}

	accessToken, accessExpiry, err := issueAccessToken(user)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_issue_access_token", "details": err.Error()})
		return
	}

	refreshToken, refreshExpiry, err := issueRefreshToken(user)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_issue_refresh_token", "details": err.Error()})
		return
	}
	if err := deploymentStore.StoreRefreshToken(c.Request.Context(), user.ID, hashToken(refreshToken), refreshExpiry); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_store_refresh_token", "details": err.Error()})
		return
	}

	apiKeyName := strings.TrimSpace(req.APIKeyName)
	if apiKeyName == "" {
		apiKeyName = "initial-access"
	}
	apiKey, err := createAPIKeyForUser(c.Request.Context(), user.ID, apiKeyName)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_create_api_key", "details": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, AuthTokenResponse{
		User: AuthUserResponse{
			ID:          user.ID,
			Email:       user.Email,
			DisplayName: user.DisplayName,
		},
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		APIKey:       apiKey,
		TokenType:    tokenTypeAccess,
		ExpiresIn:    int64(accessExpiry.Sub(time.Now().UTC()).Seconds()),
	})
}

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

	user, err := deploymentStore.GetUserByEmail(c.Request.Context(), email)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid_credentials"})
		return
	}
	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid_credentials"})
		return
	}

	accessToken, accessExpiry, err := issueAccessToken(user)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_issue_access_token", "details": err.Error()})
		return
	}

	refreshToken, refreshExpiry, err := issueRefreshToken(user)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_issue_refresh_token", "details": err.Error()})
		return
	}
	if err := deploymentStore.StoreRefreshToken(c.Request.Context(), user.ID, hashToken(refreshToken), refreshExpiry); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_store_refresh_token", "details": err.Error()})
		return
	}

	c.JSON(http.StatusOK, AuthTokenResponse{
		User: AuthUserResponse{
			ID:          user.ID,
			Email:       user.Email,
			DisplayName: user.DisplayName,
		},
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		TokenType:    tokenTypeAccess,
		ExpiresIn:    int64(accessExpiry.Sub(time.Now().UTC()).Seconds()),
	})
}

func refreshHandler(c *gin.Context) {
	var req AuthRefreshRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		sendBadRequest(c, "invalid JSON body", err)
		return
	}

	claims, err := parseAndValidateToken(req.RefreshToken, tokenTypeRefresh)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid_refresh_token"})
		return
	}

	user, err := deploymentStore.GetUserByID(c.Request.Context(), claims.Subject)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid_refresh_token"})
		return
	}

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
	if _, err := deploymentStore.ConsumeRefreshToken(c.Request.Context(), req.RefreshToken, hashToken(newRefreshToken), refreshExpiry); err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid_refresh_token", "details": err.Error()})
		return
	}

	c.JSON(http.StatusOK, AuthTokenResponse{
		User: AuthUserResponse{
			ID:          user.ID,
			Email:       user.Email,
			DisplayName: user.DisplayName,
		},
		AccessToken:  newAccessToken,
		RefreshToken: newRefreshToken,
		TokenType:    tokenTypeAccess,
		ExpiresIn:    int64(accessExpiry.Sub(time.Now().UTC()).Seconds()),
	})
}

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

	apiKey, err := createAPIKeyForUser(c.Request.Context(), user.ID, req.Name)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_create_api_key", "details": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"name":    strings.TrimSpace(req.Name),
		"apiKey":  apiKey,
		"userId":  user.ID,
		"message": "api key created successfully",
	})
}

func AuthMiddleware(allowAPIKey bool) gin.HandlerFunc {
	return func(c *gin.Context) {
		user, err := authenticateRequest(c, allowAPIKey)
		if err != nil {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "unauthorized", "details": err.Error()})
			return
		}

		c.Set(currentUserContextKey, user)
		c.Next()
	}
}

func currentAuthUser(c *gin.Context) (UserRecord, bool) {
	value, ok := c.Get(currentUserContextKey)
	if !ok {
		return UserRecord{}, false
	}

	user, ok := value.(UserRecord)
	return user, ok
}

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

func authenticateRequest(c *gin.Context, allowAPIKey bool) (UserRecord, error) {
	if bearerToken := extractBearerToken(c.GetHeader("Authorization")); bearerToken != "" {
		claims, err := parseAndValidateToken(bearerToken, tokenTypeAccess)
		if err != nil {
			return UserRecord{}, err
		}

		return deploymentStore.GetUserByID(c.Request.Context(), claims.Subject)
	}

	// WebSocket clients cannot set Authorization headers; accept access JWT via query.
	if queryToken := strings.TrimSpace(c.Query("token")); queryToken != "" {
		claims, err := parseAndValidateToken(queryToken, tokenTypeAccess)
		if err != nil {
			return UserRecord{}, err
		}

		return deploymentStore.GetUserByID(c.Request.Context(), claims.Subject)
	}

	if allowAPIKey {
		if apiKey := extractAPIKey(c); apiKey != "" {
			return deploymentStore.GetUserByAPIKey(c.Request.Context(), apiKey)
		}
	}

	return UserRecord{}, fmt.Errorf("missing bearer token or api key")
}

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

func extractAPIKey(c *gin.Context) string {
	if apiKey := strings.TrimSpace(c.GetHeader("X-API-Key")); apiKey != "" {
		return apiKey
	}

	authorization := strings.TrimSpace(c.GetHeader("Authorization"))
	if len(authorization) > 7 && strings.EqualFold(authorization[:7], "ApiKey ") {
		return strings.TrimSpace(authorization[7:])
	}

	return ""
}

func issueAccessToken(user UserRecord) (string, time.Time, error) {
	ttl := tokenTTLFromEnv("AUTH_ACCESS_TOKEN_TTL", 15*time.Minute)
	return issueToken(user, tokenTypeAccess, ttl)
}

func issueRefreshToken(user UserRecord) (string, time.Time, error) {
	ttl := tokenTTLFromEnv("AUTH_REFRESH_TOKEN_TTL", 30*24*time.Hour)
	return issueToken(user, tokenTypeRefresh, ttl)
}

func issueToken(user UserRecord, tokenType string, ttl time.Duration) (string, time.Time, error) {
	secret := []byte(envOrDefault("AUTH_JWT_SECRET", "dev-auth-secret-change-me"))
	now := time.Now().UTC()
	expiresAt := now.Add(ttl)
	jti, err := generateRandomToken(24)
	if err != nil {
		return "", time.Time{}, err
	}

	claims := authClaims{
		TokenType:   tokenType,
		DisplayName: user.DisplayName,
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

func parseAndValidateToken(tokenString string, expectedType string) (*authClaims, error) {
	secret := []byte(envOrDefault("AUTH_JWT_SECRET", "dev-auth-secret-change-me"))
	parsed, err := jwt.ParseWithClaims(tokenString, &authClaims{}, func(token *jwt.Token) (any, error) {
		if token.Method != jwt.SigningMethodHS256 {
			return nil, fmt.Errorf("unexpected signing method: %s", token.Method.Alg())
		}

		return secret, nil
	}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}))
	if err != nil {
		return nil, err
	}

	claims, ok := parsed.Claims.(*authClaims)
	if !ok || !parsed.Valid {
		return nil, errors.New("token is invalid")
	}
	if claims.TokenType != expectedType {
		return nil, fmt.Errorf("unexpected token type")
	}

	return claims, nil
}

func createAPIKeyForUser(ctx context.Context, userID string, name string) (string, error) {
	prefix, err := generateRandomToken(8)
	if err != nil {
		return "", err
	}
	secret, err := generateRandomToken(32)
	if err != nil {
		return "", err
	}

	apiKey := fmt.Sprintf("gfy_%s_%s", prefix, secret)
	if err := deploymentStore.StoreAPIKey(ctx, userID, name, prefix, hashToken(apiKey), nil); err != nil {
		return "", err
	}

	return apiKey, nil
}

func tokenTTLFromEnv(key string, fallback time.Duration) time.Duration {
	ttl, err := durationFromEnv(key, fallback)
	if err != nil || ttl <= 0 {
		return fallback
	}

	return ttl
}

func hmacEqual(a string, b string) bool {
	return hmac.Equal([]byte(a), []byte(b))
}

func fingerprintAPIKey(apiKey string) string {
	sum := sha256.Sum256([]byte(apiKey))
	return hex.EncodeToString(sum[:8])
}
