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
	Modules   []ModuleStat `json:"modules"`
	Cycles    [][]string   `json:"cycles"`
	Top       []Ranked     `json:"top"`
	Truncated bool         `json:"truncated"`
}

// BuildMap ranks symbols with PageRank over call edges, rolls modules up into a
// dependency graph, and finds cycles. The symbol list is trimmed to the token
// budget; the module section always fits because it is small.
func BuildMap(ctx context.Context, st *store.Store, maxTokens int, minConfidence float64) (MapResult, error) {
	nodes, err := st.Nodes(ctx)
	if err != nil {
		return MapResult{}, err
	}
	all, err := st.GraphEdges(ctx, minConfidence)
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

	edges := make([]store.EdgeOut, 0, len(all))
	for _, e := range all {
		if e.Kind == "CALLS" || e.Kind == "REFERENCES" {
			edges = append(edges, e)
		}
	}

	ranks, callers := pageRank(nodes, edges)

	result := MapResult{}

	// Module section: symbol counts and outgoing module dependencies.
	moduleSymbols := map[string]int{}
	moduleDeps := map[string]map[string]bool{}
	for _, n := range nodes {
		name := moduleNames[moduleOf[n.FileID]]
		if name == "" {
			continue
		}
		moduleSymbols[name]++
	}
	for _, e := range edges {
		from := moduleNames[moduleOf[e.SrcNode.FileID]]
		to := moduleNames[moduleOf[e.DstNode.FileID]]
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
	result.Cycles = cycles(moduleDeps)

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
func pageRank(nodes []store.NodeRow, edges []store.EdgeOut) (map[int64]float64, map[int64]int) {
	const damping = 0.85
	const iterations = 20

	total := float64(len(nodes))
	ranks := make(map[int64]float64, len(nodes))
	callers := make(map[int64]int, len(nodes))
	outgoing := make(map[int64][]int64, len(nodes))

	for _, n := range nodes {
		ranks[n.ID] = 1.0 / total
	}
	for _, e := range edges {
		outgoing[e.Src] = append(outgoing[e.Src], e.Dst)
		callers[e.Dst]++
	}

	for i := 0; i < iterations; i++ {
		next := make(map[int64]float64, len(nodes))
		leak := 0.0
		for _, n := range nodes {
			targets := outgoing[n.ID]
			if len(targets) == 0 {
				leak += ranks[n.ID]
				continue
			}
			share := damping * ranks[n.ID] / float64(len(targets))
			for _, dst := range targets {
				next[dst] += share
			}
		}
		// Dangling mass is spread evenly instead of vanishing.
		base := (1-damping)/total + damping*leak/total
		for _, n := range nodes {
			next[n.ID] += base
		}
		ranks = next
	}

	return ranks, callers
}

// cycles returns strongly connected components with more than one module, plus
// self-loops, using Tarjan's algorithm.
func cycles(deps map[string]map[string]bool) [][]string {
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
