package index_test

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/Goutam-11/repath/internal/graph"
	"github.com/Goutam-11/repath/internal/index"
	"github.com/Goutam-11/repath/internal/store"
)

func copyXlangFixture(t *testing.T) string {
	t.Helper()
	src := filepath.Join("..", "..", "testdata", "fixtures", "xlang", "http")
	dst := t.TempDir()
	err := filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		t.Fatalf("copy fixture: %v", err)
	}
	return dst
}

func openXlangStore(t *testing.T) (context.Context, *store.Store, string) {
	t.Helper()
	root := copyXlangFixture(t)
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(root, ".repath", "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := index.Run(ctx, st, root, index.Options{}); err != nil {
		t.Fatal(err)
	}
	return ctx, st, root
}

func xlangNode(t *testing.T, ctx context.Context, st *store.Store, path, qname string) store.NodeRow {
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
			return node
		}
	}
	t.Fatalf("node %s in %s not found", qname, path)
	return store.NodeRow{}
}

func xlangEdges(t *testing.T, ctx context.Context, st *store.Store) []store.EdgeDetail {
	t.Helper()
	edges, err := st.Edges(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []store.EdgeDetail
	for _, edge := range edges {
		if strings.HasPrefix(edge.Rule, "xlang.") {
			out = append(out, edge)
		}
	}
	return out
}

func xlangEdgeRows(t *testing.T, ctx context.Context, st *store.Store) []string {
	t.Helper()
	rows := make([]string, 0)
	for _, edge := range xlangEdges(t, ctx, st) {
		rows = append(rows, edge.SrcPath+"|"+edge.SrcQName+" -> "+edge.DstPath+"|"+edge.DstQName+
			"|"+edge.Kind+"|"+edge.Rule)
	}
	sort.Strings(rows)
	return rows
}

func TestXlangHTTPBridge(t *testing.T) {
	ctx, st, _ := openXlangStore(t)

	anchors, err := st.Anchors(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(anchors) != 11 {
		t.Fatalf("anchors = %d, want the fixture's 11 literal route facts", len(anchors))
	}

	edges := xlangEdges(t, ctx, st)
	want := []string{
		"client/api.ts|addItem -> server/flask_app.py|add_item|REFERENCES|xlang.http.route",
		"client/api.ts|createUser -> server/app.py|create_user|REFERENCES|xlang.http.route",
		"client/api.ts|loadUsers -> server/app.py|list_users|REFERENCES|xlang.http.route",
	}
	got := xlangEdgeRows(t, ctx, st)
	if len(got) != len(want) {
		t.Fatalf("xlang edges = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("xlang edge %d = %q, want %q", i, got[i], want[i])
		}
		if edges[i].Confidence != 0.45 {
			t.Fatalf("edge %+v confidence = %v, want 0.45", edges[i], edges[i].Confidence)
		}
	}

	// Negatives: same-language pair, missing handler, dynamic URL, method
	// mismatch, and a plain string literal must produce no cross-language edge.
	for _, edge := range edges {
		switch edge.DstQName {
		case "products", "health", "missing", "dynamic":
			t.Fatalf("unexpected cross-language edge to %s: %+v", edge.DstQName, edge)
		}
	}
}

func TestXlangDefaultFloorHidesLinks(t *testing.T) {
	ctx, st, _ := openXlangStore(t)
	users := xlangNode(t, ctx, st, "server/app.py", "list_users")
	loadUsers := xlangNode(t, ctx, st, "client/api.ts", "loadUsers")

	// The default 0.5 floor must not change: the xlang edges are 0.45.
	blast, err := graph.Blast(ctx, st, []store.NodeRow{users}, 2, 0.5, 50)
	if err != nil {
		t.Fatal(err)
	}
	for _, dependent := range blast.Dependents {
		if dependent.QName == "loadUsers" {
			t.Fatalf("default impact reached loadUsers at the 0.5 floor: %+v", dependent)
		}
	}
	if conn, err := graph.Connect(ctx, st, loadUsers, users, 3, 0.5, 0); err != nil {
		t.Fatal(err)
	} else if len(conn.Hops) != 0 {
		t.Fatalf("default path found a cross-language hop: %+v", conn.Hops)
	}

	// Lowering the dial opts in: impact reaches the client and path finds one hop.
	blast, err = graph.Blast(ctx, st, []store.NodeRow{users}, 2, 0.45, 50)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, dependent := range blast.Dependents {
		if dependent.QName == "loadUsers" && dependent.Rule == "xlang.http.route" {
			found = true
			if dependent.Confidence != 0.45 || dependent.EdgeKind != "REFERENCES" {
				t.Fatalf("dependent = %+v, want REFERENCES at 0.45", dependent)
			}
		}
	}
	if !found {
		t.Fatalf("impact at 0.45 did not reach loadUsers: %+v", blast.Dependents)
	}

	conn, err := graph.Connect(ctx, st, loadUsers, users, 3, 0.45, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(conn.Hops) != 1 || conn.Hops[0].Rule != "xlang.http.route" || conn.Hops[0].Confidence != 0.45 {
		t.Fatalf("path = %+v, want one xlang.http.route hop at 0.45", conn.Hops)
	}
}

func TestXlangColdIncrementalFullParity(t *testing.T) {
	ctx, st, root := openXlangStore(t)
	cold := xlangEdgeRows(t, ctx, st)
	if len(cold) == 0 {
		t.Fatal("cold run produced no cross-language edges")
	}
	meta, err := st.Meta(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if meta["schema_version"] != "6" {
		t.Fatalf("schema_version = %q, want 6", meta["schema_version"])
	}
	if meta["xlang_indexed"] != "1" {
		t.Fatalf("xlang_indexed = %q, want 1", meta["xlang_indexed"])
	}

	// A migrated index without the marker must re-read every file once so the
	// anchors table is backfilled; simulate that by clearing the marker and
	// running an incremental pass with an edit to a different file.
	if err := st.SetMeta(ctx, "xlang_indexed", ""); err != nil {
		t.Fatal(err)
	}

	client := filepath.Join(root, "client", "api.ts")
	data, err := os.ReadFile(client)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(client, append(data, []byte("\n// parity edit\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := index.Run(ctx, st, root, index.Options{}); err != nil {
		t.Fatal(err)
	}
	incremental := xlangEdgeRows(t, ctx, st)
	if _, err := index.Run(ctx, st, root, index.Options{Full: true}); err != nil {
		t.Fatal(err)
	}
	full := xlangEdgeRows(t, ctx, st)

	if strings.Join(cold, "\n") != strings.Join(incremental, "\n") {
		t.Fatalf("cold vs incremental xlang rows differ:\n%v\n%v", cold, incremental)
	}
	if strings.Join(cold, "\n") != strings.Join(full, "\n") {
		t.Fatalf("cold vs full xlang rows differ:\n%v\n%v", cold, full)
	}
}
