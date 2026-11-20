package cli

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/Goutam-11/repath/internal/graph"
	"github.com/Goutam-11/repath/internal/query"
	"github.com/Goutam-11/repath/internal/store"
)

// The export command writes the graph as a standard text format so a reviewer
// can open it in a viewer (Gephi, yEd, Obsidian, Graphviz) without repath
// owning a renderer. It never adds nodes or edges: both modes are read-only
// projections of the index at the --min-confidence floor.
type exportNode struct {
	ID      string
	QName   string
	Kind    string
	File    string
	Line    int
	Symbols int // module mode only
}

type exportEdge struct {
	Src        string
	Dst        string
	Kind       string
	Confidence float64
	Rule       string
	Weight     int // module mode only
}

type exportGraph struct {
	Nodes []exportNode
	Edges []exportEdge
}

func exportCmd(args []string, stdout, stderr io.Writer) int {
	opts, positional, err := parseOptions(args)
	if err != nil {
		fmt.Fprintf(stderr, "%v\n\n%s", err, usage)
		return 2
	}
	if len(positional) != 0 {
		fmt.Fprintf(stderr, "export takes no arguments\n\n%s", usage)
		return 2
	}
	if opts.graphml == opts.dot {
		fmt.Fprintf(stderr, "export needs exactly one of --graphml or --dot\n\n%s", usage)
		return 2
	}
	if opts.artifact != "" {
		fmt.Fprintf(stderr, "export reads the local index; pull the artifact first (repath pull <dir>)\n\n%s", usage)
		return 2
	}
	if opts.md || opts.json {
		fmt.Fprintf(stderr, "export writes GraphML or DOT; --md is a map flag and --json is not used here\n\n%s", usage)
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

	var g exportGraph
	if opts.modules {
		g, err = buildModuleExport(ctx, st.Store, opts.minConfidence)
	} else {
		g, err = buildSymbolExport(ctx, st.Store, opts.minConfidence)
	}
	if err != nil {
		fmt.Fprintf(stderr, "export: %v\n", err)
		return 1
	}

	var data []byte
	format := "graphml"
	if opts.graphml {
		data = marshalGraphML(g, opts.modules)
	} else {
		format = "dot"
		data = marshalDOT(g, opts.modules)
	}

	if opts.out == "" {
		if _, err := stdout.Write(data); err != nil {
			fmt.Fprintf(stderr, "write: %v\n", err)
			return 1
		}
		return 0
	}
	if err := os.WriteFile(opts.out, data, 0o644); err != nil {
		fmt.Fprintf(stderr, "write %s: %v\n", opts.out, err)
		return 1
	}
	fmt.Fprintf(stdout, "wrote %s (%s, %d nodes, %d edges, min-confidence %.2f)\n",
		opts.out, format, len(g.Nodes), len(g.Edges), opts.minConfidence)
	return 0
}

// buildSymbolExport is the full symbol graph at the confidence floor. Nodes are
// every indexed symbol; edges are every resolved edge (no pending rows, no new
// rules).
func buildSymbolExport(ctx context.Context, st *store.Store, floor float64) (exportGraph, error) {
	nodes, err := st.Nodes(ctx)
	if err != nil {
		return exportGraph{}, err
	}
	paths, err := st.FilePaths(ctx)
	if err != nil {
		return exportGraph{}, err
	}
	edges, err := st.GraphEdges(ctx, floor)
	if err != nil {
		return exportGraph{}, err
	}

	g := exportGraph{Nodes: make([]exportNode, 0, len(nodes)), Edges: make([]exportEdge, 0, len(edges))}
	for _, n := range nodes {
		g.Nodes = append(g.Nodes, exportNode{
			ID:    symbolID(n.ID),
			QName: n.QName,
			Kind:  n.Kind,
			File:  paths[n.FileID],
			Line:  n.StartLine,
		})
	}
	sort.Slice(g.Nodes, func(i, j int) bool {
		a, b := g.Nodes[i], g.Nodes[j]
		if a.File != b.File {
			return a.File < b.File
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		if a.QName != b.QName {
			return a.QName < b.QName
		}
		return a.ID < b.ID
	})
	for _, e := range edges {
		g.Edges = append(g.Edges, exportEdge{
			Src:        symbolID(e.Src),
			Dst:        symbolID(e.Dst),
			Kind:       e.Kind,
			Confidence: e.Confidence,
			Rule:       e.Rule,
			Weight:     1,
		})
	}
	sortEdges(g.Edges)
	return g, nil
}

// buildModuleExport collapses the graph to the module dimension: one node per
// module and one edge per ordered module pair, carrying the number of symbol
// edges behind it and the strongest confidence among them. Self edges are
// dropped, matching how cycles treat the module graph.
func buildModuleExport(ctx context.Context, st *store.Store, floor float64) (exportGraph, error) {
	moduleOf, moduleNames, err := graph.ModuleIndex(ctx, st)
	if err != nil {
		return exportGraph{}, err
	}
	nodes, err := st.Nodes(ctx)
	if err != nil {
		return exportGraph{}, err
	}
	edges, err := st.GraphEdges(ctx, floor)
	if err != nil {
		return exportGraph{}, err
	}

	counts := make(map[string]int, len(moduleNames))
	for _, n := range nodes {
		if name := moduleNames[moduleOf[n.FileID]]; name != "" {
			counts[name]++
		}
	}
	names := make([]string, 0, len(counts))
	for name := range counts {
		names = append(names, name)
	}
	sort.Strings(names)

	idOf := make(map[string]string, len(names))
	g := exportGraph{Nodes: make([]exportNode, 0, len(names))}
	for i, name := range names {
		id := "m" + strconv.Itoa(i)
		idOf[name] = id
		g.Nodes = append(g.Nodes, exportNode{ID: id, QName: name, Kind: "module", Symbols: counts[name]})
	}

	type pair struct {
		src, dst string
	}
	agg := make(map[pair]*exportEdge)
	for _, e := range edges {
		from := moduleNames[moduleOf[e.SrcNode.FileID]]
		to := moduleNames[moduleOf[e.DstNode.FileID]]
		if from == "" || to == "" || from == to {
			continue
		}
		key := pair{from, to}
		edge, ok := agg[key]
		if !ok {
			edge = &exportEdge{
				Src: idOf[from], Dst: idOf[to], Kind: "DEPENDS",
				Confidence: e.Confidence, Rule: "module.aggregate",
			}
			agg[key] = edge
		}
		edge.Weight++
		if e.Confidence > edge.Confidence {
			edge.Confidence = e.Confidence
		}
	}
	for _, edge := range agg {
		g.Edges = append(g.Edges, *edge)
	}
	sortEdges(g.Edges)
	return g, nil
}

func symbolID(id int64) string { return "n" + strconv.FormatInt(id, 10) }

func sortEdges(edges []exportEdge) {
	sort.Slice(edges, func(i, j int) bool {
		a, b := edges[i], edges[j]
		if a.Src != b.Src {
			return a.Src < b.Src
		}
		if a.Dst != b.Dst {
			return a.Dst < b.Dst
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Rule != b.Rule {
			return a.Rule < b.Rule
		}
		return a.Confidence < b.Confidence
	})
}

// graphml keys are declared per mode so a viewer shows only attributes that
// mean something: a symbol node has no weight, a module node has no line.
type graphmlKey struct {
	id, domain, name, typ string
}

func graphmlKeys(modules bool) []graphmlKey {
	if modules {
		return []graphmlKey{
			{"d0", "node", "qname", "string"},
			{"d1", "node", "kind", "string"},
			{"d2", "node", "symbols", "int"},
			{"d3", "edge", "kind", "string"},
			{"d4", "edge", "confidence", "double"},
			{"d5", "edge", "rule", "string"},
			{"d6", "edge", "weight", "int"},
		}
	}
	return []graphmlKey{
		{"d0", "node", "qname", "string"},
		{"d1", "node", "kind", "string"},
		{"d2", "node", "file", "string"},
		{"d3", "node", "line", "int"},
		{"d4", "edge", "kind", "string"},
		{"d5", "edge", "confidence", "double"},
		{"d6", "edge", "rule", "string"},
	}
}

// marshalGraphML writes GraphML 1.0 by hand so the byte stream is stable; only
// escaping comes from encoding/xml. Key ids differ per mode because a module
// node has no line and a symbol edge has no weight.
func marshalGraphML(g exportGraph, modules bool) []byte {
	var b bytes.Buffer
	b.WriteString(xml.Header)
	b.WriteString(`<graphml xmlns="http://graphml.graphdrawing.org/xmlns" xmlns:xsi="http://www.w3.org/2001/XMLSchema-instance" xsi:schemaLocation="http://graphml.graphdrawing.org/xmlns http://graphml.graphdrawing.org/xmlns/1.0/graphml.xsd">` + "\n")
	for _, k := range graphmlKeys(modules) {
		fmt.Fprintf(&b, "  <key id=%q for=%q attr.name=%q attr.type=%q/>\n",
			k.id, k.domain, k.name, k.typ)
	}
	b.WriteString("  <graph id=\"repath\" edgedefault=\"directed\">\n")
	for _, n := range g.Nodes {
		fmt.Fprintf(&b, "    <node id=%q>\n", n.ID)
		fmt.Fprintf(&b, "      <data key=\"d0\">%s</data>\n", xmlEscape(n.QName))
		fmt.Fprintf(&b, "      <data key=\"d1\">%s</data>\n", xmlEscape(n.Kind))
		if modules {
			fmt.Fprintf(&b, "      <data key=\"d2\">%d</data>\n", n.Symbols)
		} else {
			fmt.Fprintf(&b, "      <data key=\"d2\">%s</data>\n", xmlEscape(n.File))
			fmt.Fprintf(&b, "      <data key=\"d3\">%d</data>\n", n.Line)
		}
		b.WriteString("    </node>\n")
	}
	for _, e := range g.Edges {
		fmt.Fprintf(&b, "    <edge source=%q target=%q>\n", e.Src, e.Dst)
		if modules {
			fmt.Fprintf(&b, "      <data key=\"d3\">%s</data>\n", xmlEscape(e.Kind))
			fmt.Fprintf(&b, "      <data key=\"d4\">%.2f</data>\n", e.Confidence)
			fmt.Fprintf(&b, "      <data key=\"d5\">%s</data>\n", xmlEscape(e.Rule))
			fmt.Fprintf(&b, "      <data key=\"d6\">%d</data>\n", e.Weight)
		} else {
			fmt.Fprintf(&b, "      <data key=\"d4\">%s</data>\n", xmlEscape(e.Kind))
			fmt.Fprintf(&b, "      <data key=\"d5\">%.2f</data>\n", e.Confidence)
			fmt.Fprintf(&b, "      <data key=\"d6\">%s</data>\n", xmlEscape(e.Rule))
		}
		b.WriteString("    </edge>\n")
	}
	b.WriteString("  </graph>\n</graphml>\n")
	return b.Bytes()
}

// marshalDOT mirrors the GraphML attributes as DOT node and edge attributes.
func marshalDOT(g exportGraph, modules bool) []byte {
	var b bytes.Buffer
	b.WriteString("digraph repath {\n")
	b.WriteString("  rankdir=LR;\n")
	b.WriteString("  node [shape=box];\n")
	for _, n := range g.Nodes {
		if modules {
			fmt.Fprintf(&b, "  %s [label=%s, qname=%s, kind=%s, symbols=%s];\n",
				dotQuote(n.ID), dotQuote(n.QName), dotQuote(n.QName), dotQuote(n.Kind),
				dotQuote(strconv.Itoa(n.Symbols)))
			continue
		}
		fmt.Fprintf(&b, "  %s [label=%s, qname=%s, kind=%s, file=%s, line=%s];\n",
			dotQuote(n.ID), dotQuote(n.QName), dotQuote(n.QName), dotQuote(n.Kind),
			dotQuote(n.File), dotQuote(strconv.Itoa(n.Line)))
	}
	for _, e := range g.Edges {
		if modules {
			fmt.Fprintf(&b, "  %s -> %s [label=%s, kind=%s, confidence=%s, rule=%s, weight=%s];\n",
				dotQuote(e.Src), dotQuote(e.Dst), dotQuote(e.Kind), dotQuote(e.Kind),
				dotQuote(fmt.Sprintf("%.2f", e.Confidence)), dotQuote(e.Rule),
				dotQuote(strconv.Itoa(e.Weight)))
			continue
		}
		fmt.Fprintf(&b, "  %s -> %s [label=%s, kind=%s, confidence=%s, rule=%s];\n",
			dotQuote(e.Src), dotQuote(e.Dst), dotQuote(e.Kind), dotQuote(e.Kind),
			dotQuote(fmt.Sprintf("%.2f", e.Confidence)), dotQuote(e.Rule))
	}
	b.WriteString("}\n")
	return b.Bytes()
}

// dotQuote quotes one DOT string. Go's %q escaping is not DOT escaping, so
// backslashes and quotes are escaped here and newlines become \n.
func dotQuote(s string) string {
	s = strings.ReplaceAll(s, "\\", "\\\\")
	s = strings.ReplaceAll(s, "\"", "\\\"")
	s = strings.ReplaceAll(s, "\n", "\\n")
	return "\"" + s + "\""
}

// xmlEscape escapes text and attribute content the way encoding/xml does.
func xmlEscape(s string) string {
	var b bytes.Buffer
	if err := xml.EscapeText(&b, []byte(s)); err != nil {
		return strings.ReplaceAll(s, "&", "&amp;")
	}
	return b.String()
}
