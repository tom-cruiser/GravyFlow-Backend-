package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// A git killed mid-fetch leaves .git/shallow.lock behind, after which every
// fetch in that checkout fails with "Unable to create ... File exists".
func TestRunGitSyncRecoversFromStaleShallowLock(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	tmp := t.TempDir()
	origin := filepath.Join(tmp, "origin")
	dest := filepath.Join(tmp, "checkout")

	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main", origin)
	if err := os.WriteFile(filepath.Join(origin, "a.txt"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("-C", origin, "add", ".")
	git("-C", origin, "commit", "-qm", "one")
	originURL := "file://" + origin
	git("clone", "-q", "--depth", "1", originURL, dest)

	for _, lock := range []string{"shallow.lock", "index.lock", "refs/heads/main.lock"} {
		if err := os.WriteFile(filepath.Join(dest, ".git", lock), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	if err := runGitSync(ctx, dest, originURL, GitCloneOptions{}); err == nil {
		t.Fatal("expected the stale lock to break a plain sync")
	}

	removeStaleGitLocks(dest)
	if err := runGitSync(ctx, dest, originURL, GitCloneOptions{}); err != nil {
		t.Fatalf("sync after removing stale locks: %v", err)
	}
}
