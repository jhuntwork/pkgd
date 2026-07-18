package publish

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codeberg.org/merelinux/pkgd/internal/config"
)

// writeFakeMere writes a stand-in shell script for the real `mere` binary.
// It is NOT the real mere CLI — pkgd's tests must not depend on that
// binary existing — but a tiny recorder: every invocation appends its argv
// to logPath (one line per call), so tests can assert call order and
// arguments without shelling out to anything real.
//
// If failOn is non-empty, an invocation whose first argument equals failOn
// exits non-zero (after still recording itself), so callers can verify
// that a failing step short-circuits the pipeline.
func writeFakeMere(t *testing.T, logPath, failOn string) string {
	t.Helper()
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "mere")

	script := "#!/bin/sh\n" +
		"echo \"$@\" >> " + shellQuote(logPath) + "\n"
	if failOn != "" {
		script += "if [ \"$1\" = " + shellQuote(failOn) + " ]; then echo fake-mere-failure >&2; exit 3; fi\n"
	}
	script += "exit 0\n"

	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake mere: %v", err)
	}
	return scriptPath
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func readLines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read %s: %v", path, err)
	}
	s := strings.TrimRight(string(data), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func testConfig(t *testing.T, merePath string) config.Config {
	t.Helper()
	return config.Config{
		MereRoot:     t.TempDir(),
		DevRepoName:  "pkgd",
		OutputDir:    t.TempDir(),
		MereBinPath:  merePath,
		PublishToken: "s3cr3t-token",
		ListenSocket: filepath.Join(t.TempDir(), "pkgd.sock"),
		TempDir:      t.TempDir(),
	}
}

// --- Auth rejection ---------------------------------------------------

func TestPublishHandlerRejectsMissingToken(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "mere.log")
	merePath := writeFakeMere(t, logPath, "")
	cfg := testConfig(t, merePath)
	p := NewPublisher(cfg)

	req := httptest.NewRequest(http.MethodPost, "/publish", strings.NewReader("irrelevant"))
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if lines := readLines(t, logPath); lines != nil {
		t.Fatalf("mere was invoked despite missing auth: %v", lines)
	}
}

func TestPublishHandlerRejectsWrongToken(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "mere.log")
	merePath := writeFakeMere(t, logPath, "")
	cfg := testConfig(t, merePath)
	p := NewPublisher(cfg)

	req := httptest.NewRequest(http.MethodPost, "/publish", strings.NewReader("irrelevant"))
	req.Header.Set("Authorization", "Bearer wrong-token")
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if lines := readLines(t, logPath); lines != nil {
		t.Fatalf("mere was invoked despite wrong auth: %v", lines)
	}
}

func TestPublishHandlerRejectsMalformedAuthHeader(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "mere.log")
	merePath := writeFakeMere(t, logPath, "")
	cfg := testConfig(t, merePath)
	p := NewPublisher(cfg)

	req := httptest.NewRequest(http.MethodPost, "/publish", strings.NewReader("irrelevant"))
	req.Header.Set("Authorization", cfg.PublishToken) // missing "Bearer " prefix
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if lines := readLines(t, logPath); lines != nil {
		t.Fatalf("mere was invoked despite malformed auth header: %v", lines)
	}
}

func TestPublishHandlerAcceptsCorrectToken(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "mere.log")
	merePath := writeFakeMere(t, logPath, "")
	cfg := testConfig(t, merePath)
	p := NewPublisher(cfg)

	body, contentType := multipartBody(t, map[string]string{
		"foo-1.0-1.pkg.tar.zst": "fake archive bytes",
	})
	req := httptest.NewRequest(http.MethodPost, "/publish", body)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", "Bearer "+cfg.PublishToken)
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if lines := readLines(t, logPath); len(lines) != 2 {
		t.Fatalf("expected 2 mere invocations (import + publish), got %v", lines)
	}

	var result Result
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !result.Success {
		t.Fatalf("result.Success = false, want true: %+v", result)
	}
	if len(result.Packages) != 1 || result.Packages[0] != "foo-1.0-1.pkg.tar.zst" {
		t.Fatalf("result.Packages = %v, want [foo-1.0-1.pkg.tar.zst]", result.Packages)
	}

	// The staged archive path handed to `mere dev import` should end in the
	// uploaded filename (the real mere binary is responsible for copying it
	// into DevRepoPath's own packages/ pool; the fake stand-in here only
	// records argv).
	lines := readLines(t, logPath)
	if len(lines) != 2 || !strings.HasSuffix(lines[0], "foo-1.0-1.pkg.tar.zst") {
		t.Fatalf("import call = %v, want it to end with the staged archive path", lines)
	}
}

func multipartBody(t *testing.T, files map[string]string) (*bytes.Buffer, string) {
	t.Helper()
	buf := &bytes.Buffer{}
	w := multipart.NewWriter(buf)
	for name, contents := range files {
		part, err := w.CreateFormFile("package", name)
		if err != nil {
			t.Fatalf("create form file: %v", err)
		}
		if _, err := part.Write([]byte(contents)); err != nil {
			t.Fatalf("write form file: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}
	return buf, w.FormDataContentType()
}

// --- Subprocess orchestration ------------------------------------------

func TestPublishRunsImportThenPublishInOrderWithExpectedArgs(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "mere.log")
	merePath := writeFakeMere(t, logPath, "")
	cfg := testConfig(t, merePath)
	p := NewPublisher(cfg)

	archives := []string{
		filepath.Join(t.TempDir(), "foo-1.0-1.pkg.tar.zst"),
		filepath.Join(t.TempDir(), "bar-2.0-1.pkg.tar.zst"),
	}

	res := p.Publish(context.Background(), archives)
	if !res.Success {
		t.Fatalf("Publish failed: %+v", res)
	}

	lines := readLines(t, logPath)
	if len(lines) != 2 {
		t.Fatalf("expected 2 invocations, got %d: %v", len(lines), lines)
	}

	wantImport := "--root " + cfg.MereRoot + " dev import " + cfg.DevRepoName + " " + archives[0] + " " + archives[1]
	if lines[0] != wantImport {
		t.Errorf("import call = %q, want %q", lines[0], wantImport)
	}

	wantPublish := "release publish --dev-repo " + cfg.DevRepoPath() + " --output " + cfg.OutputDir
	if lines[1] != wantPublish {
		t.Errorf("publish call = %q, want %q", lines[1], wantPublish)
	}

	if res.Import == nil || res.Import.ExitCode != 0 {
		t.Errorf("Import step result missing or non-zero: %+v", res.Import)
	}
	if res.Publish == nil || res.Publish.ExitCode != 0 {
		t.Errorf("Publish step result missing or non-zero: %+v", res.Publish)
	}
}

func TestPublishIncludesSigningKeyFlagWhenConfigured(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "mere.log")
	merePath := writeFakeMere(t, logPath, "")
	cfg := testConfig(t, merePath)
	cfg.SigningKeyPath = "/etc/pkgd/signing.key"
	p := NewPublisher(cfg)

	res := p.Publish(context.Background(), []string{filepath.Join(t.TempDir(), "foo.pkg.tar.zst")})
	if !res.Success {
		t.Fatalf("Publish failed: %+v", res)
	}

	lines := readLines(t, logPath)
	if len(lines) != 2 {
		t.Fatalf("expected 2 invocations, got %d: %v", len(lines), lines)
	}
	if !strings.Contains(lines[1], "--key "+cfg.SigningKeyPath) {
		t.Errorf("publish call = %q, want it to contain --key %s", lines[1], cfg.SigningKeyPath)
	}
}

func TestPublishShortCircuitsWhenImportFails(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "mere.log")
	// Fake mere exits non-zero on any invocation whose first arg is
	// "--root" (i.e. `mere --root ... dev import ...`).
	merePath := writeFakeMere(t, logPath, "--root")
	cfg := testConfig(t, merePath)
	p := NewPublisher(cfg)

	res := p.Publish(context.Background(), []string{filepath.Join(t.TempDir(), "foo.pkg.tar.zst")})

	if res.Success {
		t.Fatalf("expected Publish to report failure, got success: %+v", res)
	}
	if res.Stage != "import" {
		t.Errorf("res.Stage = %q, want %q", res.Stage, "import")
	}
	if res.Publish != nil {
		t.Errorf("release publish should never have run, but got: %+v", res.Publish)
	}

	lines := readLines(t, logPath)
	if len(lines) != 1 {
		t.Fatalf("expected exactly 1 invocation (import only), got %d: %v", len(lines), lines)
	}
	if !strings.HasPrefix(lines[0], "--root ") {
		t.Errorf("recorded call = %q, want it to start with %q", lines[0], "--root ")
	}
	if res.Import == nil || res.Import.ExitCode == 0 {
		t.Errorf("expected non-zero import exit code, got: %+v", res.Import)
	}
	if !strings.Contains(res.Reason, "mere dev import failed") {
		t.Errorf("res.Reason = %q, want it to mention the import failure", res.Reason)
	}
}

func TestPublishHandlerReportsFailureAsInternalServerError(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "mere.log")
	merePath := writeFakeMere(t, logPath, "--root")
	cfg := testConfig(t, merePath)
	p := NewPublisher(cfg)

	body, contentType := multipartBody(t, map[string]string{
		"foo-1.0-1.pkg.tar.zst": "fake archive bytes",
	})
	req := httptest.NewRequest(http.MethodPost, "/publish", body)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", "Bearer "+cfg.PublishToken)
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", rec.Code, rec.Body.String())
	}

	var result Result
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if result.Success {
		t.Fatalf("result.Success = true, want false")
	}
	if result.Stage != "import" {
		t.Errorf("result.Stage = %q, want %q", result.Stage, "import")
	}
}

// --- Filename validation -----------------------------------------------

func TestSanitizeArchiveNameRejectsTraversalAndBadSuffix(t *testing.T) {
	cases := []string{
		"../../etc/passwd",
		"sub/dir/foo.pkg.tar.zst",
		"foo.tar.gz",
		"",
		".",
		"..",
	}
	for _, name := range cases {
		if _, err := sanitizeArchiveName(name); err == nil {
			t.Errorf("sanitizeArchiveName(%q): expected error, got none", name)
		}
	}
}

func TestSanitizeArchiveNameAcceptsValidName(t *testing.T) {
	got, err := sanitizeArchiveName("foo-1.2.3-1.pkg.tar.zst")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "foo-1.2.3-1.pkg.tar.zst" {
		t.Errorf("got %q, want %q", got, "foo-1.2.3-1.pkg.tar.zst")
	}
}
