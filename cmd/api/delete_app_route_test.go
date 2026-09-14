package main

import (
	"testing"

	"github.com/gin-gonic/gin"
)

func TestSetupRouterRegistersDeleteAppRoute(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := setupRouter(ServerConfig{Debug: true})

	want := "/api/v1/apps/:id"
	for _, route := range router.Routes() {
		if route.Method == "DELETE" && route.Path == want {
			return
		}
	}
	t.Fatalf("DELETE %s not registered", want)
}
