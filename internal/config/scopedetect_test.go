package config

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestADefaultGlobListIsScopedToTheBuildDirectory(t *testing.T) {
	cfg, err := LoadRepoConfig(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg.Build.Dir = "./docusaurus/"
	cfg.ScopeToBuildDir()

	want := []string{"docusaurus/**", RepoConfigName}
	if !slices.Equal(cfg.Detect.Paths, want) {
		t.Fatalf("paths = %v, want %v", cfg.Detect.Paths, want)
	}
}

func TestARootBuildDirectoryKeepsTheDefaults(t *testing.T) {
	cfg, err := LoadRepoConfig(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg.ScopeToBuildDir()

	if !slices.Equal(cfg.Detect.Paths, DefaultRepoConfig().Detect.Paths) {
		t.Fatalf("paths = %v, want the defaults", cfg.Detect.Paths)
	}
}

func TestPathsTheRepositoryWroteAreNotScoped(t *testing.T) {
	dir := t.TempDir()
	body := "build:\n  dir: docusaurus\ndetect:\n  paths: [\"docusaurus/docs/**\", \"shared/**\"]\n"
	if err := os.WriteFile(filepath.Join(dir, RepoConfigName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadRepoConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ScopeToBuildDir()

	want := []string{"docusaurus/docs/**", "shared/**"}
	if !slices.Equal(cfg.Detect.Paths, want) {
		t.Fatalf("paths = %v, want %v", cfg.Detect.Paths, want)
	}
}

func TestARepositoryFileWithoutPathsIsStillScoped(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, RepoConfigName), []byte("build:\n  dir: site\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadRepoConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ScopeToBuildDir()

	want := []string{"site/**", RepoConfigName}
	if !slices.Equal(cfg.Detect.Paths, want) {
		t.Fatalf("paths = %v, want %v", cfg.Detect.Paths, want)
	}
}
