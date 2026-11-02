package main

import "testing"

func TestRedactQuery(t *testing.T) {
	cases := map[string]string{
		"/api/v1/apps":                         "/api/v1/apps",
		"/api/v1/apps/1/logs?token=eyJabc.def": "/api/v1/apps/1/logs?token=REDACTED",
		"/cb?code=abc&installation_id=9&setup_action=update&state=eyJx": "/cb?code=REDACTED&installation_id=9&setup_action=update&state=REDACTED",
		"/logs?tail=50&TOKEN=x&filter=error":                            "/logs?tail=50&TOKEN=REDACTED&filter=error",
		"/logs?token":                                                   "/logs?token",
		"/x?status=open":                                                "/x?status=open",
	}
	for in, want := range cases {
		if got := redactQuery(in); got != want {
			t.Errorf("redactQuery(%q) = %q, want %q", in, got, want)
		}
	}
}
