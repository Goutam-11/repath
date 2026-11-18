package trace_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Goutam-11/repath/internal/index"
	"github.com/Goutam-11/repath/internal/store"
	"github.com/Goutam-11/repath/internal/trace"
)

func copyTraceFixture(t *testing.T) string {
	t.Helper()
	src := filepath.Join("..", "..", "testdata", "fixtures", "trace", "cpu")
	dst := t.TempDir()
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
	return dst
}

func openTraceStore(t *testing.T) (context.Context, *store.Store, string) {
	t.Helper()
	root := copyTraceFixture(t)
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(root, ".repath", "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := index.Run(ctx, st, root, index.Options{}); err != nil {
		t.Fatal(err)
	}
	return ctx, st, root
}

func profilePath() string {
	return filepath.Join("..", "..", "testdata", "fixtures", "trace", "cpu", "profile.cpuprofile")
}

func findEdge(report trace.Report, caller, callee string) *trace.Edge {
	for i := range report.Edges {
		if report.Edges[i].Caller.QName == caller && report.Edges[i].Callee.QName == callee {
			return &report.Edges[i]
		}
	}
	return nil
}

func TestTraceImport(t *testing.T) {
	ctx, st, _ := openTraceStore(t)
	report, err := trace.Import(ctx, st, profilePath(), 50)
	if err != nil {
		t.Fatal(err)
	}
	if report.Frames < 4 || report.Mapped != 3 {
		t.Fatalf("frames = %d, mapped = %d, want >=4/3", report.Frames, report.Mapped)
	}
	if report.ObservedEdges != 2 || report.Confirmed != 1 || report.ObservedOnly != 1 {
		t.Fatalf("edges = %d, confirmed = %d, observed only = %d, want 2/1/1",
			report.ObservedEdges, report.Confirmed, report.ObservedOnly)
	}

	// The static graph already proves run -> dispatch (ts.local); the profile
	// confirms it. dispatch -> greet is dynamic dispatch the static graph misses.
	confirmed := findEdge(report, "run", "dispatch")
	if confirmed == nil || !confirmed.Static || confirmed.Rule != trace.RuleDyn || confirmed.Confidence != trace.DynConfidence {
		t.Fatalf("run -> dispatch = %+v, want a static-confirmed dyn.trace edge", confirmed)
	}
	dynamic := findEdge(report, "dispatch", "greet")
	if dynamic == nil {
		t.Fatalf("dispatch -> greet missing from %+v", report.Edges)
	}
	if dynamic.Static || dynamic.Rule != trace.RuleDyn || dynamic.Confidence != trace.DynConfidence || dynamic.Count < 1 {
		t.Fatalf("dispatch -> greet = %+v, want an observed-only dyn.trace edge", dynamic)
	}
	if dynamic.Callee.File != "src/dynamic.js" || dynamic.Callee.Line != 3 {
		t.Fatalf("dispatch -> greet target = %+v, want src/dynamic.js:3", dynamic.Callee)
	}
	// A name-only matcher would attach the frame to other.js's greet.
	for _, edge := range report.Edges {
		if edge.Callee.File == "src/other.js" {
			t.Fatalf("trace frame misattached to other.js: %+v", edge)
		}
	}
}

func TestTraceStaleFramesAreSkipped(t *testing.T) {
	ctx, st, root := openTraceStore(t)
	profile, err := os.ReadFile(filepath.Join(root, "src", "dynamic.js"))
	if err != nil {
		t.Fatal(err)
	}
	shifted := append([]byte("\n\n"), profile...)
	if err := os.WriteFile(filepath.Join(root, "src", "dynamic.js"), shifted, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := index.Run(ctx, st, root, index.Options{Full: true}); err != nil {
		t.Fatal(err)
	}

	report, err := trace.Import(ctx, st, profilePath(), 50)
	if err != nil {
		t.Fatal(err)
	}
	if report.ObservedOnly != 0 || findEdge(report, "dispatch", "greet") != nil {
		t.Fatalf("stale profile produced a false edge: %+v", report.Edges)
	}
	if report.Skipped == 0 || len(report.SkippedReasons) == 0 {
		t.Fatalf("stale frames were not counted: %+v", report)
	}
	// The same-name function in other.js still must not be a fallback.
	for _, edge := range report.Edges {
		if edge.Callee.File == "src/other.js" {
			t.Fatalf("stale frame fell back to other.js: %+v", edge)
		}
	}
}

func TestTraceMalformedInputs(t *testing.T) {
	ctx, st, _ := openTraceStore(t)
	dir := t.TempDir()

	badJSON := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(badJSON, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := trace.Import(ctx, st, badJSON, 50); !errors.Is(err, trace.ErrMalformed) {
		t.Fatalf("bad JSON error = %v, want ErrMalformed", err)
	}

	notProfile := filepath.Join(dir, "object.json")
	if err := os.WriteFile(notProfile, []byte(`{"hello":"world"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := trace.Import(ctx, st, notProfile, 50); !errors.Is(err, trace.ErrMalformed) || !strings.Contains(err.Error(), "nodes") {
		t.Fatalf("wrong-shape error = %v, want ErrMalformed about nodes", err)
	}

	empty := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(empty, []byte(`{"nodes":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	report, err := trace.Import(ctx, st, empty, 50)
	if err != nil {
		t.Fatalf("empty profile: %v", err)
	}
	if report.Frames != 0 || report.ObservedEdges != 0 || report.Edges != nil {
		t.Fatalf("empty profile report = %+v, want zeros", report)
	}

	if _, err := trace.Import(ctx, st, filepath.Join(dir, "missing.json"), 50); !errors.Is(err, trace.ErrNotFound) {
		t.Fatalf("missing error = %v, want ErrNotFound", err)
	}
}

func TestTraceLimitAndDeterminism(t *testing.T) {
	ctx, st, _ := openTraceStore(t)
	limited, err := trace.Import(ctx, st, profilePath(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if !limited.Truncated || len(limited.Edges) != 1 || limited.ObservedEdges != 2 {
		t.Fatalf("limited report = %+v, want 1 listed of 2 total and truncated", limited)
	}
	first, err := trace.Import(ctx, st, profilePath(), 50)
	if err != nil {
		t.Fatal(err)
	}
	second, err := trace.Import(ctx, st, profilePath(), 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Edges) != len(second.Edges) {
		t.Fatalf("report is not deterministic: %d vs %d edges", len(first.Edges), len(second.Edges))
	}
	for i := range first.Edges {
		if first.Edges[i] != second.Edges[i] {
			t.Fatalf("report edge %d differs across runs: %+v vs %+v", i, first.Edges[i], second.Edges[i])
		}
	}
}
