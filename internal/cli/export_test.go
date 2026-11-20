package cli

import (
	"encoding/xml"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// exportFixture writes a two-module Python repository: beta calls into alpha,
// so the module graph has one dependency edge and the symbol graph has a
// cross-module CALLS edge.
func exportFixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeFile(t, root, "alpha/util.py", "def clamp(value: int) -> int:\n    return max(0, value)\n")
	writeFile(t, root, "alpha/main.py", "from alpha.util import clamp\n\n\ndef run(value: int) -> int:\n    return clamp(value)\n")
	writeFile(t, root, "beta/main.py", "from alpha.util import clamp\n\n\ndef call(value: int) -> int:\n    return clamp(value)\n")
	return root
}

func readTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = string(data)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	return out
}

func TestMapMarkdownBundleIsSortedDeterministicAndBounded(t *testing.T) {
	root := exportFixture(t)
	outA := t.TempDir()

	code, stdout, stderr := run(t, "map", "--md", "--out", outA, "--max-tokens", "600", "--root", root)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(stdout, "index.md") || !strings.Contains(stdout, "modules/alpha.md") ||
		!strings.Contains(stdout, "modules/beta.md") {
		t.Fatalf("stdout does not list the bundle:\n%s", stdout)
	}
	if !strings.Contains(stdout, "tokens (budget 600)") {
		t.Fatalf("stdout does not report the budget:\n%s", stdout)
	}

	files := readTree(t, outA)
	if len(files) != 3 {
		t.Fatalf("files = %v, want index.md plus one page per module", keys(files))
	}
	index := files["index.md"]
	if !strings.Contains(index, "| [`alpha`](modules/alpha.md)") {
		t.Fatalf("index missing the alpha module link:\n%s", index)
	}
	// Module rows are name-sorted: alpha before beta.
	if strings.Index(index, "[`alpha`]") > strings.Index(index, "[`beta`]") {
		t.Fatalf("module table is not sorted:\n%s", index)
	}
	if !strings.Contains(files["modules/beta.md"], "depends on: alpha") {
		t.Fatalf("beta page missing its dependency:\n%s", files["modules/beta.md"])
	}
	if !strings.Contains(files["modules/beta.md"], "`call`") {
		t.Fatalf("beta page missing its own top symbol:\n%s", files["modules/beta.md"])
	}
	if !strings.Contains(files["modules/alpha.md"], "`clamp`") {
		t.Fatalf("alpha page missing its top symbol:\n%s", files["modules/alpha.md"])
	}
	if !strings.Contains(index, "`clamp`") {
		t.Fatalf("index missing the ranked symbols:\n%s", index)
	}

	// The reported bundle total is the sum of the per-file estimates and fits
	// the budget.
	total := 0
	for _, content := range files {
		total += tokenEstimate(content)
	}
	if total > 600 {
		t.Fatalf("bundle is %d tokens, over the 600 budget", total)
	}

	// A second run over the same index is byte-identical.
	outB := t.TempDir()
	if code, _, stderr := run(t, "map", "--md", "--out", outB, "--max-tokens", "600", "--root", root); code != 0 {
		t.Fatalf("second run exit code = %d, stderr = %s", code, stderr)
	}
	second := readTree(t, outB)
	if len(second) != len(files) {
		t.Fatalf("second run wrote %v, first wrote %v", keys(second), keys(files))
	}
	for name, content := range files {
		if second[name] != content {
			t.Fatalf("%s is not byte-identical between runs", name)
		}
	}
}

func TestMapMarkdownBundleStructuralOverflowIsHonest(t *testing.T) {
	root := exportFixture(t)
	out := t.TempDir()
	code, stdout, _ := run(t, "map", "--md", "--out", out, "--max-tokens", "10", "--root", root)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (a tiny budget is reported, not an error)", code)
	}
	if !strings.Contains(stdout, "the structural pages alone exceed the budget") {
		t.Fatalf("stdout does not admit the overflow:\n%s", stdout)
	}
	if _, err := os.Stat(filepath.Join(out, "index.md")); err != nil {
		t.Fatalf("index.md missing: %v", err)
	}
}

func TestMapMarkdownDefaultsUnderRepath(t *testing.T) {
	root := exportFixture(t)
	code, stdout, stderr := run(t, "map", "--md", "--root", root)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr)
	}
	// The default bundle is anchored at the repository root, not the process
	// working directory.
	want := filepath.Join(root, ".repath", "map", "index.md")
	if !strings.Contains(stdout, want) {
		t.Fatalf("stdout does not name the default bundle %q:\n%s", want, stdout)
	}
	if _, err := os.Stat(filepath.Join(root, ".repath", "map", "index.md")); err != nil {
		t.Fatalf("default bundle not written under the root: %v", err)
	}
}

func TestMapOutputFlagWithoutMarkdownIsUsage(t *testing.T) {
	root := exportFixture(t)
	if code, _, stderr := run(t, "map", "--out", t.TempDir(), "--root", root); code != 2 {
		t.Fatalf("exit code = %d, want 2; stderr = %s", code, stderr)
	}
	if code, _, _ := run(t, "map", "--md", "--json", "--root", root); code != 2 {
		t.Fatalf("--md with --json exit code = %d, want 2", code)
	}
}

// graphMLDoc mirrors the subset of GraphML the exporter emits, enough to prove
// the file is well-formed and carries the documented attributes.
type graphMLDoc struct {
	XMLName xml.Name `xml:"graphml"`
	Keys    []struct {
		ID   string `xml:"id,attr"`
		For  string `xml:"for,attr"`
		Name string `xml:"attr.name,attr"`
		Type string `xml:"attr.type,attr"`
	} `xml:"key"`
	Graph struct {
		ID          string `xml:"id,attr"`
		EdgeDefault string `xml:"edgedefault,attr"`
		Nodes       []struct {
			ID   string `xml:"id,attr"`
			Data []struct {
				Key   string `xml:"key,attr"`
				Value string `xml:",chardata"`
			} `xml:"data"`
		} `xml:"node"`
		Edges []struct {
			Source string `xml:"source,attr"`
			Target string `xml:"target,attr"`
			Data   []struct {
				Key   string `xml:"key,attr"`
				Value string `xml:",chardata"`
			} `xml:"data"`
		} `xml:"edge"`
	} `xml:"graph"`
}

func TestExportGraphMLAndDOTDeterministic(t *testing.T) {
	root := exportFixture(t)

	code, out, stderr := run(t, "export", "--graphml", "--root", root)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr)
	}
	var doc graphMLDoc
	if err := xml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("graphml is not well-formed: %v\n%s", err, out)
	}
	if doc.Graph.ID != "repath" || doc.Graph.EdgeDefault != "directed" {
		t.Fatalf("graph attrs = %q/%q", doc.Graph.ID, doc.Graph.EdgeDefault)
	}
	if len(doc.Keys) != 7 {
		t.Fatalf("keys = %d, want 7 (4 node + 3 edge)", len(doc.Keys))
	}
	if len(doc.Graph.Nodes) < 3 {
		t.Fatalf("nodes = %d, want the fixture's symbols", len(doc.Graph.Nodes))
	}
	checkGraphMLShape(t, doc)
	nodeKeys := map[string]string{}
	for _, data := range doc.Graph.Nodes[0].Data {
		nodeKeys[data.Key] = data.Value
	}
	if nodeKeys["d0"] == "" || nodeKeys["d1"] == "" || nodeKeys["d2"] == "" || nodeKeys["d3"] == "" {
		t.Fatalf("node missing qname/kind/file/line: %+v", nodeKeys)
	}
	if !strings.Contains(nodeKeys["d2"], ".py") {
		t.Fatalf("node file = %q, want a repo path", nodeKeys["d2"])
	}
	if len(doc.Graph.Edges) == 0 {
		t.Fatal("graph has no edges")
	}
	edgeKeys := map[string]string{}
	for _, data := range doc.Graph.Edges[0].Data {
		edgeKeys[data.Key] = data.Value
	}
	if edgeKeys["d4"] == "" || edgeKeys["d5"] == "" || edgeKeys["d6"] == "" {
		t.Fatalf("edge missing kind/confidence/rule: %+v", edgeKeys)
	}
	if !strings.HasPrefix(edgeKeys["d6"], "py.") {
		t.Fatalf("edge rule = %q, want a py.* rule", edgeKeys["d6"])
	}

	// Byte-identical on a second run.
	code, again, _ := run(t, "export", "--graphml", "--root", root)
	if code != 0 || again != out {
		t.Fatalf("graphml is not deterministic (code %d)", code)
	}

	// --out writes the same bytes and prints a summary instead of the graph.
	file := filepath.Join(t.TempDir(), "graph.graphml")
	code, summary, stderr := run(t, "export", "--graphml", "--out", file, "--root", root)
	if code != 0 {
		t.Fatalf("--out exit code = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(summary, "wrote ") || !strings.Contains(summary, "graphml") {
		t.Fatalf("summary = %q", summary)
	}
	written, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(written) != out {
		t.Fatal("--out file differs from the stdout graph")
	}

	// DOT carries the same attributes.
	code, dot, stderr := run(t, "export", "--dot", "--root", root)
	if code != 0 {
		t.Fatalf("dot exit code = %d, stderr = %s", code, stderr)
	}
	for _, want := range []string{"digraph repath {", "kind=\"function\"", "rule=\"py."} {
		if !strings.Contains(dot, want) {
			t.Fatalf("dot missing %q:\n%s", want, dot)
		}
	}
	if code, dot2, _ := run(t, "export", "--dot", "--root", root); code != 0 || dot2 != dot {
		t.Fatal("dot is not deterministic")
	}
}

func TestExportModulesCollapsesToModuleEdges(t *testing.T) {
	root := exportFixture(t)
	code, out, stderr := run(t, "export", "--graphml", "--modules", "--root", root)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr = %s", code, stderr)
	}
	var doc graphMLDoc
	if err := xml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("graphml is not well-formed: %v", err)
	}
	if len(doc.Graph.Nodes) != 2 {
		t.Fatalf("module nodes = %d, want 2", len(doc.Graph.Nodes))
	}
	names := make([]string, 0, 2)
	for _, node := range doc.Graph.Nodes {
		for _, data := range node.Data {
			if data.Key == "d1" && data.Value != "module" {
				t.Fatalf("module node kind = %q", data.Value)
			}
			if data.Key == "d0" {
				names = append(names, data.Value)
			}
		}
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "alpha,beta" {
		t.Fatalf("module nodes = %v", names)
	}
	if len(doc.Graph.Edges) != 1 {
		t.Fatalf("module edges = %d, want one beta -> alpha edge", len(doc.Graph.Edges))
	}
	edge := doc.Graph.Edges[0]
	attrs := map[string]string{}
	for _, data := range edge.Data {
		attrs[data.Key] = data.Value
	}
	if attrs["d3"] != "DEPENDS" || attrs["d5"] != "module.aggregate" || attrs["d6"] == "" {
		t.Fatalf("module edge attrs = %+v, want DEPENDS/module.aggregate/weight", attrs)
	}
	if attrs["d6"] == "0" {
		t.Fatalf("module edge weight = %q, want the underlying symbol edge count", attrs["d6"])
	}
}

func TestExportUsageErrors(t *testing.T) {
	root := exportFixture(t)
	cases := []struct {
		name string
		args []string
	}{
		{"neither format", []string{"export", "--root", root}},
		{"both formats", []string{"export", "--graphml", "--dot", "--root", root}},
		{"positional", []string{"export", "--dot", "extra", "--root", root}},
		{"artifact flag", []string{"export", "--dot", "--artifact", t.TempDir(), "--root", root}},
		{"json flag", []string{"export", "--dot", "--json", "--root", root}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if code, _, _ := run(t, tc.args...); code != 2 {
				t.Fatalf("exit code = %d, want 2", code)
			}
		})
	}
}

// checkGraphMLShape is the minimal schema check: unique node ids, every edge
// endpoint declared, every declared key present on every element, and a
// confidence that parses as a probability.
func checkGraphMLShape(t *testing.T, doc graphMLDoc) {
	t.Helper()
	declared := map[string]bool{}
	for _, node := range doc.Graph.Nodes {
		if declared[node.ID] {
			t.Fatalf("duplicate node id %q", node.ID)
		}
		declared[node.ID] = true
		keysSeen := map[string]string{}
		for _, data := range node.Data {
			keysSeen[data.Key] = data.Value
		}
		for _, key := range []string{"d0", "d1", "d2", "d3"} {
			if _, ok := keysSeen[key]; !ok {
				t.Fatalf("node %q missing data key %s", node.ID, key)
			}
		}
	}
	for _, edge := range doc.Graph.Edges {
		if !declared[edge.Source] || !declared[edge.Target] {
			t.Fatalf("edge %s -> %s has an undeclared endpoint", edge.Source, edge.Target)
		}
		keysSeen := map[string]string{}
		for _, data := range edge.Data {
			keysSeen[data.Key] = data.Value
		}
		for _, key := range []string{"d4", "d5", "d6"} {
			if _, ok := keysSeen[key]; !ok {
				t.Fatalf("edge %s -> %s missing data key %s", edge.Source, edge.Target, key)
			}
		}
		if _, err := strconv.ParseFloat(keysSeen["d5"], 64); err != nil {
			t.Fatalf("edge confidence %q is not a float", keysSeen["d5"])
		}
	}
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
