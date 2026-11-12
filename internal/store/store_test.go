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
	if meta["schema_version"] != "4" {
		t.Fatalf("schema_version = %q, want 4", meta["schema_version"])
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
	if inserted != 1 {
		t.Fatalf("inserted = %d, want 1 (duplicates collapse)", inserted)
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
