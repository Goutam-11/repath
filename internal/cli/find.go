package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/Goutam-11/repath/internal/index"
	"github.com/Goutam-11/repath/internal/store"
)

type symbolRef struct {
	QName    string `json:"qname"`
	Kind     string `json:"kind"`
	File     string `json:"file"`
	Line     int    `json:"line"`
	Exported bool   `json:"exported"`
}

func toRef(n store.NodeRow, paths map[int64]string) symbolRef {
	return symbolRef{
		QName:    n.QName,
		Kind:     n.Kind,
		File:     paths[n.FileID],
		Line:     n.StartLine,
		Exported: n.Exported,
	}
}

func findCmd(args []string, stdout, stderr io.Writer) int {
	opts, positional, err := parseOptions(args)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n\n%s", err, usage)
		return 2
	}
	if len(positional) != 1 {
		fmt.Fprintf(stderr, "find needs exactly one symbol name\n\n%s", usage)
		return 2
	}

	return withSymbol(opts, positional[0], stdout, stderr,
		func(st *store.Store, ctx context.Context, n store.NodeRow, paths map[int64]string) int {
			callers, err := st.CallerCount(ctx, n.ID, opts.minConfidence)
			if err != nil {
				fmt.Fprintf(stderr, "read callers: %v\n", err)
				return 1
			}
			callees, err := st.CalleeCount(ctx, n.ID, opts.minConfidence)
			if err != nil {
				fmt.Fprintf(stderr, "read callees: %v\n", err)
				return 1
			}

			if opts.json {
				payload := struct {
					Symbol        symbolRef `json:"symbol"`
					Callers       int       `json:"callers"`
					Callees       int       `json:"callees"`
					MinConfidence float64   `json:"min_confidence"`
				}{toRef(n, paths), callers, callees, opts.minConfidence}
				if err := writeJSON(stdout, payload); err != nil {
					fmt.Fprintf(stderr, "encode: %v\n", err)
					return 1
				}
				return 0
			}

			exported := ""
			if n.Exported {
				exported = " · exported"
			}
			fmt.Fprintf(stdout, "%s  %s  %s:%d%s\n", n.QName, n.Kind, paths[n.FileID], n.StartLine, exported)
			fmt.Fprintf(stdout, "callers %d · callees %d · min-confidence %.2f\n", callers, callees, opts.minConfidence)
			return 0
		})
}

func callersCmd(args []string, stdout, stderr io.Writer) int {
	opts, positional, err := parseOptions(args)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n\n%s", err, usage)
		return 2
	}
	if len(positional) != 1 {
		fmt.Fprintf(stderr, "callers needs exactly one symbol name\n\n%s", usage)
		return 2
	}
	depth := opts.depth
	if depth == 0 {
		depth = 1
	}
	if depth != 1 {
		fmt.Fprintln(stderr, "callers supports --depth 1; use impact for transitive reach")
		return 2
	}

	return withSymbol(opts, positional[0], stdout, stderr,
		func(st *store.Store, ctx context.Context, n store.NodeRow, paths map[int64]string) int {
			edges, err := st.Callers(ctx, n.ID, opts.minConfidence)
			if err != nil {
				fmt.Fprintf(stderr, "read callers: %v\n", err)
				return 1
			}

			if opts.json {
				type caller struct {
					QName      string  `json:"qname"`
					File       string  `json:"file"`
					Line       int     `json:"line"`
					Kind       string  `json:"kind"`
					Confidence float64 `json:"confidence"`
					Rule       string  `json:"rule"`
					CallLine   int     `json:"call_line"`
				}
				payload := struct {
					Symbol        symbolRef `json:"symbol"`
					MinConfidence float64   `json:"min_confidence"`
					Callers       []caller  `json:"callers"`
				}{toRef(n, paths), opts.minConfidence, make([]caller, 0, len(edges))}

				for _, e := range edges {
					payload.Callers = append(payload.Callers, caller{
						QName:      e.SrcNode.QName,
						File:       e.SrcPath,
						Line:       e.SrcNode.StartLine,
						Kind:       e.Kind,
						Confidence: e.Confidence,
						Rule:       e.Rule,
						CallLine:   e.Line,
					})
				}
				if err := writeJSON(stdout, payload); err != nil {
					fmt.Fprintf(stderr, "encode: %v\n", err)
					return 1
				}
				return 0
			}

			fmt.Fprintf(stdout, "callers of %s  (%s:%d)  min-confidence %.2f\n",
				n.QName, paths[n.FileID], n.StartLine, opts.minConfidence)
			if len(edges) == 0 {
				fmt.Fprintln(stdout, "  none above the threshold")
				return 0
			}
			for _, e := range edges {
				fmt.Fprintf(stdout, "  %-40s %s:%d  %.2f  %s\n",
					e.SrcNode.QName, e.SrcPath, e.SrcNode.StartLine, e.Confidence, strings.ToLower(e.Kind))
			}
			return 0
		})
}

// withSymbol resolves one symbol reference (qname or bare name), building the
// index if it is missing, and calls fn with the single match. It owns the exit
// codes 3 (missing) and 4 (ambiguous).
func withSymbol(opts options, name string, stdout, stderr io.Writer,
	fn func(*store.Store, context.Context, store.NodeRow, map[int64]string) int) int {

	ctx := context.Background()
	root, err := resolveRoot(opts.root)
	if err != nil {
		fmt.Fprintf(stderr, "resolve root: %v\n", err)
		return 1
	}

	st, err := openIndex(ctx, root)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n", err)
		return 1
	}
	defer st.Close()

	counts, err := st.Counts(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "read index: %v\n", err)
		return 1
	}
	if counts.Files == 0 {
		if _, err := index.Run(ctx, st, root, index.Options{}); err != nil {
			fmt.Fprintf(stderr, "index: %v\n", err)
			return 1
		}
	}

	matches, err := st.SearchSymbols(ctx, name, 20)
	if err != nil {
		fmt.Fprintf(stderr, "search: %v\n", err)
		return 1
	}
	if len(matches) == 0 {
		if opts.json {
			_ = writeJSON(stdout, map[string]string{"error": "not found", "name": name})
		} else {
			fmt.Fprintf(stderr, "not found: %s\n", name)
		}
		return 3
	}

	chosen, ambiguous := chooseSymbol(matches, name)
	if ambiguous {
		if opts.json {
			type candidate struct {
				QName    string `json:"qname"`
				Kind     string `json:"kind"`
				FileID   int64  `json:"-"`
				Location string `json:"location,omitempty"`
			}
			payload := struct {
				Error      string      `json:"error"`
				Name       string      `json:"name"`
				Candidates []candidate `json:"candidates"`
			}{Error: "ambiguous", Name: name}
			for _, m := range matches {
				payload.Candidates = append(payload.Candidates, candidate{QName: m.QName, Kind: m.Kind})
			}
			_ = writeJSON(stdout, payload)
		} else {
			fmt.Fprintf(stderr, "ambiguous: %s matches %d symbols\n", name, len(matches))
			for _, m := range matches {
				fmt.Fprintf(stderr, "  %s  %s\n", m.QName, m.Kind)
			}
			fmt.Fprintln(stderr, "use a qualified name to disambiguate")
		}
		return 4
	}

	paths, err := st.FilePaths(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "read files: %v\n", err)
		return 1
	}
	return fn(st, ctx, chosen, paths)
}

// chooseSymbol prefers an exact qname, then a single name match.
func chooseSymbol(matches []store.NodeRow, name string) (store.NodeRow, bool) {
	var exact []store.NodeRow
	for _, m := range matches {
		if m.QName == name {
			exact = append(exact, m)
		}
	}
	switch len(exact) {
	case 1:
		return exact[0], false
	case 0:
		if len(matches) == 1 {
			return matches[0], false
		}
		return store.NodeRow{}, true
	default:
		return store.NodeRow{}, true
	}
}
