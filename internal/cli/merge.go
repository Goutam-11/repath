package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/Goutam-11/repath/internal/artifact"
	"github.com/Goutam-11/repath/internal/merge"
)

// mergeCmd builds one read-only index from several single-repo artifacts.
// Inputs are artifact directories (or databases beside a manifest); the output
// is a new artifact directory whose manifest lists every part.
func mergeCmd(args []string, stdout, stderr io.Writer) int {
	opts, positional, err := parseOptions(args)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n\n%s", err, usage)
		return 2
	}
	if len(positional) == 0 {
		fmt.Fprintf(stderr, "merge needs at least one artifact directory\n\n%s", usage)
		return 2
	}
	if opts.out == "" {
		fmt.Fprintf(stderr, "merge needs --out DIR\n\n%s", usage)
		return 2
	}

	result, err := merge.Run(context.Background(), positional, opts.out, version)
	if err != nil {
		fmt.Fprintf(stderr, "merge: %v\n", err)
		switch {
		case errors.Is(err, artifact.ErrNotFound):
			return 3
		case errors.Is(err, merge.ErrExists):
			return 2
		default:
			return 1
		}
	}

	if opts.json {
		if err := writeJSON(stdout, result); err != nil {
			fmt.Fprintf(stderr, "merge: %v\n", err)
			return 1
		}
		return 0
	}

	fmt.Fprintf(stdout, "merged %d parts into %s\n", len(result.Parts), result.OutDir)
	for _, p := range result.Parts {
		fmt.Fprintf(stdout, "  %-28s %6d files %7d nodes %7d edges %6d pending\n",
			p.Name, p.Files, p.Nodes, p.Edges, p.Pending)
	}
	fmt.Fprintf(stdout, "total: %d files, %d nodes, %d edges, %d pending, %d entrypoints\n",
		result.Files, result.Nodes, result.Edges, result.Pending, result.Entrypoints)
	fmt.Fprintf(stdout, "cross-repo links: %d (%s at %.2f)\n",
		result.CrossRepoEdges, merge.CrossRepoRule, merge.CrossRepoConfidence)
	fmt.Fprintf(stdout, "artifact: %s\n", result.ManifestPath)
	return 0
}
