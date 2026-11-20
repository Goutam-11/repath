package artifact

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Goutam-11/repath/internal/index"
	"github.com/Goutam-11/repath/internal/query"
	"github.com/Goutam-11/repath/internal/store"
)

func writeFile(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func buildIndex(t *testing.T, root string) *store.Store {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(root, ".repath", "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx); err != nil {
		st.Close()
		t.Fatal(err)
	}
	if _, err := index.Run(ctx, st, root, index.Options{}); err != nil {
		st.Close()
		t.Fatal(err)
	}
	return st
}

func exportFixture(t *testing.T) (root, outDir string) {
	return exportFixtureWith(t, Options{})
}

func exportFixtureWith(t *testing.T, opts Options) (root, outDir string) {
	t.Helper()
	root = t.TempDir()
	writeFile(t, root, "src/a.ts", "export function add(a: number, b: number): number { return a + b; }\n")
	writeFile(t, root, "src/b.ts", "import { add } from \"./a\";\nexport function sum(): number { return add(1, 2); }\n")
	st := buildIndex(t, root)
	defer st.Close()

	outDir = t.TempDir()
	if _, err := ExportWithOptions(context.Background(), st, root, outDir, "test-version", opts); err != nil {
		t.Fatalf("export: %v", err)
	}
	return root, outDir
}

func TestExportPullRoundTrip(t *testing.T) {
	ctx := context.Background()
	_, outDir := exportFixture(t)

	manifest, err := ReadManifest(filepath.Join(outDir, ManifestName))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if manifest.SchemaVersion != store.SchemaVersion {
		t.Fatalf("schema = %d, want %d", manifest.SchemaVersion, store.SchemaVersion)
	}
	if manifest.ToolVersion != "test-version" || manifest.Format != "sqlite" {
		t.Fatalf("manifest = %+v", manifest)
	}
	if manifest.FileCount != 2 || manifest.NodeCount != 2 || manifest.EdgeCount != 1 {
		t.Fatalf("counts = files %d nodes %d edges %d, want 2/2/1",
			manifest.FileCount, manifest.NodeCount, manifest.EdgeCount)
	}
	if manifest.DBFile != "repath-dev.sqlite" {
		t.Fatalf("db_file = %q", manifest.DBFile)
	}

	dbPath := filepath.Join(outDir, manifest.DBFile)
	info, err := os.Stat(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o200 != 0 {
		t.Fatalf("artifact mode = %v, want read-only", info.Mode().Perm())
	}
	if err := os.Chmod(dbPath, 0o644); err != nil {
		t.Fatal(err)
	}

	// Paths are stored repo-relative, which is what makes the artifact movable.
	inspect, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := inspect.FilePaths(ctx)
	if err != nil {
		inspect.Close()
		t.Fatal(err)
	}
	inspect.Close()
	for _, path := range paths {
		if filepath.IsAbs(path) || strings.HasPrefix(path, "/") {
			t.Fatalf("stored path is not repo-relative: %q", path)
		}
	}
	if len(paths) != 2 {
		t.Fatalf("files in artifact = %d, want 2", len(paths))
	}

	// A laptop checkout: same source tree, different directory.
	laptop := t.TempDir()
	writeFile(t, laptop, "src/a.ts", "export function add(a: number, b: number): number { return a + b; }\n")
	writeFile(t, laptop, "src/b.ts", "import { add } from \"./a\";\nexport function sum(): number { return add(1, 2); }\n")

	if _, err := Pull(ctx, outDir, laptop, "test-version"); err != nil {
		t.Fatalf("pull: %v", err)
	}

	session, err := query.Open(ctx, laptop, true)
	if err != nil {
		t.Fatalf("open pulled index: %v", err)
	}
	defer session.Close()

	counts, err := session.Store.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Files != 2 || counts.Nodes != 2 {
		t.Fatalf("pulled counts = %+v, want 2 files/2 nodes", counts)
	}
	meta, err := session.Store.Meta(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if meta["root"] != laptop {
		t.Fatalf("meta.root = %q, want %q", meta["root"], laptop)
	}

	answer, code, err := query.Impact(ctx, session.Store, "add", 2, 0.5, 50)
	if err != nil || code != 0 {
		t.Fatalf("impact after pull: code %d err %v", code, err)
	}
	if len(answer.Result.Dependents) != 1 || answer.Result.Dependents[0].QName != "sum" {
		t.Fatalf("impact after pull = %+v", answer.Result.Dependents)
	}
	if answer.Result.Dependents[0].File != "src/b.ts" {
		t.Fatalf("dependent file = %q, want src/b.ts", answer.Result.Dependents[0].File)
	}
}

func TestCompressedExportPullRoundTrip(t *testing.T) {
	ctx := context.Background()
	root, outDir := exportFixtureWith(t, Options{Compress: true})

	manifest, err := ReadManifest(filepath.Join(outDir, ManifestName))
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Format != "sqlite+zstd" || !strings.HasSuffix(manifest.DBFile, ".sqlite.zst") {
		t.Fatalf("manifest = %+v", manifest)
	}
	compressed := filepath.Join(outDir, manifest.DBFile)
	info, err := os.Stat(compressed)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o200 != 0 {
		t.Fatalf("artifact mode = %v, want read-only", info.Mode().Perm())
	}
	if info.Size() == 0 {
		t.Fatal("compressed artifact is empty")
	}

	laptop := t.TempDir()
	writeFile(t, laptop, "src/a.ts", "export function add(a: number, b: number): number { return a + b; }\n")
	writeFile(t, laptop, "src/b.ts", "import { add } from \"./a\";\nexport function sum(): number { return add(1, 2); }\n")
	if _, err := Pull(ctx, outDir, laptop, "test-version"); err != nil {
		t.Fatalf("pull compressed: %v", err)
	}
	session, err := query.Open(ctx, laptop, true)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	answer, code, err := query.Impact(ctx, session.Store, "add", 2, 0.5, 10)
	if err != nil || code != 0 {
		t.Fatalf("impact after compressed pull: code %d err %v", code, err)
	}
	if len(answer.Result.Dependents) != 1 || answer.Result.Dependents[0].QName != "sum" {
		t.Fatalf("dependents = %+v", answer.Result.Dependents)
	}
	if root == "" {
		t.Fatal("root unexpectedly empty")
	}
}

func TestCompressedPullRefusesUnknownSchemaVersion(t *testing.T) {
	_, outDir := exportFixtureWith(t, Options{Compress: true})
	manifestPath := filepath.Join(outDir, ManifestName)
	manifest, err := ReadManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	manifest.SchemaVersion = 99
	if err := writeManifest(manifestPath, manifest); err != nil {
		t.Fatal(err)
	}

	laptop := t.TempDir()
	_, err = Pull(context.Background(), outDir, laptop, "test-version")
	if err == nil || !strings.Contains(err.Error(), "expects v6") {
		t.Fatalf("error = %v, want schema refusal", err)
	}
	if _, statErr := os.Stat(filepath.Join(laptop, ".repath", "index.db")); !os.IsNotExist(statErr) {
		t.Fatalf("failed compressed pull left an index: %v", statErr)
	}
}

func TestPullRefusesUnknownSchemaVersion(t *testing.T) {
	_, outDir := exportFixture(t)

	manifestPath := filepath.Join(outDir, ManifestName)
	manifest, err := ReadManifest(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	manifest.SchemaVersion = 99
	data := []byte(`{"schema_version":99,"tool_version":"future","commit":"","file_count":2,"node_count":2,"edge_count":1,"unresolved_rate":0,"indexed_at":"2026-01-01T00:00:00Z","format":"sqlite","db_file":"` + manifest.DBFile + `"}`)
	if err := os.WriteFile(manifestPath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	laptop := t.TempDir()
	_, err = Pull(context.Background(), outDir, laptop, "test-version")
	if err == nil {
		t.Fatal("pull accepted schema v99")
	}
	message := err.Error()
	for _, want := range []string{"schema v99", "expects v6", "upgrade"} {
		if !strings.Contains(message, want) {
			t.Fatalf("error %q does not contain %q", message, want)
		}
	}
	if _, statErr := os.Stat(filepath.Join(laptop, ".repath", "index.db")); !os.IsNotExist(statErr) {
		t.Fatalf("refused pull still wrote an index: %v", statErr)
	}
}

func TestPullFromURL(t *testing.T) {
	_, outDir := exportFixture(t)

	server := httptest.NewServer(http.FileServer(http.Dir(outDir)))
	defer server.Close()

	manifest, err := ReadManifest(filepath.Join(outDir, ManifestName))
	if err != nil {
		t.Fatal(err)
	}

	laptop := t.TempDir()
	writeFile(t, laptop, "src/a.ts", "export function add(a: number, b: number): number { return a + b; }\n")
	writeFile(t, laptop, "src/b.ts", "import { add } from \"./a\";\nexport function sum(): number { return add(1, 2); }\n")

	pulled, err := Pull(context.Background(), server.URL+"/"+manifest.DBFile, laptop, "test-version")
	if err != nil {
		t.Fatalf("pull from URL: %v", err)
	}
	if pulled.FileCount != 2 || pulled.EdgeCount != 1 {
		t.Fatalf("pulled manifest = %+v", pulled)
	}

	session, err := query.Open(context.Background(), laptop, true)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	counts, err := session.Store.Counts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if counts.Files != 2 || counts.Nodes != 2 {
		t.Fatalf("counts = %+v", counts)
	}
}

func TestPullMissingArtifactIsNotFound(t *testing.T) {
	_, err := Pull(context.Background(), filepath.Join(t.TempDir(), "nope"), t.TempDir(), "test-version")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
}

func TestPullKeepsSnapshotWhenCheckoutIsEmpty(t *testing.T) {
	_, outDir := exportFixture(t)
	laptop := t.TempDir()

	if _, err := Pull(context.Background(), outDir, laptop, "test-version"); err != nil {
		t.Fatalf("pull into empty directory: %v", err)
	}

	session, err := query.Open(context.Background(), laptop, true)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	counts, err := session.Store.Counts(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if counts.Files != 2 || counts.Nodes != 2 {
		t.Fatalf("empty checkout wiped the snapshot: %+v", counts)
	}
}

func TestPullCompressedFromURL(t *testing.T) {
	_, outDir := exportFixtureWith(t, Options{Compress: true})
	server := httptest.NewServer(http.FileServer(http.Dir(outDir)))
	defer server.Close()

	manifest, err := ReadManifest(filepath.Join(outDir, ManifestName))
	if err != nil {
		t.Fatal(err)
	}
	laptop := t.TempDir()
	writeFile(t, laptop, "src/a.ts", "export function add(a: number, b: number): number { return a + b; }\n")
	writeFile(t, laptop, "src/b.ts", "import { add } from \"./a\";\nexport function sum(): number { return add(1, 2); }\n")

	pulled, err := Pull(context.Background(), server.URL+"/"+manifest.DBFile, laptop, "test-version")
	if err != nil {
		t.Fatalf("pull compressed from URL: %v", err)
	}
	if pulled.Format != "sqlite+zstd" || pulled.FileCount != 2 || pulled.EdgeCount != 1 {
		t.Fatalf("pulled manifest = %+v", pulled)
	}
}

// TestScopedArtifactRecordsScope pins the #86 manifest contract: an artifact
// built with --scope records that scope, pull preserves it in the seeded index,
// and a manifest that disagrees with its database is refused instead of mixed.
func TestScopedArtifactRecordsScope(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeFile(t, root, "a/x.ts", "export function ax(): number { return 1; }\n")
	writeFile(t, root, "b/y.ts", "export function by(): number { return 2; }\n")

	st, err := store.Open(ctx, filepath.Join(root, ".repath", "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := index.Run(ctx, st, root, index.Options{Scope: []string{"a"}}); err != nil {
		t.Fatalf("scoped index: %v", err)
	}
	outDir := t.TempDir()
	exported, err := ExportWithOptions(ctx, st, root, outDir, "test-version", Options{})
	st.Close()
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if len(exported.Manifest.Scope) != 1 || exported.Manifest.Scope[0] != "a" {
		t.Fatalf("manifest scope = %v, want [a]", exported.Manifest.Scope)
	}

	// Pull into an empty directory: the snapshot (and its scope) is preserved.
	other := t.TempDir()
	manifest, err := Pull(ctx, outDir, other, "test-version")
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if len(manifest.Scope) != 1 || manifest.Scope[0] != "a" {
		t.Fatalf("pulled manifest scope = %v, want [a]", manifest.Scope)
	}
	seeded, err := store.Open(ctx, filepath.Join(other, ".repath", "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	meta, err := seeded.Meta(ctx)
	if err != nil {
		t.Fatal(err)
	}
	seeded.Close()
	if got := index.ScopeFromMeta(meta); len(got) != 1 || got[0] != "a" {
		t.Fatalf("seeded index scope = %v, want [a]", got)
	}

	// A manifest that claims a different scope than its database is refused.
	tampered, err := ReadManifest(filepath.Join(outDir, ManifestName))
	if err != nil {
		t.Fatal(err)
	}
	tampered.Scope = []string{"b"}
	if err := writeManifest(filepath.Join(outDir, ManifestName), tampered); err != nil {
		t.Fatal(err)
	}
	if _, err := Pull(ctx, outDir, t.TempDir(), "test-version"); err == nil ||
		!strings.Contains(err.Error(), "does not match") {
		t.Fatalf("mismatched manifest scope was accepted (err = %v)", err)
	}
}
