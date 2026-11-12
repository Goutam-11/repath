package graph

import (
	"context"

	"github.com/Goutam-11/repath/internal/store"
)

// FlowStep is one forward edge in an execution path.
type FlowStep struct {
	Endpoint
	Depth      int     `json:"depth"`
	EdgeKind   string  `json:"edge_kind"`
	Confidence float64 `json:"confidence"`
	Rule       string  `json:"rule"`
	CallLine   int     `json:"call_line"`
}

// FlowResult is one bounded forward traversal from an entrypoint. Paths are
// leaf paths (a chain per path); Frontier names leaves that did not continue at
// the confidence floor or depth budget.
type FlowResult struct {
	Entrypoint     Endpoint     `json:"entrypoint"`
	EntrypointRule string       `json:"entrypoint_rule"`
	Paths          [][]FlowStep `json:"paths"`
	Frontier       []Endpoint   `json:"frontier,omitempty"`
	Depth          int          `json:"depth"`
	MinConfidence  float64      `json:"min_confidence"`
	Truncated      bool         `json:"truncated"`
}

// Flows walks outgoing edges from one entrypoint. It is cycle-safe, bounds both
// depth and path count, and returns the leaves as the nearest frontier.
func Flows(ctx context.Context, st *store.Store, entrypoint store.NodeRow, rule string, maxDepth int, minConfidence float64, limit int) (FlowResult, error) {
	pathsByID, err := st.FilePaths(ctx)
	if err != nil {
		return FlowResult{}, err
	}
	if maxDepth <= 0 {
		maxDepth = 8
	}
	if limit <= 0 {
		limit = 50
	}

	result := FlowResult{
		Entrypoint:     endpointOf(entrypoint, pathsByID),
		EntrypointRule: rule,
		Paths:          make([][]FlowStep, 0),
		Depth:          maxDepth,
		MinConfidence:  minConfidence,
	}

	visited := map[int64]bool{entrypoint.ID: true}
	parent := map[int64]flowEdge{}
	frontier := []int64{entrypoint.ID}
	leaves := []int64{entrypoint.ID}
	truncatedByDepth := false

	for depth := 1; depth <= maxDepth; depth++ {
		edges, err := st.Outgoing(ctx, frontier, minConfidence)
		if err != nil {
			return FlowResult{}, err
		}
		next := make([]int64, 0, len(edges))
		for _, edge := range edges {
			if visited[edge.Dst] {
				continue
			}
			visited[edge.Dst] = true
			parent[edge.Dst] = flowEdge{from: edge.Src, step: FlowStep{
				Endpoint:   endpointOf(edge.DstNode, pathsByID),
				Depth:      depth,
				EdgeKind:   edge.Kind,
				Confidence: edge.Confidence,
				Rule:       edge.Rule,
				CallLine:   edge.Line,
			}}
			next = append(next, edge.Dst)
		}
		if len(next) == 0 {
			leaves = frontier
			break
		}
		frontier = next
		leaves = next
		if depth == maxDepth {
			truncatedByDepth = true
		}
	}

	result.Truncated = truncatedByDepth
	for _, leaf := range leaves {
		if leaf == entrypoint.ID {
			continue
		}
		path, ok := flowPath(leaf, parent)
		if !ok {
			continue
		}
		if len(result.Paths) >= limit {
			result.Truncated = true
			break
		}
		result.Paths = append(result.Paths, path)
	}
	if len(result.Paths) == 0 {
		result.Frontier = []Endpoint{result.Entrypoint}
		return result, nil
	}
	for _, leaf := range leaves {
		if leaf == entrypoint.ID {
			continue
		}
		result.Frontier = append(result.Frontier, endpointOfID(leaf, parent))
	}
	return result, nil
}

type flowEdge struct {
	from int64
	step FlowStep
}

func flowPath(leaf int64, parent map[int64]flowEdge) ([]FlowStep, bool) {
	var reversed []FlowStep
	for current := leaf; ; {
		edge, ok := parent[current]
		if !ok {
			break
		}
		reversed = append(reversed, edge.step)
		current = edge.from
		if len(reversed) > 64 {
			return nil, false
		}
	}
	if len(reversed) == 0 {
		return nil, false
	}
	for i, j := 0, len(reversed)-1; i < j; i, j = i+1, j-1 {
		reversed[i], reversed[j] = reversed[j], reversed[i]
	}
	return reversed, true
}

func endpointOfID(id int64, parent map[int64]flowEdge) Endpoint {
	return parent[id].step.Endpoint
}
