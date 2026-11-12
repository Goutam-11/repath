package query

import (
	"context"
	"fmt"
	"sort"

	"github.com/Goutam-11/repath/internal/graph"
	"github.com/Goutam-11/repath/internal/store"
)

// FlowsAnswer carries the matched entrypoint and its forward traversal.
type FlowsAnswer struct {
	Target Target
	Result graph.FlowResult
}

// Flows resolves one entrypoint by qname or name and walks its outgoing edges.
// A symbol that is not an entrypoint returns 3; several matching entrypoints
// return 4 with candidates.
func Flows(ctx context.Context, st *store.Store, arg string, depth int, minConfidence float64, limit int) (FlowsAnswer, int, error) {
	answer := FlowsAnswer{Target: Target{Label: arg}}
	entries, err := st.Entrypoints(ctx)
	if err != nil {
		return answer, 1, err
	}

	var exact []store.Entrypoint
	var named []store.Entrypoint
	for _, entry := range entries {
		if entry.Node.QName == arg {
			exact = append(exact, entry)
		}
		if entry.Node.Name == arg {
			named = append(named, entry)
		}
	}
	matches := exact
	if len(matches) == 0 {
		matches = named
	}
	if len(matches) == 0 {
		return answer, 3, nil
	}

	// Several rules can anchor the same node (for example a Rust main that is
	// also a test). Only different nodes are ambiguous.
	byNode := make(map[int64]store.Entrypoint)
	for _, match := range matches {
		if _, ok := byNode[match.NodeID]; !ok {
			byNode[match.NodeID] = match
		}
	}
	if len(byNode) > 1 {
		answer.Target.Candidates = make([]store.NodeRow, 0, len(byNode))
		for _, match := range byNode {
			answer.Target.Candidates = append(answer.Target.Candidates, match.Node)
		}
		sort.Slice(answer.Target.Candidates, func(i, j int) bool {
			a, b := answer.Target.Candidates[i], answer.Target.Candidates[j]
			if a.QName != b.QName {
				return a.QName < b.QName
			}
			if a.FileID != b.FileID {
				return a.FileID < b.FileID
			}
			return a.StartLine < b.StartLine
		})
		return answer, 4, nil
	}

	// Entrypoint rows are ordered by file, line, rule, so the first match is a
	// deterministic rule choice when one node has several anchors.
	chosen := matches[0]
	answer.Target.Nodes = []store.NodeRow{chosen.Node}
	result, err := graph.Flows(ctx, st, chosen.Node, chosen.Rule, depth, minConfidence, limit)
	if err != nil {
		return answer, 1, err
	}
	answer.Result = result
	return answer, 0, nil
}

// EntrypointSummary formats one entrypoint for text output.
func EntrypointSummary(entry store.Entrypoint) string {
	return fmt.Sprintf("%s (%s, %s:%d)", entry.Node.QName, entry.Rule, entry.File, entry.Line)
}
