// Package serve implements pkgd's read-only HTTP surface for the published
// package repository: repo.db, repo.db.sig, and the packages/ tree.
package serve

import (
	"net/http"

	"codeberg.org/merelinux/pkgd/internal/config"
)

// NewHandler returns the handler that serves the production output
// directory (repo.db, repo.db.sig, packages/*) over HTTP.
//
// It is a thin wrapper around http.FileServer(http.Dir(cfg.OutputDir)),
// which gives us, for free:
//
//   - Range request support (via http.ServeContent), so mere's client can
//     resume interrupted downloads.
//   - Path-traversal safety: http.Dir cleans the request path by treating
//     it as rooted (it Cleans "/"+name before joining with the served
//     directory), so a path like "/packages/../../etc/passwd" can never
//     resolve outside OutputDir. http.ServeMux additionally redirects
//     requests containing ".." elements to their cleaned equivalent before
//     the handler even runs. We rely on both layers rather than any
//     hand-rolled string surgery on the request path.
//
// The returned handler is mounted at "/" by the caller. Future HTTP
// surface (telemetry/stats endpoints, a package browser API) should be
// registered on sibling routes on the same mux in cmd/pkgd/main.go rather
// than folded into this handler.
func NewHandler(cfg config.Config) http.Handler {
	return http.FileServer(http.Dir(cfg.OutputDir))
}
