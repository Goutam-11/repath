// Package eval measures the graph against hand-labelled expectations.
//
// The fixtures are the gate from PLAN.md §12: TS CALLS precision ≥ 0.90 and
// recall ≥ 0.75; Python precision ≥ 0.88 and recall ≥ 0.80, at the default 0.5
// confidence. It is not a benchmark; it is a regression net for resolution
// rules.
package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/Goutam-11/repath/internal/index"
	"github.com/Goutam-11/repath/internal/store"
)

type expectedEdge struct {
	Src  string `json:"src"`
	Dst  string `json:"dst"`
	Kind string `json:"kind"`
}

type fixtureSpec struct {
	name          string
	precision     float64
	recall        float64
	maxUnresolved int
}

func edgeKey(srcPath, srcQName, dstPath, dstQName, kind string) string {
	return fmt.Sprintf("%s:%s -> %s:%s %s", srcPath, srcQName, dstPath, dstQName, kind)
}

func expectedKey(edge expectedEdge) string {
	return edgeKey(splitLeft(edge.Src), splitRight(edge.Src), splitLeft(edge.Dst), splitRight(edge.Dst), edge.Kind)
}

func copyTree(t *testing.T, src, dst string) {
	t.Helper()
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
}

// indexFixture copies one fixture into a temp repo, indexes it, and returns the
// repo path, the live store, and the run result.
func indexFixture(t *testing.T, name string) (string, *store.Store, index.Result) {
	t.Helper()
	fixture := filepath.Join("..", "..", "testdata", "fixtures", name)
	dir := t.TempDir()
	copyTree(t, fixture, dir)

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(dir, ".repath", "index.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	result, err := index.Run(ctx, st, dir, index.Options{})
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	if result.ParseFailures != 0 {
		t.Fatalf("parse failures = %d, fixture must parse cleanly", result.ParseFailures)
	}
	return dir, st, result
}

func TestFixturePrecisionAndRecall(t *testing.T) {
	specs := []fixtureSpec{
		{name: "ts", precision: 0.90, recall: 0.75, maxUnresolved: 3},
		{name: "py", precision: 0.88, recall: 0.80, maxUnresolved: 2},
		{name: "rs", precision: 0.85, recall: 0.70, maxUnresolved: 2},
	}

	for _, spec := range specs {
		t.Run(spec.name, func(t *testing.T) {
			fixture := filepath.Join("..", "..", "testdata", "fixtures", spec.name)
			_, st, result := indexFixture(t, spec.name)

			expected := loadExpected(t, filepath.Join(fixture, "expected.json"))
			edges, err := st.Edges(context.Background(), 0.5)
			if err != nil {
				t.Fatalf("read edges: %v", err)
			}

			got := make(map[string]bool, len(edges))
			for _, e := range edges {
				got[edgeKey(e.SrcPath, e.SrcQName, e.DstPath, e.DstQName, e.Kind)] = true
			}

			want := make(map[string]bool, len(expected))
			matched := 0
			var missing []string
			for _, edge := range expected {
				key := expectedKey(edge)
				want[key] = true
				if got[key] {
					matched++
				} else {
					missing = append(missing, key)
				}
			}

			var unexpected []string
			for key := range got {
				if !want[key] {
					unexpected = append(unexpected, key)
				}
			}

			precision := 1.0
			if len(got) > 0 {
				precision = float64(matched) / float64(len(got))
			}
			recall := 0.0
			if len(expected) > 0 {
				recall = float64(matched) / float64(len(expected))
			}

			sort.Strings(missing)
			sort.Strings(unexpected)
			t.Logf("edges=%d expected=%d matched=%d precision=%.2f recall=%.2f unresolved=%d",
				len(got), len(expected), matched, precision, recall, result.Unresolved)

			if precision < spec.precision {
				t.Errorf("precision %.2f < %.2f; unexpected edges:\n  %v", precision, spec.precision, unexpected)
			}
			if recall < spec.recall {
				t.Errorf("recall %.2f < %.2f; missing edges:\n  %v", recall, spec.recall, missing)
			}
			if result.Unresolved > spec.maxUnresolved {
				t.Errorf("unresolved references = %d, want <= %d", result.Unresolved, spec.maxUnresolved)
			}
		})
	}
}

func TestAmbiguousEdgesAreLowConfidence(t *testing.T) {
	specs := []struct {
		fixture string
		rules   []string
	}{
		{fixture: "ts", rules: []string{"ts.global.ambiguous"}},
		{fixture: "py", rules: []string{"py.global.ambiguous"}},
		{fixture: "rs", rules: []string{"rs.typed.method.ambiguous", "rs.generic.bound.ambiguous", "rs.trait.method.ambiguous"}},
	}

	for _, spec := range specs {
		t.Run(spec.fixture, func(t *testing.T) {
			_, st, _ := indexFixture(t, spec.fixture)

			edges, err := st.Edges(context.Background(), 0.35)
			if err != nil {
				t.Fatalf("read edges: %v", err)
			}

			for _, rule := range spec.rules {
				ambiguous := 0
				highConfidence := 0
				for _, e := range edges {
					if e.Rule != rule {
						continue
					}
					ambiguous++
					if e.Confidence >= 0.5 {
						highConfidence++
					}
					// The candidates column is what keeps an ambiguous group
					// visible to `status` and to the SCIP adapter; never pick one.
					candidates := strings.Split(e.Candidates, ",")
					if len(candidates) < 2 {
						t.Errorf("%s edge %s -> %s has candidates=%q, want at least two",
							rule, e.SrcQName, e.DstQName, e.Candidates)
					}
				}
				if ambiguous == 0 {
					t.Fatalf("expected %s edges for duplicated symbols", rule)
				}
				if highConfidence != 0 {
					t.Fatalf("%d %s edges are above the default threshold", highConfidence, rule)
				}
			}
		})
	}
}

// TestTypeScriptUniqueNameStaysBelowFloor pins #82. A bare TypeScript
// identifier with exactly one repo-wide declaration and no import, scope, or
// receiver-type evidence is a name-only guess. The Kibana audit found the
// same-package signal did not separate correct guesses from prop callbacks and
// test-local helpers, so the rung is 0.40: visible with --min-confidence 0.4,
// never part of default answers. Both fixture shapes are pinned: a prop
// callback whose target lives in another file, and a correct same-package
// helper (which loses default visibility too, the accepted trade-off).
func TestTypeScriptUniqueNameStaysBelowFloor(t *testing.T) {
	_, st, _ := indexFixture(t, "ts")

	low, err := st.Edges(context.Background(), 0.35)
	if err != nil {
		t.Fatalf("read edges: %v", err)
	}
	want := map[string]bool{
		"src/button.ts:Button -> src/clipboard.ts:Clipboard.onClick CALLS":     false,
		"src/global-use.ts:callUnique -> src/global-def.ts:uniqueByName CALLS": false,
	}
	for _, e := range low {
		if e.Rule != "ts.global.unique" {
			continue
		}
		key := edgeKey(e.SrcPath, e.SrcQName, e.DstPath, e.DstQName, e.Kind)
		if _, ok := want[key]; !ok {
			continue
		}
		want[key] = true
		if e.Confidence != 0.40 {
			t.Errorf("%s confidence = %v, want 0.40", key, e.Confidence)
		}
	}
	for key, seen := range want {
		if !seen {
			t.Errorf("missing ts.global.unique edge %q at 0.35", key)
		}
	}

	high, err := st.Edges(context.Background(), 0.5)
	if err != nil {
		t.Fatalf("read default edges: %v", err)
	}
	for _, e := range high {
		if e.Rule == "ts.global.unique" {
			t.Errorf("name-only edge entered default answers: %s -> %s (%.2f)",
				e.SrcQName, e.DstQName, e.Confidence)
		}
	}
}

// TestPythonRuleIDs pins the Python ladder: every new rule id must be reachable
// from the fixture, and a Python file must never produce a ts.* rule.
func TestPythonRuleIDs(t *testing.T) {
	_, st, _ := indexFixture(t, "py")
	edges, err := st.Edges(context.Background(), 0.5)
	if err != nil {
		t.Fatalf("read edges: %v", err)
	}

	got := make(map[string]string, len(edges))
	for _, e := range edges {
		got[edgeKey(e.SrcPath, e.SrcQName, e.DstPath, e.DstQName, e.Kind)] = e.Rule
	}

	want := map[string]string{
		"app/util.py:parse -> app/util.py:clamp CALLS":                       "py.local",
		"app/service.py:Service -> app/models.py:Base EXTENDS":               "py.import.named",
		"app/service.py:Service -> app/decorators.py:logged REFERENCES":      "py.import.named",
		"app/worker.py:measure -> app/util.py:parse CALLS":                   "py.import.alias",
		"app/worker.py:measure -> app/util.py:clamp CALLS":                   "py.module.relative",
		"app/module_use.py:measure -> app/util.py:clamp CALLS":               "py.import.module",
		"app/module_use.py:measure_plain -> app/util.py:clamp CALLS":         "py.import.module",
		"app/star.py:bounded -> app/util.py:clamp CALLS":                     "py.import.star",
		"app/reexports_use.py:start -> app/service.py:Service REFERENCES":    "py.import.reexport",
		"app/compat_use.py:cap -> app/util.py:clamp CALLS":                   "py.import.reexport",
		"app/service.py:Service.label -> app/service.py:Service.total CALLS": "py.this.method",
		"app/service.py:build -> app/service.py:Service.total CALLS":         "py.typed.method",
		"app/service.py:make -> app/service.py:Service.create CALLS":         "py.class.method",
		"app/unique_use.py:call_global -> app/util.py:parse CALLS":           "py.global.unique",

		// #54: inheritance, super(), typed receivers, and decorators.
		"app/mro.py:Diamond.go -> app/mro.py:Root.ping CALLS":                                 "py.self.inherited",
		"app/mro.py:Leaf.step -> app/mro.py:Middle.step CALLS":                                "py.self.super",
		"app/inherit_use.py:Admin.shout -> app/models.py:User.greet CALLS":                    "py.self.inherited",
		"app/inherit_use.py:Admin.shout -> app/models.py:Base.identify CALLS":                 "py.self.inherited",
		"app/inherit_use.py:describe_admin -> app/models.py:User.greet CALLS":                 "py.typed.inherited",
		"app/inherit_use.py:via_factory -> app/models.py:User.greet CALLS":                    "py.typed.inherited",
		"app/attrs.py:MemoryRepo.load -> app/attrs.py:Cache.read CALLS":                       "py.typed.method",
		"app/service.py:build_from_factory -> app/service.py:Service.total CALLS":             "py.typed.method",
		"app/service.py:build_from_classmethod -> app/service.py:Service.create CALLS":        "py.class.method",
		"app/decorators_use.py:Cached.value -> app/decorators_use.py:Cached.value REFERENCES": "py.property.alias",
	}
	for key, rule := range want {
		if got[key] != rule {
			t.Errorf("edge %q rule = %q, want %q", key, got[key], rule)
		}
	}

	for key, rule := range got {
		if strings.HasPrefix(rule, "ts.") {
			t.Errorf("Python edge %q used TypeScript rule %q", key, rule)
		}
	}
}

// TestRustRuleIDs pins the Rust ladder: every new rule id must be reachable
// from the fixture, and a Rust file must never produce a ts.* or py.* rule.
func TestRustRuleIDs(t *testing.T) {
	_, st, _ := indexFixture(t, "rs")
	edges, err := st.Edges(context.Background(), 0.5)
	if err != nil {
		t.Fatalf("read edges: %v", err)
	}

	got := make(map[string]string, len(edges))
	for _, e := range edges {
		got[edgeKey(e.SrcPath, e.SrcQName, e.DstPath, e.DstQName, e.Kind)] = e.Rule
	}

	want := map[string]string{
		"src/util.rs:helper -> src/util.rs:clamp CALLS":                           "rs.local",
		"src/service.rs:Service -> src/service.rs:Describe IMPLEMENTS":            "rs.local",
		"src/service.rs:Service.new -> src/service.rs:Service REFERENCES":         "rs.self.type",
		"src/service.rs:Service.make_default -> src/service.rs:Service.new CALLS": "rs.self.assoc",
		"src/service.rs:Service.summary -> src/service.rs:Service.total CALLS":    "rs.self.method",
		"src/service.rs:Service.label -> src/models.rs:User.greet CALLS":          "rs.typed.method",
		"src/service.rs:Service.total -> src/util.rs:clamp CALLS":                 "rs.use.named",
		"src/service.rs:Service.total -> src/util.rs:helper CALLS":                "rs.use.alias",
		"src/glob_use.rs:bounded -> src/util.rs:clamp CALLS":                      "rs.use.glob",
		"src/lib.rs:root_helper -> src/util.rs:clamp CALLS":                       "rs.module.path",
		"src/reexport_use.rs:literal -> src/models.rs:User REFERENCES":            "rs.use.reexport",
		"src/assoc.rs:build -> src/store.rs:Store.new CALLS":                      "rs.assoc.fn",
		"src/global_use.rs:call_global -> src/util.rs:helper CALLS":               "rs.global.unique",

		// #55: generic bounds, trait defaults, workspace members, and module layout.
		"src/generic.rs:bounded -> src/generic.rs:Describe.describe CALLS":           "rs.generic.bound",
		"src/generic.rs:where_clause -> src/generic.rs:Describe.describe CALLS":      "rs.generic.bound",
		"src/generic.rs:supertrait_call -> src/generic.rs:Describe.describe CALLS":   "rs.generic.bound",
		"src/generic_use.rs:show -> src/service.rs:Describe.describe CALLS":          "rs.generic.bound",
		"src/generic.rs:greet_counter -> src/generic.rs:Greet.greet CALLS":           "rs.trait.method",
		"src/generic.rs:use_return -> src/generic.rs:Counter.bump CALLS":             "rs.typed.method.return",
		"src/generic.rs:use_chained -> src/generic.rs:Counter.bump CALLS":            "rs.typed.method.return",
		"src/generic.rs:via_assoc -> src/generic.rs:Service2.make CALLS":             "rs.typed.method",
		"src/generic.rs:via_assoc -> src/generic.rs:Counter.bump CALLS":              "rs.typed.method.return",
		"src/generic.rs:Counter -> src/generic.rs:Greet IMPLEMENTS":                  "rs.local",
		"src/inline_use.rs:call_inline -> src/lib.rs:inline.inner CALLS":             "rs.use.named",
		"src/inline_use.rs:direct_inline -> src/lib.rs:inline.inner CALLS":           "rs.module.inline",
		"src/path_use.rs:via_use -> src/alt/impl_file.rs:alt_helper CALLS":           "rs.use.named",
		"src/path_use.rs:via_attr -> src/alt/impl_file.rs:alt_helper CALLS":          "rs.module.path.attr",
		"src/workspace_use.rs:measure -> crates/extra/src/lib.rs:extra_helper CALLS": "rs.use.workspace",
		"src/workspace_use.rs:direct -> crates/extra/src/lib.rs:model.clamp2 CALLS":  "rs.module.workspace",
		"src/cross_use.rs:run -> src/cross.rs:Cross.cross CALLS":                     "rs.typed.method.crate",
		"src/cross_use.rs:build -> src/cross.rs:Cross.make CALLS":                    "rs.assoc.fn.crate",
	}
	for key, rule := range want {
		if got[key] != rule {
			t.Errorf("edge %q rule = %q, want %q", key, got[key], rule)
		}
	}

	for key, rule := range got {
		if strings.HasPrefix(rule, "ts.") || strings.HasPrefix(rule, "py.") {
			t.Errorf("Rust edge %q used foreign rule %q", key, rule)
		}
	}
}

// TestIncrementalReindexKeepsEdges guards the incremental contract: editing a
// file without changing its symbols must not orphan incoming edges or retry
// member references by bare name.
func TestIncrementalReindexKeepsEdges(t *testing.T) {
	specs := []struct {
		fixture string
		file    string
	}{
		{fixture: "py", file: "app/service.py"},
		{fixture: "rs", file: "src/service.rs"},
	}

	for _, spec := range specs {
		t.Run(spec.fixture, func(t *testing.T) {
			dir, st, _ := indexFixture(t, spec.fixture)
			ctx := context.Background()

			before := edgeSet(t, st)
			pendingBefore, err := st.CountPending(ctx)
			if err != nil {
				t.Fatalf("pending: %v", err)
			}

			path := filepath.Join(dir, filepath.FromSlash(spec.file))
			f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
			if err != nil {
				t.Fatalf("open for append: %v", err)
			}
			if _, err := f.WriteString("\n"); err != nil {
				t.Fatalf("append: %v", err)
			}
			if err := f.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}

			result, err := index.Run(ctx, st, dir, index.Options{})
			if err != nil {
				t.Fatalf("re-index: %v", err)
			}
			if result.Indexed != 1 || result.Unchanged == 0 {
				t.Fatalf("incremental run indexed=%d unchanged=%d, want 1 changed file", result.Indexed, result.Unchanged)
			}

			after := edgeSet(t, st)
			if len(after) != len(before) {
				t.Fatalf("edges changed after a no-op edit: %d -> %d", len(before), len(after))
			}
			for key, confidence := range before {
				if after[key] != confidence {
					t.Errorf("edge %q changed or disappeared: %.2f -> %.2f", key, confidence, after[key])
				}
			}
			pendingAfter, err := st.CountPending(ctx)
			if err != nil {
				t.Fatalf("pending after: %v", err)
			}
			if pendingAfter != pendingBefore {
				t.Fatalf("unresolved changed after a no-op edit: %d -> %d", pendingBefore, pendingAfter)
			}
		})
	}
}

func edgeSet(t *testing.T, st *store.Store) map[string]float64 {
	t.Helper()
	edges, err := st.Edges(context.Background(), 0)
	if err != nil {
		t.Fatalf("read edges: %v", err)
	}
	out := make(map[string]float64, len(edges))
	for _, e := range edges {
		out[edgeKey(e.SrcPath, e.SrcQName, e.DstPath, e.DstQName, e.Kind)] = e.Confidence
	}
	return out
}

// splitLeft/splitRight separate "src/util.ts:add" without breaking Windows
// paths that contain a colon.
func splitLeft(ref string) string {
	for i := len(ref) - 1; i >= 0; i-- {
		if ref[i] == ':' {
			return ref[:i]
		}
	}
	return ref
}

func splitRight(ref string) string {
	for i := len(ref) - 1; i >= 0; i-- {
		if ref[i] == ':' {
			return ref[i+1:]
		}
	}
	return ref
}

func loadExpected(t *testing.T, path string) []expectedEdge {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read expectations: %v", err)
	}
	var edges []expectedEdge
	if err := json.Unmarshal(data, &edges); err != nil {
		t.Fatalf("parse expectations: %v", err)
	}
	return edges
}

// TestTypeScriptRuleIDs pins the #56 ladder: every barrel, workspace, and
// static-dynamic-import rule id must be reachable from the fixture, and a
// TypeScript file must never produce a Python or Rust rule.
func TestTypeScriptRuleIDs(t *testing.T) {
	_, st, _ := indexFixture(t, "ts")
	edges, err := st.Edges(context.Background(), 0.5)
	if err != nil {
		t.Fatalf("read edges: %v", err)
	}

	got := make(map[string]string, len(edges))
	for _, e := range edges {
		got[edgeKey(e.SrcPath, e.SrcQName, e.DstPath, e.DstQName, e.Kind)] = e.Rule
	}

	want := map[string]string{
		// Direct imports stay on the existing rules.
		"src/math.ts:sum -> src/util.ts:add CALLS":             "ts.import.named",
		"src/fmt.ts:print -> src/format.ts:format CALLS":       "ts.import.default",
		"src/cart.ts:Cart.total -> src/cart.ts:Cart.add CALLS": "ts.this.method",

		// #56: `export *`, named re-export, and default re-export barrels.
		"src/barrel-use.ts:viaBarrel -> src/util.ts:add CALLS":        "ts.import.star",
		"src/barrel-use.ts:viaBarrel -> src/util.ts:clamp CALLS":      "ts.import.reexport",
		"src/barrel-use.ts:viaBarrel -> src/format.ts:format CALLS":   "ts.import.reexport",
		"src/barrel-use.ts:viaBarrel -> src/cart.ts:Cart REFERENCES":  "ts.import.reexport",
		"src/barrel-use.ts:viaBarrel -> src/cart.ts:Cart.total CALLS": "ts.typed.method.reexport",
		"src/cycle-use.ts:both -> src/cycle-a.ts:alpha CALLS":         "ts.import.named",
		"src/cycle-use.ts:both -> src/cycle-b.ts:beta CALLS":          "ts.import.star",
		"src/deep-use.ts:capped -> src/util.ts:clamp CALLS":           "ts.import.reexport",

		// #56: static require()/import() bindings.
		"src/dynamic-use.ts:dynamic -> src/util.ts:add CALLS":        "ts.import.namespace",
		"src/dynamic-use.ts:dynamic -> src/util.ts:clamp CALLS":      "ts.import.reexport",
		"src/dynamic-use.ts:dynamicAwait -> src/util.ts:clamp CALLS": "ts.import.namespace",

		// #56: package.json workspaces, direct entry and re-exported entry.
		"src/workspace-use.ts:viaWorkspace -> packages/gizmo/index.ts:spin CALLS":       "ts.import.workspace",
		"src/workspace-use.ts:viaWorkspace -> packages/widget/src/impl.ts:gadget CALLS": "ts.import.workspace.star",
	}
	for key, rule := range want {
		if got[key] != rule {
			t.Errorf("edge %q rule = %q, want %q", key, got[key], rule)
		}
	}

	for key, rule := range got {
		if strings.HasPrefix(rule, "py.") || strings.HasPrefix(rule, "rs.") {
			t.Errorf("TypeScript edge %q used rule %q", key, rule)
		}
	}
}
