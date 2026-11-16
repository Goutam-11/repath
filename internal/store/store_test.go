package store

import (
	"context"
	"path/filepath"
	"strconv"
	"testing"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(context.Background(), filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return st
}

func TestMigrateIsIdempotent(t *testing.T) {
	st := openTestStore(t)
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatalf("second migrate: %v", err)
	}

	meta, err := st.Meta(context.Background())
	if err != nil {
		t.Fatalf("meta: %v", err)
	}
	if meta["schema_version"] != "5" {
		t.Fatalf("schema_version = %q, want 5", meta["schema_version"])
	}
}

func TestMigrateAddsImportPublic(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	// Simulate an M3 index whose imports table predates the public column.
	if _, err := st.db.ExecContext(ctx, `ALTER TABLE imports DROP COLUMN public`); err != nil {
		t.Fatalf("drop public: %v", err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate old schema: %v", err)
	}

	rows, err := st.db.QueryContext(ctx, `PRAGMA table_info(imports)`)
	if err != nil {
		t.Fatalf("table info: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, pk int
		var name, typ string
		var dflt any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if name == "public" {
			return
		}
	}
	t.Fatal("imports.public was not added by Migrate")
}

func TestMigrateAddsPendingForm(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	// Simulate an M2 index whose pending table predates the form column.
	if _, err := st.db.ExecContext(ctx, `ALTER TABLE pending DROP COLUMN form`); err != nil {
		t.Fatalf("drop form: %v", err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate old schema: %v", err)
	}

	rows, err := st.db.QueryContext(ctx, `PRAGMA table_info(pending)`)
	if err != nil {
		t.Fatalf("table info: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, pk int
		var name, typ string
		var dflt any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if name == "form" {
			return
		}
	}
	t.Fatal("pending.form was not added by Migrate")
}

func TestWriteFileAndNodes(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	tx, err := st.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	id, err := tx.UpsertFile(ctx, File{
		Path: "src/a.ts", Lang: "typescript", Size: 10, MTime: 100, Hash: "abc", ParseOK: true,
	})
	if err != nil {
		t.Fatalf("upsert file: %v", err)
	}
	if id == 0 {
		t.Fatal("file id = 0")
	}
	inserted, err := tx.ReplaceNodes(ctx, id, "typescript", []Node{
		{Kind: "function", Name: "run", QName: "run", Arity: 1, StartLine: 1, EndLine: 3, Exported: true},
		{Kind: "function", Name: "run", QName: "run", Arity: 1, StartLine: 1, EndLine: 3},
	})
	if err != nil {
		t.Fatalf("replace nodes: %v", err)
	}
	if inserted.Written != 1 || inserted.Added != 1 {
		t.Fatalf("inserted = %+v, want one added symbol (duplicates collapse)", inserted)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	counts, err := st.Counts(ctx)
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if counts.Files != 1 || counts.Nodes != 1 || counts.ParseFailures != 0 {
		t.Fatalf("counts = %+v", counts)
	}
	if counts.ByKind["function"] != 1 {
		t.Fatalf("by kind = %+v", counts.ByKind)
	}
}

func TestWriteFileAndImports(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	tx, err := st.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	id, err := tx.UpsertFile(ctx, File{
		Path: "pkg/__init__.py", Lang: "python", Size: 10, MTime: 100, Hash: "abc", ParseOK: true,
	})
	if err != nil {
		t.Fatalf("upsert file: %v", err)
	}
	if err := tx.ReplaceImports(ctx, id, []ImportRow{
		{Local: "Service", Imported: "Service", Source: ".service", Line: 1, Public: true},
		{Local: "util", Imported: "*", Source: "util", Line: 2},
	}); err != nil {
		t.Fatalf("replace imports: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	imports, err := st.Imports(ctx)
	if err != nil {
		t.Fatalf("imports: %v", err)
	}
	if len(imports) != 2 || imports[0].Local != "Service" || imports[0].Source != ".service" || !imports[0].Public {
		t.Fatalf("imports = %+v", imports)
	}

	tx, err = st.Begin(ctx)
	if err != nil {
		t.Fatalf("begin replace: %v", err)
	}
	if err := tx.ReplaceImports(ctx, id, []ImportRow{{Local: "util", Imported: "*", Source: "util"}}); err != nil {
		t.Fatalf("replace imports: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit replace: %v", err)
	}
	imports, err = st.Imports(ctx)
	if err != nil {
		t.Fatalf("imports after replace: %v", err)
	}
	if len(imports) != 1 || imports[0].Local != "util" {
		t.Fatalf("imports after replace = %+v, want one util binding", imports)
	}
}

func TestReplaceNodesKeepsStableIds(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	tx, err := st.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	fileID, err := tx.UpsertFile(ctx, File{
		Path: "pkg/mod.py", Lang: "python", Size: 10, MTime: 100, Hash: "abc", ParseOK: true,
	})
	if err != nil {
		t.Fatalf("upsert file: %v", err)
	}
	if _, err := tx.ReplaceNodes(ctx, fileID, "python", []Node{
		{Kind: "function", Name: "keep", QName: "keep", Arity: 1, StartLine: 1, EndLine: 2, Exported: true},
		{Kind: "function", Name: "drop", QName: "drop", Arity: 0, StartLine: 4, EndLine: 5, Exported: true},
	}); err != nil {
		t.Fatalf("first replace: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	before, err := st.NodeIDsByFile(ctx, fileID)
	if err != nil {
		t.Fatalf("ids: %v", err)
	}

	tx, err = st.Begin(ctx)
	if err != nil {
		t.Fatalf("begin second: %v", err)
	}
	if _, err := tx.ReplaceNodes(ctx, fileID, "python", []Node{
		{Kind: "function", Name: "keep", QName: "keep", Arity: 1, StartLine: 1, EndLine: 3, Exported: true},
		{Kind: "class", Name: "added", QName: "added", Arity: 0, StartLine: 7, EndLine: 9, Exported: true},
	}); err != nil {
		t.Fatalf("second replace: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit second: %v", err)
	}
	after, err := st.NodeIDsByFile(ctx, fileID)
	if err != nil {
		t.Fatalf("ids after: %v", err)
	}

	if after["keep"] != before["keep"] {
		t.Fatalf("keep id changed: %d -> %d", before["keep"], after["keep"])
	}
	if _, found := after["drop"]; found {
		t.Fatal("drop symbol survived a re-index")
	}
	if _, found := after["added"]; !found {
		t.Fatal("added symbol missing")
	}
}

func TestUpsertKeepsOneRowPerPath(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	for _, hash := range []string{"one", "two"} {
		tx, err := st.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := tx.UpsertFile(ctx, File{Path: "a.ts", Lang: "typescript", Hash: hash, ParseOK: true}); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}

	files, err := st.FileMap(ctx)
	if err != nil {
		t.Fatalf("file map: %v", err)
	}
	if len(files) != 1 {
		t.Fatalf("files = %d, want 1", len(files))
	}
	if files["a.ts"].Hash != "two" {
		t.Fatalf("hash = %q, want two", files["a.ts"].Hash)
	}
}

func TestDeleteFileRemovesNodes(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	tx, err := st.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	id, err := tx.UpsertFile(ctx, File{Path: "a.ts", Lang: "typescript", Hash: "h", ParseOK: true})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if _, err := tx.ReplaceNodes(ctx, id, "typescript", []Node{{Kind: "function", Name: "f", QName: "f"}}); err != nil {
		t.Fatalf("nodes: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	tx, err = st.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := tx.DeleteFile(ctx, "a.ts"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	counts, err := st.Counts(ctx)
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if counts.Files != 0 || counts.Nodes != 0 {
		t.Fatalf("counts after delete = %+v", counts)
	}
}

func TestGraphEdgeSkeletonsMatchGraphEdges(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	tx, err := st.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	fileID, err := tx.UpsertFile(ctx, File{
		Path: "src/a.ts", Lang: "typescript", Size: 10, MTime: 100, Hash: "abc", ParseOK: true,
	})
	if err != nil {
		t.Fatalf("upsert file: %v", err)
	}
	if _, err := tx.ReplaceNodes(ctx, fileID, "typescript", []Node{
		{Kind: "function", Name: "a", QName: "a", StartLine: 1, EndLine: 2, Exported: true},
		{Kind: "function", Name: "b", QName: "b", StartLine: 4, EndLine: 5, Exported: true},
	}); err != nil {
		t.Fatalf("replace nodes: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	ids, err := st.NodeIDsByFile(ctx, fileID)
	if err != nil {
		t.Fatalf("node ids: %v", err)
	}
	edges := []Edge{
		{Src: ids["a"], Dst: ids["b"], Kind: "CALLS", Confidence: 0.9, Rule: "ts.global.unique", Line: 2},
		{Src: ids["b"], Dst: ids["a"], Kind: "REFERENCES", Confidence: 0.4, Rule: "ts.global.ambiguous", Line: 5},
	}
	if _, err := st.InsertEdges(ctx, edges); err != nil {
		t.Fatalf("insert edges: %v", err)
	}

	full, err := st.GraphEdges(ctx, 0.5)
	if err != nil {
		t.Fatalf("graph edges: %v", err)
	}
	skeletons, err := st.GraphEdgeSkeletons(ctx, 0.5)
	if err != nil {
		t.Fatalf("skeletons: %v", err)
	}
	if len(skeletons) != len(full) || len(skeletons) != 1 {
		t.Fatalf("skeletons = %d, full = %d, want 1 each", len(skeletons), len(full))
	}
	got := skeletons[0]
	if got.Src != edges[0].Src || got.Dst != edges[0].Dst || got.Kind != edges[0].Kind ||
		got.Confidence != edges[0].Confidence || got.Rule != edges[0].Rule || got.Line != edges[0].Line {
		t.Fatalf("skeleton = %+v, want %+v", got, edges[0])
	}
}

func TestMergeEdgesNeverOverwrites(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	tx, err := st.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	fileID, err := tx.UpsertFile(ctx, File{
		Path: "src/a.ts", Lang: "typescript", Size: 10, MTime: 100, Hash: "abc", ParseOK: true,
	})
	if err != nil {
		t.Fatalf("upsert file: %v", err)
	}
	if _, err := tx.ReplaceNodes(ctx, fileID, "typescript", []Node{
		{Kind: "function", Name: "a", QName: "a", StartLine: 1, EndLine: 2, Exported: true},
		{Kind: "function", Name: "b", QName: "b", StartLine: 4, EndLine: 5, Exported: true},
	}); err != nil {
		t.Fatalf("replace nodes: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	ids, err := st.NodeIDsByFile(ctx, fileID)
	if err != nil {
		t.Fatalf("node ids: %v", err)
	}
	existing := Edge{Src: ids["a"], Dst: ids["b"], Kind: "CALLS", Confidence: 1.0, Rule: "ts.import.named", Line: 2}
	if _, err := st.InsertEdges(ctx, []Edge{existing}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	inserted, conflicts, err := st.MergeEdges(ctx, []Edge{
		{Src: ids["a"], Dst: ids["b"], Kind: "CALLS", Confidence: 0.99, Rule: "scip.ts.method", Line: 9},
		{Src: ids["b"], Dst: ids["a"], Kind: "REFERENCES", Confidence: 0.4, Rule: "ts.global.ambiguous", Line: 4},
	})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if inserted != 1 || conflicts != 1 {
		t.Fatalf("merge inserted=%d conflicts=%d, want 1/1", inserted, conflicts)
	}

	edges, err := st.GraphEdges(ctx, 0)
	if err != nil {
		t.Fatalf("graph edges: %v", err)
	}
	for _, edge := range edges {
		if edge.Src == ids["a"] && edge.Dst == ids["b"] && edge.Kind == "CALLS" {
			if edge.Rule != "ts.import.named" || edge.Confidence != 1.0 {
				t.Fatalf("tree-sitter edge was overwritten: %+v", edge)
			}
		}
	}
}

func TestOpenReadOnlyDoesNotWrite(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "index.db")

	w, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}

	ro, err := OpenReadOnly(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if _, err := ro.Counts(ctx); err != nil {
		t.Fatalf("read: %v", err)
	}
	if _, err := ro.db.ExecContext(ctx, `INSERT INTO meta (key, value) VALUES ('x', 'y')`); err == nil {
		t.Fatal("read-only store accepted a write")
	}

	if _, err := OpenReadOnly(ctx, filepath.Join(dir, "missing.db")); err == nil {
		t.Fatal("read-only open created a missing database")
	}
}

func TestReplacePendingFilesReplacesBatch(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	if err := st.ReplacePendingFiles(ctx, []int64{1}, []PendingRef{
		{Name: "old", FileID: 1, Kind: "call", Form: "ident", Line: 3},
	}); err != nil {
		t.Fatalf("seed file 1: %v", err)
	}
	if err := st.ReplacePendingFiles(ctx, []int64{2}, []PendingRef{
		{Name: "keep", FileID: 2, Kind: "call", Form: "ident", Line: 4},
	}); err != nil {
		t.Fatalf("seed file 2: %v", err)
	}
	if err := st.ReplacePendingFiles(ctx, []int64{1, 2}, []PendingRef{
		{Name: "new", FileID: 1, Kind: "call", Form: "ident", Line: 9},
		{Name: "next", FileID: 2, Kind: "new", Form: "ident", Line: 10},
	}); err != nil {
		t.Fatalf("replace batch: %v", err)
	}

	rows, err := st.Pending(ctx)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("pending rows = %d, want 2: %+v", len(rows), rows)
	}
	got := map[string]int64{rows[0].Name: rows[0].FileID, rows[1].Name: rows[1].FileID}
	if got["new"] != 1 || got["next"] != 2 {
		t.Fatalf("pending = %+v, want new→1 next→2", rows)
	}
}

func TestPendingRetryByNameFiltersForm(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	if err := st.ReplacePendingFiles(ctx, []int64{1}, []PendingRef{
		{Name: "x", FileID: 1, Kind: "call", Form: "ident", Line: 1},
		{Name: "x", FileID: 1, Kind: "call", Form: "member", Line: 2},
		{Name: "y", FileID: 1, Kind: "new", Form: "ident", Line: 3},
		{Name: "z", FileID: 1, Kind: "call", Form: "", Line: 4},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rows, err := st.PendingRetryByName(ctx, []string{"x", "y"})
	if err != nil {
		t.Fatalf("retry by name: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (member form excluded): %+v", len(rows), rows)
	}
	for _, row := range rows {
		if row.Name != "x" && row.Name != "y" {
			t.Fatalf("unexpected row %+v", row)
		}
		if row.Form != "ident" {
			t.Fatalf("form = %q, want ident", row.Form)
		}
	}
}

func TestIndexedPathsAndHashesByPaths(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	tx, err := st.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i, path := range []string{"a.ts", "b.ts"} {
		if _, err := tx.UpsertFile(ctx, File{
			Path: path, Lang: "typescript", Hash: "hash-" + path, Size: int64(i + 1), ParseOK: true,
		}); err != nil {
			t.Fatalf("upsert %s: %v", path, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	indexed, err := st.IndexedPaths(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !indexed["a.ts"] || !indexed["b.ts"] || indexed["missing.ts"] {
		t.Fatalf("indexed paths = %+v", indexed)
	}

	hashes, err := st.FileHashesByPaths(ctx, []string{"a.ts", "missing.ts"})
	if err != nil {
		t.Fatal(err)
	}
	if hashes["a.ts"].Hash != "hash-a.ts" {
		t.Fatalf("hash = %+v, want hash-a.ts", hashes["a.ts"])
	}
	if _, found := hashes["missing.ts"]; found {
		t.Fatal("missing path returned a hash")
	}
}

func TestNodeSpansByFilesOrdersSmallestFirst(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	tx, err := st.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fileID, err := tx.UpsertFile(ctx, File{Path: "a.ts", Lang: "typescript", Hash: "h", ParseOK: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ReplaceNodes(ctx, fileID, "typescript", []Node{
		{Kind: "class", Name: "C", QName: "C", StartLine: 1, EndLine: 20},
		{Kind: "method", Name: "m", QName: "C.m", StartLine: 5, EndLine: 9},
	}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	spans, err := st.NodeSpansByFiles(ctx, []int64{fileID})
	if err != nil {
		t.Fatal(err)
	}
	got := spans[fileID]
	if len(got) != 2 {
		t.Fatalf("spans = %+v", got)
	}
	if got[0].StartLine != 5 || got[0].EndLine != 9 {
		t.Fatalf("first span = %+v, want the smaller method span", got[0])
	}
}

func TestUnresolvedSummaryClassifiesPending(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	modules, err := st.UpsertModules(ctx, []Module{
		{Name: "pkg/one", Kind: "dir", Root: "pkg/one"},
		{Name: "pkg/two", Kind: "dir", Root: "pkg/two"},
	})
	if err != nil {
		t.Fatalf("modules: %v", err)
	}
	tx, err := st.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	fileIDs := map[string]int64{}
	for _, f := range []File{
		{Path: "pkg/one/a.ts", Lang: "typescript", ModuleID: modules["pkg/one"], Size: 10, MTime: 1, Hash: "a", ParseOK: true},
		{Path: "pkg/two/b.py", Lang: "python", ModuleID: modules["pkg/two"], Size: 10, MTime: 1, Hash: "b", ParseOK: true},
		{Path: "pkg/one/broken.ts", Lang: "typescript", ModuleID: modules["pkg/one"], Size: 10, MTime: 1, Hash: "c", ParseOK: false},
	} {
		id, err := tx.UpsertFile(ctx, f)
		if err != nil {
			t.Fatalf("file %s: %v", f.Path, err)
		}
		fileIDs[f.Path] = id
	}
	if _, err := tx.ReplaceNodes(ctx, fileIDs["pkg/one/a.ts"], "typescript", []Node{
		{Kind: "function", Name: "known", QName: "known", Arity: 0, StartLine: 1, EndLine: 1},
		{Kind: "method", Name: "memberKnown", QName: "Box.memberKnown", Arity: 0, StartLine: 5, EndLine: 5},
	}); err != nil {
		t.Fatalf("nodes: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Each bucket gets at least one row, and the parse-failure file proves the
	// precedence: its member reference would otherwise be member-scope.
	refs := []PendingRef{
		{Name: "known", FileID: fileIDs["pkg/one/a.ts"], Kind: "call", Form: "ident", Line: 2},
		{Name: "known", FileID: fileIDs["pkg/one/a.ts"], Kind: "call", Form: "ident", Line: 3},
		{Name: "memberKnown", FileID: fileIDs["pkg/one/a.ts"], Kind: "call", Form: "member", Line: 4},
		{Name: "memberKnown", FileID: fileIDs["pkg/one/a.ts"], Kind: "call", Form: "member", Line: 5},
		{Name: "externalThing", FileID: fileIDs["pkg/two/b.py"], Kind: "call", Form: "ident", Line: 1},
		{Name: "alpha", FileID: fileIDs["pkg/two/b.py"], Kind: "call", Form: "ident", Line: 2},
		{Name: "zeta", FileID: fileIDs["pkg/two/b.py"], Kind: "call", Form: "ident", Line: 3},
		{Name: "known", FileID: fileIDs["pkg/one/broken.ts"], Kind: "call", Form: "member", Line: 1},
		{Name: "unknown", FileID: fileIDs["pkg/one/broken.ts"], Kind: "call", Form: "ident", Line: 2},
	}
	if err := st.ReplacePendingFiles(ctx, []int64{
		fileIDs["pkg/one/a.ts"], fileIDs["pkg/two/b.py"], fileIDs["pkg/one/broken.ts"],
	}, refs); err != nil {
		t.Fatalf("pending: %v", err)
	}

	total, err := st.CountPending(ctx)
	if err != nil {
		t.Fatalf("count pending: %v", err)
	}
	summary, err := st.UnresolvedSummary(ctx, 2)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if summary.Total != total || total != 9 {
		t.Fatalf("total = %d, count_pending = %d, want 9", summary.Total, total)
	}
	wantReasons := map[string]int{
		PendingReasonParseFailure:      2,
		PendingReasonMemberScope:       2,
		PendingReasonMissingDefinition: 3,
		PendingReasonScopeGap:          2,
	}
	sum := 0
	for _, reason := range PendingReasons {
		if summary.ByReason[reason] != wantReasons[reason] {
			t.Fatalf("reason %q = %d, want %d (%+v)",
				reason, summary.ByReason[reason], wantReasons[reason], summary.ByReason)
		}
		sum += summary.ByReason[reason]
	}
	if sum != summary.Total {
		t.Fatalf("reason sum = %d, want total %d", sum, summary.Total)
	}
	if len(summary.ByLang) != 2 || summary.ByLang["typescript"] != 6 || summary.ByLang["python"] != 3 {
		t.Fatalf("by_lang = %+v, want typescript 6 python 3", summary.ByLang)
	}

	// topN truncates both lists; ties break by name ascending.
	if len(summary.TopNames) != 2 || summary.TopNames[0] != (PendingBucket{Name: "known", Count: 3}) ||
		summary.TopNames[1] != (PendingBucket{Name: "memberKnown", Count: 2}) {
		t.Fatalf("top names (limit 2) = %+v", summary.TopNames)
	}
	if len(summary.TopModules) != 2 || summary.TopModules[0] != (PendingBucket{Name: "pkg/one", Count: 6}) ||
		summary.TopModules[1] != (PendingBucket{Name: "pkg/two", Count: 3}) {
		t.Fatalf("top modules (limit 2) = %+v", summary.TopModules)
	}

	full, err := st.UnresolvedSummary(ctx, 10)
	if err != nil {
		t.Fatalf("full summary: %v", err)
	}
	wantNames := []PendingBucket{
		{Name: "known", Count: 3},
		{Name: "memberKnown", Count: 2},
		{Name: "alpha", Count: 1},
		{Name: "externalThing", Count: 1},
		{Name: "unknown", Count: 1},
		{Name: "zeta", Count: 1},
	}
	if len(full.TopNames) != len(wantNames) {
		t.Fatalf("top names = %+v, want %+v", full.TopNames, wantNames)
	}
	for i := range wantNames {
		if full.TopNames[i] != wantNames[i] {
			t.Fatalf("top names[%d] = %+v, want %+v (all: %+v)", i, full.TopNames[i], wantNames[i], full.TopNames)
		}
	}
}

func TestUnresolvedSummaryEmptyGraph(t *testing.T) {
	st := openTestStore(t)
	summary, err := st.UnresolvedSummary(context.Background(), 10)
	if err != nil {
		t.Fatalf("summary: %v", err)
	}
	if summary.Total != 0 {
		t.Fatalf("total = %d, want 0", summary.Total)
	}
	for _, reason := range PendingReasons {
		if count, ok := summary.ByReason[reason]; !ok || count != 0 {
			t.Fatalf("reason %q = %d (present=%v), want a zero bucket", reason, count, ok)
		}
	}
	if len(summary.ByLang) != 0 || len(summary.TopNames) != 0 || len(summary.TopModules) != 0 {
		t.Fatalf("empty graph summary is not empty: %+v", summary)
	}
}

func TestReplaceAmbiguousGroupGuards(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	tx, err := st.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	fileID, err := tx.UpsertFile(ctx, File{
		Path: "src/a.ts", Lang: "typescript", Size: 10, MTime: 100, Hash: "abc", ParseOK: true,
	})
	if err != nil {
		t.Fatalf("upsert file: %v", err)
	}
	if _, err := tx.ReplaceNodes(ctx, fileID, "typescript", []Node{
		{Kind: "function", Name: "a", QName: "a", StartLine: 1, EndLine: 2, Exported: true},
		{Kind: "function", Name: "b", QName: "b", StartLine: 4, EndLine: 5, Exported: true},
		{Kind: "function", Name: "c", QName: "c", StartLine: 7, EndLine: 8, Exported: true},
		{Kind: "function", Name: "d", QName: "d", StartLine: 10, EndLine: 11, Exported: true},
	}); err != nil {
		t.Fatalf("replace nodes: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	ids, err := st.NodeIDsByFile(ctx, fileID)
	if err != nil {
		t.Fatalf("node ids: %v", err)
	}
	a, b, c, d := ids["a"], ids["b"], ids["c"], ids["d"]
	// joinIDs writes the numeric candidate ids in order.
	candidates := strconv.FormatInt(b, 10) + "," + strconv.FormatInt(c, 10)
	if _, err := st.InsertEdges(ctx, []Edge{
		{Src: a, Dst: b, Kind: "CALLS", Confidence: 0.40, Rule: "ts.global.ambiguous", Line: 2, Candidates: candidates},
		{Src: a, Dst: c, Kind: "CALLS", Confidence: 0.40, Rule: "ts.global.ambiguous", Line: 2, Candidates: candidates},
		{Src: a, Dst: d, Kind: "REFERENCES", Confidence: 0.90, Rule: "ts.typed.method", Line: 2},
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// The exact target already owns the key with a resolved edge: conflict,
	// and the ambiguous group keeps both candidate rows.
	deleted, inserted, conflict, err := st.ReplaceAmbiguousGroup(ctx, a, 2, "CALLS", []int64{b, c}, Edge{
		Src: a, Dst: d, Kind: "REFERENCES", Confidence: 0.99, Rule: "scip.ts.reference", Line: 2,
	})
	if err != nil {
		t.Fatalf("conflict replace: %v", err)
	}
	if deleted != 0 || inserted || !conflict {
		t.Fatalf("conflict replace = deleted %d inserted %v conflict %v, want 0/false/true", deleted, inserted, conflict)
	}
	if rows, err := st.AmbiguousCandidateEdges(ctx); err != nil || len(rows) != 2 {
		t.Fatalf("ambiguous rows after conflict = %d (%v), want 2", len(rows), err)
	}

	// A stale line still finds the group's row at the exact key but cannot
	// delete it: nothing changes and the mismatch is reported as a conflict.
	deleted, inserted, conflict, err = st.ReplaceAmbiguousGroup(ctx, a, 99, "CALLS", []int64{b, c}, Edge{
		Src: a, Dst: b, Kind: "CALLS", Confidence: 0.99, Rule: "scip.ts.call", Line: 99,
	})
	if err != nil {
		t.Fatalf("stale replace: %v", err)
	}
	if deleted != 0 || inserted || !conflict {
		t.Fatalf("stale replace = deleted %d inserted %v conflict %v, want 0/false/true", deleted, inserted, conflict)
	}
	if rows, err := st.AmbiguousCandidateEdges(ctx); err != nil || len(rows) != 2 {
		t.Fatalf("ambiguous rows after stale replace = %d (%v), want 2", len(rows), err)
	}

	// The real replacement deletes exactly the group's two rows and preserves
	// the resolved edge.
	deleted, inserted, conflict, err = st.ReplaceAmbiguousGroup(ctx, a, 2, "CALLS", []int64{b, c}, Edge{
		Src: a, Dst: b, Kind: "CALLS", Confidence: 0.99, Rule: "scip.ts.call", Line: 2,
	})
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	if deleted != 2 || !inserted || conflict {
		t.Fatalf("replace = deleted %d inserted %v conflict %v, want 2/true/false", deleted, inserted, conflict)
	}
	edges, err := st.Edges(ctx, 0)
	if err != nil {
		t.Fatalf("edges: %v", err)
	}
	var ambiguousRows, exactRows, resolvedRows int
	for _, edge := range edges {
		switch {
		case edge.Rule == "ts.global.ambiguous":
			ambiguousRows++
		case edge.Rule == "scip.ts.call" && edge.DstQName == "b":
			exactRows++
			if edge.Confidence != 0.99 || edge.Kind != "CALLS" {
				t.Fatalf("replacement edge = %+v", edge)
			}
		case edge.Rule == "ts.typed.method" && edge.DstQName == "d":
			resolvedRows++
			if edge.Confidence != 0.90 {
				t.Fatalf("resolved edge changed: %+v", edge)
			}
		}
	}
	if ambiguousRows != 0 || exactRows != 1 || resolvedRows != 1 {
		t.Fatalf("rows = ambiguous %d exact %d resolved %d, want 0/1/1", ambiguousRows, exactRows, resolvedRows)
	}

	// Replaying the same replacement finds no group and changes nothing.
	deleted, inserted, conflict, err = st.ReplaceAmbiguousGroup(ctx, a, 2, "CALLS", []int64{b, c}, Edge{
		Src: a, Dst: c, Kind: "CALLS", Confidence: 0.99, Rule: "scip.ts.call", Line: 2,
	})
	if err != nil {
		t.Fatalf("replay replace: %v", err)
	}
	if deleted != 0 || inserted || conflict {
		t.Fatalf("replay = deleted %d inserted %v conflict %v, want 0/false/false", deleted, inserted, conflict)
	}
}
