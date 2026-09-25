package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEffectiveDockerfilePath(t *testing.T) {
	empty := t.TempDir()
	withRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(withRoot, "Dockerfile"), []byte("FROM scratch\nEXPOSE 3000\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name, root, configured, want string
	}{
		{"explicit setting wins", withRoot, "services/api/Dockerfile", "services/api/Dockerfile"},
		{"root Dockerfile picked up", withRoot, "", "Dockerfile"},
		{"no Dockerfile falls back to detection", empty, "", ""},
		{"blank setting treated as unset", withRoot, "  ", "Dockerfile"},
	}
	for _, c := range cases {
		if got := effectiveDockerfilePath(c.root, c.configured); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}
