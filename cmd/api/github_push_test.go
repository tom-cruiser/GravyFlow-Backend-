package main

import "testing"

// TestHandleGitHubPushVerdict covers the synchronous decision handleGitHubPush
// returns: only a push to the repository's default branch (not a tag, a
// deletion, or a feature branch) is worth acting on. The actual DB lookup and
// enqueue run in a goroutine and are not exercised here.
func TestHandleGitHubPushVerdict(t *testing.T) {
	mk := func(ref, defaultBranch string, deleted bool, repoID int64) githubWebhookPayload {
		var p githubWebhookPayload
		p.Ref = ref
		p.Deleted = deleted
		p.Repository.ID = repoID
		p.Repository.DefaultBranch = defaultBranch
		p.Repository.FullName = "tom-cruiser/example"
		return p
	}

	// The positive verdict dereferences the global deploymentStore (its async
	// lookup just logs and returns on a nil pool), so give it a non-nil store.
	prev := deploymentStore
	deploymentStore = &DeploymentStore{}
	defer func() { deploymentStore = prev }()

	cases := []struct {
		name         string
		payload      githubWebhookPayload
		installation int64
		want         bool
	}{
		{"default branch push", mk("refs/heads/main", "main", false, 42), 7, true},
		{"non-default branch ignored", mk("refs/heads/feature/x", "main", false, 42), 7, false},
		{"tag push ignored", mk("refs/tags/v1.0.0", "main", false, 42), 7, false},
		{"branch deletion ignored", mk("refs/heads/main", "main", true, 42), 7, false},
		{"empty default branch ignored", mk("refs/heads/main", "", false, 42), 7, false},
		{"missing repo id ignored", mk("refs/heads/main", "main", false, 0), 7, false},
		{"missing installation ignored", mk("refs/heads/main", "main", false, 42), 0, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := handleGitHubPush(tc.payload, tc.installation, "test-delivery"); got != tc.want {
				t.Fatalf("handleGitHubPush(%q, inst=%d) = %v, want %v", tc.payload.Ref, tc.installation, got, tc.want)
			}
		})
	}
}
