package index_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/Goutam-11/repath/internal/index"
	"github.com/Goutam-11/repath/internal/store"
)

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func newGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitRun(t, dir, "init", "-q")
	gitRun(t, dir, "config", "user.email", "repath-test@example.com")
	gitRun(t, dir, "config", "user.name", "repath test")
	gitRun(t, dir, "config", "commit.gpgsign", "false")
	return dir
}

func writeSource(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func commitAll(t *testing.T, dir, message string) {
	t.Helper()
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-q", "-m", message)
}

func openIndex(t *testing.T, root string) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(root, ".repath", "index.db"))
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	if err := st.Migrate(context.Background()); err != nil {
		st.Close()
		t.Fatalf("migrate: %v", err)
	}
	return st
}

func runIndex(t *testing.T, root string) index.Result {
	t.Helper()
	return runIndexOptions(t, root, index.Options{})
}

func runIndexOptions(t *testing.T, root string, opts index.Options) index.Result {
	t.Helper()
	st := openIndex(t, root)
	defer st.Close()
	result, err := index.Run(context.Background(), st, root, opts)
	if err != nil {
		t.Fatalf("index run: %v", err)
	}
	return result
}

// TestIncrementalFollowsPythonReexportThroughUnchangedModule is the issue #49
// regression: a comment-only edit re-indexes use.py, but the re-export chain
// it resolves through (compat.py -> impl.py) belongs to unchanged modules. A
// warm run must load their import bindings or the call becomes pending while
// its cold edge survives, reporting the same reference as both resolved and
// unresolved.
func TestIncrementalFollowsPythonReexportThroughUnchangedModule(t *testing.T) {
	dir := newGitRepo(t)
	writeSource(t, dir, "impl.py", "def helper():\n    return 1\n")
	writeSource(t, dir, "compat.py", "from impl import helper\n")
	writeSource(t, dir, "use.py", "from compat import helper\n\ndef run():\n    return helper()\n")
	commitAll(t, dir, "seed")

	cold := runIndex(t, dir)
	if cold.Unresolved != 0 {
		t.Fatalf("cold unresolved = %d, want 0", cold.Unresolved)
	}
	if cold.Edges == 0 {
		t.Fatal("cold index produced no edge through the re-export")
	}

	// Touch only the file that uses the chain; compat.py stays unchanged.
	writeSource(t, dir, "use.py", "from compat import helper\n\ndef run():\n    return helper()\n# comment\n")
	warm := runIndex(t, dir)
	if warm.Indexed != 1 || warm.Unchanged != 2 {
		t.Fatalf("warm run = indexed %d unchanged %d, want 1/2", warm.Indexed, warm.Unchanged)
	}
	if warm.Unresolved != cold.Unresolved {
		t.Fatalf("warm unresolved = %d, want cold %d", warm.Unresolved, cold.Unresolved)
	}
	if warm.Edges != cold.Edges {
		t.Fatalf("warm edges = %d, want cold %d", warm.Edges, cold.Edges)
	}

	st := openIndex(t, dir)
	defer st.Close()
	edges, err := st.Edges(context.Background(), 0.5)
	if err != nil {
		t.Fatalf("edges: %v", err)
	}
	reexport := false
	for _, edge := range edges {
		if edge.Rule == "py.import.reexport" && edge.SrcQName == "run" && edge.DstQName == "helper" {
			reexport = true
		}
	}
	if !reexport {
		t.Fatalf("warm graph lost the py.import.reexport edge: %+v", edges)
	}
	pending, err := st.CountPending(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Fatalf("pending = %d, want 0 after the warm run", pending)
	}
}

// TestIncrementalFollowsRustReexportThroughUnchangedModule is the Rust half of
// the #49 regression. lib.rs and compat.rs re-export helper; only use_mod.rs is
// re-indexed, so the chain crosses two untouched modules (the rayon shape).
func TestIncrementalFollowsRustReexportThroughUnchangedModule(t *testing.T) {
	dir := newGitRepo(t)
	writeSource(t, dir, "Cargo.toml", "[package]\nname = \"demo\"\nversion = \"0.1.0\"\nedition = \"2021\"\n")
	writeSource(t, dir, "src/imp.rs", "pub fn helper() -> i32 {\n    1\n}\n")
	writeSource(t, dir, "src/compat.rs", "pub use crate::imp::helper;\n")
	writeSource(t, dir, "src/lib.rs", "mod imp;\nmod compat;\nmod use_mod;\n\npub use self::compat::helper;\n")
	writeSource(t, dir, "src/use_mod.rs", "use crate::helper;\n\npub fn run() -> i32 {\n    helper()\n}\n")
	commitAll(t, dir, "seed")

	cold := runIndex(t, dir)
	if cold.Unresolved != 0 {
		t.Fatalf("cold unresolved = %d, want 0", cold.Unresolved)
	}

	writeSource(t, dir, "src/use_mod.rs", "use crate::helper;\n\npub fn run() -> i32 {\n    helper()\n}\n// comment\n")
	warm := runIndex(t, dir)
	if warm.Indexed != 1 || warm.Unchanged != 3 {
		t.Fatalf("warm run = indexed %d unchanged %d, want 1/3", warm.Indexed, warm.Unchanged)
	}
	if warm.Unresolved != cold.Unresolved {
		t.Fatalf("warm unresolved = %d, want cold %d", warm.Unresolved, cold.Unresolved)
	}
	if warm.Edges != cold.Edges {
		t.Fatalf("warm edges = %d, want cold %d", warm.Edges, cold.Edges)
	}

	st := openIndex(t, dir)
	defer st.Close()
	edges, err := st.Edges(context.Background(), 0.5)
	if err != nil {
		t.Fatalf("edges: %v", err)
	}
	reexport := false
	for _, edge := range edges {
		if edge.Rule == "rs.use.reexport" && edge.SrcQName == "run" && edge.DstQName == "helper" {
			reexport = true
		}
	}
	if !reexport {
		t.Fatalf("warm graph lost the rs.use.reexport edge: %+v", edges)
	}
	pending, err := st.CountPending(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Fatalf("pending = %d, want 0 after the warm run", pending)
	}
}

// TestFullRunReproducesColdGraph is the second #49 regression: a --full run
// re-resolves every file, so it must complete the pending retry exactly like a
// cold run. Before the fix a second full index on rayon left 2,077 pending
// against cold's 2,048 because the retry was skipped when the symbol set was
// unchanged.
func TestFullRunReproducesColdGraph(t *testing.T) {
	dir := newGitRepo(t)
	writeSource(t, dir, "defs.ts", "export function helper() { return 1; }\n")
	// The import target exists but does not export helper, so tree-sitter
	// leaves the call pending; the unique global name can still be linked by
	// the retry pass.
	writeSource(t, dir, "empty.ts", "export const marker = 1;\n")
	writeSource(t, dir, "use.ts", "import { helper } from './empty';\nexport function run() { return helper(); }\n")
	commitAll(t, dir, "seed")

	cold := runIndex(t, dir)
	if cold.Unresolved != 0 {
		t.Fatalf("cold unresolved = %d, want 0", cold.Unresolved)
	}

	full := runIndexOptions(t, dir, index.Options{Full: true})
	if full.Indexed != 3 {
		t.Fatalf("full run indexed %d, want 3", full.Indexed)
	}
	if full.Unresolved != cold.Unresolved {
		t.Fatalf("full run unresolved = %d, want cold %d", full.Unresolved, cold.Unresolved)
	}
	if full.Edges != cold.Edges {
		t.Fatalf("full run edges = %d, want cold %d", full.Edges, cold.Edges)
	}
}

func TestIncrementalUsesGitChangeSet(t *testing.T) {
	dir := newGitRepo(t)
	writeSource(t, dir, "a.ts", "export function a() { return 1; }\n")
	writeSource(t, dir, "b.ts", "import { a } from './a';\nexport function b() { return a(); }\n")
	commitAll(t, dir, "seed")

	cold := runIndex(t, dir)
	if cold.Files != 2 || cold.Indexed != 2 {
		t.Fatalf("cold = files %d indexed %d, want 2/2", cold.Files, cold.Indexed)
	}
	if cold.Edges == 0 {
		t.Fatal("cold index produced no edges")
	}

	again := runIndex(t, dir)
	if again.Indexed != 0 || again.Unchanged != 2 {
		t.Fatalf("clean run = indexed %d unchanged %d, want 0/2", again.Indexed, again.Unchanged)
	}
	if again.Edges != cold.Edges {
		t.Fatalf("clean run edges = %d, want %d", again.Edges, cold.Edges)
	}

	// A comment-only edit changes the hash but not the facts.
	writeSource(t, dir, "a.ts", "export function a() { return 1; }\n// comment\n")
	comment := runIndex(t, dir)
	if comment.Indexed != 1 || comment.Unchanged != 1 {
		t.Fatalf("comment run = indexed %d unchanged %d, want 1/1", comment.Indexed, comment.Unchanged)
	}
	if comment.Edges != cold.Edges {
		t.Fatalf("comment run edges = %d, want %d", comment.Edges, cold.Edges)
	}

	writeSource(t, dir, "c.ts", "export function c() { return 2; }\n")
	added := runIndex(t, dir)
	if added.Indexed != 1 || added.Files != 3 {
		t.Fatalf("new file run = indexed %d files %d, want 1/3", added.Indexed, added.Files)
	}

	if err := os.Remove(filepath.Join(dir, "c.ts")); err != nil {
		t.Fatal(err)
	}
	removed := runIndex(t, dir)
	if removed.Deleted != 1 || removed.Files != 2 {
		t.Fatalf("delete run = deleted %d files %d, want 1/2", removed.Deleted, removed.Files)
	}
}

func TestColdRunCompletesPendingRetry(t *testing.T) {
	dir := newGitRepo(t)
	writeSource(t, dir, "defs.ts", "export function helper() { return 1; }\n")
	// The import target exists but does not export helper, so tree-sitter
	// leaves the call pending; the unique global name can still be linked.
	writeSource(t, dir, "empty.ts", "export const marker = 1;\n")
	writeSource(t, dir, "use.ts", "import { helper } from './empty';\nexport function run() { return helper(); }\n")
	commitAll(t, dir, "seed")

	cold := runIndex(t, dir)
	if cold.Indexed != 3 {
		t.Fatalf("cold indexed %d, want 3", cold.Indexed)
	}

	st := openIndex(t, dir)
	defer st.Close()
	// #82: the unique-name retry is a name-only TypeScript guess at 0.40, so it
	// is visible with --min-confidence 0.4 and absent from default answers.
	edges, err := st.Edges(context.Background(), 0.35)
	if err != nil {
		t.Fatalf("edges: %v", err)
	}
	found := false
	for _, edge := range edges {
		if edge.Rule == "ts.global.unique" && edge.SrcQName == "run" && edge.DstQName == "helper" {
			found = true
			if edge.Confidence != 0.40 {
				t.Fatalf("retry confidence = %v, want 0.40", edge.Confidence)
			}
		}
	}
	if !found {
		t.Fatalf("cold index did not complete the pending retry: %+v", edges)
	}
	defaultEdges, err := st.Edges(context.Background(), 0.5)
	if err != nil {
		t.Fatalf("default edges: %v", err)
	}
	for _, edge := range defaultEdges {
		if edge.Rule == "ts.global.unique" {
			t.Fatalf("name-only retry edge cleared the default floor: %+v", edge)
		}
	}
	pending, err := st.CountPending(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Fatalf("pending = %d, want 0 after the cold retry", pending)
	}
}

func TestIncrementalFallsBackWhenHeadChanges(t *testing.T) {
	dir := newGitRepo(t)
	writeSource(t, dir, "a.ts", "export function a() { return 1; }\n")
	commitAll(t, dir, "seed")
	if runIndex(t, dir).Indexed != 1 {
		t.Fatal("seed index did not index a.ts")
	}

	writeSource(t, dir, "d.ts", "export function d() { return 3; }\n")
	commitAll(t, dir, "add d")
	next := runIndex(t, dir)
	if next.Indexed != 1 || next.Files != 2 {
		t.Fatalf("after HEAD change = indexed %d files %d, want 1/2", next.Indexed, next.Files)
	}
}

func TestIncrementalOutsideGitRepo(t *testing.T) {
	dir := t.TempDir()
	writeSource(t, dir, "a.ts", "export function a() { return 1; }\n")
	writeSource(t, dir, "b.ts", "import { a } from './a';\nexport function b() { return a(); }\n")

	if result := runIndex(t, dir); result.Indexed != 2 {
		t.Fatalf("cold indexed %d, want 2", result.Indexed)
	}
	writeSource(t, dir, "a.ts", "export function a() { return 2; }\n")
	changed := runIndex(t, dir)
	if changed.Indexed != 1 || changed.Unchanged != 1 {
		t.Fatalf("non-git incremental = indexed %d unchanged %d, want 1/1", changed.Indexed, changed.Unchanged)
	}
}

// graphRows renders every edge and pending reference as a stable text row, so
// warm/cold parity can be compared as row sets instead of only as counts.
// Counts alone hide a swap (one edge lost, another gained) and an edge+pending
// pair for the same reference.
func graphRows(t *testing.T, dir string) []string {
	t.Helper()
	ctx := context.Background()
	st := openIndex(t, dir)
	defer st.Close()

	edges, err := st.Edges(ctx, 0)
	if err != nil {
		t.Fatalf("edges: %v", err)
	}
	paths, err := st.FilePaths(ctx)
	if err != nil {
		t.Fatalf("file paths: %v", err)
	}
	pending, err := st.Pending(ctx)
	if err != nil {
		t.Fatalf("pending: %v", err)
	}

	rows := make([]string, 0, len(edges)+len(pending))
	for _, e := range edges {
		rows = append(rows, fmt.Sprintf("edge %s:%s -> %s:%s %s %.2f %s line %d",
			e.SrcPath, e.SrcQName, e.DstPath, e.DstQName, e.Kind, e.Confidence, e.Rule, e.Line))
	}
	for _, p := range pending {
		rows = append(rows, fmt.Sprintf("pending %s %s %s %s line %d",
			paths[p.FileID], p.Name, p.Kind, p.Form, p.Line))
	}
	sort.Strings(rows)
	return rows
}

// TestIncrementalFollowsTSBarrelThroughUnchangedModule is the TypeScript half of
// #49 and the #56 parity regression. A ten-file Kibana edit re-resolves only the
// changed files, so a barrel re-export chain continues through modules the run
// did not touch; their bindings have to be loaded lazily or the same reference
// is an edge in the cold graph and pending in the warm one.
func TestIncrementalFollowsTSBarrelThroughUnchangedModule(t *testing.T) {
	dir := newGitRepo(t)
	writeSource(t, dir, "impl.ts", "export function helper() { return 1; }\n")
	writeSource(t, dir, "barrel.ts", "export * from './impl';\n")
	writeSource(t, dir, "use.ts", "import { helper } from './barrel';\nexport function run() { return helper(); }\n")
	commitAll(t, dir, "seed")

	cold := runIndex(t, dir)
	if cold.Unresolved != 0 {
		t.Fatalf("cold unresolved = %d, want 0", cold.Unresolved)
	}
	coldRows := graphRows(t, dir)
	assertBarrelEdge(t, dir, "ts.import.star")

	// Touch only the consumer: barrel.ts and impl.ts stay unchanged.
	writeSource(t, dir, "use.ts", "import { helper } from './barrel';\nexport function run() { return helper(); }\n// comment\n")
	warm := runIndex(t, dir)
	if warm.Indexed != 1 || warm.Unchanged != 2 {
		t.Fatalf("warm run = indexed %d unchanged %d, want 1/2", warm.Indexed, warm.Unchanged)
	}
	if warm.Edges != cold.Edges || warm.Unresolved != cold.Unresolved {
		t.Fatalf("warm run = edges %d unresolved %d, want cold %d/%d",
			warm.Edges, warm.Unresolved, cold.Edges, cold.Unresolved)
	}
	if rows := graphRows(t, dir); !reflect.DeepEqual(rows, coldRows) {
		t.Fatalf("warm run changed the row set:\n cold: %v\n warm: %v", coldRows, rows)
	}
	assertBarrelEdge(t, dir, "ts.import.star")

	// A second --full run must reproduce the cold graph row for row.
	full := runIndexOptions(t, dir, index.Options{Full: true})
	if full.Edges != cold.Edges || full.Unresolved != cold.Unresolved {
		t.Fatalf("full run = edges %d unresolved %d, want cold %d/%d",
			full.Edges, full.Unresolved, cold.Edges, cold.Unresolved)
	}
	if rows := graphRows(t, dir); !reflect.DeepEqual(rows, coldRows) {
		t.Fatalf("second --full run changed the row set:\n cold: %v\n full: %v", coldRows, rows)
	}
}

// assertBarrelEdge requires the run() -> helper() edge to carry the barrel rule.
// The pending retry would otherwise link the same call as ts.global.unique once
// the target exists, which is exactly the failure mode this test guards.
func assertBarrelEdge(t *testing.T, dir, rule string) {
	t.Helper()
	st := openIndex(t, dir)
	defer st.Close()
	edges, err := st.Edges(context.Background(), 0.5)
	if err != nil {
		t.Fatalf("edges: %v", err)
	}
	for _, edge := range edges {
		if edge.SrcQName == "run" && edge.DstQName == "helper" {
			if edge.Rule != rule {
				t.Fatalf("run -> helper rule = %q, want %q", edge.Rule, rule)
			}
			return
		}
	}
	t.Fatalf("run -> helper edge missing: %+v", edges)
}
