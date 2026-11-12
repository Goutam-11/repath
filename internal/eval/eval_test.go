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
		rule    string
	}{
		{fixture: "ts", rule: "ts.global.ambiguous"},
		{fixture: "py", rule: "py.global.ambiguous"},
	}

	for _, spec := range specs {
		t.Run(spec.fixture, func(t *testing.T) {
			_, st, _ := indexFixture(t, spec.fixture)

			edges, err := st.Edges(context.Background(), 0.35)
			if err != nil {
				t.Fatalf("read edges: %v", err)
			}

			ambiguous := 0
			highConfidence := 0
			for _, e := range edges {
				if e.Rule != spec.rule {
					continue
				}
				ambiguous++
				if e.Confidence >= 0.5 {
					highConfidence++
				}
			}
			if ambiguous == 0 {
				t.Fatalf("expected %s edges for duplicated run() symbols", spec.rule)
			}
			if highConfidence != 0 {
				t.Fatalf("%d ambiguous edges are above the default threshold", highConfidence)
			}
		})
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
		"app/star.py:bounded -> app/util.py:clamp CALLS":                     "py.import.star",
		"app/reexports_use.py:start -> app/service.py:Service REFERENCES":    "py.import.reexport",
		"app/compat_use.py:cap -> app/util.py:clamp CALLS":                   "py.import.reexport",
		"app/service.py:Service.label -> app/service.py:Service.total CALLS": "py.this.method",
		"app/service.py:build -> app/service.py:Service.total CALLS":         "py.typed.method",
		"app/service.py:make -> app/service.py:Service.create CALLS":         "py.class.method",
		"app/unique_use.py:call_global -> app/util.py:parse CALLS":           "py.global.unique",
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
