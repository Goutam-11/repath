package adapter

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Goutam-11/repath/internal/store"
)

func loadLSPFixture(t *testing.T) LSPFacts {
	t.Helper()
	facts, err := LoadLSPFacts(filepath.Join("..", "..", "testdata", "fixtures", "lsp-adapter", "facts.json"))
	if err != nil {
		t.Fatal(err)
	}
	return facts
}

// nodeQName finds a node by file and qname; the LSP fixture has two `total`
// methods in one file, so a name-only lookup would be ambiguous.
func nodeQName(t *testing.T, ctx context.Context, st *store.Store, path, qname string) int64 {
	t.Helper()
	nodes, err := st.Nodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := st.FilePaths(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range nodes {
		if paths[node.FileID] == path && node.QName == qname {
			return node.ID
		}
	}
	t.Fatalf("node %s in %s not found", qname, path)
	return 0
}

func TestLSPFactsAdapterGate(t *testing.T) {
	ctx, st := openFixtureStore(t, "lsp-adapter")

	before, err := st.CountPending(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if before != 4 {
		t.Fatalf("adapter-off pending = %d, want 4 untyped total() calls", before)
	}

	stats, err := ApplyLSPFacts(ctx, st, loadLSPFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if stats.Adapter != "lsp" {
		t.Fatalf("adapter = %q, want lsp", stats.Adapter)
	}
	if stats.Definitions != 2 || stats.References != 3 {
		t.Fatalf("stats = %+v, want 2 definitions / 3 references", stats)
	}
	if stats.Matched != 2 || stats.Edges != 2 || stats.Conflicts != 0 || stats.Replaced != 0 {
		t.Fatalf("stats = %+v, want 2 matched rows / 2 edges / no conflict / no replacement", stats)
	}
	if stats.Skipped != 0 || stats.Unmatched != 1 {
		t.Fatalf("stats = %+v, want 0 skipped / 1 unmatched (the refused key's local half)", stats)
	}

	after, err := st.CountPending(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after != 2 {
		t.Fatalf("pending after = %d, want the 2-row line-8 key untouched", after)
	}

	run := nodeQName(t, ctx, st, "src/run.ts", "run")
	left := nodeQName(t, ctx, st, "src/cart.ts", "Left.total")
	right := nodeQName(t, ctx, st, "src/cart.ts", "Right.total")
	edges, err := st.Callers(ctx, left, 0.99)
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 1 || edges[0].SrcNode.ID != run {
		t.Fatalf("Left.total callers = %+v, want only run()", edges)
	}
	if edges[0].Rule != "lsp.ts.method" || edges[0].Confidence != adapterConfidence || edges[0].Kind != "CALLS" || edges[0].Line != 6 {
		t.Fatalf("Left.total edge = %+v, want lsp.ts.method CALLS at %.2f on line 6", edges[0], adapterConfidence)
	}
	edges, err = st.Callers(ctx, right, 0.99)
	if err != nil {
		t.Fatal(err)
	}
	if len(edges) != 1 || edges[0].SrcNode.ID != run || edges[0].Rule != "lsp.ts.method" {
		t.Fatalf("Right.total callers = %+v, want one lsp.ts.method edge from run()", edges)
	}

	// The refused key keeps both pending rows: the compiler accounted for one
	// of the two same-line occurrences, and the schema has no reference column
	// to attribute the local half without risking the external one.
	runFile, ok, err := st.FileByPath(ctx, "src/run.ts")
	if err != nil || !ok {
		t.Fatalf("run.ts file id: %v %v", ok, err)
	}
	rows, err := st.Pending(ctx)
	if err != nil {
		t.Fatal(err)
	}
	kept := 0
	for _, row := range rows {
		if row.FileID == runFile && row.Line == 8 && row.Name == "total" {
			kept++
		}
	}
	if kept != 2 {
		t.Fatalf("line-8 pending rows = %d, want both kept", kept)
	}

	saved, err := SavedStats(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved) != 1 || saved[0].Adapter != "lsp" || saved[0].Edges != 2 {
		t.Fatalf("saved stats = %+v, want one lsp run with 2 edges", saved)
	}
}

func TestLSPFactsRefusesWhenOccurrencesDoNotMatch(t *testing.T) {
	ctx, st := openFixtureStore(t, "lsp-adapter")

	// Three local occurrences at the line-6 key with two pending rows: the
	// occurrences cannot be paired with rows, so nothing may be deleted.
	facts := LSPFacts{
		Schema:   1,
		Language: "ts",
		Definitions: []LSPDefinition{
			{File: "src/cart.ts", Line: 2, Column: 3, Name: "total"},
			{File: "src/cart.ts", Line: 6, Column: 3, Name: "total"},
		},
		References: []LSPReference{
			{File: "src/run.ts", Line: 6, Column: 5, Name: "total", Target: LSPTarget{File: "src/cart.ts", Line: 2}},
			{File: "src/run.ts", Line: 6, Column: 16, Name: "total", Target: LSPTarget{File: "src/cart.ts", Line: 6}},
			{File: "src/run.ts", Line: 6, Column: 27, Name: "total", Target: LSPTarget{File: "src/cart.ts", Line: 2}},
		},
	}
	stats, err := ApplyLSPFacts(ctx, st, facts)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Matched != 0 || stats.Edges != 0 || stats.Unmatched != 3 {
		t.Fatalf("stats = %+v, want no merge and 3 unmatched occurrences", stats)
	}
	pending, err := st.CountPending(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pending != 4 {
		t.Fatalf("pending = %d, want all 4 rows kept", pending)
	}
}

func TestLSPFactsIdempotent(t *testing.T) {
	ctx, st := openFixtureStore(t, "lsp-adapter")
	facts := loadLSPFixture(t)

	if _, err := ApplyLSPFacts(ctx, st, facts); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyLSPFacts(ctx, st, facts); err != nil {
		t.Fatal(err)
	}
	edges, err := st.Edges(ctx, 0.99)
	if err != nil {
		t.Fatal(err)
	}
	lspEdges := 0
	for _, edge := range edges {
		if strings.HasPrefix(edge.Rule, "lsp.") {
			lspEdges++
		}
	}
	if lspEdges != 2 {
		t.Fatalf("adapter edges after two runs = %d, want the same 2", lspEdges)
	}
	pending, err := st.CountPending(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pending != 2 {
		t.Fatalf("pending after two runs = %d, want 2", pending)
	}
}

func TestLSPFactsConflictIsReported(t *testing.T) {
	ctx, st := openFixtureStore(t, "lsp-adapter")
	run := nodeQName(t, ctx, st, "src/run.ts", "run")
	left := nodeQName(t, ctx, st, "src/cart.ts", "Left.total")
	if _, err := st.InsertEdges(ctx, []store.Edge{{
		Src: run, Dst: left, Kind: "CALLS", Confidence: 0.70, Rule: "test.preexisting", Line: 6,
	}}); err != nil {
		t.Fatal(err)
	}

	stats, err := ApplyLSPFacts(ctx, st, loadLSPFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if stats.Edges != 1 || stats.Conflicts != 1 {
		t.Fatalf("stats = %+v, want 1 inserted edge and 1 conflict", stats)
	}
	callers, err := st.Callers(ctx, left, 0.5)
	if err != nil {
		t.Fatal(err)
	}
	if len(callers) != 1 || callers[0].Rule != "test.preexisting" || callers[0].Confidence != 0.70 {
		t.Fatalf("existing edge = %+v, want it unchanged", callers)
	}
}

func TestLSPAmbiguousReplacement(t *testing.T) {
	ctx, st := openFixtureStore(t, "py-adapter-ambiguous")
	if ambiguous, err := st.AmbiguousEdges(ctx); err != nil || ambiguous != 4 {
		t.Fatalf("ambiguous before = %d (%v), want 4", ambiguous, err)
	}

	facts := LSPFacts{
		Schema:   1,
		Language: "python",
		Definitions: []LSPDefinition{
			{File: "models_a.py", Line: 1, Column: 5, Name: "total"},
			{File: "models_b.py", Line: 1, Column: 5, Name: "total"},
		},
		References: []LSPReference{
			{File: "app.py", Line: 2, Column: 12, Name: "total", Target: LSPTarget{File: "models_a.py", Line: 1}},
		},
	}
	stats, err := ApplyLSPFacts(ctx, st, facts)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Replaced != 1 || stats.ReplacedEdges != 2 || stats.Edges != 1 || stats.Conflicts != 0 {
		t.Fatalf("stats = %+v, want 1 replaced group / 2 rows / 1 edge", stats)
	}
	one := nodeQName(t, ctx, st, "app.py", "one")
	totalA := nodeQName(t, ctx, st, "models_a.py", "total")
	callers, err := st.Callers(ctx, totalA, 0.99)
	if err != nil {
		t.Fatal(err)
	}
	if len(callers) != 1 || callers[0].SrcNode.ID != one {
		t.Fatalf("exact callers = %+v, want only one()", callers)
	}
	if callers[0].Rule != "lsp.py.call" || callers[0].Kind != "CALLS" || callers[0].Confidence != adapterConfidence {
		t.Fatalf("replacement edge = %+v, want lsp.py.call CALLS at 0.99", callers[0])
	}
	ambiguous, err := st.AmbiguousEdges(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ambiguous != 2 {
		t.Fatalf("ambiguous after = %d, want the two() group untouched", ambiguous)
	}
}

func TestLSPAmbiguousSameLineTwoOccurrencesNotMerged(t *testing.T) {
	ctx, st := openFixtureStore(t, "py-adapter-ambiguous")

	facts := LSPFacts{
		Schema:   1,
		Language: "python",
		Definitions: []LSPDefinition{
			{File: "models_a.py", Line: 1, Column: 5, Name: "total"},
			{File: "models_b.py", Line: 1, Column: 5, Name: "total"},
		},
		References: []LSPReference{
			{File: "app.py", Line: 6, Column: 12, Name: "total", Target: LSPTarget{File: "models_a.py", Line: 1}},
			{File: "app.py", Line: 6, Column: 22, Name: "total", Target: LSPTarget{File: "models_b.py", Line: 1}},
		},
	}
	stats, err := ApplyLSPFacts(ctx, st, facts)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Replaced != 0 || stats.Edges != 0 || stats.Unmatched != 2 {
		t.Fatalf("stats = %+v, want the same-line pair left alone", stats)
	}
	ambiguous, err := st.AmbiguousEdges(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ambiguous != 4 {
		t.Fatalf("ambiguous = %d, want all 4 candidate rows untouched", ambiguous)
	}
}

func TestLSPFactsEmptyIsValid(t *testing.T) {
	ctx, st := openFixtureStore(t, "lsp-adapter")
	facts, err := ParseLSPFacts([]byte(`{"schema":1,"language":"python","definitions":[],"references":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	stats, err := ApplyLSPFacts(ctx, st, facts)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Adapter != "lsp-python" || stats.Definitions != 0 || stats.References != 0 || stats.Edges != 0 {
		t.Fatalf("stats = %+v, want a zero-fact lsp-python run", stats)
	}
	pending, err := st.CountPending(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if pending != 4 {
		t.Fatalf("pending = %d, want unchanged", pending)
	}
}

func TestParseLSPFactsValidation(t *testing.T) {
	valid := `{"schema":1,"language":"ts","definitions":[{"file":"src/a.ts","line":1,"column":1,"name":"a"}],"references":[{"file":"src/b.ts","line":2,"column":1,"name":"a","target":{"file":"src/a.ts","line":1}}]}`
	if _, err := ParseLSPFacts([]byte(valid)); err != nil {
		t.Fatalf("valid facts: %v", err)
	}

	cases := map[string]struct {
		body string
		want string
	}{
		"invalid json":       {`{`, "invalid JSON"},
		"trailing data":      {valid + `{}`, "unexpected data"},
		"unsupported schema": {strings.Replace(valid, `"schema":1`, `"schema":2`, 1), "schema 2 is not supported"},
		"unknown language":   {strings.Replace(valid, `"language":"ts"`, `"language":"go"`, 1), `language "go"`},
		"absolute path":      {strings.Replace(valid, `"src/a.ts"`, `"/tmp/a.ts"`, 1), "repo-relative"},
		"wrong extension":    {strings.Replace(valid, `"language":"ts"`, `"language":"python"`, 1), "not a python source file"},
		"zero line":          {strings.Replace(valid, `"line":2`, `"line":0`, 1), "not 1-based"},
		"zero column":        {strings.Replace(valid, `"column":1`, `"column":0`, 1), "not 1-based"},
		"empty name":         {strings.Replace(valid, `"name":"a"`, `"name":" "`, 1), "name is empty"},
		"unknown target":     {strings.Replace(valid, `"line":1}}`, `"line":9}}`, 1), "is not a reported definition"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := ParseLSPFacts([]byte(tc.body))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestLSPFactsUnknownLanguageRejectedByApply(t *testing.T) {
	_, st := openFixtureStore(t, "lsp-adapter")
	_, err := ApplyLSPFacts(context.Background(), st, LSPFacts{Schema: 1, Language: "go"})
	if err == nil || !strings.Contains(err.Error(), "unsupported language") {
		t.Fatalf("error = %v, want unsupported language", err)
	}
}

func TestLoadLSPFactsMissingFile(t *testing.T) {
	_, err := LoadLSPFacts(filepath.Join(t.TempDir(), "missing.json"))
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
}
