package mirror

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

type fakeBackend struct {
	objects map[string]RemoteObject
	bodies  map[string][]byte
	puts    []string
	deletes []string
}

func (f *fakeBackend) List(_ context.Context, prefix string) ([]RemoteObject, error) {
	objects := make([]RemoteObject, 0)
	for key, object := range f.objects {
		if strings.HasPrefix(key, prefix) {
			objects = append(objects, object)
		}
	}
	return objects, nil
}

func (f *fakeBackend) Put(_ context.Context, key string, body io.Reader, size int64, _ string) error {
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	if int64(len(data)) != size {
		return io.ErrShortBuffer
	}
	f.objects[key] = RemoteObject{Key: key, Size: int64(len(data))}
	f.bodies[key] = data
	f.puts = append(f.puts, key)
	return nil
}

func (f *fakeBackend) Delete(_ context.Context, keys []string) error {
	f.deletes = append(f.deletes, keys...)
	for _, key := range keys {
		delete(f.objects, key)
	}
	return nil
}

func TestSyncTreeUploadsMissingAndMutableObjectsAndDeletesStaleObjects(t *testing.T) {
	output := t.TempDir()
	writeMirrorFile(t, filepath.Join(output, "repo.db"), "new")
	writeMirrorFile(t, filepath.Join(output, "repo.db.sig"), "sig")
	writeMirrorFile(t, filepath.Join(output, "repo.lock"), "")
	writeMirrorFile(t, filepath.Join(output, "packages", "keep.pkg.tar.zst"), "keep")
	writeMirrorFile(t, filepath.Join(output, "packages", "new.pkg.tar.zst"), "new")

	backend := &fakeBackend{
		objects: map[string]RemoteObject{
			"pkgd/repo.db":                   {Key: "pkgd/repo.db", Size: 3},
			"pkgd/repo.db.sig":               {Key: "pkgd/repo.db.sig", Size: 3},
			"pkgd/repo.lock":                 {Key: "pkgd/repo.lock", Size: 0},
			"pkgd/packages/keep.pkg.tar.zst": {Key: "pkgd/packages/keep.pkg.tar.zst", Size: 4},
			"pkgd/packages/old.pkg.tar.zst":  {Key: "pkgd/packages/old.pkg.tar.zst", Size: 3},
			"unrelated/leave-me.txt":         {Key: "unrelated/leave-me.txt", Size: 10},
		},
		bodies: make(map[string][]byte),
	}

	if err := syncTree(context.Background(), backend, output, "pkgd/"); err != nil {
		t.Fatalf("syncTree: %v", err)
	}

	if got, want := backend.puts, []string{"pkgd/packages/new.pkg.tar.zst", "pkgd/repo.db", "pkgd/repo.db.sig", "pkgd/repo.lock"}; !equalStrings(got, want) {
		t.Errorf("puts = %v, want %v", got, want)
	}
	if got, want := backend.deletes, []string{"pkgd/packages/old.pkg.tar.zst"}; !equalStrings(got, want) {
		t.Errorf("deletes = %v, want %v", got, want)
	}
	if _, exists := backend.objects["unrelated/leave-me.txt"]; !exists {
		t.Error("sync deleted an object outside its configured prefix")
	}
}

func TestCollectLocalObjectsRejectsSymlinks(t *testing.T) {
	output := t.TempDir()
	writeMirrorFile(t, filepath.Join(output, "repo.db"), "database")
	target := filepath.Join(output, "outside")
	writeMirrorFile(t, target, "secret")
	if err := os.Symlink(target, filepath.Join(output, "link")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	_, err := collectLocalObjects(output, "pkgd/")
	if err == nil || !strings.Contains(err.Error(), "unsupported symlink") {
		t.Fatalf("error = %v, want unsupported symlink", err)
	}
}

func TestCollectLocalObjectsRequiresRepoDB(t *testing.T) {
	_, err := collectLocalObjects(t.TempDir(), "pkgd/")
	if err == nil || !strings.Contains(err.Error(), "without repo.db") {
		t.Fatalf("error = %v, want missing repo.db", err)
	}
}

func TestNormalizePrefix(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"pkgd", "pkgd/"},
		{"/backups/pkgd/", "backups/pkgd/"},
		{"", ""},
	}
	for _, tc := range cases {
		got, err := normalizePrefix(tc.input)
		if err != nil {
			t.Errorf("normalizePrefix(%q): %v", tc.input, err)
			continue
		}
		if got != tc.want {
			t.Errorf("normalizePrefix(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}

	for _, input := range []string{"../pkgd", "pkgd/../other", "pkgd//objects"} {
		if _, err := normalizePrefix(input); err == nil {
			t.Errorf("normalizePrefix(%q): expected error", input)
		}
	}
}

func TestParseEndpoint(t *testing.T) {
	cases := []struct {
		input  string
		host   string
		secure bool
	}{
		{"s3.us-west-004.backblazeb2.com", "s3.us-west-004.backblazeb2.com", true},
		{"https://s3.us-west-004.backblazeb2.com", "s3.us-west-004.backblazeb2.com", true},
		{"http://localhost:9000/", "localhost:9000", false},
	}
	for _, tc := range cases {
		host, secure, err := parseEndpoint(tc.input)
		if err != nil {
			t.Errorf("parseEndpoint(%q): %v", tc.input, err)
			continue
		}
		if host != tc.host || secure != tc.secure {
			t.Errorf("parseEndpoint(%q) = (%q, %t), want (%q, %t)", tc.input, host, secure, tc.host, tc.secure)
		}
	}

	for _, input := range []string{"", "ftp://example.com", "https://example.com/path", "https://example.com?x=1"} {
		if _, _, err := parseEndpoint(input); err == nil {
			t.Errorf("parseEndpoint(%q): expected error", input)
		}
	}
}

func writeMirrorFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func equalStrings(got, want []string) bool {
	got = append([]string(nil), got...)
	want = append([]string(nil), want...)
	sort.Strings(got)
	sort.Strings(want)
	return bytes.Equal([]byte(strings.Join(got, "\x00")), []byte(strings.Join(want, "\x00")))
}
