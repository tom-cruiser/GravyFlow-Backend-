package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type projectKind int

const (
	projectKindUnknown projectKind = iota
	projectKindNodeNext
	projectKindVite
	projectKindNode
)

type packageManifest struct {
	Scripts         map[string]string `json:"scripts"`
	Dependencies    map[string]string `json:"dependencies"`
	DevDependencies map[string]string `json:"devDependencies"`
	Main            string            `json:"main"`
}

func projectKindLabel(kind projectKind) string {
	switch kind {
	case projectKindNodeNext:
		return "nextjs"
	case projectKindVite:
		return "vite"
	case projectKindNode:
		return "node"
	default:
		return "unknown"
	}
}

func detectProjectKind(appPath string) projectKind {
	manifestPath := filepath.Join(appPath, "package.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return projectKindUnknown
	}

	var manifest packageManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return projectKindUnknown
	}

	if hasNextJS(appPath, manifest) {
		return projectKindNodeNext
	}

	if hasVite(appPath, manifest) && hasBuildScript(manifest) {
		return projectKindVite
	}

	for _, scriptName := range []string{"start", "serve"} {
		if _, ok := manifest.Scripts[scriptName]; ok {
			return projectKindNode
		}
	}

	if strings.TrimSpace(manifest.Main) != "" {
		return projectKindNode
	}

	return projectKindUnknown
}

func hasBuildScript(manifest packageManifest) bool {
	_, ok := manifest.Scripts["build"]
	return ok
}

func hasVite(appPath string, manifest packageManifest) bool {
	if _, ok := manifest.Dependencies["vite"]; ok {
		return true
	}
	if _, ok := manifest.DevDependencies["vite"]; ok {
		return true
	}
	matches, _ := filepath.Glob(filepath.Join(appPath, "vite.config.*"))
	return len(matches) > 0
}

func hasNextJS(appPath string, manifest packageManifest) bool {
	if _, ok := manifest.Dependencies["next"]; ok {
		return true
	}
	if _, ok := manifest.DevDependencies["next"]; ok {
		return true
	}

	matches, _ := filepath.Glob(filepath.Join(appPath, "next.config.*"))
	return len(matches) > 0
}

func packageManager(appPath string) string {
	if _, err := os.Stat(filepath.Join(appPath, "pnpm-lock.yaml")); err == nil {
		return "pnpm"
	}
	if _, err := os.Stat(filepath.Join(appPath, "yarn.lock")); err == nil {
		return "yarn"
	}
	return "npm"
}

func installCommand(pm string) string {
	switch pm {
	case "pnpm":
		return "corepack enable && pnpm install --frozen-lockfile --prefer-offline"
	case "yarn":
		return "yarn install --frozen-lockfile --prefer-offline"
	default:
		return "npm ci --prefer-offline --no-audit --no-fund"
	}
}

func buildCommand(pm string) string {
	switch pm {
	case "pnpm":
		return "pnpm run build"
	case "yarn":
		return "yarn build"
	default:
		return "npm run build"
	}
}

func buildNodeDockerImage(appPath string, appName string, kind projectKind) error {
	pm := packageManager(appPath)
	installCmd := installCommand(pm)

	var dockerfile string
	switch kind {
	case projectKindNodeNext:
		dockerfile = nextJSDockerfile(appPath, installCmd, pm)
	case projectKindVite:
		dockerfile = viteDockerfile(installCmd, buildCommand(pm))
	case projectKindNode:
		dockerfile = genericNodeDockerfile(installCmd, nodeStartCommand(appPath, pm))
	default:
		return fmt.Errorf("unsupported node project kind")
	}

	dockerfilePath := filepath.Join(os.TempDir(), fmt.Sprintf("gravyflow-%s.Dockerfile", sanitizeFileToken(appName)))
	if err := os.WriteFile(dockerfilePath, []byte(dockerfile), 0o644); err != nil {
		return fmt.Errorf("write dockerfile: %w", err)
	}
	defer os.Remove(dockerfilePath)

	return runDockerBuild(appPath, dockerImageTag(appName), dockerfilePath)
}

func nextJSDockerfile(appPath string, installCmd string, pm string) string {
	startCmd := `["npm", "start", "--", "-p", "8080"]`
	if pm == "pnpm" {
		startCmd = `["pnpm", "start", "--", "-p", "8080"]`
	} else if pm == "yarn" {
		startCmd = `["yarn", "start", "-p", "8080"]`
	}

	_ = appPath

	publicCopy := ""
	if _, err := os.Stat(filepath.Join(appPath, "public")); err == nil {
		publicCopy = "COPY --from=builder /app/public ./public\n"
	}

	return fmt.Sprintf(`FROM node:20-alpine AS deps
WORKDIR /app
COPY package.json package-lock.json* pnpm-lock.yaml* yarn.lock* ./
RUN %s

FROM node:20-alpine AS builder
WORKDIR /app
COPY --from=deps /app/node_modules ./node_modules
COPY . .
ENV NEXT_TELEMETRY_DISABLED=1
RUN mkdir -p public && npm run build

FROM node:20-alpine AS runner
WORKDIR /app
ENV NODE_ENV=production
ENV NEXT_TELEMETRY_DISABLED=1
ENV PORT=8080
ENV HOSTNAME=0.0.0.0
%sCOPY --from=builder /app/.next ./.next
COPY --from=builder /app/node_modules ./node_modules
COPY --from=builder /app/package.json ./package.json
EXPOSE 8080
CMD %s
`, installCmd, publicCopy, startCmd)
}

func viteDockerfile(installCmd string, buildCmd string) string {
	return fmt.Sprintf(`FROM node:20-alpine AS deps
WORKDIR /app
COPY package.json package-lock.json* pnpm-lock.yaml* yarn.lock* ./
RUN %s

FROM node:20-alpine AS builder
WORKDIR /app
COPY --from=deps /app/node_modules ./node_modules
COPY . .
RUN %s

FROM nginx:1.27-alpine AS runner
RUN printf 'server { listen 8080; listen [::]:8080; root /usr/share/nginx/html; index index.html; location / { try_files $uri $uri/ /index.html; } }\n' > /etc/nginx/conf.d/default.conf
COPY --from=builder /app/dist /usr/share/nginx/html
EXPOSE 8080
CMD ["nginx", "-g", "daemon off;"]
`, installCmd, buildCmd)
}

func nodeStartCommand(appPath string, pm string) string {
	manifestPath := filepath.Join(appPath, "package.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return `["npm", "start"]`
	}

	var manifest packageManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return `["npm", "start"]`
	}

	if _, ok := manifest.Scripts["start"]; ok {
		switch pm {
		case "pnpm":
			return `["pnpm", "start"]`
		case "yarn":
			return `["yarn", "start"]`
		default:
			return `["npm", "start"]`
		}
	}

	if _, ok := manifest.Scripts["serve"]; ok {
		switch pm {
		case "pnpm":
			return `["pnpm", "run", "serve"]`
		case "yarn":
			return `["yarn", "serve"]`
		default:
			return `["npm", "run", "serve"]`
		}
	}

	if main := strings.TrimSpace(manifest.Main); main != "" {
		return fmt.Sprintf(`["node", %q]`, main)
	}

	return `["npm", "start"]`
}

func genericNodeDockerfile(installCmd string, startCmd string) string {
	return fmt.Sprintf(`FROM node:20-alpine
WORKDIR /app
COPY package.json package-lock.json* pnpm-lock.yaml* yarn.lock* ./
RUN %s
COPY . .
ENV NODE_ENV=production
ENV PORT=8080
EXPOSE 8080
CMD %s
`, installCmd, startCmd)
}

func runDockerBuild(appPath string, appName string, dockerfilePath string) error {
	if _, err := exec.LookPath("docker"); err != nil {
		return fmt.Errorf("docker CLI not found in PATH: %w", err)
	}

	args := []string{
		"build",
		"--pull=false",
		"-t", appName,
		"-f", dockerfilePath,
		appPath,
	}

	// Reuse previous image layers when rebuilding the same app name.
	if _, err := exec.Command("docker", "image", "inspect", appName).CombinedOutput(); err == nil {
		args = append(args, "--cache-from", appName)
	}

	return runDockerBuildWithEnv(args, dockerCommandEnv(), true)
}

func runDockerBuildWithEnv(args []string, env []string, allowBuildKitRetry bool) error {
	var stderr bytes.Buffer
	cmd := exec.Command("docker", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = io.MultiWriter(os.Stderr, &stderr)
	cmd.Env = env

	if err := cmd.Run(); err != nil {
		if trimmed := strings.TrimSpace(stderr.String()); trimmed != "" {
			err = fmt.Errorf("%w: %s", err, trimmed)
		}
		if allowBuildKitRetry && isBuildKitMissingError(err) {
			log.Printf("docker build: BuildKit/buildx unavailable, retrying with legacy docker builder")
			retryErr := runDockerBuildWithEnv(args, dockerCommandEnvForceLegacyBuilder(), false)
			if retryErr != nil {
				return fmt.Errorf("%w (install docker-buildx or set GRAVYFLOW_DISABLE_BUILDKIT=1)", retryErr)
			}
			return nil
		}
		return err
	}

	return nil
}

func sanitizeFileToken(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "app"
	}
	var b strings.Builder
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	return b.String()
}
