package main

import (
	"testing"

	"github.com/gin-gonic/gin"
)

// setupRouter panics on a gin wildcard conflict, which a compile-time check
// cannot catch. Building the full router asserts that the bulk env route
// coexists with the /apps/:id/env/:key delete route.
func TestSetupRouterRegistersBulkEnvRoute(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := setupRouter(ServerConfig{Debug: true})

	want := "/api/v1/apps/:id/env/bulk"
	for _, route := range router.Routes() {
		if route.Method == "POST" && route.Path == want {
			return
		}
	}
	t.Fatalf("POST %s not registered", want)
}
