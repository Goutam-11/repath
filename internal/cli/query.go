package cli

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/Goutam-11/repath/internal/graph"
	"github.com/Goutam-11/repath/internal/index"
	"github.com/Goutam-11/repath/internal/store"
)

// targetSet is what a query starts from: one symbol, or every symbol in a file.
type targetSet struct {
	label      string
	nodes      []store.NodeRow
	candidates []store.NodeRow // set when the reference is ambiguous
}

func impactCmd(args []string, stdout, stderr io.Writer) int {
	opts, positional, err := parseOptions(args)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n\n%s", err, usage)
		return 2
	}
	if len(positional) != 1 {
		fmt.Fprintf(stderr, "impact needs exactly one symbol or file:line\n\n%s", usage)
		return 2
	}

	ctx := context.Background()
	st, root, code := openForQuery(ctx, opts, stderr)
	if code != 0 {
		return code
	}
	defer st.Close()

	target, code, err := resolveTarget(ctx, st, positional[0])
	if err != nil {
		fmt.Fprintf(stderr, "resolve: %v\n", err)
		return 1
	}
	if code != 0 {
		reportUnresolved(stderr, positional[0], code, target)
		return code
	}

	depth := opts.depth
	if depth == 0 {
		depth = 8
	}
	result, err := graph.Blast(ctx, st, target.nodes, depth, opts.minConfidence, opts.limit)
	if err != nil {
		fmt.Fprintf(stderr, "impact: %v\n", err)
		return 1
	}

	if opts.json {
		payload := struct {
			graph.Impact
			Root          string  `json:"root"`
			MinConfidence float64 `json:"min_confidence"`
		}{result, root, opts.minConfidence}
		if err := writeJSON(stdout, payload); err != nil {
			fmt.Fprintf(stderr, "encode: %v\n", err)
			return 1
		}
		return 0
	}

	printImpact(stdout, target.label, result, opts.minConfidence)
	return 0
}

func pathCmd(args []string, stdout, stderr io.Writer) int {
	opts, positional, err := parseOptions(args)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n\n%s", err, usage)
		return 2
	}
	if len(positional) != 2 {
		fmt.Fprintf(stderr, "path needs exactly two symbols\n\n%s", usage)
		return 2
	}

	ctx := context.Background()
	st, root, code := openForQuery(ctx, opts, stderr)
	if code != 0 {
		return code
	}
	defer st.Close()

	from, code, err := resolveTarget(ctx, st, positional[0])
	if err != nil {
		fmt.Fprintf(stderr, "resolve: %v\n", err)
		return 1
	}
	if code != 0 {
		reportUnresolved(stderr, positional[0], code, from)
		return code
	}

	to, code, err := resolveTarget(ctx, st, positional[1])
	if err != nil {
		fmt.Fprintf(stderr, "resolve: %v\n", err)
		return 1
	}
	if code != 0 {
		reportUnresolved(stderr, positional[1], code, to)
		return code
	}

	depth := opts.depth
	if depth == 0 {
		depth = 8
	}
	result, err := graph.Connect(ctx, st, from.nodes[0], to.nodes[0], depth, opts.minConfidence, 3)
	if err != nil {
		fmt.Fprintf(stderr, "path: %v\n", err)
		return 1
	}

	if opts.json {
		payload := struct {
			graph.PathResult
			Root          string  `json:"root"`
			MinConfidence float64 `json:"min_confidence"`
		}{result, root, opts.minConfidence}
		if err := writeJSON(stdout, payload); err != nil {
			fmt.Fprintf(stderr, "encode: %v\n", err)
			return 1
		}
		return 0
	}

	printPath(stdout, result, opts.minConfidence, depth)
	return 0
}

func mapCmd(args []string, stdout, stderr io.Writer) int {
	opts, positional, err := parseOptions(args)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n\n%s", err, usage)
		return 2
	}
	if len(positional) != 0 {
		fmt.Fprintf(stderr, "map takes no arguments\n\n%s", usage)
		return 2
	}

	ctx := context.Background()
	st, root, code := openForQuery(ctx, opts, stderr)
	if code != 0 {
		return code
	}
	defer st.Close()

	result, err := graph.BuildMap(ctx, st, opts.maxTokens, opts.minConfidence)
	if err != nil {
		fmt.Fprintf(stderr, "map: %v\n", err)
		return 1
	}

	if opts.json {
		payload := struct {
			graph.MapResult
			Root          string  `json:"root"`
			MinConfidence float64 `json:"min_confidence"`
			MaxTokens     int     `json:"max_tokens"`
		}{result, root, opts.minConfidence, opts.maxTokens}
		if err := writeJSON(stdout, payload); err != nil {
			fmt.Fprintf(stderr, "encode: %v\n", err)
			return 1
		}
		return 0
	}

	printMap(stdout, result, opts.maxTokens)
	return 0
}

// openForQuery opens the index, building it once when it is empty. Queries do
// not otherwise refresh: `status` is the command that keeps the index current.
func openForQuery(ctx context.Context, opts options, stderr io.Writer) (*store.Store, string, int) {
	root, err := resolveRoot(opts.root)
	if err != nil {
		fmt.Fprintf(stderr, "resolve root: %v\n", err)
		return nil, "", 1
	}

	st, err := openIndex(ctx, root)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return nil, "", 1
	}

	counts, err := st.Counts(ctx)
	if err != nil {
		st.Close()
		fmt.Fprintf(stderr, "read index: %v\n", err)
		return nil, "", 1
	}
	if counts.Files == 0 {
		if _, err := index.Run(ctx, st, root, index.Options{}); err != nil {
			st.Close()
			fmt.Fprintf(stderr, "index: %v\n", err)
			return nil, "", 1
		}
	}

	return st, root, 0
}

// resolveTarget accepts a symbol, file path, or file:line reference.
func resolveTarget(ctx context.Context, st *store.Store, arg string) (targetSet, int, error) {
	if path, line, ok := splitFileLine(arg); ok {
		fileID, found, err := st.FileByPath(ctx, path)
		if err != nil {
			return targetSet{label: arg}, 1, err
		}
		if found {
			nodeID, ok, err := st.EnclosingNode(ctx, fileID, line)
			if err != nil {
				return targetSet{label: arg}, 1, err
			}
			if ok {
				node, err := st.NodeByID(ctx, nodeID)
				if err != nil {
					return targetSet{label: arg}, 1, err
				}
				return targetSet{label: arg, nodes: []store.NodeRow{node}}, 0, nil
			}
		}
		return targetSet{label: arg}, 3, nil
	}

	if looksLikeFile(arg) {
		_, found, err := st.FileByPath(ctx, arg)
		if err != nil {
			return targetSet{label: arg}, 1, err
		}
		if found {
			nodes, err := st.NodesByFile(ctx, arg)
			if err != nil {
				return targetSet{label: arg}, 1, err
			}
			if len(nodes) > 0 {
				return targetSet{label: arg, nodes: nodes}, 0, nil
			}
		}
		return targetSet{label: arg}, 3, nil
	}

	matches, err := st.SearchSymbols(ctx, arg, 20)
	if err != nil {
		return targetSet{label: arg}, 1, err
	}
	if len(matches) == 0 {
		return targetSet{label: arg}, 3, nil
	}
	chosen, ambiguous := chooseSymbol(matches, arg)
	if ambiguous {
		return targetSet{label: arg, candidates: matches}, 4, nil
	}
	return targetSet{label: arg, nodes: []store.NodeRow{chosen}}, 0, nil
}

func reportUnresolved(out io.Writer, name string, code int, target targetSet) {
	if code == 3 {
		fmt.Fprintf(out, "not found: %s\n", name)
		return
	}
	fmt.Fprintf(out, "ambiguous: %s matches %d symbols\n", name, len(target.nodes)+len(target.candidates))
	for _, m := range target.candidates {
		fmt.Fprintf(out, "  %s  %s\n", m.QName, m.Kind)
	}
	fmt.Fprintln(out, "use a qualified name to disambiguate")
}

func splitFileLine(arg string) (string, int, bool) {
	index := strings.LastIndex(arg, ":")
	if index <= 0 || index == len(arg)-1 {
		return "", 0, false
	}
	line, err := strconv.Atoi(arg[index+1:])
	if err != nil {
		return "", 0, false
	}
	path := arg[:index]
	if !looksLikeFile(path) {
		return "", 0, false
	}
	return path, line, true
}

func looksLikeFile(arg string) bool {
	if strings.Contains(arg, "/") {
		return true
	}
	for _, ext := range []string{".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs"} {
		if strings.HasSuffix(arg, ext) {
			return true
		}
	}
	return false
}

// printImpact keeps the answer near forty lines: depth sections show at most ten
// dependents, and the whole report stops with a pointer to --json.
func printImpact(out io.Writer, label string, res graph.Impact, minConfidence float64) {
	if len(res.Targets) == 0 {
		fmt.Fprintln(out, "nothing to report")
		return
	}

	switch {
	case len(res.Targets) > 1:
		fmt.Fprintf(out, "impact of %s  (%d symbols in the file)\n", label, len(res.Targets))
	case looksLikeFile(label) || strings.Contains(label, ":"):
		fmt.Fprintf(out, "impact of %s\n", label)
	default:
		target := res.Targets[0]
		fmt.Fprintf(out, "impact of %s  %s  %s:%d\n", target.QName, target.Kind, target.File, target.Line)
	}
	fmt.Fprintf(out, "  %d dependents · %d files · %d modules · min-confidence %.2f · depth %d\n",
		len(res.Dependents), len(res.Files), len(res.Modules), minConfidence, res.Depth)

	if len(res.Dependents) == 0 {
		fmt.Fprintln(out, "\nnothing reaches this symbol at or above the threshold")
		return
	}

	byDepth := make(map[int][]graph.Dependent)
	maxDepth := 0
	for _, d := range res.Dependents {
		byDepth[d.Depth] = append(byDepth[d.Depth], d)
		if d.Depth > maxDepth {
			maxDepth = d.Depth
		}
	}

	lines := 0
	const budget = 34
	for depth := 1; depth <= maxDepth; depth++ {
		list := byDepth[depth]
		if len(list) == 0 {
			continue
		}
		if lines >= budget {
			fmt.Fprintf(out, "\n… truncated; use --json or --depth to see the rest\n")
			break
		}
		fmt.Fprintf(out, "\ndepth %d\n", depth)
		lines += 2
		for i, d := range list {
			if i >= 10 || lines >= budget {
				fmt.Fprintf(out, "  +%d more at depth %d\n", len(list)-i, depth)
				break
			}
			fmt.Fprintf(out, "  %-42s %s:%d  %.2f  %s\n",
				d.QName, d.File, d.Line, d.Confidence, strings.ToLower(d.EdgeKind))
			lines++
		}
	}

	if len(res.Files) > 0 {
		fmt.Fprintln(out, "\naffected files")
		for i, f := range res.Files {
			if i >= 8 {
				fmt.Fprintf(out, "  +%d more\n", len(res.Files)-i)
				break
			}
			fmt.Fprintf(out, "  %-52s %d\n", f.Path, f.Symbols)
		}
	}
}

func printPath(out io.Writer, res graph.PathResult, minConfidence float64, maxDepth int) {
	fmt.Fprintf(out, "path from %s to %s  (min-confidence %.2f · depth %d)\n",
		res.From.QName, res.To.QName, minConfidence, maxDepth)

	if len(res.Hops) == 0 {
		fmt.Fprintln(out, "  no path within the depth limit")
		if len(res.FrontierA) > 0 {
			fmt.Fprintf(out, "  from %s you can reach: %s\n", res.From.QName, joinEndpoints(res.FrontierA))
		}
		if len(res.FrontierB) > 0 {
			fmt.Fprintf(out, "  things that reach %s: %s\n", res.To.QName, joinEndpoints(res.FrontierB))
		}
		return
	}

	fmt.Fprintf(out, "  %s  %s:%d\n", res.From.QName, res.From.File, res.From.Line)
	for _, hop := range res.Hops {
		fmt.Fprintf(out, "    → %-7s %-44s %s:%d  %.2f\n",
			strings.ToLower(hop.Kind), hop.Dst.QName, hop.Dst.File, hop.Dst.Line, hop.Confidence)
	}

	for i, alt := range res.Alternate {
		fmt.Fprintf(out, "\nalternate %d\n", i+1)
		for _, hop := range alt {
			fmt.Fprintf(out, "    → %-7s %-44s %s:%d  %.2f\n",
				strings.ToLower(hop.Kind), hop.Dst.QName, hop.Dst.File, hop.Dst.Line, hop.Confidence)
		}
	}
}

func printMap(out io.Writer, res graph.MapResult, maxTokens int) {
	fmt.Fprintf(out, "repath map · %d modules · %d symbols shown · budget %d tokens\n",
		len(res.Modules), len(res.Top), maxTokens)

	if len(res.Modules) > 0 {
		fmt.Fprintln(out, "\nmodules")
		for _, m := range res.Modules {
			deps := ""
			if len(m.Deps) > 0 {
				deps = "  ← " + strings.Join(m.Deps, ", ")
			}
			fmt.Fprintf(out, "  %-30s %6d%s\n", m.Name, m.Symbols, deps)
		}
	}

	if len(res.Cycles) > 0 {
		fmt.Fprintln(out, "\ncycles")
		for _, cycle := range res.Cycles {
			fmt.Fprintf(out, "  %s → %s\n", strings.Join(cycle, " → "), cycle[0])
		}
	}

	if len(res.Top) > 0 {
		fmt.Fprintln(out, "\ntop symbols")
		for _, s := range res.Top {
			fmt.Fprintf(out, "  %s  %-9s %-46s %s:%d  callers %d\n",
				graph.FormatRank(s.Rank), s.Kind, s.QName, s.File, s.Line, s.Callers)
		}
	}

	if res.Truncated {
		fmt.Fprintf(out, "\n… trimmed to the token budget; raise --max-tokens for more\n")
	}
}

func joinEndpoints(nodes []graph.Endpoint) string {
	parts := make([]string, 0, len(nodes))
	for _, n := range nodes {
		parts = append(parts, n.QName)
	}
	sort.Strings(parts)
	return strings.Join(parts, ", ")
}
