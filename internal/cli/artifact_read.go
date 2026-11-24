package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/Goutam-11/repath/internal/artifact"
	"github.com/Goutam-11/repath/internal/query"
	"github.com/Goutam-11/repath/internal/store"
)

// artifactAnswerCommands are the commands where --artifact has a meaning. For
// the read commands it selects a read-only input; for `index` it is the export
// output (a write) and for `export`/`map --md` it is the existing read seam.
// Every other command refuses the flag with exit 2 instead of silently ignoring
// it.
var artifactAnswerCommands = map[string]bool{
	"index": true, "find": true, "callers": true, "impact": true,
	"path": true, "map": true, "status": true, "flows": true,
	"tests": true, "export": true,
}

// rejectUnsupportedArtifact returns exit 2 when a command that has no
// --artifact meaning receives one. The check runs before option parsing so the
// answer is the same for `--artifact DIR` and `--artifact=DIR`.
func rejectUnsupportedArtifact(args []string, stderr io.Writer) int {
	if len(args) == 0 || artifactAnswerCommands[args[0]] {
		return 0
	}
	for _, arg := range args[1:] {
		if arg == "--artifact" || strings.HasPrefix(arg, "--artifact=") {
			fmt.Fprintf(stderr, "--artifact is not supported by %q; it is accepted by index (output), export, map, find, callers, impact, path, status, flows, and tests\n\n%s", args[0], usage)
			return 2
		}
	}
	return 0
}

// openAnswerStore opens the graph a read command answers from: either a
// read-only artifact opened in place (no pull, no refresh, nothing written
// beside it) or the live `.repath` index, built lazily when noBuild is false.
//
// The returned root is what the answer reports as the tree it describes. For an
// artifact that is the checkout recorded in its manifest (export writes it), so
// a single-repo artifact answers byte-identically to the live index it was
// exported from. A merged artifact — or one exported before the field existed —
// records no root, and the answer reports none instead of guessing from the
// artifact's own directory. The caller must defer cleanup.
func openAnswerStore(ctx context.Context, opts options, noBuild bool, stderr io.Writer) (*store.Store, func(), string, int) {
	if opts.artifact != "" {
		st, done, manifest, err := artifact.OpenReadOnly(ctx, opts.artifact)
		if err != nil {
			fmt.Fprintf(stderr, "artifact: %v\n", err)
			if errors.Is(err, artifact.ErrNotFound) {
				return nil, func() {}, "", 3
			}
			return nil, func() {}, "", 1
		}
		// The manifest records the checkout the snapshot describes. Without it
		// (a merged artifact, or one exported before the field existed) there is
		// no checkout to report or probe; the answer says so instead of guessing
		// from the artifact's own directory.
		return st, done, manifest.Root, 0
	}
	root, err := query.ResolveRoot(opts.root)
	if err != nil {
		fmt.Fprintf(stderr, "resolve root: %v\n", err)
		return nil, func() {}, "", 1
	}
	session, err := query.Open(ctx, root, noBuild)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return nil, func() {}, "", 1
	}
	return session.Store, func() { _ = session.Close() }, root, 0
}

// openDiffStore opens the graph for a `--diff` answer. The checkout supplies the
// change set and the artifact supplies the graph, so a CI job can diff a
// pre-built snapshot without seeding `.repath/`; the live path refreshes first
// so the answer describes the current tree.
func openDiffStore(ctx context.Context, opts options, root string, stderr io.Writer) (*store.Store, func(), int) {
	if opts.artifact != "" {
		st, done, _, err := artifact.OpenReadOnly(ctx, opts.artifact)
		if err != nil {
			fmt.Fprintf(stderr, "artifact: %v\n", err)
			if errors.Is(err, artifact.ErrNotFound) {
				return nil, func() {}, 3
			}
			return nil, func() {}, 1
		}
		return st, done, 0
	}
	session, err := query.Open(ctx, root, false)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return nil, func() {}, 1
	}
	st := session.Store
	// Diff answers must describe the current tree, not a stale snapshot.
	if _, err := query.Status(ctx, st, root, true, false); err != nil {
		_ = session.Close()
		fmt.Fprintf(stderr, "refresh: %v\n", err)
		return nil, func() {}, 1
	}
	return st, func() { _ = session.Close() }, 0
}
