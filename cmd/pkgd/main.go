// Command pkgd is the single network-facing binary for Mere Linux's package
// repository: it serves the published repo (repo.db, repo.db.sig,
// packages/*) over HTTP and accepts new package publishes from CI over
// HTTPS at POST /publish.
package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"codeberg.org/merelinux/pkgd/internal/config"
	"codeberg.org/merelinux/pkgd/internal/publish"
	"codeberg.org/merelinux/pkgd/internal/serve"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	cfg, err := config.FromEnv()
	if err != nil {
		return err
	}

	if err := ensureDirs(cfg); err != nil {
		return err
	}

	publisher := publish.NewPublisher(cfg)

	mux := http.NewServeMux()
	mux.Handle("/publish", publisher.Handler())
	// Future HTTP surface (telemetry/stats, a package browser API) belongs
	// on additional routes registered here, alongside a stats/packages
	// data layer under internal/ — not folded into serve.NewHandler.
	mux.Handle("/", serve.NewHandler(cfg))

	ln, err := listen(cfg.ListenSocket)
	if err != nil {
		return err
	}
	defer ln.Close()

	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 1)
	go func() {
		log.Printf("pkgd listening on unix:%s", cfg.ListenSocket)
		serveErr <- srv.Serve(ln)
	}()

	select {
	case err := <-serveErr:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
		log.Print("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

// listen binds the Unix domain socket pkgd serves on, removing any stale
// socket file left behind by a previous, uncleanly-terminated run.
func listen(socketPath string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o755); err != nil {
		return nil, err
	}
	if err := removeStaleSocket(socketPath); err != nil {
		return nil, err
	}
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, err
	}
	// nginx runs as its own user/group; pkgd relies on group membership
	// (configured at deploy time, see deploy/README.md) rather than
	// world-writable permissions.
	if err := os.Chmod(socketPath, 0o660); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

func removeStaleSocket(socketPath string) error {
	if _, err := os.Stat(socketPath); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return os.Remove(socketPath)
}

// ensureDirs creates the directories pkgd owns if they don't already exist.
// It does NOT create OutputDir's contents beyond the directory itself —
// repo.db et al. are entirely owned by `mere release publish`.
//
// Deliberately does NOT create the dev repo's own leaf directory
// (cfg.DevRepoPath()): `mere dev import` only auto-bootstraps a dev repo
// addressed by --root + name when that directory doesn't exist yet: a
// pre-existing-but-empty directory is treated as an invalid repo ("no
// repo.db") rather than something to initialize. MereRoot itself is safe to
// create — mere creates everything under it on demand.
func ensureDirs(cfg config.Config) error {
	dirs := []string{cfg.MereRoot, cfg.OutputDir}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	return nil
}
