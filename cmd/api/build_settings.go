package main

import (
	"bufio"
	"context"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
)

// ============================================================================
// MONOREPO BUILD SETTINGS
// ============================================================================
//
// By default GravyFlow builds the repository root (a generated Dockerfile for
// Node projects, Nixpacks for everything else) and expects the app on port
// 8080. A monorepo — several services under services/*, each with its own
// Dockerfile built from the repo root — has nothing to start at the root, so
// each service instead names the Dockerfile to build:
//
//	dockerfile_path  e.g. "services/auth-tenant/Dockerfile", relative to the
//	                 repo root, which stays the build context (that's how
//	                 monorepo Dockerfiles are written: COPY packages/ ...)
//	container_port   the port the app listens on; 0 = read the Dockerfile's
//	                 EXPOSE, falling back to the default 8080

type BuildSettings struct {
	DockerfilePath string `json:"dockerfilePath"`
	ContainerPort  int    `json:"containerPort"`
}

const maxDockerfilePathLen = 200

// normalizeDockerfilePath validates a user-supplied path and returns its
// clean, slash-separated form ("" means "not set"). It must stay inside the
// repository, so absolute paths, ".." and option-looking values are rejected.
func normalizeDockerfilePath(raw string) (string, error) {
	p := strings.TrimSpace(strings.ReplaceAll(raw, `\`, "/"))
	if p == "" {
		return "", nil
	}
	if len(p) > maxDockerfilePathLen {
		return "", fmt.Errorf("dockerfilePath is too long (max %d characters)", maxDockerfilePathLen)
	}
	if strings.ContainsAny(p, "\x00\r\n") {
		return "", fmt.Errorf("dockerfilePath contains invalid characters")
	}
	if strings.HasPrefix(p, "/") || strings.HasPrefix(p, "-") {
		return "", fmt.Errorf("dockerfilePath must be a relative path inside the repository (e.g. services/api/Dockerfile)")
	}
	cleaned := path.Clean(p)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("dockerfilePath must stay inside the repository")
	}
	return cleaned, nil
}

func validateContainerPort(port int) error {
	if port < 0 || port > 65535 {
		return fmt.Errorf("containerPort must be between 1 and 65535 (or 0 to auto-detect)")
	}
	return nil
}

func (b BuildSettings) validated() (BuildSettings, error) {
	p, err := normalizeDockerfilePath(b.DockerfilePath)
	if err != nil {
		return BuildSettings{}, err
	}
	if err := validateContainerPort(b.ContainerPort); err != nil {
		return BuildSettings{}, err
	}
	return BuildSettings{DockerfilePath: p, ContainerPort: b.ContainerPort}, nil
}

// resolveDockerfile returns the absolute path of rel inside repoRoot, and
// fails if it doesn't exist, isn't a regular file, or (via a symlink) points
// outside the checkout.
func resolveDockerfile(repoRoot string, rel string) (string, error) {
	clean, err := normalizeDockerfilePath(rel)
	if err != nil {
		return "", err
	}
	if clean == "" {
		return "", fmt.Errorf("no Dockerfile path given")
	}

	root, err := filepath.EvalSymlinks(repoRoot)
	if err != nil {
		return "", fmt.Errorf("resolve repository path: %w", err)
	}
	full := filepath.Join(root, filepath.FromSlash(clean))
	resolved, err := filepath.EvalSymlinks(full)
	if err != nil {
		if os.IsNotExist(err) {
			return "", fmt.Errorf("Dockerfile %q was not found in the repository", clean)
		}
		return "", fmt.Errorf("resolve Dockerfile %q: %w", clean, err)
	}
	if rel, err := filepath.Rel(root, resolved); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("Dockerfile %q resolves outside the repository", clean)
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("Dockerfile %q is not a regular file", clean)
	}
	return resolved, nil
}

// effectiveDockerfilePath is the Dockerfile a build should use: the explicit
// setting, else a Dockerfile at the repository root (the Railway/Render
// convention — a repo that ships one knows how it must be built better than
// the generated templates do), else "" for language detection.
func effectiveDockerfilePath(repoRoot string, configured string) string {
	if strings.TrimSpace(configured) != "" {
		return configured
	}
	if _, err := resolveDockerfile(repoRoot, "Dockerfile"); err == nil {
		return "Dockerfile"
	}
	return ""
}

// detectDockerfilePort returns the port from the Dockerfile's last EXPOSE
// instruction (the final stage's, in a multi-stage build), or 0 if there is
// none. "3001", "3001/tcp" and "$PORT"-style values are handled; only a
// literal number counts.
func detectDockerfilePort(dockerfilePath string) int {
	f, err := os.Open(dockerfilePath)
	if err != nil {
		return 0
	}
	defer f.Close()

	port := 0
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(strings.TrimSpace(scanner.Text()))
		if len(fields) < 2 || !strings.EqualFold(fields[0], "EXPOSE") {
			continue
		}
		for _, spec := range fields[1:] {
			spec = strings.SplitN(spec, "/", 2)[0]
			if n, err := strconv.Atoi(spec); err == nil && n > 0 && n <= 65535 {
				port = n
				break
			}
		}
	}
	return port
}

// findDockerfiles lists Dockerfiles in a checkout (relative, slash-separated,
// shallowest first) so an error can point at what could be built instead.
func findDockerfiles(root string, limit int) []string {
	const maxDepth = 4
	var found []string
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "dist", "build":
				return filepath.SkipDir
			}
			if rel != "." && strings.Count(rel, string(filepath.Separator)) >= maxDepth {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if name == "Dockerfile" || strings.HasPrefix(name, "Dockerfile.") || strings.HasSuffix(name, ".Dockerfile") {
			found = append(found, filepath.ToSlash(rel))
		}
		return nil
	})
	sort.Slice(found, func(i, j int) bool {
		di, dj := strings.Count(found[i], "/"), strings.Count(found[j], "/")
		if di != dj {
			return di < dj
		}
		return found[i] < found[j]
	})
	if limit > 0 && len(found) > limit {
		found = found[:limit]
	}
	return found
}

// monorepoHint is appended to the "no start command" build failure. That
// error is exactly what a monorepo produces when built from its root, and on
// its own gives no hint about the fix.
func monorepoHint(repoRoot string) string {
	dockerfiles := findDockerfiles(repoRoot, 12)
	if len(dockerfiles) == 0 {
		return ""
	}
	return fmt.Sprintf(
		"This repository has no start command at its root but contains Dockerfiles (%s). "+
			"If it is a monorepo, set the service's Dockerfile path (Source settings, or when creating the service) "+
			"to the one this service should build, e.g. %s, then redeploy.",
		strings.Join(dockerfiles, ", "), dockerfiles[0],
	)
}

// ============================================================================
// STORE
// ============================================================================

func (s *DeploymentStore) GetBuildSettings(ctx context.Context, deploymentID string) (BuildSettings, error) {
	if s == nil || s.pool == nil {
		return BuildSettings{}, fmt.Errorf("deployment store is not initialized")
	}
	var b BuildSettings
	err := s.pool.QueryRow(ctx, `
	SELECT dockerfile_path, container_port FROM deployments WHERE id = $1
	`, deploymentID).Scan(&b.DockerfilePath, &b.ContainerPort)
	if err != nil {
		return BuildSettings{}, fmt.Errorf("load build settings: %w", err)
	}
	return b, nil
}

func (s *DeploymentStore) SetBuildSettings(ctx context.Context, deploymentID string, settings BuildSettings) error {
	if s == nil || s.pool == nil {
		return fmt.Errorf("deployment store is not initialized")
	}
	validated, err := settings.validated()
	if err != nil {
		return err
	}
	tag, err := s.pool.Exec(ctx, `
	UPDATE deployments SET dockerfile_path = $2, container_port = $3, updated_at = now()
	WHERE id = $1 AND deleted_at IS NULL
	`, deploymentID, validated.DockerfilePath, validated.ContainerPort)
	if err != nil {
		return fmt.Errorf("save build settings: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("deployment not found")
	}
	return nil
}

// UpdateDeploymentPortMap records the container port a build resolved to, so
// the container, the Caddy route and the UI all agree on it.
func (s *DeploymentStore) UpdateDeploymentPortMap(ctx context.Context, deploymentID string, portMap string) error {
	if s == nil || s.pool == nil {
		return fmt.Errorf("deployment store is not initialized")
	}
	_, err := s.pool.Exec(ctx, `
	UPDATE deployments SET port_map = $2, updated_at = now() WHERE id = $1
	`, deploymentID, portMap)
	return err
}

// ============================================================================
// HTTP
// ============================================================================

func getBuildSettingsHandler(c *gin.Context) {
	_, deployment, ok := currentUserDeployment(c)
	if !ok {
		return
	}
	settings, err := deploymentStore.GetBuildSettings(c.Request.Context(), deployment.DeploymentID)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_load_build_settings", "details": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"buildSettings": settings})
}

func updateBuildSettingsHandler(c *gin.Context) {
	_, deployment, ok := currentUserDeployment(c)
	if !ok {
		return
	}

	var req BuildSettings
	if err := c.ShouldBindJSON(&req); err != nil {
		sendBadRequest(c, "invalid JSON body", err)
		return
	}
	validated, err := req.validated()
	if err != nil {
		sendBadRequest(c, err.Error(), nil)
		return
	}
	if err := deploymentStore.SetBuildSettings(c.Request.Context(), deployment.DeploymentID, validated); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_save_build_settings", "details": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"buildSettings": validated,
		"message":       "saved; redeploy the service to build with these settings",
	})
}
