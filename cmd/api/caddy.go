package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

const caddyAdminLoadURL = "http://localhost:2019/load"

func caddySyncRequired() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("GRAVYFLOW_REQUIRE_CADDY")), "1")
}

func caddyAdminHealthy(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost:2019/config/", nil)
	if err != nil {
		return false
	}
	resp, err := (&http.Client{Timeout: 2 * time.Second}).Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices
}

func caddyLoadSucceededDespiteClose(err error) bool {
	if err == nil {
		return true
	}
	if !strings.Contains(strings.ToLower(err.Error()), "connection reset") {
		return false
	}
	return caddyAdminHealthy(context.Background())
}

func isCaddyUnavailable(err error) bool {
	if err == nil {
		return false
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		if errors.Is(opErr.Err, syscall.ECONNREFUSED) || errors.Is(opErr.Err, syscall.ECONNRESET) {
			return true
		}
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "connect: connection refused") ||
		strings.Contains(msg, "connection reset by peer") ||
		strings.Contains(msg, "read: connection reset") ||
		strings.Contains(msg, "no such host")
}

// SyncCaddyRoutesFromRunningContainers rebuilds the Caddy HTTP config from the current live Docker state.
// This is the deletion strategy as well: when a container stops, the next sync omits it, so Caddy drops the route safely.
// When Caddy admin is not running locally, sync is skipped unless GRAVYFLOW_REQUIRE_CADDY=1.
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
		"admin": map[string]any{
			"listen": "0.0.0.0:2019",
		},
		"apps": map[string]any{
			"http": map[string]any{
				"servers": map[string]any{
					"gravyflow": map[string]any{
						// HTTP only for local *.localhost routing; :443 without TLS breaks Caddy load.
						"listen": []string{":80"},
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

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		if caddyLoadSucceededDespiteClose(err) {
			return nil
		}
		// Caddy closes the admin connection after /load even on success; treat reset as soft failure.
		if !caddySyncRequired() && (isCaddyUnavailable(err) || strings.Contains(strings.ToLower(err.Error()), "connection reset")) {
			log.Printf("caddy sync: admin API error (%v) — deployment continues; verify Caddy on :2019 for *.localhost routing", err)
			return nil
		}
		return fmt.Errorf("load caddy config: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		if !caddySyncRequired() {
			log.Printf("caddy sync: load rejected (%s) — deployment continues", resp.Status)
			return nil
		}
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
