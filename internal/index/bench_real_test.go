package index_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Goutam-11/repath/internal/adapter"
	"github.com/Goutam-11/repath/internal/artifact"
	"github.com/Goutam-11/repath/internal/check"
	"github.com/Goutam-11/repath/internal/index"
	"github.com/Goutam-11/repath/internal/query"
	"github.com/Goutam-11/repath/internal/store"
	"github.com/Goutam-11/repath/internal/testpath"
)

// TestBenchReal is the opt-in real-repository validation from issue #31. It is
// deliberately not a CI gate: it mutates the pinned clone's `.repath/` and,
// briefly, ten source files (restored from memory), so it requires an explicit
// clean clone:
//
//	REPATH_BENCH=1 REPATH_BENCH_ROOT=/path/to/clone \
//	REPATH_BENCH_COMMIT=<sha> make bench-real
//
// It reports cold/RSS/incremental/query/check/artifact/zstd numbers and marks
// PLAN §10 misses without failing the run; the synthetic `make perf` budgets
// remain the gate.
func TestBenchReal(t *testing.T) {
	if os.Getenv("REPATH_BENCH") != "1" {
		t.Skip("set REPATH_BENCH=1 and REPATH_BENCH_ROOT (or run make bench-real)")
	}
	root := os.Getenv("REPATH_BENCH_ROOT")
	if root == "" {
		t.Fatal("REPATH_BENCH_ROOT is required")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	runRealBench(t, root, os.Getenv("REPATH_BENCH_COMMIT"), realBenchOptions{comment: "// "})
}

// realBenchOptions adapts the shared real-repository matrix to one corpus.
// The zero value reproduces the Kibana run from issue #31 exactly.
type realBenchOptions struct {
	label       string // log prefix, e.g. "python "; empty for the Kibana run
	comment     string // line-comment prefix used to touch ten files
	scipPath    string // optional SCIP index for the adapter before/after pass
	adapterName string // expected stats.adapter, e.g. "scip-rust"
	rulePrefix  string // adapter rules to count, e.g. "scip.rs."
}

// runRealBench measures the cold/incremental/query/map/check/artifact matrix
// on one clean, pinned clone. It logs PLAN §10 misses instead of failing.
func runRealBench(t *testing.T, root, wantCommit string, opts realBenchOptions) {
	t.Helper()
	if wantCommit != "" {
		head := benchGit(t, root, "rev-parse", "HEAD")
		if head != wantCommit {
			t.Fatalf("%sbenchmark root HEAD = %s, want pinned %s", opts.label, head, wantCommit)
		}
	}
	for _, line := range strings.Split(benchGit(t, root, "status", "--porcelain"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasSuffix(line, ".repath/") || strings.Contains(line, " .repath/") {
			continue
		}
		t.Fatalf("%sbenchmark root is dirty; use a throwaway clean clone:\n%s", opts.label, line)
	}

	ctx := context.Background()
	st, err := openBenchStore(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()

	// Cold index.
	start := time.Now()
	cold, err := index.Run(ctx, st, root, index.Options{})
	coldDuration := time.Since(start)
	if err != nil {
		t.Fatalf("%scold index: %v", opts.label, err)
	}
	coldRSS := peakRSSBytes()
	t.Logf("%scold index: %d files, %d nodes in %s; peak RSS %s",
		opts.label, cold.Files, cold.Nodes, coldDuration, formatBytes(coldRSS))
	warnBudget(t, "cold <3m", coldDuration, 3*time.Minute)
	warnBudgetBytes(t, "RSS <1GiB", coldRSS, 1<<30)

	// Ten changed files through the normal incremental path; restore them so
	// the clone stays clean.
	sources, err := index.Discover(ctx, root)
	if err != nil {
		t.Fatalf("%sdiscover: %v", opts.label, err)
	}
	if len(sources) < 10 {
		t.Fatalf("%sonly %d indexable files", opts.label, len(sources))
	}
	comment := opts.comment
	if comment == "" {
		comment = "// "
	}
	type savedFile struct {
		path string
		data []byte
		mode os.FileMode
	}
	saved := make([]savedFile, 0, 10)
	for _, source := range sources[:10] {
		data, err := os.ReadFile(source.Abs)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(source.Abs)
		if err != nil {
			t.Fatal(err)
		}
		saved = append(saved, savedFile{path: source.Abs, data: data, mode: info.Mode().Perm()})
		file, err := os.OpenFile(source.Abs, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.WriteString("\n" + comment + "repath bench-real incremental touch\n"); err != nil {
			file.Close()
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
	}
	start = time.Now()
	incremental, err := index.Run(ctx, st, root, index.Options{})
	incrementalDuration := time.Since(start)
	if err != nil {
		t.Fatalf("%sincremental index: %v", opts.label, err)
	}
	for _, file := range saved {
		if err := os.WriteFile(file.path, file.data, file.mode); err != nil {
			t.Fatal(err)
		}
	}
	if incremental.Indexed != 10 {
		t.Fatalf("%sincremental indexed = %d, want 10", opts.label, incremental.Indexed)
	}
	t.Logf("%s10-file incremental: %s (peak RSS %s; %d files, edges %d, unresolved %d; cold edges %d, unresolved %d)",
		opts.label, incrementalDuration, formatBytes(peakRSSBytes()), incremental.Indexed,
		incremental.Edges, incremental.Unresolved, cold.Edges, cold.Unresolved)
	if incremental.Edges != cold.Edges || incremental.Unresolved != cold.Unresolved {
		t.Logf("%sINCREMENTAL DIVERGENCE: edges %d vs cold %d, unresolved %d vs cold %d",
			opts.label, incremental.Edges, cold.Edges, incremental.Unresolved, cold.Unresolved)
	}
	warnBudget(t, "10-file incremental <1s", incrementalDuration, time.Second)

	// Query samples over real symbols with both callers and callees.
	nodes, err := st.Nodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	paths, err := st.FilePaths(ctx)
	if err != nil {
		t.Fatal(err)
	}
	skeletons, err := st.GraphEdgeSkeletons(ctx, 0.5)
	if err != nil {
		t.Fatal(err)
	}
	in, out := make(map[int64]int), make(map[int64]int)
	for _, edge := range skeletons {
		in[edge.Dst]++
		out[edge.Src]++
	}
	var candidates []store.NodeRow
	for _, node := range nodes {
		if node.Kind != "function" && node.Kind != "method" {
			continue
		}
		if testpath.IsTest(paths[node.FileID]) || in[node.ID] == 0 || out[node.ID] == 0 {
			continue
		}
		candidates = append(candidates, node)
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].QName < candidates[j].QName })
	if len(candidates) < 5 {
		t.Fatalf("%sonly %d sampled candidates", opts.label, len(candidates))
	}

	// Evenly spaced, deterministic samples.
	const wantSamples = 20
	step := len(candidates) / wantSamples
	if step < 1 {
		step = 1
	}
	var impactSamples, callerSamples, pathSamples []time.Duration
	for i := 0; i < len(candidates) && len(impactSamples) < wantSamples; i += step {
		node := candidates[i]
		start = time.Now()
		answer, code, err := query.Impact(ctx, st, node.QName, 8, 0.5, 50)
		impactSamples = append(impactSamples, time.Since(start))
		if err != nil || code != 0 || len(answer.Result.Dependents) == 0 {
			continue
		}
		start = time.Now()
		callers, code, err := query.Callers(ctx, st, node.QName, 0.5, 200)
		callerSamples = append(callerSamples, time.Since(start))
		if err != nil || code != 0 || len(callers.Result.Callers) == 0 {
			continue
		}
		caller := callers.Result.Callers[0].QName
		start = time.Now()
		pathAnswer, code, err := query.Path(ctx, st, caller, node.QName, 8, 0.5, 3)
		elapsed := time.Since(start)
		if err == nil && code == 0 && len(pathAnswer.Result.Hops) > 0 {
			pathSamples = append(pathSamples, elapsed)
		}
	}

	var mapSamples []time.Duration
	for i := 0; i < 5; i++ {
		start = time.Now()
		answer, err := query.Map(ctx, st, 2000, 0.5)
		mapSamples = append(mapSamples, time.Since(start))
		if err != nil || len(answer.Result.Top) == 0 || len(answer.Result.Modules) == 0 {
			t.Fatalf("%smap returned no answer: %v", opts.label, err)
		}
	}

	impactP50, impactP95 := percentile(impactSamples)
	callerP50, callerP95 := percentile(callerSamples)
	pathP50, pathP95 := percentile(pathSamples)
	mapP50, mapP95 := percentile(mapSamples)
	t.Logf("%simpact p50=%s p95=%s (n=%d, budget p95 <150ms)", opts.label, impactP50, impactP95, len(impactSamples))
	t.Logf("%scallers p50=%s p95=%s (n=%d, budget p95 <150ms)", opts.label, callerP50, callerP95, len(callerSamples))
	t.Logf("%spath p50=%s p95=%s (n=%d, budget p95 <250ms)", opts.label, pathP50, pathP95, len(pathSamples))
	t.Logf("%smap p50=%s p95=%s (n=%d, budget p95 <300ms)", opts.label, mapP50, mapP95, len(mapSamples))
	warnBudget(t, "impact p95 <150ms", impactP95, 150*time.Millisecond)
	warnBudget(t, "callers p95 <150ms", callerP95, 150*time.Millisecond)
	warnBudget(t, "path p95 <250ms", pathP95, 250*time.Millisecond)
	warnBudget(t, "map p95 <300ms", mapP95, 300*time.Millisecond)

	// check over the full graph.
	start = time.Now()
	report, err := check.Run(ctx, st, root, check.Options{MinConfidence: 0.5, MaxFindings: 200})
	checkDuration := time.Since(start)
	if err != nil {
		t.Fatalf("%scheck: %v", opts.label, err)
	}
	t.Logf("%scheck: %s (%d findings)", opts.label, checkDuration, len(report.Findings))

	// Artifact export/pull, uncompressed and opt-in zstd.
	outDir := t.TempDir()
	start = time.Now()
	exported, err := artifact.Export(ctx, st, root, outDir, "bench-real")
	exportDuration := time.Since(start)
	if err != nil {
		t.Fatalf("%sartifact export: %v", opts.label, err)
	}
	info, err := os.Stat(exported.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	pullRoot := t.TempDir()
	start = time.Now()
	manifest, err := artifact.Pull(ctx, outDir, pullRoot, "bench-real")
	pullDuration := time.Since(start)
	if err != nil {
		t.Fatalf("%sartifact pull: %v", opts.label, err)
	}
	if manifest.FileCount != cold.Files {
		t.Fatalf("%spulled manifest files = %d, want %d", opts.label, manifest.FileCount, cold.Files)
	}
	t.Logf("%sartifact: %s export=%s pull=%s (schema v%d, %d files, %d nodes, %d edges, unresolved %.1f%%)",
		opts.label, formatBytes(uint64(info.Size())), exportDuration, pullDuration, manifest.SchemaVersion,
		manifest.FileCount, manifest.NodeCount, manifest.EdgeCount, manifest.UnresolvedRate*100)

	zstDir := t.TempDir()
	start = time.Now()
	compressed, err := artifact.ExportWithOptions(ctx, st, root, zstDir, "bench-real", artifact.Options{Compress: true})
	zstExport := time.Since(start)
	if err != nil {
		t.Fatalf("%scompressed export: %v", opts.label, err)
	}
	zstInfo, err := os.Stat(compressed.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	zstPullRoot := t.TempDir()
	start = time.Now()
	zstManifest, err := artifact.Pull(ctx, zstDir, zstPullRoot, "bench-real")
	zstPull := time.Since(start)
	if err != nil {
		t.Fatalf("%scompressed pull: %v", opts.label, err)
	}
	if zstManifest.Format != "sqlite+zstd" {
		t.Fatalf("%scompressed manifest format = %q", opts.label, zstManifest.Format)
	}
	t.Logf("%sartifact zstd: %s export=%s pull=%s (%.1f%% of uncompressed)",
		opts.label, formatBytes(uint64(zstInfo.Size())), zstExport, zstPull,
		100*float64(zstInfo.Size())/float64(info.Size()))

	// Optional SCIP adapter pass. The incremental run above indexes the
	// touched content, so rebuild the cold graph first: tree-sitter resolution
	// and the adapter before/after must be measured on the same pinned state
	// the docs quote. The full re-index cost is logged, not hidden.
	if opts.scipPath != "" {
		// Rebuild from an empty .repath so the adapter starts from the same
		// cold graph the docs quote. A warm `--full` run is not equivalent: it
		// skips the pending retry when the symbol set is unchanged.
		if err := st.Close(); err != nil {
			t.Fatal(err)
		}
		st, err = openBenchStore(ctx, root)
		if err != nil {
			t.Fatal(err)
		}
		baselineStart := time.Now()
		baseline, err := index.Run(ctx, st, root, index.Options{})
		if err != nil {
			t.Fatalf("%sadapter baseline cold re-index: %v", opts.label, err)
		}
		match := "matches cold"
		if baseline.Edges != cold.Edges || baseline.Unresolved != cold.Unresolved {
			match = "DIFFERS from cold"
		}
		t.Logf("%sadapter baseline cold re-index: %s (%d files, edges %d, unresolved %d; %s)",
			opts.label, time.Since(baselineStart), baseline.Files, baseline.Edges, baseline.Unresolved, match)
		runAdapterBench(t, ctx, st, opts)
	}

	t.Logf("%speak RSS after all measurements: %s", opts.label, formatBytes(peakRSSBytes()))
}

// runAdapterBench applies one pinned SCIP index and reports the before/after
// unresolved count plus production/test and rule splits of the new edges.
func runAdapterBench(t *testing.T, ctx context.Context, st *store.Store, opts realBenchOptions) {
	t.Helper()
	unresolvedBefore, err := st.CountPending(ctx)
	if err != nil {
		t.Fatal(err)
	}
	edgesBefore, err := st.CountEdges(ctx)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	idx, err := adapter.LoadSCIP(opts.scipPath)
	if err != nil {
		t.Fatalf("%sadapter: load %s: %v", opts.label, opts.scipPath, err)
	}
	statsList, err := adapter.ApplySCIPAll(ctx, st, idx)
	duration := time.Since(start)
	if err != nil {
		t.Fatalf("%sadapter: %v", opts.label, err)
	}
	var stats adapter.Stats
	found := false
	for _, candidate := range statsList {
		if candidate.Adapter == opts.adapterName {
			stats, found = candidate, true
		}
	}
	if !found {
		t.Fatalf("%sadapter: no %s run (got %d adapter runs)", opts.label, opts.adapterName, len(statsList))
	}
	t.Logf("%sadapter %s: %d definitions, %d references, %d matched, %d edges, %d conflicts, %d skipped, %d unmatched in %s",
		opts.label, stats.Adapter, stats.Definitions, stats.References, stats.Matched, stats.Edges,
		stats.Conflicts, stats.Skipped, stats.Unmatched, duration)

	unresolvedAfter, err := st.CountPending(ctx)
	if err != nil {
		t.Fatal(err)
	}
	edgesAfter, err := st.CountEdges(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%sadapter %s: unresolved %d -> %d (%.2f%% -> %.2f%%; edges %d -> %d)",
		opts.label, stats.Adapter, unresolvedBefore, unresolvedAfter,
		unresolvedRate(edgesBefore, unresolvedBefore), unresolvedRate(edgesAfter, unresolvedAfter),
		edgesBefore, edgesAfter)

	details, err := st.Edges(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	testNames := map[string]bool{}
	entrypoints, err := st.Entrypoints(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entrypoints {
		if strings.HasSuffix(entry.Rule, ".test") {
			testNames[entry.Node.QName] = true
		}
	}
	var production, testEdges, testPath, testEntry, method, call, newEdges, reference int
	roots := map[string]int{}
	for _, edge := range details {
		if !strings.HasPrefix(edge.Rule, opts.rulePrefix) {
			continue
		}
		// Test classification: a test path, or a #[test]/pytest entrypoint.
		// Test-support modules that are neither (e.g. src/iter/test.rs helpers)
		// stay in the non-test bucket, so the production share is an upper bound.
		switch {
		case testpath.IsTest(edge.SrcPath):
			testEdges++
			testPath++
		case testNames[edge.SrcQName]:
			testEdges++
			testEntry++
		default:
			production++
		}
		roots[sourceRoot(edge.SrcPath)]++
		switch strings.TrimPrefix(edge.Rule, opts.rulePrefix) {
		case "method":
			method++
		case "call":
			call++
		case "new":
			newEdges++
		default:
			reference++
		}
	}
	t.Logf("%sadapter %s: new edges by source: %d non-test, %d test (path %d, entrypoint %d); rules method=%d call=%d new=%d reference=%d",
		opts.label, stats.Adapter, production, testEdges, testPath, testEntry, method, call, newEdges, reference)
	t.Logf("%sadapter %s: new-edge source roots: %s",
		opts.label, stats.Adapter, formatRootCounts(roots))
}

func unresolvedRate(edges, unresolved int) float64 {
	if edges+unresolved == 0 {
		return 0
	}
	return 100 * float64(unresolved) / float64(edges+unresolved)
}

func sourceRoot(path string) string {
	if i := strings.IndexByte(path, '/'); i > 0 {
		return path[:i]
	}
	return "."
}

func formatRootCounts(counts map[string]int) string {
	type rootCount struct {
		root  string
		count int
	}
	ordered := make([]rootCount, 0, len(counts))
	for root, count := range counts {
		ordered = append(ordered, rootCount{root, count})
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].count != ordered[j].count {
			return ordered[i].count > ordered[j].count
		}
		return ordered[i].root < ordered[j].root
	})
	parts := make([]string, 0, len(ordered))
	for _, item := range ordered {
		parts = append(parts, item.root+" "+strconv.Itoa(item.count))
	}
	return strings.Join(parts, ", ")
}

// openBenchStore starts one run from an empty `.repath/`, the same state a
// fresh clone has. It never seeds from the commit cache, so the measured graph
// is the cold graph and not whatever a previous run cached.
func openBenchStore(ctx context.Context, root string) (*store.Store, error) {
	if err := os.RemoveAll(filepath.Join(root, ".repath")); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(root, ".repath"), 0o755); err != nil {
		return nil, err
	}
	st, err := store.Open(ctx, filepath.Join(root, ".repath", "index.db"))
	if err != nil {
		return nil, err
	}
	if err := st.Migrate(ctx); err != nil {
		_ = st.Close()
		return nil, err
	}
	return st, nil
}

func benchGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// warnBudget logs a PLAN §10 miss instead of failing: real-repo numbers are
// evidence, not a CI gate.
func warnBudget(t *testing.T, name string, got, budget time.Duration) {
	t.Helper()
	if got > budget {
		t.Logf("BUDGET MISS: %s = %s (> %s)", name, got, budget)
		return
	}
	t.Logf("budget met: %s = %s (<= %s)", name, got, budget)
}

func warnBudgetBytes(t *testing.T, name string, got, budget uint64) {
	t.Helper()
	if got > budget {
		t.Logf("BUDGET MISS: %s = %s (> %s)", name, formatBytes(got), formatBytes(budget))
		return
	}
	t.Logf("budget met: %s = %s (<= %s)", name, formatBytes(got), formatBytes(budget))
}
