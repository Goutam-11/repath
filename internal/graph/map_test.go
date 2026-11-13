package graph_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/Goutam-11/repath/internal/graph"
	"github.com/Goutam-11/repath/internal/store"
)

func buildMapFixture(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	moduleIDs, err := st.UpsertModules(ctx, []store.Module{
		{Name: "a", Kind: "package", Root: "a"},
		{Name: "b", Kind: "package", Root: "b"},
	})
	if err != nil {
		t.Fatal(err)
	}

	type fileSpec struct {
		path  string
		nodes []string
	}
	files := []fileSpec{
		{path: "a/one.ts", nodes: []string{"f1", "f2"}},
		{path: "a/two.ts", nodes: []string{"f3"}},
		{path: "b/one.ts", nodes: []string{"f4"}},
	}
	extra := make([]string, 0, 10)
	for i := 0; i < 10; i++ {
		extra = append(extra, "extra"+string(rune('a'+i)))
	}
	files = append(files, fileSpec{path: "b/extra.ts", nodes: extra})

	ids := map[string]int64{}
	tx, err := st.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		module := "a"
		if file.path[0] == 'b' {
			module = "b"
		}
		fileID, err := tx.UpsertFile(ctx, store.File{
			Path: file.path, Lang: "typescript", ModuleID: moduleIDs[module],
			Hash: "h-" + file.path, ParseOK: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		nodes := make([]store.Node, 0, len(file.nodes))
		for i, name := range file.nodes {
			nodes = append(nodes, store.Node{
				Kind: "function", Name: name, QName: name,
				StartLine: i*10 + 1, EndLine: i*10 + 5, Exported: true,
			})
		}
		if _, err := tx.ReplaceNodes(ctx, fileID, "typescript", nodes); err != nil {
			t.Fatal(err)
		}
		fileIDs, err := tx.NodeIDsByFile(ctx, fileID)
		if err != nil {
			t.Fatal(err)
		}
		for name, id := range fileIDs {
			ids[name] = id
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	edges := []store.Edge{
		{Src: ids["f1"], Dst: ids["f2"], Kind: "CALLS", Confidence: 0.9, Rule: "ts.local"},
		{Src: ids["f2"], Dst: ids["f3"], Kind: "CALLS", Confidence: 0.9, Rule: "ts.local"},
		{Src: ids["f3"], Dst: ids["f4"], Kind: "CALLS", Confidence: 0.9, Rule: "ts.local"},
	}
	if _, err := st.InsertEdges(ctx, edges); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestBuildMapRollsUpModulesAndPaths(t *testing.T) {
	ctx := context.Background()
	st := buildMapFixture(t)

	result, err := graph.BuildMap(ctx, st, 2000, 0.5)
	if err != nil {
		t.Fatalf("build map: %v", err)
	}
	if len(result.Modules) != 2 {
		t.Fatalf("modules = %+v, want 2", result.Modules)
	}
	byName := map[string]graph.ModuleStat{}
	for _, module := range result.Modules {
		byName[module.Name] = module
	}
	if byName["a"].Symbols != 3 || byName["b"].Symbols != 11 {
		t.Fatalf("module symbols = %+v, want a=3 b=11", result.Modules)
	}
	if len(byName["a"].Deps) != 1 || byName["a"].Deps[0] != "b" {
		t.Fatalf("a deps = %+v, want [b]", byName["a"].Deps)
	}
	if len(byName["b"].Deps) != 0 {
		t.Fatalf("b deps = %+v, want none", byName["b"].Deps)
	}

	if len(result.Top) != 14 {
		t.Fatalf("top = %d, want all 14 symbols", len(result.Top))
	}
	for _, ranked := range result.Top {
		if ranked.File == "" || ranked.QName == "" {
			t.Fatalf("ranked endpoint missing fields: %+v", ranked)
		}
	}
	for i := 1; i < len(result.Top); i++ {
		prev, cur := result.Top[i-1], result.Top[i]
		if cur.Rank > prev.Rank || (cur.Rank == prev.Rank && cur.QName < prev.QName) {
			t.Fatalf("top not ordered at %d: %+v then %+v", i, prev, cur)
		}
	}

	// The 14 nodes need ~230 tokens; a 100-token budget keeps only the head.
	truncated, err := graph.BuildMap(ctx, st, 100, 0.5)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated.Truncated || len(truncated.Top) >= 14 || len(truncated.Top) == 0 {
		t.Fatalf("truncated = %v top=%d, want a non-empty prefix", truncated.Truncated, len(truncated.Top))
	}
}

func TestCyclesFindsStronglyConnectedModules(t *testing.T) {
	cycles := graph.Cycles(map[string]map[string]bool{
		"a": {"b": true},
		"b": {"a": true},
		"c": {"c": true},
	})
	if len(cycles) != 2 {
		t.Fatalf("cycles = %+v, want a 2-module cycle and one self-loop", cycles)
	}
	if len(cycles[0]) != 2 || cycles[0][0] != "a" || cycles[0][1] != "b" {
		t.Fatalf("first cycle = %+v, want [a b]", cycles[0])
	}
	if len(cycles[1]) != 1 || cycles[1][0] != "c" {
		t.Fatalf("second cycle = %+v, want [c]", cycles[1])
	}
}
