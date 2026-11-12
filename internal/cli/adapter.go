package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/Goutam-11/repath/internal/adapter"
	"github.com/Goutam-11/repath/internal/query"
)

func adapterCmd(args []string, stdout, stderr io.Writer) int {
	opts, positional, err := parseOptions(args)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n\n%s", err, usage)
		return 2
	}
	if len(positional) != 0 || opts.scip == "" {
		fmt.Fprintf(stderr, "adapter needs exactly one --scip FILE and no positional arguments\n\n%s", usage)
		return 2
	}

	root, err := query.ResolveRoot(opts.root)
	if err != nil {
		fmt.Fprintf(stderr, "resolve root: %v\n", err)
		return 1
	}

	ctx := context.Background()
	st, err := query.Open(ctx, root, false)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	defer st.Close()

	index, err := adapter.LoadSCIP(opts.scip)
	if err != nil {
		fmt.Fprintf(stderr, "adapter: %v\n", err)
		if errors.Is(err, adapter.ErrNotFound) {
			return 3
		}
		return 1
	}
	stats, err := adapter.ApplySCIP(ctx, st.Store, index)
	if err != nil {
		fmt.Fprintf(stderr, "adapter: %v\n", err)
		return 1
	}

	if opts.json {
		if err := writeJSON(stdout, stats); err != nil {
			fmt.Fprintf(stderr, "encode adapter: %v\n", err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "scip adapter: %d definitions · %d references · %d matched · %d edges · %d conflicts · %d skipped · %d unmatched\n",
		stats.Definitions, stats.References, stats.Matched, stats.Edges, stats.Conflicts, stats.Skipped, stats.Unmatched)
	return 0
}
