package publish

// This file isolates the exact argv shapes pkgd uses to invoke the `mere`
// binary, so adjusting flag names or argument order later is a one-place
// change.

// mereDevImportArgs builds the argv for merging newly-uploaded archives into
// the persistent server-side dev repo: `mere --root <mere-root> dev import
// <dev-repo-name> <archive>...`.
//
// The dev repo is addressed by --root + bare name, not by its resolved
// absolute path: `mere dev import` only auto-bootstraps a dev repo that
// doesn't exist yet when addressed this way. Passing the resolved absolute
// path directly (config.Config.DevRepoPath()) fails instead of creating it
// on a repo's first-ever publish.
func mereDevImportArgs(mereRoot, devRepoName string, archives []string) []string {
	args := make([]string, 0, 5+len(archives))
	args = append(args, "--root", mereRoot, "dev", "import", devRepoName)
	args = append(args, archives...)
	return args
}

// mereReleasePublishArgs builds the argv for promoting the dev repo's full
// current state into the production output directory: `mere release
// publish --dev-repo <dev-repo-path> --output <output-dir> [--key
// <key-path>]`.
func mereReleasePublishArgs(devRepoPath, outputDir, keyPath string) []string {
	args := []string{"release", "publish", "--dev-repo", devRepoPath, "--output", outputDir}
	if keyPath != "" {
		args = append(args, "--key", keyPath)
	}
	return args
}
