package config

import (
	"strings"
	"testing"
)

func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"PKGD_MERE_ROOT", "PKGD_DEV_REPO_NAME", "PKGD_OUTPUT_DIR",
		"PKGD_MERE_BIN", "PKGD_SIGNING_KEY_PATH", "PKGD_PUBLISH_TOKEN",
		"PKGD_LISTEN_SOCKET", "PKGD_TEMP_DIR",
	} {
		t.Setenv(k, "")
	}
}

func TestFromEnvMissingRequiredFields(t *testing.T) {
	clearEnv(t)
	_, err := FromEnv()
	if err == nil {
		t.Fatal("expected error for missing required config, got nil")
	}
	for _, want := range []string{"PKGD_MERE_ROOT", "PKGD_OUTPUT_DIR", "PKGD_PUBLISH_TOKEN"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention missing %s", err.Error(), want)
		}
	}
}

func TestFromEnvDefaultsAndOverrides(t *testing.T) {
	clearEnv(t)
	t.Setenv("PKGD_MERE_ROOT", "/var/lib/pkgd")
	t.Setenv("PKGD_OUTPUT_DIR", "/srv/pkgs")
	t.Setenv("PKGD_PUBLISH_TOKEN", "s3cr3t")

	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.DevRepoName != "pkgd" {
		t.Errorf("DevRepoName default = %q, want %q", cfg.DevRepoName, "pkgd")
	}
	if cfg.DevRepoPath() != "/var/lib/pkgd/mere/dev/repo/pkgd" {
		t.Errorf("DevRepoPath() = %q, want %q", cfg.DevRepoPath(), "/var/lib/pkgd/mere/dev/repo/pkgd")
	}
	if cfg.MereBinPath != "mere" {
		t.Errorf("MereBinPath default = %q, want %q", cfg.MereBinPath, "mere")
	}
	if cfg.ListenSocket != "/run/pkgd/pkgd.sock" {
		t.Errorf("ListenSocket default = %q, want %q", cfg.ListenSocket, "/run/pkgd/pkgd.sock")
	}
	if cfg.SigningKeyPath != "" {
		t.Errorf("SigningKeyPath = %q, want empty (optional, unset)", cfg.SigningKeyPath)
	}

	t.Setenv("PKGD_MERE_BIN", "/opt/mere/bin/mere")
	cfg, err = FromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.MereBinPath != "/opt/mere/bin/mere" {
		t.Errorf("MereBinPath override = %q, want %q", cfg.MereBinPath, "/opt/mere/bin/mere")
	}
}
