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
	if len(positional) != 0 || (opts.scip == "") == (opts.lspFacts == "") {
		fmt.Fprintf(stderr, "adapter needs exactly one of --scip FILE or --lsp-facts FILE and no positional arguments\n\n%s", usage)
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

	var statsList []adapter.Stats
	if opts.scip != "" {
		index, err := adapter.LoadSCIP(opts.scip)
		if err != nil {
			fmt.Fprintf(stderr, "adapter: %v\n", err)
			if errors.Is(err, adapter.ErrNotFound) {
				return 3
			}
			return 1
		}
		statsList, err = adapter.ApplySCIPAll(ctx, st.Store, index)
		if err != nil {
			fmt.Fprintf(stderr, "adapter: %v\n", err)
			return 1
		}
	} else {
		facts, err := adapter.LoadLSPFacts(opts.lspFacts)
		if err != nil {
			fmt.Fprintf(stderr, "adapter: %v\n", err)
			if errors.Is(err, adapter.ErrNotFound) {
				return 3
			}
			return 1
		}
		stats, err := adapter.ApplyLSPFacts(ctx, st.Store, facts)
		if err != nil {
			fmt.Fprintf(stderr, "adapter: %v\n", err)
			return 1
		}
		statsList = []adapter.Stats{stats}
	}

	if opts.json {
		var payload any = statsList
		if len(statsList) == 1 {
			// Single-language indexes keep the original object shape.
			payload = statsList[0]
		} else {
			payload = struct {
				Adapters []adapter.Stats `json:"adapters"`
			}{statsList}
		}
		if err := writeJSON(stdout, payload); err != nil {
			fmt.Fprintf(stderr, "encode adapter: %v\n", err)
			return 1
		}
		return 0
	}
	for _, stats := range statsList {
		fmt.Fprintf(stdout, "%s adapter: %d definitions · %d references · %d matched · %d replaced · %d edges · %d conflicts · %d skipped · %d unmatched\n",
			stats.Adapter, stats.Definitions, stats.References, stats.Matched, stats.Replaced, stats.Edges, stats.Conflicts, stats.Skipped, stats.Unmatched)
	}
	return 0
}
