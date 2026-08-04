package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ============================================================================
// CONSTANTS & TYPES
// ============================================================================

type projectKind int

const (
	projectKindUnknown projectKind = iota
	projectKindNodeNext
	projectKindVite
	projectKindNode
	projectKindReact
	projectKindAngular
	projectKindNuxt
	projectKindSvelte
	projectKindSolid
	projectKindAstro
	projectKindRemix
)

type packageManifest struct {
	Scripts         map[string]string `json:"scripts"`
	Dependencies    map[string]string `json:"dependencies"`
	DevDependencies map[string]string `json:"devDependencies"`
	Main            string            `json:"main"`
	Type            string            `json:"type"`
	Engines         struct {
		Node string `json:"node"`
		Npm  string `json:"npm"`
		Yarn string `json:"yarn"`
		Pnpm string `json:"pnpm"`
	} `json:"engines"`
}

type BuildOptions struct {
	Platform      string
	BuildArgs     map[string]string
	Labels        map[string]string
	HealthCheck   bool
	HealthPath    string
	Target        string
	NoCache       bool
	Push          bool
	Registry      string
	Timeout       time.Duration
}

type BuildResult struct {
	ImageTag    string
	ImageID     string
	Size        int64
	BuildTime   time.Duration
	Layers      int
	CacheHit    bool
	Error       error
}

// ============================================================================
// PROJECT DETECTION
// ============================================================================

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

	// Check for Next.js (highest priority)
	if hasNextJS(appPath, manifest) {
		return projectKindNodeNext
	}

	// Check for Nuxt
	if hasNuxt(appPath, manifest) {
		return projectKindNuxt
	}

	// Check for Astro
	if hasAstro(appPath, manifest) {
		return projectKindAstro
	}

	// Check for Remix
	if hasRemix(appPath, manifest) {
		return projectKindRemix
	}

	// Check for Angular
	if hasAngular(appPath, manifest) {
		return projectKindAngular
	}

	// Check for Vite
	if hasVite(appPath, manifest) && hasBuildScript(manifest) {
		return projectKindVite
	}

	// Check for React
	if hasReact(appPath, manifest) {
		return projectKindReact
	}

	// Check for Svelte
	if hasSvelte(appPath, manifest) {
		return projectKindSvelte
	}

	// Check for Solid
	if hasSolid(appPath, manifest) {
		return projectKindSolid
	}

	// Check for generic Node.js
	if isNodeProject(appPath, manifest) {
		return projectKindNode
	}

	return projectKindUnknown
}

func hasBuildScript(manifest packageManifest) bool {
	_, ok := manifest.Scripts["build"]
	return ok
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

func hasNuxt(appPath string, manifest packageManifest) bool {
	if _, ok := manifest.Dependencies["nuxt"]; ok {
		return true
	}
	if _, ok := manifest.DevDependencies["nuxt"]; ok {
		return true
	}
	matches, _ := filepath.Glob(filepath.Join(appPath, "nuxt.config.*"))
	return len(matches) > 0
}

func hasAstro(appPath string, manifest packageManifest) bool {
	if _, ok := manifest.Dependencies["astro"]; ok {
		return true
	}
	if _, ok := manifest.DevDependencies["astro"]; ok {
		return true
	}
	matches, _ := filepath.Glob(filepath.Join(appPath, "astro.config.*"))
	return len(matches) > 0
}

func hasRemix(appPath string, manifest packageManifest) bool {
	if _, ok := manifest.Dependencies["@remix-run/react"]; ok {
		return true
	}
	if _, ok := manifest.DevDependencies["@remix-run/dev"]; ok {
		return true
	}
	matches, _ := filepath.Glob(filepath.Join(appPath, "remix.config.*"))
	return len(matches) > 0
}

func hasAngular(appPath string, manifest packageManifest) bool {
	if _, ok := manifest.Dependencies["@angular/core"]; ok {
		return true
	}
	if _, ok := manifest.DevDependencies["@angular/cli"]; ok {
		return true
	}
	if _, err := os.Stat(filepath.Join(appPath, "angular.json")); err == nil {
		return true
	}
	return false
}

func hasReact(appPath string, manifest packageManifest) bool {
	if _, ok := manifest.Dependencies["react"]; ok {
		if _, ok := manifest.Dependencies["react-dom"]; ok {
			return true
		}
	}
	if _, ok := manifest.Dependencies["react-scripts"]; ok {
		return true
	}
	return false
}

func hasSvelte(appPath string, manifest packageManifest) bool {
	if _, ok := manifest.Dependencies["svelte"]; ok {
		return true
	}
	if _, ok := manifest.DevDependencies["svelte"]; ok {
		return true
	}
	matches, _ := filepath.Glob(filepath.Join(appPath, "svelte.config.*"))
	return len(matches) > 0
}

func hasSolid(appPath string, manifest packageManifest) bool {
	if _, ok := manifest.Dependencies["solid-js"]; ok {
		return true
	}
	if _, ok := manifest.DevDependencies["solid-js"]; ok {
		return true
	}
	matches, _ := filepath.Glob(filepath.Join(appPath, "vite.config.*"))
	return len(matches) > 0 && strings.Contains(strings.ToLower(filepath.Base(appPath)), "solid")
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

func isNodeProject(appPath string, manifest packageManifest) bool {
	// Check for start or serve scripts
	for _, scriptName := range []string{"start", "serve"} {
		if _, ok := manifest.Scripts[scriptName]; ok {
			return true
		}
	}
	// Check for main field
	if strings.TrimSpace(manifest.Main) != "" {
		return true
	}
	// Check for index.js
	if _, err := os.Stat(filepath.Join(appPath, "index.js")); err == nil {
		return true
	}
	if _, err := os.Stat(filepath.Join(appPath, "server.js")); err == nil {
		return true
	}
	if _, err := os.Stat(filepath.Join(appPath, "app.js")); err == nil {
		return true
	}
	return false
}

// ============================================================================
// PACKAGE MANAGER DETECTION
// ============================================================================

func packageManager(appPath string) string {
	if _, err := os.Stat(filepath.Join(appPath, "pnpm-lock.yaml")); err == nil {
		return "pnpm"
	}
	if _, err := os.Stat(filepath.Join(appPath, "yarn.lock")); err == nil {
		return "yarn"
	}
	if _, err := os.Stat(filepath.Join(appPath, "bun.lockb")); err == nil {
		return "bun"
	}
	return "npm"
}

func installCommand(pm string) string {
	switch pm {
	case "pnpm":
		return "corepack enable && pnpm install --frozen-lockfile --prefer-offline"
	case "yarn":
		return "yarn install --frozen-lockfile --prefer-offline"
	case "bun":
		return "bun install --frozen-lockfile"
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
	case "bun":
		return "bun run build"
	default:
		return "npm run build"
	}
}

func startCommand(pm string) string {
	switch pm {
	case "pnpm":
		return "pnpm start"
	case "yarn":
		return "yarn start"
	case "bun":
		return "bun run start"
	default:
		return "npm start"
	}
}

// ============================================================================
// PROJECT KIND LABELS
// ============================================================================

func projectKindLabel(kind projectKind) string {
	switch kind {
	case projectKindNodeNext:
		return "nextjs"
	case projectKindVite:
		return "vite"
	case projectKindNode:
		return "node"
	case projectKindReact:
		return "react"
	case projectKindAngular:
		return "angular"
	case projectKindNuxt:
		return "nuxt"
	case projectKindSvelte:
		return "svelte"
	case projectKindSolid:
		return "solid"
	case projectKindAstro:
		return "astro"
	case projectKindRemix:
		return "remix"
	default:
		return "unknown"
	}
}

// ============================================================================
// MAIN BUILD FUNCTION
// ============================================================================

func buildNodeDockerImage(appPath string, appName string, kind projectKind) error {
	return buildNodeDockerImageWithOptions(appPath, appName, kind, BuildOptions{})
}

func buildNodeDockerImageWithOptions(appPath string, appName string, kind projectKind, opts BuildOptions) error {
	pm := packageManager(appPath)
	installCmd := installCommand(pm)
	buildCmd := buildCommand(pm)

	// Detect node version from engines
	nodeVersion := detectNodeVersion(appPath)

	var dockerfile string
	var dockerfilePath string
	var err error

	switch kind {
	case projectKindNodeNext:
		dockerfile = nextJSDockerfile(appPath, installCmd, pm, nodeVersion)
	case projectKindNuxt:
		dockerfile = nuxtDockerfile(appPath, installCmd, pm, nodeVersion)
	case projectKindAstro:
		dockerfile = astroDockerfile(installCmd, buildCmd, pm, nodeVersion)
	case projectKindRemix:
		dockerfile = remixDockerfile(appPath, installCmd, pm, nodeVersion)
	case projectKindAngular:
		dockerfile = angularDockerfile(installCmd, buildCmd, pm, nodeVersion)
	case projectKindVite:
		dockerfile = viteDockerfile(installCmd, buildCmd, pm, nodeVersion)
	case projectKindReact:
		dockerfile = reactDockerfile(installCmd, buildCmd, pm, nodeVersion)
	case projectKindSvelte:
		dockerfile = svelteDockerfile(installCmd, buildCmd, pm, nodeVersion)
	case projectKindSolid:
		dockerfile = solidDockerfile(installCmd, buildCmd, pm, nodeVersion)
	case projectKindNode:
		startCmd := nodeStartCommand(appPath, pm)
		dockerfile = genericNodeDockerfile(installCmd, startCmd, pm, nodeVersion)
	default:
		return fmt.Errorf("unsupported node project kind: %v", kind)
	}

	// Create Dockerfile
	dockerfilePath = filepath.Join(os.TempDir(), fmt.Sprintf("gravyflow-%s.Dockerfile", sanitizeFileToken(appName)))
	if err := os.WriteFile(dockerfilePath, []byte(dockerfile), 0o644); err != nil {
		return fmt.Errorf("write dockerfile: %w", err)
	}
	defer os.Remove(dockerfilePath)

	// Add health check if requested
	if opts.HealthCheck && opts.HealthPath != "" {
		if err := addHealthCheckToDockerfile(dockerfilePath, opts.HealthPath); err != nil {
			return err
		}
	}

	// Run build
	return runDockerBuildWithOptions(appPath, appName, dockerfilePath, opts)
}

// ============================================================================
// NODE VERSION DETECTION
// ============================================================================

func detectNodeVersion(appPath string) string {
	manifestPath := filepath.Join(appPath, "package.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		return "20-alpine"
	}

	var manifest packageManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return "20-alpine"
	}

	// Check engines field
	if manifest.Engines.Node != "" {
		version := strings.TrimPrefix(manifest.Engines.Node, "^")
		version = strings.TrimPrefix(version, "~")
		version = strings.TrimPrefix(version, ">=")
		version = strings.Split(version, ".")[0]
		if version != "" {
			return version + "-alpine"
		}
	}

	// Check for .nvmrc
	if nvmrc, err := os.ReadFile(filepath.Join(appPath, ".nvmrc")); err == nil {
		version := strings.TrimSpace(string(nvmrc))
		version = strings.TrimPrefix(version, "v")
		version = strings.Split(version, ".")[0]
		if version != "" {
			return version + "-alpine"
		}
	}

	// Default to Node 20
	return "20-alpine"
}

// ============================================================================
// DOCKERFILE GENERATORS
// ============================================================================

func genericNodeDockerfile(installCmd string, startCmd string, pm string, nodeVersion string) string {
	return fmt.Sprintf(`FROM node:%s AS deps
WORKDIR /app
COPY package.json package-lock.json* pnpm-lock.yaml* yarn.lock* bun.lockb* ./
RUN %s

FROM node:%s
WORKDIR /app
COPY --from=deps /app/node_modules ./node_modules
COPY . .
ENV NODE_ENV=production
ENV PORT=8080
EXPOSE 8080
CMD %s
`, nodeVersion, installCmd, nodeVersion, startCmd)
}

func nextJSDockerfile(appPath string, installCmd string, pm string, nodeVersion string) string {
	startCmd := fmt.Sprintf(`["%s", "start", "--", "-p", "8080"]`, pm)
	if pm == "npm" {
		startCmd = `["npm", "start", "--", "-p", "8080"]`
	}

	publicCopy := ""
	if _, err := os.Stat(filepath.Join(appPath, "public")); err == nil {
		publicCopy = "COPY --from=builder /app/public ./public\n"
	}

	return fmt.Sprintf(`FROM node:%s AS deps
WORKDIR /app
COPY package.json package-lock.json* pnpm-lock.yaml* yarn.lock* bun.lockb* ./
RUN %s

FROM node:%s AS builder
WORKDIR /app
COPY --from=deps /app/node_modules ./node_modules
COPY . .
ENV NEXT_TELEMETRY_DISABLED=1
RUN npm run build

FROM node:%s AS runner
WORKDIR /app
ENV NODE_ENV=production
ENV NEXT_TELEMETRY_DISABLED=1
ENV PORT=8080
ENV HOSTNAME=0.0.0.0
%sCOPY --from=builder /app/.next ./.next
COPY --from=builder /app/node_modules ./node_modules
COPY --from=builder /app/package.json ./package.json
COPY --from=builder /app/next.config.js ./next.config.js 2>/dev/null || true
EXPOSE 8080
CMD %s
`, nodeVersion, installCmd, nodeVersion, nodeVersion, publicCopy, startCmd)
}

func viteDockerfile(installCmd string, buildCmd string, pm string, nodeVersion string) string {
	return fmt.Sprintf(`FROM node:%s AS deps
WORKDIR /app
COPY package.json package-lock.json* pnpm-lock.yaml* yarn.lock* bun.lockb* ./
RUN %s

FROM node:%s AS builder
WORKDIR /app
COPY --from=deps /app/node_modules ./node_modules
COPY . .
RUN %s

FROM nginx:1.27-alpine AS runner
RUN printf 'server { listen 8080; listen [::]:8080; root /usr/share/nginx/html; index index.html; location / { try_files $uri $uri/ /index.html; } }\n' > /etc/nginx/conf.d/default.conf
COPY --from=builder /app/dist /usr/share/nginx/html
EXPOSE 8080
CMD ["nginx", "-g", "daemon off;"]
`, nodeVersion, installCmd, nodeVersion, buildCmd)
}

func reactDockerfile(installCmd string, buildCmd string, pm string, nodeVersion string) string {
	return fmt.Sprintf(`FROM node:%s AS deps
WORKDIR /app
COPY package.json package-lock.json* pnpm-lock.yaml* yarn.lock* bun.lockb* ./
RUN %s

FROM node:%s AS builder
WORKDIR /app
COPY --from=deps /app/node_modules ./node_modules
COPY . .
RUN %s

FROM nginx:1.27-alpine AS runner
RUN printf 'server { listen 8080; listen [::]:8080; root /usr/share/nginx/html; index index.html; location / { try_files $uri $uri/ /index.html; } }\n' > /etc/nginx/conf.d/default.conf
COPY --from=builder /app/build /usr/share/nginx/html
EXPOSE 8080
CMD ["nginx", "-g", "daemon off;"]
`, nodeVersion, installCmd, nodeVersion, buildCmd)
}

func angularDockerfile(installCmd string, buildCmd string, pm string, nodeVersion string) string {
	return fmt.Sprintf(`FROM node:%s AS deps
WORKDIR /app
COPY package.json package-lock.json* pnpm-lock.yaml* yarn.lock* bun.lockb* ./
RUN %s

FROM node:%s AS builder
WORKDIR /app
COPY --from=deps /app/node_modules ./node_modules
COPY . .
RUN %s

FROM nginx:1.27-alpine AS runner
RUN printf 'server { listen 8080; listen [::]:8080; root /usr/share/nginx/html; index index.html; location / { try_files $uri $uri/ /index.html; } }\n' > /etc/nginx/conf.d/default.conf
COPY --from=builder /app/dist/* /usr/share/nginx/html
EXPOSE 8080
CMD ["nginx", "-g", "daemon off;"]
`, nodeVersion, installCmd, nodeVersion, buildCmd)
}

func nuxtDockerfile(appPath string, installCmd string, pm string, nodeVersion string) string {
	startCmd := fmt.Sprintf(`["%s", "start"]`, pm)
	if pm == "npm" {
		startCmd = `["npm", "start"]`
	}

	return fmt.Sprintf(`FROM node:%s AS deps
WORKDIR /app
COPY package.json package-lock.json* pnpm-lock.yaml* yarn.lock* bun.lockb* ./
RUN %s

FROM node:%s AS builder
WORKDIR /app
COPY --from=deps /app/node_modules ./node_modules
COPY . .
RUN npm run build

FROM node:%s AS runner
WORKDIR /app
ENV NODE_ENV=production
ENV PORT=8080
COPY --from=builder /app/.output ./.output
COPY --from=builder /app/node_modules ./node_modules
COPY --from=builder /app/package.json ./package.json
EXPOSE 8080
CMD %s
`, nodeVersion, installCmd, nodeVersion, nodeVersion, startCmd)
}

func astroDockerfile(installCmd string, buildCmd string, pm string, nodeVersion string) string {
	return fmt.Sprintf(`FROM node:%s AS deps
WORKDIR /app
COPY package.json package-lock.json* pnpm-lock.yaml* yarn.lock* bun.lockb* ./
RUN %s

FROM node:%s AS builder
WORKDIR /app
COPY --from=deps /app/node_modules ./node_modules
COPY . .
RUN %s

FROM node:%s AS runner
WORKDIR /app
ENV NODE_ENV=production
ENV PORT=8080
COPY --from=builder /app/dist ./dist
COPY --from=builder /app/package.json ./package.json
EXPOSE 8080
CMD ["node", "./dist/server/entry.mjs"]
`, nodeVersion, installCmd, nodeVersion, nodeVersion)
}

func remixDockerfile(appPath string, installCmd string, pm string, nodeVersion string) string {
	return fmt.Sprintf(`FROM node:%s AS deps
WORKDIR /app
COPY package.json package-lock.json* pnpm-lock.yaml* yarn.lock* bun.lockb* ./
RUN %s

FROM node:%s AS builder
WORKDIR /app
COPY --from=deps /app/node_modules ./node_modules
COPY . .
RUN npm run build

FROM node:%s AS runner
WORKDIR /app
ENV NODE_ENV=production
ENV PORT=8080
COPY --from=builder /app/build ./build
COPY --from=builder /app/node_modules ./node_modules
COPY --from=builder /app/package.json ./package.json
COPY --from=builder /app/public ./public
EXPOSE 8080
CMD ["npm", "start"]
`, nodeVersion, installCmd, nodeVersion, nodeVersion)
}

func svelteDockerfile(installCmd string, buildCmd string, pm string, nodeVersion string) string {
	return fmt.Sprintf(`FROM node:%s AS deps
WORKDIR /app
COPY package.json package-lock.json* pnpm-lock.yaml* yarn.lock* bun.lockb* ./
RUN %s

FROM node:%s AS builder
WORKDIR /app
COPY --from=deps /app/node_modules ./node_modules
COPY . .
RUN %s

FROM nginx:1.27-alpine AS runner
RUN printf 'server { listen 8080; listen [::]:8080; root /usr/share/nginx/html; index index.html; location / { try_files $uri $uri/ /index.html; } }\n' > /etc/nginx/conf.d/default.conf
COPY --from=builder /app/dist /usr/share/nginx/html
EXPOSE 8080
CMD ["nginx", "-g", "daemon off;"]
`, nodeVersion, installCmd, nodeVersion, buildCmd)
}

func solidDockerfile(installCmd string, buildCmd string, pm string, nodeVersion string) string {
	return fmt.Sprintf(`FROM node:%s AS deps
WORKDIR /app
COPY package.json package-lock.json* pnpm-lock.yaml* yarn.lock* bun.lockb* ./
RUN %s

FROM node:%s AS builder
WORKDIR /app
COPY --from=deps /app/node_modules ./node_modules
COPY . .
RUN %s

FROM nginx:1.27-alpine AS runner
RUN printf 'server { listen 8080; listen [::]:8080; root /usr/share/nginx/html; index index.html; location / { try_files $uri $uri/ /index.html; } }\n' > /etc/nginx/conf.d/default.conf
COPY --from=builder /app/dist /usr/share/nginx/html
EXPOSE 8080
CMD ["nginx", "-g", "daemon off;"]
`, nodeVersion, installCmd, nodeVersion, buildCmd)
}

// ============================================================================
// NODE START COMMAND
// ============================================================================

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
		case "bun":
			return `["bun", "start"]`
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
		case "bun":
			return `["bun", "run", "serve"]`
		default:
			return `["npm", "run", "serve"]`
		}
	}

	if main := strings.TrimSpace(manifest.Main); main != "" {
		return fmt.Sprintf(`["node", %q]`, main)
	}

	// Check for common entry files
	entryFiles := []string{"index.js", "server.js", "app.js"}
	for _, file := range entryFiles {
		if _, err := os.Stat(filepath.Join(appPath, file)); err == nil {
			return fmt.Sprintf(`["node", %q]`, file)
		}
	}

	return `["npm", "start"]`
}

// ============================================================================
// HEALTH CHECK
// ============================================================================

func addHealthCheckToDockerfile(dockerfilePath string, healthPath string) error {
	content, err := os.ReadFile(dockerfilePath)
	if err != nil {
		return err
	}

	healthCheck := fmt.Sprintf(`HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD curl -f http://localhost:8080%s || exit 1
`, healthPath)

	// Insert before EXPOSE or CMD
	newContent := strings.Replace(string(content), "EXPOSE 8080", healthCheck+"EXPOSE 8080", 1)
	if newContent == string(content) {
		// If no EXPOSE, add before CMD
		newContent = strings.Replace(string(content), "CMD", healthCheck+"CMD", 1)
	}

	return os.WriteFile(dockerfilePath, []byte(newContent), 0o644)
}

// ============================================================================
// DOCKER BUILD
// ============================================================================

func runDockerBuild(appPath string, appName string, dockerfilePath string) error {
	return runDockerBuildWithOptions(appPath, appName, dockerfilePath, BuildOptions{})
}

func runDockerBuildWithOptions(appPath string, appName string, dockerfilePath string, opts BuildOptions) error {
	if _, err := exec.LookPath("docker"); err != nil {
		return fmt.Errorf("docker CLI not found in PATH: %w", err)
	}

	// Build args
	args := []string{
		"build",
		"-t", appName,
		"-f", dockerfilePath,
	}

	// Add platform
	if opts.Platform != "" {
		args = append(args, "--platform", opts.Platform)
	}

	// Add build args
	for key, value := range opts.BuildArgs {
		args = append(args, "--build-arg", fmt.Sprintf("%s=%s", key, value))
	}

	// Add labels
	for key, value := range opts.Labels {
		args = append(args, "--label", fmt.Sprintf("%s=%s", key, value))
	}

	// Add target
	if opts.Target != "" {
		args = append(args, "--target", opts.Target)
	}

	// Add no-cache
	if opts.NoCache {
		args = append(args, "--no-cache")
	}

	// Add cache-from if image exists
	if _, err := exec.Command("docker", "image", "inspect", appName).CombinedOutput(); err == nil {
		args = append(args, "--cache-from", appName)
	}

	// Add path
	args = append(args, appPath)

	// Set timeout
	ctx := context.Background()
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	// Run build with retry
	return runDockerBuildWithRetry(ctx, args, 3)
}

func runDockerBuildWithRetry(ctx context.Context, args []string, maxRetries int) error {
	var lastErr error

	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			log.Printf("Build attempt %d failed, retrying...", attempt)
			time.Sleep(2 * time.Second)
		}

		err := runDockerBuildCmd(ctx, args)
		if err == nil {
			return nil
		}

		lastErr = err

		// Don't retry on certain errors
		if isFatalBuildError(err) {
			return err
		}
	}

	return fmt.Errorf("build failed after %d attempts: %w", maxRetries, lastErr)
}

func runDockerBuildCmd(ctx context.Context, args []string) error {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = io.MultiWriter(os.Stderr, &stderr)
	cmd.Env = dockerCommandEnv()

	log.Printf("Running: docker %s", strings.Join(args, " "))

	if err := cmd.Run(); err != nil {
		if trimmed := strings.TrimSpace(stderr.String()); trimmed != "" {
			err = fmt.Errorf("%w: %s", err, trimmed)
		}
		if isBuildKitMissingError(err) {
			log.Printf("BuildKit unavailable, retrying with legacy builder")
			return runDockerBuildCmd(ctx, append([]string{"build", "--disable-buildkit"}, args[1:]...))
		}
		return err
	}

	return nil
}

func isFatalBuildError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	fatalMarkers := []string{
		"no such file",
		"permission denied",
		"invalid reference",
		"dockerfile parse error",
	}
	for _, marker := range fatalMarkers {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}

// ============================================================================
// UTILITY FUNCTIONS
// ============================================================================

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

func dockerImageTag(appName string) string {
	appName = strings.ToLower(strings.TrimSpace(appName))
	if appName == "" {
		return "gravyflow-app"
	}
	var b strings.Builder
	for _, r := range appName {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' || r == '/' {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	tag := strings.Trim(b.String(), "-./")
	if tag == "" {
		return "gravyflow-app"
	}
	return tag
}

// ============================================================================
// ENVIRONMENT FUNCTIONS (Stubs - Implement based on your needs)
// ============================================================================

func dockerCommandEnv() []string {
	return dockerCommandEnvWithBuildKit(true)
}

func dockerCommandEnvForceLegacyBuilder() []string {
	return dockerCommandEnvWithBuildKit(false)
}

func dockerCommandEnvWithBuildKit(enabled bool) []string {
	env := os.Environ()
	env = withEnvVar(env, "DOCKER_BUILDKIT", "1")
	if !enabled {
		env = withEnvVar(env, "DOCKER_BUILDKIT", "0")
	}
	return env
}

func withEnvVar(env []string, key, value string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			continue
		}
		out = append(out, entry)
	}
	out = append(out, prefix+value)
	return out
}

func isBuildKitMissingError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "buildkit") &&
		(strings.Contains(s, "buildx") || strings.Contains(s, "buildkit is enabled"))
}

// ============================================================================
// USAGE EXAMPLES
// ============================================================================

/*
EXAMPLE USAGE:

1. Basic build:
   err := buildNodeDockerImage("/path/to/app", "my-app", detectProjectKind("/path/to/app"))

2. Build with options:
   opts := BuildOptions{
       Platform: "linux/amd64",
       BuildArgs: map[string]string{"VERSION": "1.0.0"},
       Labels: map[string]string{"maintainer": "team@example.com"},
       HealthCheck: true,
       HealthPath: "/health",
       NoCache: false,
       Timeout: 10 * time.Minute,
   }
   err := buildNodeDockerImageWithOptions("/path/to/app", "my-app", projectKindNodeNext, opts)

3. Detect project kind:
   kind := detectProjectKind("/path/to/app")
   label := projectKindLabel(kind)
*/