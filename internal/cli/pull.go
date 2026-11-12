package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/Goutam-11/repath/internal/artifact"
	"github.com/Goutam-11/repath/internal/query"
)

func pullCmd(args []string, stdout, stderr io.Writer) int {
	opts, positional, err := parseOptions(args)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n\n%s", err, usage)
		return 2
	}
	if len(positional) != 1 {
		fmt.Fprintf(stderr, "pull needs exactly one artifact path or URL\n\n%s", usage)
		return 2
	}

	root, err := query.ResolveRoot(opts.root)
	if err != nil {
		fmt.Fprintf(stderr, "resolve root: %v\n", err)
		return 1
	}

	ctx := context.Background()
	manifest, err := artifact.Pull(ctx, positional[0], root, version)
	if err != nil {
		fmt.Fprintf(stderr, "pull: %v\n", err)
		if errors.Is(err, artifact.ErrNotFound) {
			return 3
		}
		return 1
	}

	// Report the index as it now stands: pulling into a checkout overlays the
	// dirty files, so the manifest counts may no longer be the whole answer.
	st, err := query.Open(ctx, root, true)
	if err != nil {
		fmt.Fprintf(stderr, "pull: %v\n", err)
		return 1
	}
	defer st.Close()
	counts, err := st.Store.Counts(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "pull: %v\n", err)
		return 1
	}
	edges, err := st.Store.EdgeCounts(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "pull: %v\n", err)
		return 1
	}
	totalEdges := 0
	for _, n := range edges {
		totalEdges += n
	}

	if opts.json {
		payload := struct {
			Root     string            `json:"root"`
			Artifact artifact.Manifest `json:"artifact"`
			Files    int               `json:"files"`
			Nodes    int               `json:"nodes"`
			Edges    int               `json:"edges"`
		}{root, manifest, counts.Files, counts.Nodes, totalEdges}
		if err := writeJSON(stdout, payload); err != nil {
			fmt.Fprintf(stderr, "encode pull: %v\n", err)
			return 1
		}
		return 0
	}

	fmt.Fprintf(stdout, "pulled %s (schema v%d · commit %s · %d files · %d nodes · %d edges)\n",
		positional[0], manifest.SchemaVersion, shortSHA(manifest.Commit),
		counts.Files, counts.Nodes, totalEdges)
	return 0
}
