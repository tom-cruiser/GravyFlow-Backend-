package main

import (
    "os"
    "path/filepath"
    "testing"
)

// ============================================================================
// HELPER FUNCTIONS
// ============================================================================

func createPackageJSON(t *testing.T, dir string, content string) {
    t.Helper()
    if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(content), 0o644); err != nil {
        t.Fatal(err)
    }
}

func createFile(t *testing.T, dir string, name string, content string) {
    t.Helper()
    if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
        t.Fatal(err)
    }
}

func assertProjectKind(t *testing.T, dir string, expected ProjectKind) {
    t.Helper()
    kind := detectProjectKind(dir)
    if kind != expected {
        t.Fatalf("expected %v, got %v", expected, kind)
    }
}

// ============================================================================
// VITE TESTS
// ============================================================================

func TestDetectProjectKindViteWithoutStartScript(t *testing.T) {
    dir := t.TempDir()
    manifest := `{
        "scripts": { "dev": "vite", "build": "vite build", "preview": "vite preview" },
        "devDependencies": { "vite": "^5.4.2", "@vitejs/plugin-react": "^4.3.1" }
    }`
    createPackageJSON(t, dir, manifest)
    createFile(t, dir, "vite.config.ts", "export default {}")

    assertProjectKind(t, dir, projectKindVite)
}

func TestDetectProjectKindViteWithStartPreviewUsesViteBuilder(t *testing.T) {
    dir := t.TempDir()
    manifest := `{
        "scripts": { "build": "vite build", "start": "vite preview --host" },
        "devDependencies": { "vite": "^5.4.2" }
    }`
    createPackageJSON(t, dir, manifest)

    assertProjectKind(t, dir, projectKindVite)
}

func TestDetectProjectKindViteWithoutConfigFile(t *testing.T) {
    dir := t.TempDir()
    manifest := `{
        "scripts": { "build": "vite build", "start": "vite preview" },
        "devDependencies": { "vite": "^5.4.2" }
    }`
    createPackageJSON(t, dir, manifest)

    assertProjectKind(t, dir, projectKindVite)
}

func TestDetectProjectKindViteWithNoDevDependencies(t *testing.T) {
    dir := t.TempDir()
    manifest := `{
        "scripts": { "build": "vite build" },
        "dependencies": { "vite": "^5.4.2" }
    }`
    createPackageJSON(t, dir, manifest)
    createFile(t, dir, "vite.config.js", "export default {}")

    assertProjectKind(t, dir, projectKindVite)
}

func TestDetectProjectKindViteWithDifferentConfigExtensions(t *testing.T) {
    configs := []string{"vite.config.js", "vite.config.mjs", "vite.config.cjs", "vite.config.ts"}
    
    for _, config := range configs {
        t.Run(config, func(t *testing.T) {
            dir := t.TempDir()
            manifest := `{
                "scripts": { "build": "vite build" },
                "devDependencies": { "vite": "^5.4.2" }
            }`
            createPackageJSON(t, dir, manifest)
            createFile(t, dir, config, "export default {}")

            assertProjectKind(t, dir, projectKindVite)
        })
    }
}

// ============================================================================
// NEXT.JS TESTS
// ============================================================================

func TestDetectProjectKindNextJS(t *testing.T) {
    dir := t.TempDir()
    manifest := `{
        "scripts": { "dev": "next dev", "build": "next build", "start": "next start" },
        "dependencies": { "next": "^14.0.0", "react": "^18.0.0" }
    }`
    createPackageJSON(t, dir, manifest)
    createFile(t, dir, "next.config.js", "module.exports = {}")

    assertProjectKind(t, dir, projectKindNextJS)
}

func TestDetectProjectKindNextJSWithDifferentConfigExtensions(t *testing.T) {
    configs := []string{"next.config.js", "next.config.ts", "next.config.mjs", "next.config.cjs"}
    
    for _, config := range configs {
        t.Run(config, func(t *testing.T) {
            dir := t.TempDir()
            manifest := `{
                "scripts": { "dev": "next dev" },
                "dependencies": { "next": "^14.0.0" }
            }`
            createPackageJSON(t, dir, manifest)
            createFile(t, dir, config, "module.exports = {}")

            assertProjectKind(t, dir, projectKindNextJS)
        })
    }
}

// ============================================================================
// REACT TESTS
// ============================================================================

func TestDetectProjectKindReact(t *testing.T) {
    dir := t.TempDir()
    manifest := `{
        "scripts": { "start": "react-scripts start", "build": "react-scripts build" },
        "dependencies": { "react": "^18.0.0", "react-dom": "^18.0.0", "react-scripts": "^5.0.0" }
    }`
    createPackageJSON(t, dir, manifest)

    assertProjectKind(t, dir, projectKindReact)
}

// ============================================================================
// ANGULAR TESTS
// ============================================================================

func TestDetectProjectKindAngular(t *testing.T) {
    dir := t.TempDir()
    manifest := `{
        "scripts": { "ng": "ng", "start": "ng serve", "build": "ng build" },
        "dependencies": { "@angular/core": "^17.0.0" }
    }`
    createPackageJSON(t, dir, manifest)

    assertProjectKind(t, dir, projectKindAngular)
}

// ============================================================================
// PYTHON TESTS
// ============================================================================

func TestDetectProjectKindPython(t *testing.T) {
    dir := t.TempDir()
    createFile(t, dir, "requirements.txt", "flask==2.0.0")
    createFile(t, dir, "app.py", "print('hello')")

    assertProjectKind(t, dir, projectKindPython)
}

func TestDetectProjectKindPythonWithPyProject(t *testing.T) {
    dir := t.TempDir()
    pyproject := `[project]
name = "myapp"
version = "0.1.0"
dependencies = ["flask"]`
    createFile(t, dir, "pyproject.toml", pyproject)

    assertProjectKind(t, dir, projectKindPython)
}

// ============================================================================
// GO TESTS
// ============================================================================

func TestDetectProjectKindGo(t *testing.T) {
    dir := t.TempDir()
    goMod := `module myapp

go 1.21

require github.com/gin-gonic/gin v1.9.0`
    createFile(t, dir, "go.mod", goMod)

    assertProjectKind(t, dir, projectKindGo)
}

// ============================================================================
// RUST TESTS
// ============================================================================

func TestDetectProjectKindRust(t *testing.T) {
    dir := t.TempDir()
    cargoToml := `[package]
name = "myapp"
version = "0.1.0"
edition = "2021"

[dependencies]
tokio = { version = "1.0", features = ["full"] }`
    createFile(t, dir, "Cargo.toml", cargoToml)

    assertProjectKind(t, dir, projectKindRust)
}

// ============================================================================
// PRIORITY TESTS
// ============================================================================

func TestDetectProjectKindPriority_NextJSOverVite(t *testing.T) {
    dir := t.TempDir()
    manifest := `{
        "scripts": { "dev": "next dev", "build": "next build" },
        "dependencies": { "next": "^14.0.0" },
        "devDependencies": { "vite": "^5.4.2" }
    }`
    createPackageJSON(t, dir, manifest)
    createFile(t, dir, "next.config.js", "module.exports = {}")
    createFile(t, dir, "vite.config.js", "export default {}")

    assertProjectKind(t, dir, projectKindNextJS)
}

func TestDetectProjectKindPriority_ViteOverReact(t *testing.T) {
    dir := t.TempDir()
    manifest := `{
        "scripts": { "build": "vite build" },
        "dependencies": { "react": "^18.0.0" },
        "devDependencies": { "vite": "^5.4.2" }
    }`
    createPackageJSON(t, dir, manifest)
    createFile(t, dir, "vite.config.js", "export default {}")

    assertProjectKind(t, dir, projectKindVite)
}

// ============================================================================
// EDGE CASE TESTS
// ============================================================================

func TestDetectProjectKindWithNoPackageJSON(t *testing.T) {
    dir := t.TempDir()
    assertProjectKind(t, dir, projectKindUnknown)
}

func TestDetectProjectKindWithInvalidPackageJSON(t *testing.T) {
    dir := t.TempDir()
    invalidJSON := `{ "scripts": { "dev": "vite" }, "devDependencies": { "vite": } }`
    createFile(t, dir, "package.json", invalidJSON)

    assertProjectKind(t, dir, projectKindUnknown)
}

func TestDetectProjectKindUnknown(t *testing.T) {
    dir := t.TempDir()
    createFile(t, dir, "README.md", "# My Project")
    createFile(t, dir, ".gitignore", "*.log")

    assertProjectKind(t, dir, projectKindUnknown)
}

// ============================================================================
// BENCHMARK TESTS
// ============================================================================

func BenchmarkDetectProjectKindVite(b *testing.B) {
    dir := b.TempDir()
    manifest := `{
        "scripts": { "build": "vite build" },
        "devDependencies": { "vite": "^5.4.2" }
    }`
    if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(manifest), 0o644); err != nil {
        b.Fatal(err)
    }
    if err := os.WriteFile(filepath.Join(dir, "vite.config.js"), []byte("export default {}"), 0o644); err != nil {
        b.Fatal(err)
    }

    b.ResetTimer()
    for i := 0; i < b.N; i++ {
        detectProjectKind(dir)
    }
}

func BenchmarkDetectProjectKindNextJS(b *testing.B) {
    dir := b.TempDir()
    manifest := `{
        "scripts": { "dev": "next dev" },
        "dependencies": { "next": "^14.0.0" }
    }`
    if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(manifest), 0o644); err != nil {
        b.Fatal(err)
    }
    if err := os.WriteFile(filepath.Join(dir, "next.config.js"), []byte("module.exports = {}"), 0o644); err != nil {
        b.Fatal(err)
    }

    b.ResetTimer()
    for i := 0; i < b.N; i++ {
        detectProjectKind(dir)
    }
}