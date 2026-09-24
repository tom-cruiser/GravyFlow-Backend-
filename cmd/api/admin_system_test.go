package main

import "testing"

func TestParseRedisInfo(t *testing.T) {
	fields := parseRedisInfo("# Server\r\nredis_version:7.2.4\r\nuptime_in_seconds:42\r\n\r\n# Clients\r\nconnected_clients:3\r\n")
	if fields["redis_version"] != "7.2.4" || fields["uptime_in_seconds"] != "42" || fields["connected_clients"] != "3" {
		t.Fatalf("unexpected fields: %#v", fields)
	}
}

func TestOverallStatus(t *testing.T) {
	cases := []struct {
		components []ComponentHealth
		want       string
	}{
		{[]ComponentHealth{{Name: "PostgreSQL", Status: componentHealthy}, {Name: "GitHub App", Status: componentNotConfigured}}, componentHealthy},
		{[]ComponentHealth{{Name: "PostgreSQL", Status: componentHealthy}, {Name: "Caddy", Status: componentUnhealthy}}, componentDegraded},
		{[]ComponentHealth{{Name: "Redis", Status: componentUnhealthy}, {Name: "Caddy", Status: componentDegraded}}, componentUnhealthy},
	}
	for _, tc := range cases {
		if got := overallStatus(tc.components); got != tc.want {
			t.Errorf("overallStatus(%v) = %q, want %q", tc.components, got, tc.want)
		}
	}
}

func TestCollectHostInfo(t *testing.T) {
	host := collectHostInfo()
	if host.CPUCount <= 0 {
		t.Fatalf("expected a CPU count, got %d", host.CPUCount)
	}
	t.Logf("%+v cpu=%v", host, host.CPUUsagePercent)
}
