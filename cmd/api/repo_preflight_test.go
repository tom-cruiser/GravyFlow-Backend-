package main

import (
	"strings"
	"testing"
)

func TestSummarizeTreeFindsTheHeavyDirectory(t *testing.T) {
	// Shaped like the real Nerva-backend problem: a tiny codebase next to a
	// committed browser profile.
	entries := []GitHubTreeEntry{
		{Path: "package.json", Type: "blob", Size: 2_000},
		{Path: "services/auth/index.ts", Type: "blob", Size: 50_000},
		{Path: "services/whatsapp-engine/.wwebjs_auth/session/Default/Cache/a", Type: "blob", Size: 40 << 20},
		{Path: "services/whatsapp-engine/.wwebjs_auth/session/Default/Cache/b", Type: "blob", Size: 30 << 20},
		{Path: "services/whatsapp-engine", Type: "tree"},
		{Path: "vendor/lib", Type: "commit"},
	}

	total, files, top := summarizeTree(entries)
	if files != 4 {
		t.Fatalf("files = %d, want 4 (trees and submodules aren't counted)", files)
	}
	if want := int64(2_000 + 50_000 + 70<<20); total != want {
		t.Fatalf("total = %d, want %d", total, want)
	}
	if len(top) == 0 || !strings.HasPrefix(top[0].Path, "services/whatsapp-engine/.wwebjs_auth") {
		t.Fatalf("top offender = %+v, want the .wwebjs_auth directory", top)
	}
	if top[0].Bytes != 70<<20 {
		t.Fatalf("top offender size = %d, want %d", top[0].Bytes, 70<<20)
	}
}

func TestTooLargeErrorNamesTheOffendersAndTheKnob(t *testing.T) {
	err := tooLargeError("tom/Nerva-backend", 1193<<20, 500<<20, false,
		[]pathSize{{Path: "services/whatsapp-engine/.wwebjs_auth/session", Bytes: 1192 << 20}})
	msg := err.Error()
	for _, want := range []string{"tom/Nerva-backend", "1193 MB", "500 MB", ".wwebjs_auth", "GRAVYFLOW_MAX_CLONE_SIZE_MB"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q is missing %q", msg, want)
		}
	}

	truncated := tooLargeError("o/r", 600<<20, 500<<20, true, nil).Error()
	if !strings.Contains(truncated, "at least") {
		t.Errorf("a truncated listing must say the size is a lower bound: %q", truncated)
	}
}

func TestMaxCloneSizeBytesEnv(t *testing.T) {
	t.Setenv("GRAVYFLOW_MAX_CLONE_SIZE_MB", "")
	if got := maxCloneSizeBytes(); got != defaultMaxCloneSizeMB<<20 {
		t.Fatalf("default = %d", got)
	}
	t.Setenv("GRAVYFLOW_MAX_CLONE_SIZE_MB", "2048")
	if got := maxCloneSizeBytes(); got != 2048<<20 {
		t.Fatalf("configured = %d", got)
	}
	t.Setenv("GRAVYFLOW_MAX_CLONE_SIZE_MB", "0")
	if got := maxCloneSizeBytes(); got != 0 {
		t.Fatalf("0 must disable the check, got %d", got)
	}
	t.Setenv("GRAVYFLOW_MAX_CLONE_SIZE_MB", "lots")
	if got := maxCloneSizeBytes(); got != defaultMaxCloneSizeMB<<20 {
		t.Fatalf("invalid must fall back to the default, got %d", got)
	}
}

func TestBuildPostgresConnStringDatabaseTarget(t *testing.T) {
	remote := "postgresql://u:p@ep-x.neon.tech/neondb?sslmode=require"
	t.Setenv("DATABASE_URL", remote)
	t.Setenv("PGHOST", "postgres")
	t.Setenv("PGPORT", "")
	t.Setenv("PGDATABASE", "gravyflow")
	t.Setenv("PGUSER", "postgres")
	t.Setenv("PGPASSWORD", "p@ss word")
	t.Setenv("PGSSLMODE", "require") // a .env written for Neon

	t.Setenv("GRAVYFLOW_DB_TARGET", "")
	got, err := buildPostgresConnString()
	if err != nil || got != remote {
		t.Fatalf("default target = %q, %v; want DATABASE_URL untouched", got, err)
	}

	t.Setenv("GRAVYFLOW_DB_TARGET", "remote")
	if got, _ := buildPostgresConnString(); got != remote {
		t.Fatalf("remote target = %q, want DATABASE_URL", got)
	}

	t.Setenv("GRAVYFLOW_DB_TARGET", "LOCAL")
	got, err = buildPostgresConnString()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "neon.tech") {
		t.Fatalf("local target still points at the remote database: %q", got)
	}
	for _, want := range []string{"postgres:5432", "/gravyflow", "sslmode=disable", "p%40ss%20word"} {
		if !strings.Contains(got, want) {
			t.Errorf("local conn string %q is missing %q", got, want)
		}
	}
}
