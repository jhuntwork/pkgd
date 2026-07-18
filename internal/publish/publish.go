// Package publish implements pkgd's publish-intake pipeline: authenticated
// upload of new package archives, staging, merge into the persistent
// server-side dev repo, and promotion to the production output directory.
//
// Upload format (POST /publish):
//
//	Authorization: Bearer <token>
//	Content-Type: multipart/form-data; boundary=...
//
//	One or more file parts, all using the field name "package", one per
//	archive:
//
//	    --boundary
//	    Content-Disposition: form-data; name="package"; filename="foo-1.2.3-1.pkg.tar.zst"
//	    Content-Type: application/octet-stream
//
//	    <raw archive bytes>
//	    --boundary--
//
// Filenames must be bare basenames ending in ".pkg.tar.zst" (no path
// separators, no ".." components) — this is enforced server-side
// regardless of what the client sends. There is no other required field;
// which packages "landed" is reported back in the JSON response, not
// supplied by the client.
package publish

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"codeberg.org/merelinux/pkgd/internal/config"
)

// maxUploadMemory bounds how much of a multipart request ParseMultipartForm
// keeps in memory before spilling to temp files on disk.
const maxUploadMemory = 32 << 20 // 32 MiB

// maxCapturedOutput caps how much of a subprocess's stdout/stderr we retain
// and echo back in the JSON response, so a runaway or chatty `mere`
// invocation can't blow up the response body.
const maxCapturedOutput = 16 << 10 // 16 KiB

const archiveSuffix = ".pkg.tar.zst"

// StepResult captures the outcome of a single `mere` subprocess invocation.
type StepResult struct {
	Args     []string `json:"args"`
	ExitCode int      `json:"exit_code"`
	Stdout   string   `json:"stdout,omitempty"`
	Stderr   string   `json:"stderr,omitempty"`
}

// Result is the JSON body returned by POST /publish.
type Result struct {
	Success  bool        `json:"success"`
	Packages []string    `json:"packages,omitempty"`
	Stage    string      `json:"stage,omitempty"` // "import" or "publish"; set only on failure
	Reason   string      `json:"reason,omitempty"`
	Import   *StepResult `json:"import,omitempty"`
	Publish  *StepResult `json:"publish,omitempty"`
}

// Publisher owns the persistent dev-repo mutex and knows how to run the
// mere subprocess pipeline against it.
type Publisher struct {
	cfg config.Config

	// mu serializes every write to the persistent dev repo. This is
	// defense in depth — `mere`'s own flock is the real correctness
	// guarantee — but it keeps pkgd from ever running two `mere`
	// subprocesses against the same dev repo concurrently, and it keeps
	// concurrent /publish requests from interleaving their output.
	mu sync.Mutex
}

// NewPublisher returns a Publisher for the given config.
func NewPublisher(cfg config.Config) *Publisher {
	return &Publisher{cfg: cfg}
}

// Handler returns the bearer-token-authenticated POST /publish handler.
func (p *Publisher) Handler() http.Handler {
	return bearerAuth(p.cfg.PublishToken, http.HandlerFunc(p.handlePublish))
}

func (p *Publisher) handlePublish(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	if err := r.ParseMultipartForm(maxUploadMemory); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid multipart form: "+err.Error())
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()

	fileHeaders := r.MultipartForm.File["package"]
	if len(fileHeaders) == 0 {
		writeJSONError(w, http.StatusBadRequest, `no package archives provided (expected one or more multipart file parts named "package")`)
		return
	}

	tempDir, err := os.MkdirTemp(p.cfg.TempDir, "pkgd-publish-")
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "failed to create staging directory: "+err.Error())
		return
	}
	defer os.RemoveAll(tempDir)

	names := make([]string, 0, len(fileHeaders))
	staged := make([]string, 0, len(fileHeaders))
	for _, fh := range fileHeaders {
		name, err := sanitizeArchiveName(fh.Filename)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		dst := filepath.Join(tempDir, name)
		if err := saveUploadedFile(fh, dst); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "failed to stage upload: "+err.Error())
			return
		}
		names = append(names, name)
		staged = append(staged, dst)
	}

	// Hand the staged (temp-dir) archive paths straight to `mere dev
	// import`, which copies them into the persistent dev repo's own
	// packages/ pool itself — pkgd doesn't own or manage a separate archive
	// pool directory. The temp dir (and these paths) are removed once the
	// request completes; that's fine, mere has already made its own copy.
	result := p.Publish(r.Context(), staged)
	result.Packages = names

	status := http.StatusOK
	if !result.Success {
		status = http.StatusInternalServerError
	}
	writeJSON(w, status, result)
}

// Publish runs the two-step mere subprocess pipeline against the given
// archive paths (any readable location — typically the temp staging dir
// handlePublish just wrote them to; `mere dev import` copies them into the
// dev repo's own packages/ pool itself, pkgd does not manage a separate
// pool directory): first merging them into the persistent dev repo, then
// promoting the dev repo's current state to the production output
// directory. It is exported and HTTP-independent so it can be exercised
// directly in tests.
//
// A failing import step short-circuits: release publish is never run, and
// the dev repo is left exactly as `mere dev import` left it.
func (p *Publisher) Publish(ctx context.Context, archives []string) *Result {
	p.mu.Lock()
	defer p.mu.Unlock()

	importStep := p.runMere(ctx, mereDevImportArgs(p.cfg.MereRoot, p.cfg.DevRepoName, archives))
	res := &Result{Import: &importStep}
	if importStep.ExitCode != 0 {
		res.Success = false
		res.Stage = "import"
		res.Reason = fmt.Sprintf("mere dev import failed (exit %d): %s", importStep.ExitCode, excerpt(importStep.Stderr))
		return res
	}

	publishStep := p.runMere(ctx, mereReleasePublishArgs(p.cfg.DevRepoPath(), p.cfg.OutputDir, p.cfg.SigningKeyPath))
	res.Publish = &publishStep
	if publishStep.ExitCode != 0 {
		res.Success = false
		res.Stage = "publish"
		res.Reason = fmt.Sprintf("mere release publish failed (exit %d): %s", publishStep.ExitCode, excerpt(publishStep.Stderr))
		return res
	}

	// Clean up the dev repo now that its contents have been promoted to
	// OutputDir. The dev repo is ephemeral per-publish — `mere dev import`
	// re-bootstraps it on the next invocation when addressed by --root +
	// bare name. Removing it prevents unbounded accumulation of package
	// archives on disk.
	_ = os.RemoveAll(p.cfg.DevRepoPath())

	res.Success = true
	return res
}

// runMere execs the configured mere binary with args and captures its
// outcome. It never returns an error itself — subprocess failures are
// reported through StepResult.ExitCode/Stderr so callers can decide how to
// react (in particular, so Publish can short-circuit on a failed import).
func (p *Publisher) runMere(ctx context.Context, args []string) StepResult {
	cmd := exec.CommandContext(ctx, p.cfg.MereBinPath, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()

	step := StepResult{
		Args:   args,
		Stdout: capOutput(stdout.String()),
		Stderr: capOutput(stderr.String()),
	}

	var exitErr *exec.ExitError
	switch {
	case runErr == nil:
		step.ExitCode = 0
	case errors.As(runErr, &exitErr):
		step.ExitCode = exitErr.ExitCode()
	default:
		// Failed to even start the process (e.g. binary not found).
		step.ExitCode = -1
		if step.Stderr == "" {
			step.Stderr = runErr.Error()
		}
	}
	return step
}

func capOutput(s string) string {
	if len(s) <= maxCapturedOutput {
		return s
	}
	return s[:maxCapturedOutput] + "...(truncated)"
}

// excerpt returns a short, single-line-friendly tail of s suitable for
// embedding in a JSON "reason" field.
func excerpt(s string) string {
	s = strings.TrimSpace(s)
	const maxLen = 500
	if len(s) > maxLen {
		s = "..." + s[len(s)-maxLen:]
	}
	if s == "" {
		return "(no output)"
	}
	return s
}

// sanitizeArchiveName validates an uploaded filename and returns the bare
// basename to stage it under. It rejects anything that isn't a plain
// "*.pkg.tar.zst" basename, so a malicious filename can never be used to
// escape the staging or archive-pool directories.
func sanitizeArchiveName(filename string) (string, error) {
	if filename == "" {
		return "", fmt.Errorf("archive upload has empty filename")
	}
	base := filepath.Base(filepath.Clean(filename))
	if base != filename || strings.ContainsAny(filename, `/\`) {
		return "", fmt.Errorf("invalid archive filename %q: must be a bare filename", filename)
	}
	if base == "." || base == ".." || base == "" {
		return "", fmt.Errorf("invalid archive filename %q", filename)
	}
	if !strings.HasSuffix(base, archiveSuffix) {
		return "", fmt.Errorf("invalid archive filename %q: must end in %q", filename, archiveSuffix)
	}
	return base, nil
}

// saveUploadedFile copies a multipart file part to dst.
func saveUploadedFile(fh *multipart.FileHeader, dst string) error {
	src, err := fh.Open()
	if err != nil {
		return err
	}
	defer src.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err := io.Copy(out, src); err != nil {
		return err
	}
	return out.Close()
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, struct {
		Error string `json:"error"`
	}{Error: msg})
}
