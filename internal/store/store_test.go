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
	if meta["schema_version"] != "2" {
		t.Fatalf("schema_version = %q, want 2", meta["schema_version"])
	}
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
