package config

import (
	"strings"
	"testing"
	"time"
)

func clearEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"PKGD_MERE_ROOT", "PKGD_DEV_REPO_NAME", "PKGD_OUTPUT_DIR",
		"PKGD_MERE_BIN", "PKGD_SIGNING_KEY_PATH", "PKGD_PUBLISH_TOKEN",
		"PKGD_LISTEN_SOCKET", "PKGD_TEMP_DIR",
		"PKGD_MIRROR_ENDPOINT", "PKGD_MIRROR_REGION", "PKGD_MIRROR_BUCKET",
		"PKGD_MIRROR_PREFIX", "PKGD_MIRROR_ACCESS_KEY_ID",
		"PKGD_MIRROR_SECRET_ACCESS_KEY", "PKGD_MIRROR_RETRY_INTERVAL",
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

func TestFromEnvConfiguresMirror(t *testing.T) {
	clearEnv(t)
	t.Setenv("PKGD_MERE_ROOT", "/var/lib/pkgd")
	t.Setenv("PKGD_OUTPUT_DIR", "/srv/pkgs")
	t.Setenv("PKGD_PUBLISH_TOKEN", "s3cr3t")
	t.Setenv("PKGD_MIRROR_ENDPOINT", "https://s3.us-west-004.backblazeb2.com")
	t.Setenv("PKGD_MIRROR_REGION", "us-west-004")
	t.Setenv("PKGD_MIRROR_BUCKET", "mere-backup")
	t.Setenv("PKGD_MIRROR_ACCESS_KEY_ID", "key-id")
	t.Setenv("PKGD_MIRROR_SECRET_ACCESS_KEY", "application-key")
	t.Setenv("PKGD_MIRROR_RETRY_INTERVAL", "30s")

	cfg, err := FromEnv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !cfg.Mirror.Enabled {
		t.Fatal("Mirror.Enabled = false, want true")
	}
	if cfg.Mirror.Prefix != "pkgd" {
		t.Errorf("Mirror.Prefix = %q, want %q", cfg.Mirror.Prefix, "pkgd")
	}
	if cfg.Mirror.RetryInterval != 30*time.Second {
		t.Errorf("Mirror.RetryInterval = %s, want 30s", cfg.Mirror.RetryInterval)
	}
}

func TestFromEnvRejectsPartialMirrorConfig(t *testing.T) {
	clearEnv(t)
	t.Setenv("PKGD_MERE_ROOT", "/var/lib/pkgd")
	t.Setenv("PKGD_OUTPUT_DIR", "/srv/pkgs")
	t.Setenv("PKGD_PUBLISH_TOKEN", "s3cr3t")
	t.Setenv("PKGD_MIRROR_BUCKET", "mere-backup")

	_, err := FromEnv()
	if err == nil || !strings.Contains(err.Error(), "PKGD_MIRROR_ENDPOINT") {
		t.Fatalf("error = %v, want missing mirror endpoint", err)
	}
}
