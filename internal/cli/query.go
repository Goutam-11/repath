package cli

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/Goutam-11/repath/internal/graph"
	"github.com/Goutam-11/repath/internal/query"
)

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
	root, err := query.ResolveRoot(opts.root)
	if err != nil {
		fmt.Fprintf(stderr, "resolve root: %v\n", err)
		return 1
	}
	st, err := query.Open(ctx, root, false)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	defer st.Close()

	answer, code, err := query.Impact(ctx, st.Store, positional[0], opts.depth, opts.minConfidence, opts.limit)
	if err != nil {
		fmt.Fprintf(stderr, "resolve: %v\n", err)
		return 1
	}
	if code != 0 {
		reportUnresolved(stderr, positional[0], code, answer.Target)
		return code
	}

	if opts.json {
		payload := struct {
			graph.Impact
			Root          string  `json:"root"`
			MinConfidence float64 `json:"min_confidence"`
		}{answer.Result, root, opts.minConfidence}
		if err := writeJSON(stdout, payload); err != nil {
			fmt.Fprintf(stderr, "encode: %v\n", err)
			return 1
		}
		return 0
	}

	printImpact(stdout, answer)
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
	root, err := query.ResolveRoot(opts.root)
	if err != nil {
		fmt.Fprintf(stderr, "resolve root: %v\n", err)
		return 1
	}
	st, err := query.Open(ctx, root, false)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	defer st.Close()

	answer, code, err := query.Path(ctx, st.Store, positional[0], positional[1], opts.depth, opts.minConfidence, 3)
	if err != nil {
		fmt.Fprintf(stderr, "resolve: %v\n", err)
		return 1
	}
	if code != 0 {
		if answer.Failed != nil {
			reportUnresolved(stderr, answer.Failed.Arg, answer.Failed.Code, answer.Failed.Target)
		}
		return code
	}

	if opts.json {
		payload := struct {
			graph.PathResult
			Root          string  `json:"root"`
			MinConfidence float64 `json:"min_confidence"`
			MaxDepth      int     `json:"max_depth"`
		}{answer.Result, root, opts.minConfidence, answer.MaxDepth}
		if err := writeJSON(stdout, payload); err != nil {
			fmt.Fprintf(stderr, "encode: %v\n", err)
			return 1
		}
		return 0
	}

	printPath(stdout, answer)
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
	root, err := query.ResolveRoot(opts.root)
	if err != nil {
		fmt.Fprintf(stderr, "resolve root: %v\n", err)
		return 1
	}
	st, err := query.Open(ctx, root, false)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	defer st.Close()

	answer, err := query.Map(ctx, st.Store, opts.maxTokens, opts.minConfidence)
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
		}{answer.Result, root, opts.minConfidence, answer.MaxTokens}
		if err := writeJSON(stdout, payload); err != nil {
			fmt.Fprintf(stderr, "encode: %v\n", err)
			return 1
		}
		return 0
	}

	printMap(stdout, answer)
	return 0
}

// printImpact keeps the answer near forty lines: depth sections show at most ten
// dependents, and the whole report stops with a pointer to --json.
func printImpact(out io.Writer, answer query.ImpactAnswer) {
	res := answer.Result
	label := answer.Target.Label
	if len(res.Targets) == 0 {
		fmt.Fprintln(out, "nothing to report")
		return
	}

	switch {
	case len(res.Targets) > 1:
		fmt.Fprintf(out, "impact of %s  (%d symbols in the file)\n", label, len(res.Targets))
	case query.LooksLikeFile(label) || strings.Contains(label, ":"):
		fmt.Fprintf(out, "impact of %s\n", label)
	default:
		target := res.Targets[0]
		fmt.Fprintf(out, "impact of %s  %s  %s:%d\n", target.QName, target.Kind, target.File, target.Line)
	}
	fmt.Fprintf(out, "  %d dependents · %d files · %d modules · min-confidence %.2f · depth %d\n",
		len(res.Dependents), len(res.Files), len(res.Modules), answer.MinConfidence, res.Depth)

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

func printPath(out io.Writer, answer query.PathAnswer) {
	res := answer.Result
	fmt.Fprintf(out, "path from %s to %s  (min-confidence %.2f · depth %d)\n",
		res.From.QName, res.To.QName, answer.MinConfidence, answer.MaxDepth)

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

func printMap(out io.Writer, answer query.MapAnswer) {
	res := answer.Result
	fmt.Fprintf(out, "repath map · %d modules · %d symbols shown · budget %d tokens\n",
		len(res.Modules), len(res.Top), answer.MaxTokens)

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
