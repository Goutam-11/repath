package merge_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Goutam-11/repath/internal/artifact"
	"github.com/Goutam-11/repath/internal/index"
	"github.com/Goutam-11/repath/internal/merge"
	"github.com/Goutam-11/repath/internal/store"
)

func write(t *testing.T, root, rel, body string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// export indexes one repo and writes a single-repo artifact for it.
func export(t *testing.T, root string) string {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(root, ".repath", "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := index.Run(ctx, st, root, index.Options{}); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "artifact")
	if _, err := artifact.Export(ctx, st, root, out, "test"); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestMergeTwoRepos(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	producer := filepath.Join(tmp, "producer")
	write(t, producer, "types.ts", "export interface Widget {\n  id: string;\n}\n")
	write(t, producer, "use.ts", "import { Widget } from './types';\nexport function build(w: Widget) {\n  return w;\n}\n")
	consumer := filepath.Join(tmp, "consumer")
	write(t, consumer, "models.ts", "export interface Widget {\n  label: string;\n}\n")
	write(t, consumer, "app.ts", "import { Widget } from './models';\nexport const made: Widget = { label: 'x' };\n")

	out := filepath.Join(tmp, "merged")
	result, err := merge.Run(ctx, []string{export(t, producer), export(t, consumer)}, out, "test")
	if err != nil {
		t.Fatal(err)
	}
	if result.CrossRepoEdges != 1 {
		t.Fatalf("cross-repo edges = %d, want 1", result.CrossRepoEdges)
	}
	if len(result.Parts) != 2 || result.Parts[0].Name != "consumer" || result.Parts[1].Name != "producer" {
		t.Fatalf("parts = %+v, want consumer then producer", result.Parts)
	}

	st, done, manifest, err := artifact.OpenReadOnly(ctx, out)
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	if manifest.Commit != "merged" || len(manifest.Parts) != 2 {
		t.Fatalf("manifest = %+v", manifest)
	}

	paths, err := st.FilePaths(ctx)
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := st.Nodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[int64]store.NodeRow{}
	prefixed := map[string]bool{}
	for _, n := range nodes {
		byID[n.ID] = n
	}
	for _, p := range paths {
		if !strings.HasPrefix(p, "producer/") && !strings.HasPrefix(p, "consumer/") {
			t.Fatalf("file %q is not namespaced", p)
		}
		prefixed[p] = true
	}
	if !prefixed["producer/types.ts"] || !prefixed["consumer/models.ts"] {
		t.Fatalf("namespaced paths missing: %v", prefixed)
	}

	edges, err := st.AllEdges(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var linked bool
	for _, e := range edges {
		if e.Rule != merge.CrossRepoRule {
			continue
		}
		linked = true
		if e.Confidence != merge.CrossRepoConfidence {
			t.Fatalf("cross-repo confidence = %v", e.Confidence)
		}
		src, dst := paths[byID[e.Src].FileID], paths[byID[e.Dst].FileID]
		if src != "consumer/models.ts" || dst != "producer/types.ts" {
			t.Fatalf("link = %s -> %s, want consumer/models.ts -> producer/types.ts", src, dst)
		}
	}
	if !linked {
		t.Fatal("no xrepo.same_type_as edge")
	}
}

func TestMergeIsByteIdentical(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	a := filepath.Join(tmp, "alpha")
	write(t, a, "one.ts", "export interface Shared { id: string }\n")
	b := filepath.Join(tmp, "beta")
	write(t, b, "two.ts", "export interface Shared { id: string }\nexport function use(): Shared {\n  return { id: 'x' };\n}\n")
	goFixture, err := filepath.Abs(filepath.Join("..", "..", "testdata", "fixtures", "go"))
	if err != nil {
		t.Fatal(err)
	}
	// Three parts, one of them carrying entrypoints, so insertion-order bugs
	// surface as a byte difference.
	sources := []string{export(t, a), export(t, b), export(t, goFixture)}

	out1 := filepath.Join(tmp, "m1")
	out2 := filepath.Join(tmp, "m2")
	if _, err := merge.Run(ctx, sources, out1, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := merge.Run(ctx, sources, out2, "test"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{artifact.ManifestName} {
		first, err := os.ReadFile(filepath.Join(out1, name))
		if err != nil {
			t.Fatal(err)
		}
		second, err := os.ReadFile(filepath.Join(out2, name))
		if err != nil {
			t.Fatal(err)
		}
		if string(first) != string(second) {
			t.Fatalf("%s differs between merges", name)
		}
	}
	m1, err := filepath.Glob(filepath.Join(out1, "*.sqlite"))
	if err != nil || len(m1) != 1 {
		t.Fatalf("merged db = %v, %v", m1, err)
	}
	m2, _ := filepath.Glob(filepath.Join(out2, "*.sqlite"))
	if len(m2) != 1 {
		t.Fatalf("merged db = %v", m2)
	}
	h1, h2 := sha256.Sum256(mustRead(t, m1[0])), sha256.Sum256(mustRead(t, m2[0]))
	if h1 != h2 {
		t.Fatalf("merged database bytes differ: %x vs %x", h1, h2)
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestMergeAmbiguousTypeIsNotLinked(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	producer := filepath.Join(tmp, "producer")
	write(t, producer, "a.ts", "export interface Widget { id: string }\n")
	consumer := filepath.Join(tmp, "consumer")
	write(t, consumer, "one.ts", "export interface Widget { a: string }\n")
	write(t, consumer, "two.ts", "export interface Widget { b: string }\n")

	result, err := merge.Run(ctx, []string{export(t, producer), export(t, consumer)}, filepath.Join(tmp, "out"), "test")
	if err != nil {
		t.Fatal(err)
	}
	if result.CrossRepoEdges != 0 {
		t.Fatalf("cross-repo edges = %d, want 0 for an ambiguous type", result.CrossRepoEdges)
	}
}

func TestMergeRefusesExistingOutput(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	root := filepath.Join(tmp, "alpha")
	write(t, root, "a.ts", "export const a = 1;\n")
	source := export(t, root)
	out := filepath.Join(tmp, "out")
	if _, err := merge.Run(ctx, []string{source}, out, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := merge.Run(ctx, []string{source}, out, "test"); !errors.Is(err, merge.ErrExists) {
		t.Fatalf("second merge error = %v, want ErrExists", err)
	}
}

func TestMergeMissingPartIsNotFound(t *testing.T) {
	_, err := merge.Run(context.Background(), []string{filepath.Join(t.TempDir(), "missing")}, filepath.Join(t.TempDir(), "out"), "test")
	if !errors.Is(err, artifact.ErrNotFound) {
		t.Fatalf("error = %v, want artifact.ErrNotFound", err)
	}
}

func TestMergeRefusesSchemaMismatch(t *testing.T) {
	ctx := context.Background()
	tmp := t.TempDir()
	root := filepath.Join(tmp, "alpha")
	write(t, root, "a.ts", "export const a = 1;\n")
	source := export(t, root)
	// Rewrite the manifest so the artifact claims a future schema.
	manifestPath := filepath.Join(source, artifact.ManifestName)
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	bad := []byte(string(data))
	for i := 0; i+1 < len(bad); i++ {
		// Replace "schema_version": 6 with 999 without depending on field order.
		bad = []byte(stringReplaceOnce(string(bad), "\"schema_version\": 6", "\"schema_version\": 999"))
	}
	if err := os.WriteFile(manifestPath, bad, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := merge.Run(ctx, []string{source}, filepath.Join(tmp, "out"), "test"); !errors.Is(err, artifact.ErrSchema) {
		t.Fatalf("error = %v, want artifact.ErrSchema", err)
	}
}

func stringReplaceOnce(s, old, new string) string {
	for i := 0; i+len(old) <= len(s); i++ {
		if s[i:i+len(old)] == old {
			return s[:i] + new + s[i+len(old):]
		}
	}
	return s
}
