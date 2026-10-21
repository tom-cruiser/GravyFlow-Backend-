package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
)

// ============================================================================
// PERSISTENT VOLUMES
// ============================================================================
//
// Every redeploy recreates the app's container, so anything the app writes to
// its own filesystem is lost. volume_paths lists container directories that
// must survive that (e.g. a WhatsApp Web login store): each one is backed by
// a named docker volume tied to the deployment, created on first deploy and
// re-mounted on every later one. Docker seeds a new, empty volume with the
// image's contents at that path, so mounting over a path the image already
// populates is safe.
//
// Volumes outlive soft deletes (the app may be restored) and are removed only
// by the hard-delete path, RemoveDeploymentVolumes.

const (
	maxVolumePaths      = 5
	maxVolumePathLen    = 200
	volumePathLabelKey  = "gravyflow.volume-path"
	volumeDeploymentKey = "gravyflow.deployment-id"
)

var volumePathPattern = regexp.MustCompile(`^/[A-Za-z0-9._/-]+$`)

// normalizeVolumePaths validates user-supplied container paths and returns
// their clean, de-duplicated form. nil stays nil ("not given").
func normalizeVolumePaths(raw []string) ([]string, error) {
	if raw == nil {
		return nil, nil
	}
	if len(raw) > maxVolumePaths {
		return nil, fmt.Errorf("volumePaths allows at most %d paths", maxVolumePaths)
	}
	seen := map[string]bool{}
	out := []string{}
	for _, p := range raw {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if len(p) > maxVolumePathLen {
			return nil, fmt.Errorf("volume path is too long (max %d characters)", maxVolumePathLen)
		}
		if !volumePathPattern.MatchString(p) {
			return nil, fmt.Errorf("volume path %q must be absolute and use only letters, digits, '.', '_', '-' and '/'", p)
		}
		cleaned := path.Clean(p)
		if cleaned == "/" || strings.Contains(p, "..") {
			return nil, fmt.Errorf("volume path %q must name a directory below /", p)
		}
		if !seen[cleaned] {
			seen[cleaned] = true
			out = append(out, cleaned)
		}
	}
	return out, nil
}

// volumeNameFor is stable per (deployment, path), so a redeploy finds the
// same volume again.
func volumeNameFor(deploymentID string, containerPath string) string {
	sum := sha256.Sum256([]byte(containerPath))
	return fmt.Sprintf("gravyflow-%s-%s", deploymentID, hex.EncodeToString(sum[:6]))
}

// deploymentVolumeMounts creates (idempotently) the deployment's volumes and
// returns the mounts for its container. An error here must fail the deploy:
// starting without the mounts would look like the app's data vanished.
func deploymentVolumeMounts(ctx context.Context, dockerClient *client.Client, deploymentID string) ([]mount.Mount, error) {
	if deploymentStore == nil {
		return nil, nil
	}
	settings, err := deploymentStore.GetBuildSettings(ctx, deploymentID)
	if err != nil {
		return nil, fmt.Errorf("load volume paths: %w", err)
	}
	mounts := make([]mount.Mount, 0, len(settings.VolumePaths))
	for _, p := range settings.VolumePaths {
		name := volumeNameFor(deploymentID, p)
		if _, err := dockerClient.VolumeCreate(ctx, volume.CreateOptions{
			Name: name,
			Labels: map[string]string{
				managedByLabelKey:   managedByLabelValue,
				volumeDeploymentKey: deploymentID,
				volumePathLabelKey:  p,
			},
		}); err != nil {
			return nil, fmt.Errorf("create volume for %q: %w", p, err)
		}
		mounts = append(mounts, mount.Mount{Type: mount.TypeVolume, Source: name, Target: p})
	}
	return mounts, nil
}

// RemoveDeploymentVolumes deletes every volume created for a deployment. The
// container using them must already be removed.
func RemoveDeploymentVolumes(ctx context.Context, deploymentID string) error {
	dockerClient, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return fmt.Errorf("create docker client: %w", err)
	}
	defer dockerClient.Close()

	list, err := dockerClient.VolumeList(ctx, volume.ListOptions{
		Filters: filters.NewArgs(filters.Arg("label", volumeDeploymentKey+"="+deploymentID)),
	})
	if err != nil {
		return fmt.Errorf("list volumes: %w", err)
	}
	for _, v := range list.Volumes {
		if err := dockerClient.VolumeRemove(ctx, v.Name, false); err != nil {
			return fmt.Errorf("remove volume %q: %w", v.Name, err)
		}
	}
	return nil
}
