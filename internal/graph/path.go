package graph

import (
	"context"
	"fmt"

	"github.com/Goutam-11/repath/internal/store"
)

// Hop is one edge on a connection path.
type Hop struct {
	Src        Endpoint `json:"from"`
	Dst        Endpoint `json:"to"`
	Kind       string   `json:"kind"`
	Confidence float64  `json:"confidence"`
	Rule       string   `json:"rule"`
	Line       int      `json:"line"`
}

// PathResult is a connection from one symbol to another.
type PathResult struct {
	From      Endpoint `json:"from"`
	To        Endpoint `json:"to"`
	Hops      []Hop    `json:"hops"`
	Alternate [][]Hop  `json:"alternates,omitempty"`
	// Frontier is filled when no path exists within the depth limit: what each
	// end can reach toward the other.
	FrontierA []Endpoint `json:"frontier_from,omitempty"`
	FrontierB []Endpoint `json:"frontier_to,omitempty"`
}

// Connect finds the shortest directed path from → to, then up to alternates
// further simple paths by banning one edge of the found path at a time.
func Connect(ctx context.Context, st *store.Store, from, to store.NodeRow, maxDepth int, minConfidence float64, alternates int) (PathResult, error) {
	paths, err := st.FilePaths(ctx)
	if err != nil {
		return PathResult{}, err
	}

	result := PathResult{From: endpointOf(from, paths), To: endpointOf(to, paths)}

	edges, ok, err := shortestPath(ctx, st, from.ID, to.ID, maxDepth, minConfidence, nil)
	if err != nil {
		return result, err
	}
	if !ok {
		result.FrontierA, result.FrontierB, err = frontier(ctx, st, from.ID, to.ID, maxDepth, minConfidence)
		return result, err
	}
	result.Hops = toHops(edges, paths)

	banned := make(map[[2]int64]bool)
	seen := map[string]bool{pathKey(result.Hops): true}
	for i := 0; i < len(result.Hops) && len(result.Alternate) < alternates; i++ {
		banned[[2]int64{result.Hops[i].Src.ID, result.Hops[i].Dst.ID}] = true
		altEdges, found, err := shortestPath(ctx, st, from.ID, to.ID, maxDepth, minConfidence, banned)
		if err != nil {
			return result, err
		}
		if !found {
			continue
		}
		alt := toHops(altEdges, paths)
		key := pathKey(alt)
		if seen[key] {
			continue
		}
		seen[key] = true
		result.Alternate = append(result.Alternate, alt)
	}

	return result, nil
}

// shortestPath is a BFS over forward edges with an optional edge ban list.
func shortestPath(ctx context.Context, st *store.Store, from, to int64, maxDepth int, minConfidence float64, banned map[[2]int64]bool) ([]store.EdgeOut, bool, error) {
	type node struct {
		id    int64
		depth int
	}

	parent := make(map[int64]store.EdgeOut)
	seen := map[int64]bool{from: true}
	queue := []node{{id: from}}

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if current.depth >= maxDepth {
			continue
		}

		edges, err := st.Outgoing(ctx, []int64{current.id}, minConfidence)
		if err != nil {
			return nil, false, err
		}
		for _, e := range edges {
			if banned[[2]int64{e.Src, e.Dst}] || seen[e.Dst] {
				continue
			}
			seen[e.Dst] = true
			parent[e.Dst] = e
			if e.Dst == to {
				return reconstruct(parent, from, to), true, nil
			}
			queue = append(queue, node{id: e.Dst, depth: current.depth + 1})
		}
	}

	return nil, false, nil
}

func reconstruct(parent map[int64]store.EdgeOut, from, to int64) []store.EdgeOut {
	var reversed []store.EdgeOut
	for current := to; current != from; {
		edge, ok := parent[current]
		if !ok {
			return nil
		}
		reversed = append(reversed, edge)
		current = edge.Src
	}
	out := make([]store.EdgeOut, 0, len(reversed))
	for i := len(reversed) - 1; i >= 0; i-- {
		out = append(out, reversed[i])
	}
	return out
}

func toHops(edges []store.EdgeOut, paths map[int64]string) []Hop {
	hops := make([]Hop, 0, len(edges))
	for _, e := range edges {
		hops = append(hops, Hop{
			Src:        endpointOf(e.SrcNode, paths),
			Dst:        endpointOf(e.DstNode, paths),
			Kind:       e.Kind,
			Confidence: e.Confidence,
			Rule:       e.Rule,
			Line:       e.Line,
		})
	}
	return hops
}

// frontier reports what each end can reach within the depth limit, which is
// more useful than "no path" alone.
func frontier(ctx context.Context, st *store.Store, from, to int64, maxDepth int, minConfidence float64) ([]Endpoint, []Endpoint, error) {
	paths, err := st.FilePaths(ctx)
	if err != nil {
		return nil, nil, err
	}

	forward, err := reachable(ctx, st, from, maxDepth, minConfidence)
	if err != nil {
		return nil, nil, err
	}
	backward, err := reverseReachable(ctx, st, to, maxDepth, minConfidence)
	if err != nil {
		return nil, nil, err
	}

	return trim(forward, 5, paths), trim(backward, 5, paths), nil
}

func reachable(ctx context.Context, st *store.Store, from int64, maxDepth int, minConfidence float64) ([]store.NodeRow, error) {
	seen := map[int64]bool{from: true}
	frontier := []int64{from}
	var reached []store.NodeRow

	for depth := 0; depth < maxDepth && len(frontier) > 0; depth++ {
		edges, err := st.Outgoing(ctx, frontier, minConfidence)
		if err != nil {
			return nil, err
		}
		next := make([]int64, 0, len(edges))
		for _, e := range edges {
			if seen[e.Dst] {
				continue
			}
			seen[e.Dst] = true
			reached = append(reached, e.DstNode)
			next = append(next, e.Dst)
		}
		frontier = next
	}
	return reached, nil
}

func reverseReachable(ctx context.Context, st *store.Store, to int64, maxDepth int, minConfidence float64) ([]store.NodeRow, error) {
	seen := map[int64]bool{to: true}
	frontier := []int64{to}
	var reached []store.NodeRow

	for depth := 0; depth < maxDepth && len(frontier) > 0; depth++ {
		edges, err := st.Incoming(ctx, frontier, minConfidence)
		if err != nil {
			return nil, err
		}
		next := make([]int64, 0, len(edges))
		for _, e := range edges {
			if seen[e.Src] {
				continue
			}
			seen[e.Src] = true
			reached = append(reached, e.SrcNode)
			next = append(next, e.Src)
		}
		frontier = next
	}
	return reached, nil
}

func trim(nodes []store.NodeRow, limit int, paths map[int64]string) []Endpoint {
	out := make([]Endpoint, 0, limit)
	for i, n := range nodes {
		if i >= limit {
			break
		}
		out = append(out, endpointOf(n, paths))
	}
	return out
}

func pathKey(hops []Hop) string {
	key := ""
	for _, hop := range hops {
		key += fmt.Sprintf("%d>%d;", hop.Src.ID, hop.Dst.ID)
	}
	return key
}
