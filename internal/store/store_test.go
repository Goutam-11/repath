package store

import (
	"context"
	"path/filepath"
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
