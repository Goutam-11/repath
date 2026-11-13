package resolve_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Goutam-11/repath/internal/index"
	"github.com/Goutam-11/repath/internal/resolve"
	"github.com/Goutam-11/repath/internal/store"
)

func writeFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestResolvePendingUsesLanguageRuleIDs pins the rule id of a pending retry to
// the referring file's language. Before the Rust case existed, every
// non-Python file fell through to the TypeScript ladder, so Rust retry edges
// were emitted as ts.global.unique (24 of them on rayon).
func TestResolvePendingUsesLanguageRuleIDs(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "Cargo.toml", "[package]\nname = \"demo\"\nversion = \"0.1.0\"\nedition = \"2021\"\n")
	writeFile(t, root, "src/lib.rs", "pub fn rs_helper() -> i32 {\n    1\n}\n\npub struct RsStruct;\n")
	writeFile(t, root, "use.ts", "export function ts_helper() { return 1; }\n")

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(root, ".repath", "index.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := index.Run(ctx, st, root, index.Options{}); err != nil {
		t.Fatalf("index run: %v", err)
	}

	rustID, ok, err := st.FileByPath(ctx, "src/lib.rs")
	if err != nil || !ok {
		t.Fatalf("file id for src/lib.rs: ok=%v err=%v", ok, err)
	}
	ix, err := resolve.Build(ctx, st, root, map[int64]bool{rustID: true})
	if err != nil {
		t.Fatalf("resolve build: %v", err)
	}

	r, found := ix.ResolvePending(rustID, "call", "rs_helper")
	if !found {
		t.Fatalf("rust retry did not resolve: %+v", r)
	}
	if r.Rule != "rs.global.unique" {
		t.Fatalf("rust retry rule = %q, want rs.global.unique", r.Rule)
	}
	if r.Confidence != 0.60 {
		t.Fatalf("rust retry confidence = %v, want 0.60", r.Confidence)
	}
	// A retry corrects the rule id, not the edge kind: a struct target must not
	// become a REFERENCES construction edge (that is rustTarget's job).
	if r.Kind != "" {
		t.Fatalf("rust retry kind = %q, want the unset CALLS kind", r.Kind)
	}
	if _, found := ix.ResolvePending(rustID, "call", "RsStruct"); !found {
		t.Fatal("rust retry did not resolve the struct name")
	}

	tsID, ok, err := st.FileByPath(ctx, "use.ts")
	if err != nil || !ok {
		t.Fatalf("file id for use.ts: ok=%v err=%v", ok, err)
	}
	tsx, err := resolve.Build(ctx, st, root, map[int64]bool{tsID: true})
	if err != nil {
		t.Fatalf("resolve build: %v", err)
	}
	r, found = tsx.ResolvePending(tsID, "call", "ts_helper")
	if !found {
		t.Fatalf("typescript retry did not resolve: %+v", r)
	}
	if r.Rule != "ts.global.unique" {
		t.Fatalf("typescript retry rule = %q, want ts.global.unique", r.Rule)
	}
}
