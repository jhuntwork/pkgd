// Package config defines pkgd's runtime configuration and how it is loaded
// from the environment.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// MirrorConfig configures the optional S3-compatible backup mirror. Backblaze
// B2 application keys map to AccessKeyID and SecretAccessKey for the B2 S3
// endpoint.
type MirrorConfig struct {
	Enabled         bool
	Endpoint        string
	Region          string
	Bucket          string
	Prefix          string
	AccessKeyID     string
	SecretAccessKey string
	RetryInterval   time.Duration
}

// Validate checks the mirror settings when the mirror is enabled.
func (m MirrorConfig) Validate() error {
	if !m.Enabled {
		return nil
	}

	var missing []string
	if m.Endpoint == "" {
		missing = append(missing, "PKGD_MIRROR_ENDPOINT")
	}
	if m.Region == "" {
		missing = append(missing, "PKGD_MIRROR_REGION")
	}
	if m.Bucket == "" {
		missing = append(missing, "PKGD_MIRROR_BUCKET")
	}
	if m.AccessKeyID == "" {
		missing = append(missing, "PKGD_MIRROR_ACCESS_KEY_ID")
	}
	if m.SecretAccessKey == "" {
		missing = append(missing, "PKGD_MIRROR_SECRET_ACCESS_KEY")
	}
	if len(missing) > 0 {
		return fmt.Errorf("config: missing mirror setting(s): %s", strings.Join(missing, ", "))
	}
	if m.RetryInterval <= 0 {
		return fmt.Errorf("config: PKGD_MIRROR_RETRY_INTERVAL must be greater than zero")
	}
	return nil
}

// Config holds everything pkgd needs to serve the package repository and
// accept new publishes. All paths are expected to already exist (or be
// creatable) on the local filesystem of the VPS pkgd runs on.
type Config struct {
	// MereRoot is the `--root` prefix passed to every `mere` invocation.
	// `mere dev import` only auto-bootstraps a dev repo that doesn't exist
	// yet when addressed by name under `<root>/mere/dev/repo/<name>/` —
	// passing an arbitrary absolute path to a not-yet-existing directory
	// fails instead of creating it. So pkgd never invents that path itself;
	// it always drives dev-repo creation through `--root` + DevRepoName and
	// lets `mere` own the resolution and bootstrap. See DevRepoPath().
	MereRoot string

	// DevRepoName is the bare name of the server-side dev repo that pkgd
	// uses as a staging area during each publish cycle (resolved by `mere`
	// to `<MereRoot>/mere/dev/repo/<DevRepoName>/`). On each publish,
	// `mere dev import` bootstraps this directory (creating it if absent),
	// merges new archives into it, then `mere release publish` promotes the
	// result to OutputDir. The dev repo is removed after a successful
	// publish and re-created on the next import — it does not accumulate
	// state across publishes.
	DevRepoName string

	// OutputDir is the production output directory: the fully-published
	// repo.db, repo.db.sig, and packages/ tree that pkgd serves read-only
	// over HTTP. It is entirely owned by `mere release publish` — pkgd
	// never writes into it directly.
	OutputDir string

	// MereBinPath is the path to the `mere` binary (or, in tests, a stand-in
	// script). Deliberately not hardcoded to "mere" on PATH so tests can
	// inject a fake implementation.
	MereBinPath string

	// SigningKeyPath is an optional path to the repo signing key, passed
	// through to `mere release publish --key <path>` when set. pkgd treats
	// this purely as a read-only reference; it never generates, rotates, or
	// otherwise manages key material.
	SigningKeyPath string

	// PublishToken is the bearer token that POST /publish requests must
	// present. There is exactly one token; rotate by changing the env var
	// and restarting pkgd.
	PublishToken string

	// ListenSocket is the filesystem path of the Unix domain socket pkgd
	// listens on. nginx (or another reverse proxy) terminates TLS and
	// proxies to this socket.
	ListenSocket string

	// TempDir is where uploaded archives are staged (written from the
	// multipart request) before being handed to `mere dev import`, which
	// copies them into the dev repo's own packages/ pool itself. Defaults to
	// os.TempDir() if unset.
	TempDir string

	// Mirror configures the optional asynchronous S3-compatible backup of
	// OutputDir. A configured mirror never blocks or fails the local publish.
	Mirror MirrorConfig
}

// DevRepoPath returns the resolved absolute path of the persistent dev repo,
// matching exactly how `mere --root <MereRoot> dev import <DevRepoName>`
// resolves it. This is what gets passed to `mere release publish
// --dev-repo` — that command takes a literal path, not a --root + name pair.
func (c Config) DevRepoPath() string {
	return filepath.Join(c.MereRoot, "mere", "dev", "repo", c.DevRepoName)
}

// envOr returns the value of the named environment variable, or def if it is
// unset or empty.
func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// durationOr returns the duration in the named environment variable, or def
// when the variable is unset or empty.
func durationOr(name, def string) (time.Duration, error) {
	value := envOr(name, def)
	d, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("config: %s must be a duration: %w", name, err)
	}
	return d, nil
}

// FromEnv loads a Config from environment variables and validates it.
//
// Recognized variables:
//
//	PKGD_MERE_ROOT          (required) --root prefix for every mere invocation
//	PKGD_DEV_REPO_NAME      (optional, default "pkgd") bare name of the persistent dev repo under PKGD_MERE_ROOT
//	PKGD_OUTPUT_DIR         (required) production output directory pkgd serves
//	PKGD_MERE_BIN           (optional, default "mere") path to the mere binary
//	PKGD_SIGNING_KEY_PATH   (optional) path to the repo signing key
//	PKGD_PUBLISH_TOKEN      (required) bearer token guarding POST /publish
//	PKGD_LISTEN_SOCKET      (optional, default "/run/pkgd/pkgd.sock") Unix socket to listen on
//	PKGD_TEMP_DIR           (optional, default os.TempDir()) upload staging directory
//	PKGD_MIRROR_ENDPOINT    (optional) S3-compatible endpoint, such as B2's S3 endpoint
//	PKGD_MIRROR_REGION      (optional) bucket region, required when the mirror is enabled
//	PKGD_MIRROR_BUCKET      (optional) bucket name
//	PKGD_MIRROR_PREFIX      (optional, default "pkgd") key prefix inside the bucket
//	PKGD_MIRROR_ACCESS_KEY_ID (optional) S3 access key ID / B2 key ID
//	PKGD_MIRROR_SECRET_ACCESS_KEY (optional) S3 secret / B2 application key
//	PKGD_MIRROR_RETRY_INTERVAL (optional, default "5m") retry interval after a failed sync
func FromEnv() (Config, error) {
	retryInterval, err := durationOr("PKGD_MIRROR_RETRY_INTERVAL", "5m")
	if err != nil {
		return Config{}, err
	}

	cfg := Config{
		MereRoot:       os.Getenv("PKGD_MERE_ROOT"),
		DevRepoName:    envOr("PKGD_DEV_REPO_NAME", "pkgd"),
		OutputDir:      os.Getenv("PKGD_OUTPUT_DIR"),
		MereBinPath:    envOr("PKGD_MERE_BIN", "mere"),
		SigningKeyPath: os.Getenv("PKGD_SIGNING_KEY_PATH"),
		PublishToken:   os.Getenv("PKGD_PUBLISH_TOKEN"),
		ListenSocket:   envOr("PKGD_LISTEN_SOCKET", "/run/pkgd/pkgd.sock"),
		TempDir:        envOr("PKGD_TEMP_DIR", os.TempDir()),
		Mirror: MirrorConfig{
			Endpoint:        os.Getenv("PKGD_MIRROR_ENDPOINT"),
			Region:          os.Getenv("PKGD_MIRROR_REGION"),
			Bucket:          os.Getenv("PKGD_MIRROR_BUCKET"),
			Prefix:          envOr("PKGD_MIRROR_PREFIX", "pkgd"),
			AccessKeyID:     os.Getenv("PKGD_MIRROR_ACCESS_KEY_ID"),
			SecretAccessKey: os.Getenv("PKGD_MIRROR_SECRET_ACCESS_KEY"),
			RetryInterval:   retryInterval,
		},
	}
	cfg.Mirror.Enabled = cfg.Mirror.Endpoint != "" ||
		cfg.Mirror.Region != "" ||
		cfg.Mirror.Bucket != "" ||
		cfg.Mirror.AccessKeyID != "" ||
		cfg.Mirror.SecretAccessKey != ""
	return cfg, cfg.Validate()
}

// Validate checks that all required fields are set.
func (c Config) Validate() error {
	var missing []string
	if c.MereRoot == "" {
		missing = append(missing, "PKGD_MERE_ROOT")
	}
	if c.DevRepoName == "" {
		missing = append(missing, "PKGD_DEV_REPO_NAME")
	}
	if c.OutputDir == "" {
		missing = append(missing, "PKGD_OUTPUT_DIR")
	}
	if c.MereBinPath == "" {
		missing = append(missing, "PKGD_MERE_BIN")
	}
	if c.PublishToken == "" {
		missing = append(missing, "PKGD_PUBLISH_TOKEN")
	}
	if c.ListenSocket == "" {
		missing = append(missing, "PKGD_LISTEN_SOCKET")
	}
	if len(missing) > 0 {
		return fmt.Errorf("config: missing required setting(s): %s", strings.Join(missing, ", "))
	}
	return c.Mirror.Validate()
}
