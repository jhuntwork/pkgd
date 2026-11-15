# pkgd

`pkgd` is the network-facing Go service for the Mere Linux package
repository at `pkgs.merelinux.org`. It runs behind nginx (which terminates
TLS) on a Unix socket, and does two things:

1. **Serves the published repo** — `repo.db`, `repo.db.sig`, and
   `packages/*` — from a production output directory, with Range-request
   support for resumable downloads.
2. **Accepts new package publishes from CI** at `POST /publish`, merges
   them into a persistent, server-owned "dev repo" via the `mere` CLI, and
   promotes that dev repo's current state into the production output
   directory.

It deliberately does **not** reimplement repo-database mutation itself:
that correctness-critical logic (atomic `flock` + staged-rename
publishing) lives in the `mere` binary (Zig), via
`mere release publish --dev-repo <path> --output <dir> [--key <path>]`.
`pkgd` only shells out to it.

## Layout

```
cmd/pkgd/            main entrypoint: config load, HTTP routing, Unix
                      socket listener, graceful shutdown
internal/config/     Config struct + env-var loading/validation
internal/publish/    POST /publish: auth, upload staging, mere subprocess
                      orchestration
internal/serve/      GET /{repo.db,repo.db.sig,packages/*}: static file
                      serving with Range support and path-traversal safety
deploy/              env file example, nginx reverse-proxy example,
                      migration notes (Mere Linux has no systemd — see
                      deploy/README.md)
```

Future work (telemetry, a package browser) is expected to add more HTTP
handlers on the same `http.ServeMux` in `cmd/pkgd/main.go`, and a
`stats`/`packages` data layer alongside `internal/publish` and
`internal/serve` — this repo intentionally leaves room for that rather
than hardcoding a single-handler assumption anywhere.

## Configuration

Environment variables (see `internal/config/config.go` for the full
list): `PKGD_MERE_ROOT`, `PKGD_DEV_REPO_NAME`, `PKGD_OUTPUT_DIR`,
`PKGD_MERE_BIN`, `PKGD_SIGNING_KEY_PATH`, `PKGD_PUBLISH_TOKEN`,
`PKGD_LISTEN_SOCKET`, `PKGD_TEMP_DIR`, and the optional `PKGD_MIRROR_*`
settings.

## Backup mirror

When the `PKGD_MIRROR_*` settings are present, pkgd asynchronously
reconciles every regular file under `PKGD_OUTPUT_DIR` with an
S3-compatible object-store prefix. The local Mere publish remains the source
of truth: a mirror outage does not fail `POST /publish`. The worker retries
failed syncs at `PKGD_MIRROR_RETRY_INTERVAL` and performs a full repair sync
when pkgd starts.

The synchronizer uploads local files, then deletes remote objects under its
configured prefix that are absent from `PKGD_OUTPUT_DIR`. Package archives
are treated as immutable because Mere gives them content-addressed names;
`repo.db` and other non-package files are replaced on each sync.

Backblaze B2 application keys are used as the S3 access key ID and secret
access key. The key must be restricted to the backup bucket and have only the
permissions needed to list, read, write, and delete objects there. Keep the
secret out of GitHub Actions and out of process command lines.

The dev repo is addressed by `--root` (`PKGD_MERE_ROOT`) + bare name
(`PKGD_DEV_REPO_NAME`, default `"pkgd"`), not by its resolved absolute
path — `mere dev import` only auto-bootstraps a dev repo that doesn't exist
yet when addressed this way. `Config.DevRepoPath()` computes the resolved
path for the (unconditionally required, since `mere release publish` takes
a literal path) `--dev-repo` flag. There is no separate archive-pool
setting — `mere` owns and manages `packages/` under the dev repo itself.

## `POST /publish`

Authenticated with `Authorization: Bearer <PKGD_PUBLISH_TOKEN>`.

Body is `multipart/form-data` with one or more file parts named
`"package"`, one per `.pkg.tar.zst` archive to publish:

```
--boundary
Content-Disposition: form-data; name="package"; filename="foo-1.2.3-1.pkg.tar.zst"
Content-Type: application/octet-stream

<raw archive bytes>
--boundary--
```

Filenames must be bare basenames ending in `.pkg.tar.zst`; the server
rejects anything containing a path separator or `..`. Response is JSON:

```json
{
  "success": true,
  "packages": ["foo-1.2.3-1.pkg.tar.zst"],
  "import": {"args": ["dev", "import", "..."], "exit_code": 0, "stdout": "...", "stderr": ""},
  "publish": {"args": ["release", "publish", "..."], "exit_code": 0, "stdout": "...", "stderr": ""}
}
```

On failure, `"success"` is `false`, `"stage"` names which step failed
(`"import"` or `"publish"`), and `"reason"` carries a short excerpt of the
failing step's stderr. A failing `mere dev import` short-circuits: `mere
release publish` is never run and `"publish"` is omitted from the
response.

## Building and testing

```
go build ./...
go vet ./...
go test ./...
```

## Deployment

See `deploy/README.md`, including an operational note on the one-time
dev-repo migration required before cutting production CI over to pkgd.
