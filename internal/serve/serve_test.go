package serve

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codeberg.org/merelinux/pkgd/internal/config"
)

// newTestTree builds:
//
//	<root>/
//	  secret.txt                (sentinel OUTSIDE the served directory)
//	  output/                   (served directory, cfg.OutputDir)
//	    repo.db
//	    packages/
//	      foo-1.0-1.pkg.tar.zst
func newTestTree(t *testing.T) (root, outputDir string) {
	t.Helper()
	root = t.TempDir()
	outputDir = filepath.Join(root, "output")

	if err := os.MkdirAll(filepath.Join(outputDir, "packages"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	writeFile(t, filepath.Join(root, "secret.txt"), "top-secret-outside-served-root\n")
	writeFile(t, filepath.Join(outputDir, "repo.db"), "fake-repo-db-contents\n")
	writeFile(t, filepath.Join(outputDir, "packages", "foo-1.0-1.pkg.tar.zst"), "fake-archive-bytes\n")

	return root, outputDir
}

func writeFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestServesRepoDBAndPackages(t *testing.T) {
	_, outputDir := newTestTree(t)
	h := NewHandler(config.Config{OutputDir: outputDir})

	for _, path := range []string{"/repo.db", "/packages/foo-1.0-1.pkg.tar.zst"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s: got status %d, want 200", path, rec.Code)
		}
	}
}

func TestRangeRequestsSupported(t *testing.T) {
	_, outputDir := newTestTree(t)
	h := NewHandler(config.Config{OutputDir: outputDir})

	req := httptest.NewRequest(http.MethodGet, "/repo.db", nil)
	req.Header.Set("Range", "bytes=0-3")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusPartialContent {
		t.Fatalf("Range request: got status %d, want 206", rec.Code)
	}
	if got := rec.Body.String(); got != "fake" {
		t.Errorf("Range request body = %q, want %q", got, "fake")
	}
}

func TestPathTraversalDoesNotEscapeOutputDir(t *testing.T) {
	root, outputDir := newTestTree(t)
	h := NewHandler(config.Config{OutputDir: outputDir})

	// Traversal attempts that, if unsafely joined, would read
	// <root>/secret.txt (outside outputDir).
	traversalPaths := []string{
		"/packages/../../secret.txt",
		"/packages/../../../secret.txt",
		"/../secret.txt",
		"/packages/..%2f..%2fsecret.txt",
	}

	for _, raw := range traversalPaths {
		req := httptest.NewRequest(http.MethodGet, raw, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code == http.StatusOK && strings.Contains(rec.Body.String(), "top-secret") {
			t.Errorf("path traversal succeeded for %q: got secret contents (status %d, body %q)", raw, rec.Code, rec.Body.String())
		}
	}

	// Sanity: the secret file really does exist just outside the served
	// root, so a passing test means the handler actually defended against
	// escape, not that there was nothing to escape to.
	if _, err := os.Stat(filepath.Join(root, "secret.txt")); err != nil {
		t.Fatalf("sentinel file missing: %v", err)
	}
}

func TestPathTraversalViaServeMuxRouting(t *testing.T) {
	// Exercise the traversal defense the way it actually runs in
	// production: mounted at "/" behind an http.ServeMux, which performs
	// its own path-cleaning redirect ahead of the file server.
	_, outputDir := newTestTree(t)
	mux := http.NewServeMux()
	mux.Handle("/", NewHandler(config.Config{OutputDir: outputDir}))

	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/packages/../../../../../../etc/passwd")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusOK {
		t.Errorf("expected traversal request to fail, got 200 OK")
	}
}
