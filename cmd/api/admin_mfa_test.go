package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestTokenCarriesMFAClaim(t *testing.T) {
	user := UserRecord{ID: "admin-1", IsAdmin: true, MFAEnabled: true}
	for _, mfa := range []bool{true, false} {
		token, _, err := issueAccessToken(user, mfa)
		if err != nil {
			t.Fatal(err)
		}
		claims, err := parseAndValidateToken(token, tokenTypeAccess)
		if err != nil {
			t.Fatal(err)
		}
		if claims.MFA != mfa {
			t.Fatalf("issued mfa=%v, parsed %v", mfa, claims.MFA)
		}
	}

	// Impersonation tokens must never count as MFA sessions.
	imp, _, err := issueImpersonationToken(user, UserRecord{ID: "user-2"})
	if err != nil {
		t.Fatal(err)
	}
	if claims, err := parseAndValidateToken(imp, tokenTypeAccess); err != nil || claims.MFA {
		t.Fatalf("impersonation token mfa=%v, err=%v", claims != nil && claims.MFA, err)
	}
}

func TestAdminMiddlewareRequiresMFASession(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cases := []struct {
		name       string
		user       UserRecord
		sessionMFA bool
		wantStatus int
	}{
		{"non-admin", UserRecord{ID: "u"}, true, http.StatusForbidden},
		{"admin not enrolled", UserRecord{ID: "a", IsAdmin: true}, false, http.StatusForbidden},
		{"admin enrolled, password-only session", UserRecord{ID: "a", IsAdmin: true, MFAEnabled: true}, false, http.StatusForbidden},
		{"admin session marked but MFA since disabled", UserRecord{ID: "a", IsAdmin: true}, true, http.StatusForbidden},
		{"admin with MFA session", UserRecord{ID: "a", IsAdmin: true, MFAEnabled: true}, true, http.StatusOK},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			router := gin.New()
			router.GET("/admin", func(c *gin.Context) {
				c.Set(currentUserContextKey, tc.user)
				c.Set(sessionMFAContextKey, tc.sessionMFA)
				c.Next()
			}, AdminMiddleware(), func(c *gin.Context) { c.Status(http.StatusOK) })

			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin", nil))
			if rec.Code != tc.wantStatus {
				t.Fatalf("status %d, want %d (body %s)", rec.Code, tc.wantStatus, rec.Body.String())
			}
		})
	}
}

func TestCanonicalRecoveryCode(t *testing.T) {
	display, hashed, err := generateRecoveryCode()
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{display, " " + display + " ", "  " + stripHyphenLower(display)} {
		if got := canonicalRecoveryCode(input); got == "" || hashToken(got) != hashed {
			t.Fatalf("%q canonicalised to %q, hash mismatch", input, got)
		}
	}
	for _, input := range []string{"", "123456", "ZZZZZ-ZZZZZ", "A1B2C-D3E4F5"} {
		if got := canonicalRecoveryCode(input); got != "" {
			t.Fatalf("%q should be rejected, got %q", input, got)
		}
	}
}

func stripHyphenLower(code string) string {
	out := make([]rune, 0, len(code))
	for _, r := range code {
		if r == '-' {
			continue
		}
		if r >= 'A' && r <= 'Z' {
			r += 'a' - 'A'
		}
		out = append(out, r)
	}
	return string(out)
}
