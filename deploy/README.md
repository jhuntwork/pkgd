# pkgd deployment

Files in this directory:

- `pkgd.env.example` — example environment file (copy to
  `/etc/pkgd/pkgd.env`, fill in real values, `chmod 600`).
- `nginx.conf.example` — example reverse-proxy config that terminates TLS
  and proxies to pkgd's Unix socket.

## Mere Linux has no systemd

`pkgs.merelinux.org` runs Mere Linux, and Mere has no systemd support — a
deliberate, permanent design stance, not a gap. Service management is
native: a package's `recipe.kdl` declares a `service` node (`type "daemon"`,
`command`, `depends-on`, etc.), which `mere` compiles into s6-rc source
directories at build time; `mere enable`/`mere start`/`mere status` operate
on services discovered from installed packages (see
`codeberg.org/merelinux/mere`'s `docs/design/recipe_spec.md`, "`service`
Node" section, and `src/s6rc.zig`).

`pkgd` is not yet packaged as a proper Mere recipe — that's real follow-up
work (packaging it as an actual `mere` package, dogfooding the same system
it publishes packages for, would be the clean long-term answer: `mere
install pkgd`, `mere enable pkgd`, `mere start pkgd` would then just work
like any other service). Until then, deployment is manual: run the built
binary directly, with its environment sourced from `pkgd.env` (see the
"Basic setup outline" below). Do not write a systemd unit for this — there
will never be a systemd on this box.

## One-time migration concern before cutover

The current production `repo.db` at `pkgs.merelinux.org` was built by the
old rsync+SSH CI process, which published packages directly, without ever
going through a dev repo. pkgd's model is different: the persistent dev
repo (`PKGD_MERE_ROOT` + `PKGD_DEV_REPO_NAME`) is the sole source of truth
going forward, and `mere release publish` only ever promotes *that* dev
repo's state into the production output directory.

**Before switching CI over to pkgd, someone needs to import the packages
currently in production into pkgd's persistent dev repo first** (e.g. via
`mere --root <PKGD_MERE_ROOT> dev import <PKGD_DEV_REPO_NAME> <archives...>`
against the existing production archives), so the dev repo's state is a
superset of what's already live. Otherwise the first `mere release publish`
pkgd triggers will overwrite `repo.db` with a dev repo that's missing every
previously-published package.

This migration is an operational, one-time step — pkgd does not include
tooling to perform it automatically.

## Basic setup outline

1. Create the `pkgd` system user/group.
2. Create and chown `PKGD_MERE_ROOT`, `PKGD_OUTPUT_DIR`, and `PKGD_TEMP_DIR`
   (see `pkgd.env.example`). Do **not** pre-create the dev repo's own leaf
   directory (`<PKGD_MERE_ROOT>/mere/dev/repo/<PKGD_DEV_REPO_NAME>/`) — `mere
   dev import` only auto-bootstraps a dev repo that doesn't exist yet when
   addressed by `--root` + name; a pre-existing empty directory there is
   treated as an invalid repo instead of something to initialize.
3. Perform the one-time dev-repo migration described above.
4. Install the `pkgd` binary to `/usr/local/bin/pkgd`.
5. Copy `pkgd.env.example` to `/etc/pkgd/pkgd.env`, fill in real values.
6. Run `pkgd` with that environment loaded (however process supervision is
   handled on this box until it's packaged as a proper Mere service — see
   above).
7. Install the nginx config from `nginx.conf.example`, making sure nginx's
   worker user can reach `/run/pkgd/pkgd.sock` (group membership, matching
   the socket's `0660` permissions pkgd sets at startup).
