package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ============================================================================
// PRE-CLONE REPOSITORY CHECK
// ============================================================================
//
// Before a GitHub App service is cloned, ask GitHub how big the checkout will
// really be and whether the App can still see the repository. A repository
// that commits something huge (a browser profile, a dataset, node_modules)
// otherwise fails only after minutes of downloading, having also held the
// worker slot the whole time.
//
//	GRAVYFLOW_MAX_CLONE_SIZE_MB   largest checkout allowed, default 500; 0 disables
//
// The check is advisory for GitHub API hiccups (it logs and lets the clone
// proceed rather than block deploys on a flaky API) but strict about the two
// things it can be sure of: the repository is inaccessible, or it's too big.

const (
	defaultMaxCloneSizeMB   = 500
	repoPreflightTimeout    = 20 * time.Second
	repoPreflightTopOffends = 3
)

func maxCloneSizeBytes() int64 {
	raw := strings.TrimSpace(os.Getenv("GRAVYFLOW_MAX_CLONE_SIZE_MB"))
	if raw == "" {
		return defaultMaxCloneSizeMB << 20
	}
	mb, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || mb < 0 {
		return defaultMaxCloneSizeMB << 20
	}
	return mb << 20
}

type pathSize struct {
	Path  string
	Bytes int64
}

// summarizeTree totals the blob sizes and ranks the heaviest directories
// (grouped three levels deep) so the error can say exactly what to remove.
func summarizeTree(entries []GitHubTreeEntry) (totalBytes int64, files int, top []pathSize) {
	byDir := map[string]int64{}
	for _, e := range entries {
		if e.Type != "blob" {
			continue
		}
		totalBytes += e.Size
		files++
		parts := strings.Split(e.Path, "/")
		if len(parts) > 3 {
			parts = parts[:3]
		} else if len(parts) > 1 {
			parts = parts[:len(parts)-1]
		}
		byDir[strings.Join(parts, "/")] += e.Size
	}
	for path, bytes := range byDir {
		top = append(top, pathSize{Path: path, Bytes: bytes})
	}
	sort.Slice(top, func(i, j int) bool { return top[i].Bytes > top[j].Bytes })
	if len(top) > repoPreflightTopOffends {
		top = top[:repoPreflightTopOffends]
	}
	return totalBytes, files, top
}

func formatMB(bytes int64) string {
	return fmt.Sprintf("%.0f MB", float64(bytes)/(1<<20))
}

func tooLargeError(fullName string, total int64, limit int64, truncated bool, top []pathSize) error {
	parts := make([]string, 0, len(top))
	for _, p := range top {
		parts = append(parts, fmt.Sprintf("%s (%s)", p.Path, formatMB(p.Bytes)))
	}
	approx := ""
	if truncated {
		approx = "at least "
	}
	return fmt.Errorf(
		"repository %s is %s%s at its latest commit, over the %s clone limit; largest paths: %s. "+
			"Remove them from the repository (and add them to .gitignore) or raise GRAVYFLOW_MAX_CLONE_SIZE_MB",
		fullName, approx, formatMB(total), formatMB(limit), strings.Join(parts, ", "),
	)
}

// preflightGitHubRepo runs the check for a GitHub-App-linked deployment that
// still needs a fresh clone. Deployments that already have a checkout only
// fetch changes and are skipped.
func preflightGitHubRepo(ctx context.Context, deploymentID string) error {
	if deploymentStore == nil {
		return nil
	}
	limit := maxCloneSizeBytes() // 0 disables the size check only
	if _, err := os.Stat(filepath.Join(appBuildRoot(), deploymentID, ".git")); err == nil {
		return nil
	}

	src, err := deploymentStore.GetDeploymentGitHubSource(ctx, deploymentID)
	if err != nil || !src.Linked || !src.InstallationActive {
		// Not linked, or the installation problem is reported with a better
		// message by githubCloneToken.
		return nil
	}
	client, err := githubApp()
	if err != nil {
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, repoPreflightTimeout)
	defer cancel()
	reporter := cloneReporterFrom(ctx)

	repo, err := client.GetRepository(ctx, src.InstallationID, src.RepositoryID)
	if err != nil {
		if isGitHubStatus(err, http.StatusNotFound) || isGitHubStatus(err, http.StatusForbidden) {
			return fmt.Errorf("the GitHub App can no longer see this repository; grant it access in the installation's settings on GitHub: %w", err)
		}
		log.Printf("[git] repository check skipped for %s: %v", deploymentID, err)
		return nil
	}

	entries, truncated, err := client.GetRepositoryTree(ctx, src.InstallationID, repo.FullName, repo.DefaultBranch)
	if err != nil {
		if isGitHubStatus(err, http.StatusConflict) {
			return fmt.Errorf("repository %s is empty (no commits to deploy)", repo.FullName)
		}
		log.Printf("[git] repository size check skipped for %s: %v", repo.FullName, err)
		return nil
	}

	total, files, top := summarizeTree(entries)
	reporter.note(fmt.Sprintf("repository %s: %d files, %s at %s", repo.FullName, files, formatMB(total), repo.DefaultBranch))
	if limit > 0 && total > limit {
		return tooLargeError(repo.FullName, total, limit, truncated, top)
	}

	// A monorepo service names its Dockerfile; catch a typo here, in
	// seconds, instead of after the clone.
	if settings, err := deploymentStore.GetBuildSettings(ctx, deploymentID); err == nil && settings.DockerfilePath != "" && !truncated {
		if err := checkDockerfileInTree(repo.FullName, settings.DockerfilePath, entries); err != nil {
			return err
		}
	}
	return nil
}

// checkDockerfileInTree verifies that path is a file in the repository tree,
// and otherwise lists the Dockerfiles that do exist.
func checkDockerfileInTree(fullName string, dockerfilePath string, entries []GitHubTreeEntry) error {
	var available []string
	for _, e := range entries {
		if e.Type != "blob" {
			continue
		}
		if e.Path == dockerfilePath {
			return nil
		}
		base := e.Path[strings.LastIndex(e.Path, "/")+1:]
		if base == "Dockerfile" || strings.HasPrefix(base, "Dockerfile.") || strings.HasSuffix(base, ".Dockerfile") {
			available = append(available, e.Path)
		}
	}
	sort.Strings(available)
	if len(available) > 12 {
		available = available[:12]
	}
	if len(available) == 0 {
		return fmt.Errorf("Dockerfile %q was not found in %s, and the repository has no Dockerfiles", dockerfilePath, fullName)
	}
	return fmt.Errorf("Dockerfile %q was not found in %s; Dockerfiles in this repository: %s", dockerfilePath, fullName, strings.Join(available, ", "))
}
