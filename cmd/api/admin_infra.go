package main

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/gin-gonic/gin"
)

// ============================================================================
// Module B: Infrastructure & Deployment Management
// ============================================================================

// AdminDeploymentSummary is a deployment row joined with its owner's email,
// for the admin-wide service search (as opposed to ListDeploymentsForUser,
// which is scoped to one owner).
type AdminDeploymentSummary struct {
	DeploymentRecord
	OwnerUserID string `json:"ownerUserId"`
	OwnerEmail  string `json:"ownerEmail"`
}

type PaginatedAdminDeployments struct {
	Items      []AdminDeploymentSummary `json:"items"`
	TotalCount int                      `json:"totalCount"`
	Page       int                      `json:"page"`
	PerPage    int                      `json:"perPage"`
	TotalPages int                      `json:"totalPages"`
}

// ClusterOverview backs Module B's live telemetry panel. It is built from
// real Docker stats (GetContainerStats in docker.go), not fabricated numbers
// — CPU/memory are summed across every deployment with a running container.
type ClusterOverview struct {
	ActiveContainers int       `json:"activeContainers"`
	TotalDeployments int       `json:"totalDeployments"`
	TotalCPUCores    float64   `json:"totalCpuCores"`
	TotalMemoryBytes float64   `json:"totalMemoryBytes"`
	TotalDiskBytes   float64   `json:"totalDiskBytes"` // image layers + volumes, see GetClusterDiskUsage
	SampledAt        time.Time `json:"sampledAt"`
	UnreachableCount int       `json:"unreachableCount"` // containers that failed to report stats
}

// ============================================================================
// STORE METHODS
// ============================================================================

func (s *DeploymentStore) AdminGetDeploymentByID(ctx context.Context, deploymentID string) (AdminDeploymentSummary, error) {
	if s == nil || s.pool == nil {
		return AdminDeploymentSummary{}, &StoreError{Type: ErrDatabase, Message: "deployment store is not initialized"}
	}

	var d AdminDeploymentSummary
	var startedAt, finishedAt *time.Time
	err := s.pool.QueryRow(ctx, `
SELECT
	d.id::text, d.project_id::text, d.app_name, d.source_repo_url, d.app_path, d.port_map,
	COALESCE(d.image_name, ''), COALESCE(d.container_id, ''), COALESCE(d.container_name, ''),
	d.status::text, COALESCE(d.status_message, ''), d.started_at, d.finished_at, d.created_at, d.updated_at,
	d.owner_user_id::text, u.email
FROM deployments d
JOIN users u ON u.id = d.owner_user_id
WHERE d.id = $1 AND d.deleted_at IS NULL
`, deploymentID).Scan(
		&d.DeploymentID, &d.ProjectID, &d.AppName, &d.SourceRepoURL, &d.AppPath, &d.PortMap,
		&d.ImageName, &d.ContainerID, &d.ContainerName, &d.Status, &d.StatusMessage, &startedAt, &finishedAt,
		&d.CreatedAt, &d.UpdatedAt, &d.OwnerUserID, &d.OwnerEmail,
	)
	if err != nil {
		return AdminDeploymentSummary{}, &StoreError{Type: ErrNotFound, Message: "deployment not found", Err: err}
	}
	d.StartedAt = startedAt
	d.FinishedAt = finishedAt
	return d, nil
}

func (s *DeploymentStore) AdminListDeployments(ctx context.Context, search string, pagination Pagination) (PaginatedAdminDeployments, error) {
	if s == nil || s.pool == nil {
		return PaginatedAdminDeployments{}, &StoreError{Type: ErrDatabase, Message: "deployment store is not initialized"}
	}
	if pagination.Limit <= 0 || pagination.Limit > 200 {
		pagination.Limit = 25
	}

	var conditions []string
	var args []any
	argIndex := 1
	conditions = append(conditions, "d.deleted_at IS NULL")

	if v := strings.TrimSpace(search); v != "" {
		conditions = append(conditions, "(d.app_name ILIKE $"+strconv.Itoa(argIndex)+" OR u.email ILIKE $"+strconv.Itoa(argIndex)+" OR d.id::text = $"+strconv.Itoa(argIndex+1)+")")
		args = append(args, "%"+v+"%", v)
		argIndex += 2
	}
	whereClause := "WHERE " + strings.Join(conditions, " AND ")

	var totalCount int
	if err := s.pool.QueryRow(ctx, `
SELECT COUNT(*) FROM deployments d JOIN users u ON u.id = d.owner_user_id `+whereClause, args...).Scan(&totalCount); err != nil {
		return PaginatedAdminDeployments{}, &StoreError{Type: ErrDatabase, Message: "failed to count deployments", Err: err}
	}

	listArgs := append(append([]any{}, args...), pagination.Limit, pagination.Offset)
	rows, err := s.pool.Query(ctx, `
SELECT
	d.id::text, d.project_id::text, d.app_name, d.source_repo_url, d.app_path, d.port_map,
	COALESCE(d.image_name, ''), COALESCE(d.container_id, ''), COALESCE(d.container_name, ''),
	d.status::text, COALESCE(d.status_message, ''), d.started_at, d.finished_at, d.created_at, d.updated_at,
	d.owner_user_id::text, u.email
FROM deployments d
JOIN users u ON u.id = d.owner_user_id
`+whereClause+`
ORDER BY d.created_at DESC
LIMIT $`+strconv.Itoa(argIndex)+` OFFSET $`+strconv.Itoa(argIndex+1), listArgs...)
	if err != nil {
		return PaginatedAdminDeployments{}, &StoreError{Type: ErrDatabase, Message: "failed to list deployments", Err: err}
	}
	defer rows.Close()

	items := make([]AdminDeploymentSummary, 0)
	for rows.Next() {
		var d AdminDeploymentSummary
		var startedAt, finishedAt *time.Time
		if err := rows.Scan(&d.DeploymentID, &d.ProjectID, &d.AppName, &d.SourceRepoURL, &d.AppPath, &d.PortMap,
			&d.ImageName, &d.ContainerID, &d.ContainerName, &d.Status, &d.StatusMessage, &startedAt, &finishedAt,
			&d.CreatedAt, &d.UpdatedAt, &d.OwnerUserID, &d.OwnerEmail); err != nil {
			return PaginatedAdminDeployments{}, &StoreError{Type: ErrDatabase, Message: "failed to scan deployment", Err: err}
		}
		d.StartedAt = startedAt
		d.FinishedAt = finishedAt
		items = append(items, d)
	}
	if err := rows.Err(); err != nil {
		return PaginatedAdminDeployments{}, &StoreError{Type: ErrDatabase, Message: "failed to iterate deployments", Err: err}
	}

	page := (pagination.Offset / pagination.Limit) + 1
	totalPages := (totalCount + pagination.Limit - 1) / pagination.Limit
	if totalPages < 1 {
		totalPages = 1
	}

	return PaginatedAdminDeployments{Items: items, TotalCount: totalCount, Page: page, PerPage: pagination.Limit, TotalPages: totalPages}, nil
}

// adminListRunningContainers returns every non-empty container ID currently
// on record for a RUNNING deployment, across all users. Shared by the
// cluster overview (Module B) and the fraud/abuse heuristic (Module C).
func (s *DeploymentStore) adminListRunningContainers(ctx context.Context) ([]AdminDeploymentSummary, error) {
	rows, err := s.pool.Query(ctx, `
SELECT d.id::text, d.container_id, d.owner_user_id::text, u.email, d.app_name
FROM deployments d
JOIN users u ON u.id = d.owner_user_id
WHERE d.status = $1 AND d.container_id IS NOT NULL AND d.container_id != '' AND d.deleted_at IS NULL
`, string(DeploymentStatusRunning))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := make([]AdminDeploymentSummary, 0)
	for rows.Next() {
		var d AdminDeploymentSummary
		if err := rows.Scan(&d.DeploymentID, &d.ContainerID, &d.OwnerUserID, &d.OwnerEmail, &d.AppName); err != nil {
			return nil, err
		}
		items = append(items, d)
	}
	return items, rows.Err()
}

// AdminGetClusterOverview samples GetContainerStats (real Docker Engine API
// calls, see docker.go) across every running deployment concurrently.
func (s *DeploymentStore) AdminGetClusterOverview(ctx context.Context) (ClusterOverview, error) {
	running, err := s.adminListRunningContainers(ctx)
	if err != nil {
		return ClusterOverview{}, &StoreError{Type: ErrDatabase, Message: "failed to list running containers", Err: err}
	}

	var totalDeployments int
	if err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM deployments WHERE deleted_at IS NULL`).Scan(&totalDeployments); err != nil {
		totalDeployments = 0
	}

	overview := ClusterOverview{
		ActiveContainers: len(running),
		TotalDeployments: totalDeployments,
		SampledAt:        time.Now().UTC(),
	}

	// Disk usage is a single cluster-wide call (image layers + volumes aren't
	// per-container), so it isn't part of the per-container fan-out below.
	// Best-effort: a Docker API hiccup here shouldn't fail the whole overview.
	if diskBytes, err := GetClusterDiskUsage(ctx); err == nil {
		overview.TotalDiskBytes = diskBytes
	}

	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8) // bound concurrent Docker API calls

	statsCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	for _, d := range running {
		wg.Add(1)
		sem <- struct{}{}
		go func(containerID string) {
			defer wg.Done()
			defer func() { <-sem }()

			stats, err := GetContainerStats(statsCtx, containerID)
			mu.Lock()
			defer mu.Unlock()
			if err != nil || stats == nil {
				overview.UnreachableCount++
				return
			}
			overview.TotalCPUCores += stats.CPUUsage
			overview.TotalMemoryBytes += stats.MemoryUsage
		}(d.ContainerID)
	}
	wg.Wait()

	return overview, nil
}

// ============================================================================
// STORE METHODS — service control
// ============================================================================

// AdminPurgeDeploymentImage removes the deployment's built Docker image so
// the next deploy is forced to rebuild from scratch. This is intentionally
// scoped to one deployment's image rather than the shared Nixpacks build
// cache directory (/var/cache/nixpacks, see build.go), which every deploy on
// the box shares — purging that wholesale would degrade every other tenant's
// build times for one support request.
func AdminPurgeDeploymentImage(ctx context.Context, imageName string) error {
	imageName = strings.TrimSpace(imageName)
	if imageName == "" {
		return nil
	}

	dockerClient, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return fmt.Errorf("create docker client: %w", err)
	}
	defer dockerClient.Close()

	if _, err := dockerClient.ImageRemove(ctx, imageName, image.RemoveOptions{Force: true, PruneChildren: true}); err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "no such image") {
			return nil
		}
		return fmt.Errorf("remove image %q: %w", imageName, err)
	}
	return nil
}

// ============================================================================
// HANDLERS
// ============================================================================

func adminClusterOverviewHandler(c *gin.Context) {
	overview, err := deploymentStore.AdminGetClusterOverview(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_load_cluster_overview", "details": err.Error(), "request_id": c.GetString("requestID")})
		return
	}
	c.JSON(http.StatusOK, overview)
}

func adminListDeploymentsHandler(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	if page < 1 {
		page = 1
	}
	perPage, _ := strconv.Atoi(c.DefaultQuery("perPage", "25"))
	if perPage <= 0 || perPage > 200 {
		perPage = 25
	}

	result, err := deploymentStore.AdminListDeployments(c.Request.Context(), c.Query("search"), Pagination{
		Limit:  perPage,
		Offset: (page - 1) * perPage,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_list_deployments", "details": err.Error(), "request_id": c.GetString("requestID")})
		return
	}
	c.JSON(http.StatusOK, result)
}

// adminRestartServiceHandler restarts any deployment regardless of owner,
// routed through the same job queue as the self-service restart so behavior
// (rebuild-if-needed, container recreation) stays identical.
func adminRestartServiceHandler(c *gin.Context) {
	deploymentID := strings.TrimSpace(c.Param("id"))
	deployment, err := deploymentStore.AdminGetDeploymentByID(c.Request.Context(), deploymentID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "deployment_not_found", "details": err.Error()})
		return
	}

	jobID, err := deploymentJobs.EnqueueDeployment(c.Request.Context(), deployment.OwnerUserID, deployment.DeploymentID, false)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_restart_service", "details": err.Error()})
		return
	}

	actorID, actorEmail := auditActorFromContext(c)
	if err := RecordAuditLog(c.Request.Context(), actorID, actorEmail, "deployment.restart", "deployment", deploymentID, map[string]any{"jobId": jobID, "ownerEmail": deployment.OwnerEmail}, c.ClientIP()); err != nil {
		fmt.Printf("[WARN] failed to record audit log for deployment.restart on %q: %v\n", deploymentID, err)
	}

	c.JSON(http.StatusAccepted, gin.H{"message": "service restart queued", "deploymentId": deploymentID, "jobId": jobID})
}

type AdminForceStopRequest struct {
	Reason string `json:"reason"`
}

// adminForceStopHandler is the "[Force Stop Container]" action: it stops and
// removes the container immediately (no job queue, no cooldown) and marks
// the deployment STOPPED so the platform doesn't try to health-check it.
func adminForceStopHandler(c *gin.Context) {
	deploymentID := strings.TrimSpace(c.Param("id"))
	deployment, err := deploymentStore.AdminGetDeploymentByID(c.Request.Context(), deploymentID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "deployment_not_found", "details": err.Error()})
		return
	}

	var req AdminForceStopRequest
	_ = c.ShouldBindJSON(&req)

	if deployment.ContainerID != "" {
		if err := StopAndRemoveContainer(deployment.ContainerID, false); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_stop_container", "details": err.Error()})
			return
		}
	}

	if err := deploymentStore.UpdateDeploymentStatus(c.Request.Context(), deploymentID, DeploymentStatusStopped, "force-stopped by admin", "", "", "", ""); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_update_status", "details": err.Error()})
		return
	}

	actorID, actorEmail := auditActorFromContext(c)
	if err := RecordAuditLog(c.Request.Context(), actorID, actorEmail, "deployment.force_stop", "deployment", deploymentID, map[string]any{"reason": req.Reason, "ownerEmail": deployment.OwnerEmail}, c.ClientIP()); err != nil {
		fmt.Printf("[WARN] failed to record audit log for deployment.force_stop on %q: %v\n", deploymentID, err)
	}

	c.JSON(http.StatusOK, gin.H{"message": "container force-stopped", "deploymentId": deploymentID})
}

// adminPurgeCacheHandler is the "[Purge Deployment Cache]" action.
func adminPurgeCacheHandler(c *gin.Context) {
	deploymentID := strings.TrimSpace(c.Param("id"))
	deployment, err := deploymentStore.AdminGetDeploymentByID(c.Request.Context(), deploymentID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "deployment_not_found", "details": err.Error()})
		return
	}

	if err := AdminPurgeDeploymentImage(c.Request.Context(), deployment.ImageName); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_purge_cache", "details": err.Error()})
		return
	}

	actorID, actorEmail := auditActorFromContext(c)
	if err := RecordAuditLog(c.Request.Context(), actorID, actorEmail, "deployment.purge_cache", "deployment", deploymentID, map[string]any{"image": deployment.ImageName, "ownerEmail": deployment.OwnerEmail}, c.ClientIP()); err != nil {
		fmt.Printf("[WARN] failed to record audit log for deployment.purge_cache on %q: %v\n", deploymentID, err)
	}

	c.JSON(http.StatusOK, gin.H{"message": "build image purged; next deploy will rebuild from scratch", "deploymentId": deploymentID})
}

// adminGetDeploymentEnvHandler is the "Environment Variable & Secret
// Inspector": sensitive values always render masked (includeSensitive is
// hardcoded false), regardless of who is asking.
func adminGetDeploymentEnvHandler(c *gin.Context) {
	deploymentID := strings.TrimSpace(c.Param("id"))
	deployment, err := deploymentStore.AdminGetDeploymentByID(c.Request.Context(), deploymentID)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "deployment_not_found", "details": err.Error()})
		return
	}

	envVars, err := deploymentStore.ListDeploymentEnvVarsWithValues(c.Request.Context(), deployment.OwnerUserID, deploymentID, false)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed_to_load_env_vars", "details": err.Error()})
		return
	}

	actorID, actorEmail := auditActorFromContext(c)
	if err := RecordAuditLog(c.Request.Context(), actorID, actorEmail, "deployment.env.view", "deployment", deploymentID, map[string]any{"ownerEmail": deployment.OwnerEmail}, c.ClientIP()); err != nil {
		fmt.Printf("[WARN] failed to record audit log for deployment.env.view on %q: %v\n", deploymentID, err)
	}

	c.JSON(http.StatusOK, gin.H{"envVars": envVars, "count": len(envVars)})
}
