package main

import (
	"context"
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const caddyAdminLoadURL = "http://localhost:2019/load"

// SyncCaddyRoutesFromRunningContainers rebuilds the Caddy HTTP config from the current live Docker state.
// This is the deletion strategy as well: when a container stops, the next sync omits it, so Caddy drops the route safely.
func SyncCaddyRoutesFromRunningContainers() error {
	containers, err := ListRunningManagedContainers()
	if err != nil {
		return err
	}

	sort.Slice(containers, func(i, j int) bool {
		return containers[i].ContainerName < containers[j].ContainerName
	})

	routes := make([]map[string]any, 0, len(containers))
	for _, containerEntry := range containers {
		if containerEntry.ContainerName == "" || containerEntry.InternalIP == "" || containerEntry.InternalPort == "" {
			continue
		}

		hosts := []string{fmt.Sprintf("%s.localhost", containerEntry.ContainerName)}
		if containerEntry.DeploymentID != "" && deploymentStore != nil {
			verifiedDomains, err := deploymentStore.ListVerifiedDomainsForDeployment(context.Background(), containerEntry.DeploymentID)
			if err != nil {
				log.Printf("caddy sync: load domains for deployment %s: %v", containerEntry.DeploymentID, err)
			} else {
				hosts = append(hosts, verifiedDomains...)
			}
		}

		route := map[string]any{
			"match": []any{
				map[string]any{"host": hosts},
			},
			"handle": []any{
				map[string]any{
					"handler": "reverse_proxy",
					"upstreams": []any{
						map[string]any{"dial": fmt.Sprintf("%s:%s", containerEntry.InternalIP, containerEntry.InternalPort)},
					},
				},
			},
			"terminal": true,
		}
		routes = append(routes, route)
	}

	payload := map[string]any{
		"apps": map[string]any{
			"http": map[string]any{
				"servers": map[string]any{
					"gravyflow": map[string]any{
						"listen": []string{":80", ":443"},
						"routes": routes,
					},
				},
			},
		},
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal caddy payload: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, caddyAdminLoadURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create caddy request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("load caddy config: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("caddy load rejected config: %s", resp.Status)
	}

	return nil
}

// DeleteCaddyRouteForContainer is the explicit stop/removal hook.
// Call it after a container is stopped or removed; it performs the same safe rebuild and removes stale routes.
func DeleteCaddyRouteForContainer(containerName string) error {
	containerName = strings.TrimSpace(containerName)
	if containerName == "" {
		return &ValidationError{
			Field:   "containerName",
			Code:    "required",
			Message: "containerName is required",
		}
	}

	return SyncCaddyRoutesFromRunningContainers()
}

func cleanupCaddyCertificatesForDomain(customDomain string) {
	customDomain = normalizeCustomDomain(customDomain)
	if customDomain == "" {
		return
	}

	storageRoot := strings.TrimSpace(os.Getenv("CADDY_DATA_DIR"))
	if storageRoot == "" {
		storageRoot = strings.TrimSpace(os.Getenv("CADDY_STORAGE_DIR"))
	}
	if storageRoot == "" {
		return
	}

	certificateRoot := filepath.Join(storageRoot, "certificates")
	_ = filepath.WalkDir(certificateRoot, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !strings.Contains(strings.ToLower(path), customDomain) {
			return nil
		}
		if entry.IsDir() {
			return os.RemoveAll(path)
		}
		_ = os.Remove(path)
		return nil
	})
}
