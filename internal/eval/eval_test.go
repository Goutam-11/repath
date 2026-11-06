// Package eval measures the graph against hand-labelled expectations.
//
// The fixture is the gate from PLAN.md §12: CALLS precision ≥ 0.90 and recall
// ≥ 0.75 at the default confidence, with few unresolved references. It is not a
// benchmark; it is a regression net for resolution rules.
package eval

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/Goutam-11/repath/internal/index"
	"github.com/Goutam-11/repath/internal/store"
)

type expectedEdge struct {
	Src  string `json:"src"`
	Dst  string `json:"dst"`
	Kind string `json:"kind"`
}

func edgeKey(srcPath, srcQName, dstPath, dstQName, kind string) string {
	return fmt.Sprintf("%s:%s -> %s:%s %s", srcPath, srcQName, dstPath, dstQName, kind)
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

func TestFixturePrecisionAndRecall(t *testing.T) {
	fixture := filepath.Join("..", "..", "testdata", "fixtures", "ts")
	dir := t.TempDir()
	copyTree(t, fixture, dir)

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(dir, ".repath", "index.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
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

	expected := loadExpected(t, filepath.Join(fixture, "expected.json"))
	edges, err := st.Edges(ctx, 0.5)
	if err != nil {
		t.Fatalf("read edges: %v", err)
	}

	got := make(map[string]bool, len(edges))
	for _, e := range edges {
		got[edgeKey(e.SrcPath, e.SrcQName, e.DstPath, e.DstQName, e.Kind)] = true
	}

	matched := 0
	var missing, unexpected []string
	for _, want := range expected {
		key := edgeKey(splitLeft(want.Src), splitRight(want.Src), splitLeft(want.Dst), splitRight(want.Dst), want.Kind)
		if got[key] {
			matched++
			continue
		}
		missing = append(missing, key)
	}
	for key := range got {
		found := false
		for _, want := range expected {
			if key == edgeKey(splitLeft(want.Src), splitRight(want.Src), splitLeft(want.Dst), splitRight(want.Dst), want.Kind) {
				found = true
				break
			}
		}
		if !found {
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

	if precision < 0.90 {
		t.Errorf("precision %.2f < 0.90; unexpected edges:\n  %v", precision, unexpected)
	}
	if recall < 0.75 {
		t.Errorf("recall %.2f < 0.75; missing edges:\n  %v", recall, missing)
	}
	if result.Unresolved > 3 {
		t.Errorf("unresolved references = %d, want <= 3; the fixture has one genuine unknown (React.createElement)",
			result.Unresolved)
	}
}

func TestAmbiguousEdgesAreLowConfidence(t *testing.T) {
	fixture := filepath.Join("..", "..", "testdata", "fixtures", "ts")
	dir := t.TempDir()
	copyTree(t, fixture, dir)

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(dir, ".repath", "index.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := index.Run(ctx, st, dir, index.Options{}); err != nil {
		t.Fatalf("index: %v", err)
	}

	edges, err := st.Edges(ctx, 0.35)
	if err != nil {
		t.Fatalf("read edges: %v", err)
	}

	ambiguous := 0
	highConfidence := 0
	for _, e := range edges {
		if e.Rule == "ts.global.ambiguous" {
			ambiguous++
			if e.Confidence >= 0.5 {
				highConfidence++
			}
		}
	}
	if ambiguous == 0 {
		t.Fatal("expected ambiguous edges for the duplicated run() fixture")
	}
	if highConfidence != 0 {
		t.Fatalf("%d ambiguous edges are above the default threshold", highConfidence)
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
