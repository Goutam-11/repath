package graph_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/Goutam-11/repath/internal/graph"
	"github.com/Goutam-11/repath/internal/store"
)

// buildWalkerFixture inserts a small symbol graph directly: a call chain with a
// back edge (cycle), a diamond with two paths, an isolated pair for the
// no-path frontier, and a forward flow with a cycle and a below-floor edge.
func buildWalkerFixture(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	moduleIDs, err := st.UpsertModules(ctx, []store.Module{{Name: "m", Kind: "package", Root: "."}})
	if err != nil {
		t.Fatalf("modules: %v", err)
	}

	nodes := []string{
		"a", "b", "c", "d", "e", "f", "x", "y", "gw", "l", "r", "dest", "iso",
		"flow", "stepA", "stepB", "endA", "endB", "low", "below", "deep1", "deep2", "deep3",
	}
	byFile := map[string][]string{
		"chain.ts":   {"a", "b", "c", "d", "e", "f"},
		"cycle.ts":   {"x", "y"},
		"diamond.ts": {"gw", "l", "r", "dest"},
		"flow.ts":    {"flow", "stepA", "stepB", "endA", "endB", "low", "below", "iso"},
		"deep.ts":    {"deep1", "deep2", "deep3"},
	}

	ids := make(map[string]int64, len(nodes))
	tx, err := st.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for path, names := range byFile {
		fileID, err := tx.UpsertFile(ctx, store.File{
			Path: path, Lang: "typescript", ModuleID: moduleIDs["m"],
			Hash: "h-" + path, ParseOK: true,
		})
		if err != nil {
			t.Fatalf("file %s: %v", path, err)
		}
		symbols := make([]store.Node, 0, len(names))
		for i, name := range names {
			symbols = append(symbols, store.Node{
				Kind: "function", Name: name, QName: name,
				StartLine: i*10 + 1, EndLine: i*10 + 5, Exported: true,
			})
		}
		if _, err := tx.ReplaceNodes(ctx, fileID, "typescript", symbols); err != nil {
			t.Fatalf("nodes %s: %v", path, err)
		}
		fileNodes, err := tx.NodeIDsByFile(ctx, fileID)
		if err != nil {
			t.Fatalf("ids %s: %v", path, err)
		}
		for name, id := range fileNodes {
			ids[name] = id
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	edge := func(src, dst string, confidence float64, rule string) store.Edge {
		return store.Edge{Src: ids[src], Dst: ids[dst], Kind: "CALLS", Confidence: confidence, Rule: rule}
	}
	edges := []store.Edge{
		// chain a -> b -> c -> d -> e -> f, plus b -> a (cycle)
		edge("a", "b", 0.9, "ts.local"),
		edge("b", "c", 0.9, "ts.local"),
		edge("c", "d", 0.9, "ts.local"),
		edge("d", "e", 0.9, "ts.local"),
		edge("e", "f", 0.9, "ts.local"),
		edge("b", "a", 0.9, "ts.local"),
		edge("low", "a", 0.4, "ts.global.ambiguous"),
		// diamond gw -> {l,r} -> dest
		edge("gw", "l", 0.9, "ts.local"),
		edge("l", "dest", 0.9, "ts.local"),
		edge("gw", "r", 0.9, "ts.local"),
		edge("r", "dest", 0.9, "ts.local"),
		// x <-> y
		edge("x", "y", 0.9, "ts.local"),
		edge("y", "x", 0.9, "ts.local"),
		// forward flow with a back edge and one below-floor continuation
		edge("flow", "stepA", 0.9, "ts.local"),
		edge("flow", "stepB", 0.9, "ts.local"),
		edge("stepA", "endA", 0.9, "ts.local"),
		edge("stepB", "endB", 0.9, "ts.local"),
		edge("stepA", "flow", 0.9, "ts.local"),
		edge("endA", "below", 0.4, "ts.global.ambiguous"),
		// a linear chain for the flow depth bound
		edge("deep1", "deep2", 0.9, "ts.local"),
		edge("deep2", "deep3", 0.9, "ts.local"),
	}
	if _, err := st.InsertEdges(ctx, edges); err != nil {
		t.Fatalf("edges: %v", err)
	}
	return st
}

func nodeByQName(t *testing.T, st *store.Store, qname string) store.NodeRow {
	t.Helper()
	nodes, err := st.SearchSymbols(context.Background(), qname, 20)
	if err != nil {
		t.Fatalf("search %s: %v", qname, err)
	}
	for _, node := range nodes {
		if node.QName == qname {
			return node
		}
	}
	t.Fatalf("node %q not found", qname)
	return store.NodeRow{}
}

func TestBlastDepthLimitAndCycleSafety(t *testing.T) {
	ctx := context.Background()
	st := buildWalkerFixture(t)
	f := nodeByQName(t, st, "f")

	full, err := graph.Blast(ctx, st, []store.NodeRow{f}, 8, 0.5, 50)
	if err != nil {
		t.Fatalf("blast: %v", err)
	}
	if len(full.Dependents) != 5 {
		t.Fatalf("dependents = %+v, want the five-symbol chain", full.Dependents)
	}
	depths := map[string]int{}
	for _, dep := range full.Dependents {
		depths[dep.QName] = dep.Depth
	}
	for name, want := range map[string]int{"e": 1, "d": 2, "c": 3, "b": 4, "a": 5} {
		if depths[name] != want {
			t.Fatalf("%s depth = %d, want %d (all %v)", name, depths[name], want, depths)
		}
	}
	if full.Truncated {
		t.Fatal("uncapped blast must not be truncated")
	}

	shallow, err := graph.Blast(ctx, st, []store.NodeRow{f}, 2, 0.5, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(shallow.Dependents) != 2 {
		t.Fatalf("depth-2 dependents = %+v", shallow.Dependents)
	}

	capped, err := graph.Blast(ctx, st, []store.NodeRow{f}, 8, 0.5, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(capped.Dependents) != 2 || !capped.Truncated {
		t.Fatalf("limit-2 blast = %d truncated=%v", len(capped.Dependents), capped.Truncated)
	}

	// b -> a is a back edge; the visited set must stop the walk instead of
	// re-adding a, b, a...
	cycle, err := graph.Blast(ctx, st, []store.NodeRow{nodeByQName(t, st, "a")}, 8, 0.5, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(cycle.Dependents) != 1 || cycle.Dependents[0].QName != "b" {
		t.Fatalf("cycle blast = %+v, want only b", cycle.Dependents)
	}

	// The low (0.40) edge low -> a is excluded at the default floor.
	loose, err := graph.Blast(ctx, st, []store.NodeRow{nodeByQName(t, st, "a")}, 8, 0.4, 50)
	if err != nil {
		t.Fatal(err)
	}
	foundLow := false
	for _, dep := range loose.Dependents {
		if dep.QName == "low" {
			foundLow = true
		}
	}
	if !foundLow {
		t.Fatalf("0.4 blast missing the below-floor dependent: %+v", loose.Dependents)
	}
}

func TestConnectAlternatesAndFrontier(t *testing.T) {
	ctx := context.Background()
	st := buildWalkerFixture(t)
	gw := nodeByQName(t, st, "gw")
	dest := nodeByQName(t, st, "dest")

	result, err := graph.Connect(ctx, st, gw, dest, 8, 0.5, 2)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if len(result.Hops) != 2 {
		t.Fatalf("hops = %+v, want 2", result.Hops)
	}
	if len(result.Alternate) != 1 {
		t.Fatalf("alternates = %+v, want 1", result.Alternate)
	}
	if result.Hops[0].Dst.QName == result.Alternate[0][0].Dst.QName {
		t.Fatalf("alternate leaves by the same branch: %+v", result)
	}

	// One hop is not enough for the diamond, so no path exists at depth 1 and
	// both frontiers report what each end can reach.
	short, err := graph.Connect(ctx, st, gw, dest, 1, 0.5, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(short.Hops) != 0 {
		t.Fatalf("depth-1 path = %+v, want none", short.Hops)
	}
	if len(short.FrontierA) == 0 || len(short.FrontierB) == 0 {
		t.Fatalf("frontiers = %+v / %+v, want both sides", short.FrontierA, short.FrontierB)
	}

	// A two-node cycle terminates and still yields the direct edge.
	cycle, err := graph.Connect(ctx, st, nodeByQName(t, st, "x"), nodeByQName(t, st, "y"), 8, 0.5, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(cycle.Hops) != 1 {
		t.Fatalf("cycle path = %+v, want one hop", cycle.Hops)
	}

	noPath, err := graph.Connect(ctx, st, nodeByQName(t, st, "x"), nodeByQName(t, st, "iso"), 8, 0.5, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(noPath.Hops) != 0 || len(noPath.FrontierA) == 0 {
		t.Fatalf("isolated target = %+v, want a non-empty frontier", noPath)
	}
}

func TestFlowsDepthLimitAndCycleSafety(t *testing.T) {
	ctx := context.Background()
	st := buildWalkerFixture(t)
	flow := nodeByQName(t, st, "flow")

	full, err := graph.Flows(ctx, st, flow, "ts.route.express", 8, 0.5, 50)
	if err != nil {
		t.Fatalf("flows: %v", err)
	}
	if len(full.Paths) != 2 {
		t.Fatalf("paths = %d, want the two forward branches", len(full.Paths))
	}
	for _, path := range full.Paths {
		if len(path) != 2 {
			t.Fatalf("path = %+v, want 2 steps", path)
		}
	}
	// endA -> low is 0.40, so it is not followed at the default floor.
	for _, path := range full.Paths {
		for _, step := range path {
			if step.QName == "low" {
				t.Fatalf("below-floor step followed: %+v", path)
			}
		}
	}

	shallow, err := graph.Flows(ctx, st, flow, "ts.route.express", 1, 0.5, 50)
	if err != nil {
		t.Fatal(err)
	}
	if !shallow.Truncated || len(shallow.Paths) != 2 {
		t.Fatalf("depth-1 flows = %d paths truncated=%v", len(shallow.Paths), shallow.Truncated)
	}

	capped, err := graph.Flows(ctx, st, flow, "ts.route.express", 8, 0.5, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !capped.Truncated || len(capped.Paths) != 1 {
		t.Fatalf("limit-1 flows = %d paths truncated=%v", len(capped.Paths), capped.Truncated)
	}

	// At 0.4 the low continuation is followed, so one branch becomes 3 steps.
	loose, err := graph.Flows(ctx, st, flow, "ts.route.express", 8, 0.4, 50)
	if err != nil {
		t.Fatal(err)
	}
	longest := 0
	for _, path := range loose.Paths {
		if len(path) > longest {
			longest = len(path)
		}
	}
	if longest != 3 {
		for _, path := range loose.Paths {
			t.Logf("path: %+v", path)
		}
		t.Fatalf("0.4 flows longest path = %d, want 3", longest)
	}

	// An isolated symbol has no paths and reports itself as the frontier.
	isolated, err := graph.Flows(ctx, st, nodeByQName(t, st, "iso"), "ts.route.express", 8, 0.5, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(isolated.Paths) != 0 || len(isolated.Frontier) != 1 || isolated.Frontier[0].QName != "iso" {
		t.Fatalf("isolated flows = %+v", isolated)
	}

	// A linear chain stops at the depth bound rather than running away.
	deep, err := graph.Flows(ctx, st, nodeByQName(t, st, "deep1"), "ts.main", 1, 0.5, 50)
	if err != nil {
		t.Fatal(err)
	}
	if !deep.Truncated || len(deep.Paths) != 1 || len(deep.Paths[0]) != 1 {
		t.Fatalf("deep chain = %+v", deep)
	}
}

func TestBuildMapEmptyGraph(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	result, err := graph.BuildMap(ctx, st, 100, 0.5)
	if err != nil {
		t.Fatalf("empty map: %v", err)
	}
	if len(result.Modules) != 0 || len(result.Top) != 0 || len(result.Cycles) != 0 {
		t.Fatalf("empty map = %+v", result)
	}
}
