package graph

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/Goutam-11/repath/internal/store"
)

// Ranked is one symbol with its PageRank score.
type Ranked struct {
	Endpoint
	Rank    float64 `json:"rank"`
	Callers int     `json:"callers"`
}

// ModuleStat is one module's rollup.
type ModuleStat struct {
	Name    string   `json:"name"`
	Symbols int      `json:"symbols"`
	Deps    []string `json:"deps"`
}

// MapResult is the repository overview.
type MapResult struct {
	Modules         []ModuleStat   `json:"modules"`
	Cycles          [][]string     `json:"cycles"`
	Top             []Ranked       `json:"top"`
	Entrypoints     int            `json:"entrypoints"`
	EntrypointRules map[string]int `json:"entrypoint_rules,omitempty"`
	Truncated       bool           `json:"truncated"`
}

// BuildMap ranks symbols with PageRank over call edges, rolls modules up into a
// dependency graph, and finds cycles. The symbol list is trimmed to the token
// budget; the module section always fits because it is small.
func BuildMap(ctx context.Context, st *store.Store, maxTokens int, minConfidence float64) (MapResult, error) {
	nodes, err := st.Nodes(ctx)
	if err != nil {
		return MapResult{}, err
	}
	all, err := st.GraphEdgeSkeletons(ctx, minConfidence)
	if err != nil {
		return MapResult{}, err
	}
	paths, err := st.FilePaths(ctx)
	if err != nil {
		return MapResult{}, err
	}
	moduleOf, moduleNames, err := moduleLookup(ctx, st)
	if err != nil {
		return MapResult{}, err
	}
	entrypointRules, err := st.EntrypointCounts(ctx)
	if err != nil {
		return MapResult{}, err
	}
	entrypointCount := 0
	for _, count := range entrypointRules {
		entrypointCount += count
	}

	edges := make([]store.EdgeSkeleton, 0, len(all))
	for _, e := range all {
		if e.Kind == "CALLS" || e.Kind == "REFERENCES" {
			edges = append(edges, e)
		}
	}

	ranks, callers := pageRank(nodes, edges)

	result := MapResult{Entrypoints: entrypointCount, EntrypointRules: entrypointRules}

	// Module section: symbol counts and outgoing module dependencies.
	moduleSymbols := map[string]int{}
	moduleDeps := map[string]map[string]bool{}
	nodeFile := make(map[int64]int64, len(nodes))
	for _, n := range nodes {
		nodeFile[n.ID] = n.FileID
		name := moduleNames[moduleOf[n.FileID]]
		if name == "" {
			continue
		}
		moduleSymbols[name]++
	}
	for _, e := range edges {
		from := moduleNames[moduleOf[nodeFile[e.Src]]]
		to := moduleNames[moduleOf[nodeFile[e.Dst]]]
		if from == "" || to == "" || from == to {
			continue
		}
		if moduleDeps[from] == nil {
			moduleDeps[from] = map[string]bool{}
		}
		moduleDeps[from][to] = true
	}
	for name, count := range moduleSymbols {
		stat := ModuleStat{Name: name, Symbols: count}
		for dep := range moduleDeps[name] {
			stat.Deps = append(stat.Deps, dep)
		}
		sort.Strings(stat.Deps)
		result.Modules = append(result.Modules, stat)
	}
	sort.Slice(result.Modules, func(i, j int) bool {
		if result.Modules[i].Symbols != result.Modules[j].Symbols {
			return result.Modules[i].Symbols > result.Modules[j].Symbols
		}
		return result.Modules[i].Name < result.Modules[j].Name
	})
	result.Cycles = Cycles(moduleDeps)

	// Symbol section: ranked, trimmed to what the budget allows.
	ranked := make([]Ranked, 0, len(nodes))
	for _, n := range nodes {
		ranked = append(ranked, Ranked{
			Endpoint: endpointOf(n, paths),
			Rank:     ranks[n.ID],
			Callers:  callers[n.ID],
		})
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].Rank != ranked[j].Rank {
			return ranked[i].Rank > ranked[j].Rank
		}
		return ranked[i].QName < ranked[j].QName
	})

	// Roughly 14 tokens per line of output; modules already claim part of the budget.
	moduleLines := len(result.Modules) + len(result.Cycles) + 4
	budget := maxTokens - moduleLines*8
	if budget < 100 {
		budget = 100
	}
	lines := budget / 12
	if lines > len(ranked) {
		lines = len(ranked)
	}
	if lines < len(ranked) {
		result.Truncated = true
	}
	result.Top = ranked[:lines]

	return result, nil
}

// pageRank scores symbols by incoming calls, damping dangling mass back into
// the graph. Returns ranks and caller counts.
func pageRank(nodes []store.NodeRow, edges []store.EdgeSkeleton) (map[int64]float64, map[int64]int) {
	const damping = 0.85
	const iterations = 20

	total := float64(len(nodes))
	// Dense slices keyed by position avoid allocating maps on every iteration;
	// at 50k symbols the map-based loop was the map command's whole budget.
	position := make(map[int64]int, len(nodes))
	if len(nodes) == 0 {
		return map[int64]float64{}, map[int64]int{}
	}
	for i, n := range nodes {
		position[n.ID] = i
	}

	callers := make(map[int64]int, len(nodes))
	outgoing := make([][]int32, len(nodes))
	for _, e := range edges {
		src, srcOK := position[e.Src]
		dst, dstOK := position[e.Dst]
		if !srcOK || !dstOK {
			continue
		}
		outgoing[src] = append(outgoing[src], int32(dst))
		callers[e.Dst]++
	}

	ranks := make([]float64, len(nodes))
	next := make([]float64, len(nodes))
	baseScore := 1.0 / total
	for i := range ranks {
		ranks[i] = baseScore
	}

	for i := 0; i < iterations; i++ {
		for j := range next {
			next[j] = 0
		}
		leak := 0.0
		for src, targets := range outgoing {
			if len(targets) == 0 {
				leak += ranks[src]
				continue
			}
			share := damping * ranks[src] / float64(len(targets))
			for _, dst := range targets {
				next[dst] += share
			}
		}
		// Dangling mass is spread evenly instead of vanishing.
		base := (1-damping)/total + damping*leak/total
		for j := range next {
			next[j] += base
		}
		ranks, next = next, ranks
	}

	out := make(map[int64]float64, len(nodes))
	for i, n := range nodes {
		out[n.ID] = ranks[i]
	}
	return out, callers
}

// Cycles returns strongly connected components with more than one module, plus
// self-loops, using Tarjan's algorithm. Components and their members are sorted
// so callers can use the result as deterministic evidence.
func Cycles(deps map[string]map[string]bool) [][]string {
	index := map[string]int{}
	low := map[string]int{}
	onStack := map[string]bool{}
	var stack []string
	var out [][]string
	next := 0

	var strongConnect func(string)
	strongConnect = func(node string) {
		index[node] = next
		low[node] = next
		next++
		stack = append(stack, node)
		onStack[node] = true

		targets := make([]string, 0, len(deps[node]))
		for dep := range deps[node] {
			targets = append(targets, dep)
		}
		sort.Strings(targets)

		for _, dep := range targets {
			if _, seen := index[dep]; !seen {
				strongConnect(dep)
				if low[dep] < low[node] {
					low[node] = low[dep]
				}
			} else if onStack[dep] && index[dep] < low[node] {
				low[node] = index[dep]
			}
		}

		if low[node] != index[node] {
			return
		}

		var component []string
		for {
			last := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			onStack[last] = false
			component = append(component, last)
			if last == node {
				break
			}
		}

		selfLoop := deps[node][node]
		if len(component) > 1 || selfLoop {
			sort.Strings(component)
			out = append(out, component)
		}
	}

	nodes := make([]string, 0, len(deps))
	for node := range deps {
		nodes = append(nodes, node)
	}
	sort.Strings(nodes)
	for _, node := range nodes {
		if _, seen := index[node]; !seen {
			strongConnect(node)
		}
	}

	sort.Slice(out, func(i, j int) bool {
		return strings.Join(out[i], "→") < strings.Join(out[j], "→")
	})
	return out
}

// FormatRank keeps text output stable.
func FormatRank(rank float64) string {
	return fmt.Sprintf("%.4f", rank)
}
