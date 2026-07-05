package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDetectProjectKindViteWithoutStartScript(t *testing.T) {
	dir := t.TempDir()
	manifest := `{
		"scripts": { "dev": "vite", "build": "vite build", "preview": "vite preview" },
		"devDependencies": { "vite": "^5.4.2", "@vitejs/plugin-react": "^4.3.1" }
	}`
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "vite.config.ts"), []byte("export default {}"), 0o644); err != nil {
		t.Fatal(err)
	}

	kind := detectProjectKind(dir)
	if kind != projectKindVite {
		t.Fatalf("expected projectKindVite, got %v", kind)
	}
}

func TestDetectProjectKindViteWithStartPreviewUsesViteBuilder(t *testing.T) {
	dir := t.TempDir()
	manifest := `{
		"scripts": { "build": "vite build", "start": "vite preview --host" },
		"devDependencies": { "vite": "^5.4.2" }
	}`
	if err := os.WriteFile(filepath.Join(dir, "package.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	kind := detectProjectKind(dir)
	if kind != projectKindVite {
		t.Fatalf("expected projectKindVite, got %v", kind)
	}
}
