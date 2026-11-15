// Package mirror asynchronously reconciles pkgd's published output directory
// with an S3-compatible object-store backup.
package mirror

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"codeberg.org/merelinux/pkgd/internal/config"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// RemoteObject is the subset of object metadata needed to reconcile a
// destination with the local output tree.
type RemoteObject struct {
	Key  string
	Size int64
}

// Backend is the small object-store surface needed by the synchronizer. It is
// deliberately separate from the MinIO client so reconciliation can be tested
// without network access.
type Backend interface {
	List(ctx context.Context, prefix string) ([]RemoteObject, error)
	Put(ctx context.Context, key string, body io.Reader, size int64, contentType string) error
	Delete(ctx context.Context, keys []string) error
}

// Syncer owns the asynchronous retry loop for one output directory and one
// object-store prefix. A sync failure never affects the local publish result;
// the worker retries it later and a startup sync repairs an interrupted or
// previously unavailable mirror.
type Syncer struct {
	outputDir     string
	prefix        string
	retryInterval time.Duration
	backend       Backend

	notify  chan struct{}
	started sync.Once
	wg      sync.WaitGroup
}

// New creates the configured backup mirror. A nil Syncer means the mirror is
// disabled, which keeps local development and existing deployments unchanged.
func New(cfg config.Config) (*Syncer, error) {
	if !cfg.Mirror.Enabled {
		return nil, nil
	}

	prefix, err := normalizePrefix(cfg.Mirror.Prefix)
	if err != nil {
		return nil, err
	}

	endpoint, secure, err := parseEndpoint(cfg.Mirror.Endpoint)
	if err != nil {
		return nil, err
	}

	client, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.Mirror.AccessKeyID, cfg.Mirror.SecretAccessKey, ""),
		Secure: secure,
		Region: cfg.Mirror.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("mirror: create S3 client: %w", err)
	}

	return newSyncer(
		cfg.OutputDir,
		prefix,
		cfg.Mirror.RetryInterval,
		&s3Backend{client: client, bucket: cfg.Mirror.Bucket},
	), nil
}

func newSyncer(outputDir, prefix string, retryInterval time.Duration, backend Backend) *Syncer {
	return &Syncer{
		outputDir:     outputDir,
		prefix:        prefix,
		retryInterval: retryInterval,
		backend:       backend,
		notify:        make(chan struct{}, 1),
	}
}

// Start launches the initial synchronization and the retry worker. Calling it
// more than once has no effect.
func (s *Syncer) Start(ctx context.Context) {
	s.started.Do(func() {
		s.wg.Add(1)
		go s.run(ctx)
		s.Notify()
	})
}

// Wait waits for the worker to stop after its context is canceled. It is
// useful to callers that need an orderly shutdown and to tests.
func (s *Syncer) Wait() {
	s.wg.Wait()
}

// Notify asks the worker to reconcile the output directory. Notifications are
// coalesced because every sync is a complete tree reconciliation.
func (s *Syncer) Notify() {
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

// SyncOnce performs one complete reconciliation synchronously. The method is
// useful for an administrative repair command and for tests; normal serving
// uses Start and Notify.
func (s *Syncer) SyncOnce(ctx context.Context) error {
	return syncTree(ctx, s.backend, s.outputDir, s.prefix)
}

func (s *Syncer) run(ctx context.Context) {
	defer s.wg.Done()

	pending := true
	var retry <-chan time.Time
	var timer *time.Timer

	stopTimer := func() {
		if timer == nil {
			return
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer = nil
		retry = nil
	}

	for {
		if pending {
			pending = false
			if err := s.SyncOnce(ctx); err != nil {
				if ctx.Err() != nil {
					return
				}
				log.Printf("pkgd mirror sync failed: %v; retrying in %s", err, s.retryInterval)
				timer = time.NewTimer(s.retryInterval)
				retry = timer.C
			}
		}

		select {
		case <-ctx.Done():
			stopTimer()
			return
		case <-s.notify:
			pending = true
			stopTimer()
		case <-retry:
			pending = true
			timer = nil
			retry = nil
		}
	}
}

type localObject struct {
	key         string
	path        string
	size        int64
	contentType string
}

type syncStats struct {
	local    int
	uploaded int
	deleted  int
}

func syncTree(ctx context.Context, backend Backend, outputDir, prefix string) (err error) {
	local, err := collectLocalObjects(outputDir, prefix)
	if err != nil {
		return err
	}

	remoteObjects, err := backend.List(ctx, prefix)
	if err != nil {
		return fmt.Errorf("list remote objects: %w", err)
	}
	remote := make(map[string]RemoteObject, len(remoteObjects))
	for _, object := range remoteObjects {
		remote[object.Key] = object
	}

	keys := make([]string, 0, len(local))
	for key := range local {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	stats := syncStats{local: len(local)}
	// Upload packages and other files before metadata and before deleting
	// stale objects. This ordering is safe even if the backup is inspected
	// during a sync: newly referenced files arrive before the database state.
	sort.SliceStable(keys, func(i, j int) bool {
		iMetadata := isMetadataKey(keys[i], prefix)
		jMetadata := isMetadataKey(keys[j], prefix)
		if iMetadata != jMetadata {
			return !iMetadata
		}
		return keys[i] < keys[j]
	})

	for _, key := range keys {
		object := local[key]
		remoteObject, exists := remote[key]
		if exists && !needsUpload(object, remoteObject, prefix) {
			continue
		}
		if err := uploadFile(ctx, backend, object); err != nil {
			return fmt.Errorf("upload %q: %w", key, err)
		}
		stats.uploaded++
	}

	stale := make([]string, 0)
	for key := range remote {
		if _, exists := local[key]; !exists {
			stale = append(stale, key)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		if err := backend.Delete(ctx, stale); err != nil {
			return fmt.Errorf("delete stale remote objects: %w", err)
		}
		stats.deleted = len(stale)
	}

	log.Printf("pkgd mirror sync complete: %d local object(s), %d uploaded, %d deleted", stats.local, stats.uploaded, stats.deleted)
	return nil
}

func collectLocalObjects(outputDir, prefix string) (map[string]localObject, error) {
	repoDB := filepath.Join(outputDir, "repo.db")
	repoInfo, err := os.Stat(repoDB)
	if err != nil {
		return nil, fmt.Errorf("refusing to sync output directory without repo.db: %w", err)
	}
	if !repoInfo.Mode().IsRegular() {
		return nil, fmt.Errorf("refusing to sync output directory: %q is not a regular file", repoDB)
	}

	objects := make(map[string]localObject)
	err = filepath.WalkDir(outputDir, func(filePath string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("output tree contains unsupported symlink %q", filePath)
		}

		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}

		relative, err := filepath.Rel(outputDir, filePath)
		if err != nil {
			return err
		}
		key := prefix + filepath.ToSlash(relative)
		objects[key] = localObject{
			key:         key,
			path:        filePath,
			size:        info.Size(),
			contentType: contentType(filePath),
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk output directory %q: %w", outputDir, err)
	}
	return objects, nil
}

func uploadFile(ctx context.Context, backend Backend, object localObject) error {
	file, err := os.Open(object.path)
	if err != nil {
		return err
	}
	defer file.Close()

	if err := backend.Put(ctx, object.key, file, object.size, object.contentType); err != nil {
		return err
	}
	return nil
}

func needsUpload(local localObject, remote RemoteObject, prefix string) bool {
	if remote.Size != local.size {
		return true
	}
	// Package archive names are content-addressed by Mere and can be treated
	// as immutable once present. Metadata files are mutable and must be
	// replaced on every publish even when their size is unchanged.
	return isMetadataKey(local.key, prefix)
}

func isMetadataKey(key, prefix string) bool {
	relative := strings.TrimPrefix(key, prefix)
	return !strings.HasPrefix(relative, "packages/")
}

func contentType(filePath string) string {
	if detected := mime.TypeByExtension(filepath.Ext(filePath)); detected != "" {
		return detected
	}
	return "application/octet-stream"
}

func normalizePrefix(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}

	trimmed := strings.Trim(raw, "/")
	for _, component := range strings.Split(trimmed, "/") {
		if component == "" || component == "." || component == ".." {
			return "", fmt.Errorf("mirror: invalid object prefix %q", raw)
		}
	}
	return trimmed + "/", nil
}

func parseEndpoint(raw string) (string, bool, error) {
	if raw == "" {
		return "", false, fmt.Errorf("mirror: endpoint is empty")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}

	u, err := url.Parse(raw)
	if err != nil {
		return "", false, fmt.Errorf("mirror: parse endpoint: %w", err)
	}
	if u.Host == "" || u.Path != "" && u.Path != "/" || u.RawQuery != "" || u.Fragment != "" {
		return "", false, fmt.Errorf("mirror: endpoint must contain only a host and optional http(s) scheme")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", false, fmt.Errorf("mirror: endpoint scheme must be http or https")
	}
	return u.Host, u.Scheme == "https", nil
}

type s3Backend struct {
	client *minio.Client
	bucket string
}

func (b *s3Backend) List(ctx context.Context, prefix string) ([]RemoteObject, error) {
	objects := make([]RemoteObject, 0)
	for object := range b.client.ListObjects(ctx, b.bucket, minio.ListObjectsOptions{
		Prefix:    prefix,
		Recursive: true,
	}) {
		if object.Err != nil {
			return nil, object.Err
		}
		objects = append(objects, RemoteObject{Key: object.Key, Size: object.Size})
	}
	return objects, nil
}

func (b *s3Backend) Put(ctx context.Context, key string, body io.Reader, size int64, contentType string) error {
	_, err := b.client.PutObject(ctx, b.bucket, key, body, size, minio.PutObjectOptions{
		ContentType: contentType,
	})
	return err
}

func (b *s3Backend) Delete(ctx context.Context, keys []string) error {
	const maxBatch = 1000
	for start := 0; start < len(keys); start += maxBatch {
		end := start + maxBatch
		if end > len(keys) {
			end = len(keys)
		}

		objects := make(chan minio.ObjectInfo, end-start)
		for _, key := range keys[start:end] {
			objects <- minio.ObjectInfo{Key: key}
		}
		close(objects)

		var deleteErrors []error
		for result := range b.client.RemoveObjects(ctx, b.bucket, objects, minio.RemoveObjectsOptions{}) {
			if result.Err != nil {
				deleteErrors = append(deleteErrors, fmt.Errorf("%s: %w", result.ObjectName, result.Err))
			}
		}
		if len(deleteErrors) > 0 {
			return fmt.Errorf("delete %d object(s): %w", len(deleteErrors), errors.Join(deleteErrors...))
		}
	}
	return nil
}
