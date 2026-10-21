package main

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func TestNormalizeVolumePaths(t *testing.T) {
	tests := []struct {
		in      []string
		want    []string
		wantErr bool
	}{
		{in: nil, want: nil},
		{in: []string{}, want: []string{}},
		{in: []string{" /app/data/ ", "/app/data", ""}, want: []string{"/app/data"}},
		{in: []string{"/app/services/whatsapp-engine/.wwebjs_auth"}, want: []string{"/app/services/whatsapp-engine/.wwebjs_auth"}},
		{in: []string{"relative/path"}, wantErr: true},
		{in: []string{"/"}, wantErr: true},
		{in: []string{"/app/../etc"}, wantErr: true},
		{in: []string{"/app/data:/host"}, wantErr: true},
		{in: []string{"/a", "/b", "/c", "/d", "/e", "/f"}, wantErr: true},
		{in: []string{"/" + strings.Repeat("a", maxVolumePathLen)}, wantErr: true},
	}
	for _, tt := range tests {
		got, err := normalizeVolumePaths(tt.in)
		if (err != nil) != tt.wantErr {
			t.Errorf("normalizeVolumePaths(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			continue
		}
		if !tt.wantErr && !reflect.DeepEqual(got, tt.want) {
			t.Errorf("normalizeVolumePaths(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestVolumeNameForIsStableAndDistinct(t *testing.T) {
	a := volumeNameFor("dep-1", "/app/data")
	if a != volumeNameFor("dep-1", "/app/data") {
		t.Fatal("same deployment and path must give the same volume name")
	}
	if a == volumeNameFor("dep-1", "/app/other") || a == volumeNameFor("dep-2", "/app/data") {
		t.Fatal("a different deployment or path must give a different volume name")
	}
}

// Saving settings without volumePaths (an older client) must keep the stored
// ones; saving an empty list clears them.
func TestVolumePathsStoreKeepsOnOmit(t *testing.T) {
	ctx := context.Background()
	store, err := NewDeploymentStore(ctx)
	if err != nil {
		t.Skipf("skipping: could not reach local postgres: %v", err)
	}
	defer store.Close()

	user, err := store.CreateUser(ctx, "regression-test-volume-paths@example.com", "regression-test", "not-a-real-hash")
	if err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	defer store.pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, user.ID)

	id, err := store.CreateDeploymentAttemptForUser(ctx, user.ID, "regression-volume-paths", "https://example.com/r.git", "https://example.com/r.git", "8080", "")
	if err != nil {
		t.Fatalf("CreateDeploymentAttemptForUser: %v", err)
	}

	if err := store.SetBuildSettings(ctx, id, BuildSettings{VolumePaths: []string{"/app/data"}}); err != nil {
		t.Fatalf("SetBuildSettings: %v", err)
	}
	if err := store.SetBuildSettings(ctx, id, BuildSettings{MemoryMB: 1024}); err != nil {
		t.Fatalf("SetBuildSettings without volumePaths: %v", err)
	}
	if got, _ := store.GetBuildSettings(ctx, id); !reflect.DeepEqual(got.VolumePaths, []string{"/app/data"}) || got.MemoryMB != 1024 {
		t.Fatalf("after omitting volumePaths = %+v, want /app/data kept", got)
	}
	if err := store.SetBuildSettings(ctx, id, BuildSettings{VolumePaths: []string{}}); err != nil {
		t.Fatalf("SetBuildSettings clearing: %v", err)
	}
	if got, _ := store.GetBuildSettings(ctx, id); got.VolumePaths != nil {
		t.Fatalf("after clearing = %+v, want no volume paths", got.VolumePaths)
	}
}
