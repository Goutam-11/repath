package index_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

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

	if want := os.Getenv("REPATH_BENCH_COMMIT"); want != "" {
		head := benchGit(t, root, "rev-parse", "HEAD")
		if head != want {
			t.Fatalf("benchmark root HEAD = %s, want pinned %s", head, want)
		}
	}
	for _, line := range strings.Split(benchGit(t, root, "status", "--porcelain"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasSuffix(line, ".repath/") || strings.Contains(line, " .repath/") {
			continue
		}
		t.Fatalf("benchmark root is dirty; use a throwaway clean clone:\n%s", line)
	}

	ctx := context.Background()
	if err := os.RemoveAll(filepath.Join(root, ".repath")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, ".repath"), 0o755); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, filepath.Join(root, ".repath", "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	// Cold index.
	start := time.Now()
	cold, err := index.Run(ctx, st, root, index.Options{})
	coldDuration := time.Since(start)
	if err != nil {
		t.Fatalf("cold index: %v", err)
	}
	coldRSS := peakRSSBytes()
	t.Logf("cold index: %d files, %d nodes in %s; peak RSS %s",
		cold.Files, cold.Nodes, coldDuration, formatBytes(coldRSS))
	warnBudget(t, "cold <3m", coldDuration, 3*time.Minute)
	warnBudgetBytes(t, "RSS <1GiB", coldRSS, 1<<30)

	// Ten changed files through the normal incremental path; restore them so
	// the clone stays clean.
	sources, err := index.Discover(ctx, root)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}
	if len(sources) < 10 {
		t.Fatalf("only %d indexable files", len(sources))
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
		if _, err := file.WriteString("\n// repath bench-real incremental touch\n"); err != nil {
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
		t.Fatalf("incremental index: %v", err)
	}
	for _, file := range saved {
		if err := os.WriteFile(file.path, file.data, file.mode); err != nil {
			t.Fatal(err)
		}
	}
	if incremental.Indexed != 10 {
		t.Fatalf("incremental indexed = %d, want 10", incremental.Indexed)
	}
	t.Logf("10-file incremental: %s (peak RSS %s)", incrementalDuration, formatBytes(peakRSSBytes()))
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
		t.Fatalf("only %d sampled candidates", len(candidates))
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
			t.Fatalf("map returned no answer: %v", err)
		}
	}

	impactP50, impactP95 := percentile(impactSamples)
	callerP50, callerP95 := percentile(callerSamples)
	pathP50, pathP95 := percentile(pathSamples)
	mapP50, mapP95 := percentile(mapSamples)
	t.Logf("impact p50=%s p95=%s (n=%d, budget p95 <150ms)", impactP50, impactP95, len(impactSamples))
	t.Logf("callers p50=%s p95=%s (n=%d, budget p95 <150ms)", callerP50, callerP95, len(callerSamples))
	t.Logf("path p50=%s p95=%s (n=%d, budget p95 <250ms)", pathP50, pathP95, len(pathSamples))
	t.Logf("map p50=%s p95=%s (n=%d, budget p95 <300ms)", mapP50, mapP95, len(mapSamples))
	warnBudget(t, "impact p95 <150ms", impactP95, 150*time.Millisecond)
	warnBudget(t, "callers p95 <150ms", callerP95, 150*time.Millisecond)
	warnBudget(t, "path p95 <250ms", pathP95, 250*time.Millisecond)
	warnBudget(t, "map p95 <300ms", mapP95, 300*time.Millisecond)

	// check over the full graph.
	start = time.Now()
	report, err := check.Run(ctx, st, root, check.Options{MinConfidence: 0.5, MaxFindings: 200})
	checkDuration := time.Since(start)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	t.Logf("check: %s (%d findings)", checkDuration, len(report.Findings))

	// Artifact export/pull, uncompressed and opt-in zstd.
	outDir := t.TempDir()
	start = time.Now()
	exported, err := artifact.Export(ctx, st, root, outDir, "bench-real")
	exportDuration := time.Since(start)
	if err != nil {
		t.Fatalf("artifact export: %v", err)
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
		t.Fatalf("artifact pull: %v", err)
	}
	if manifest.FileCount != cold.Files {
		t.Fatalf("pulled manifest files = %d, want %d", manifest.FileCount, cold.Files)
	}
	t.Logf("artifact: %s export=%s pull=%s (schema v%d, %d files, %d nodes, %d edges, unresolved %.1f%%)",
		formatBytes(uint64(info.Size())), exportDuration, pullDuration, manifest.SchemaVersion,
		manifest.FileCount, manifest.NodeCount, manifest.EdgeCount, manifest.UnresolvedRate*100)

	zstDir := t.TempDir()
	start = time.Now()
	compressed, err := artifact.ExportWithOptions(ctx, st, root, zstDir, "bench-real", artifact.Options{Compress: true})
	zstExport := time.Since(start)
	if err != nil {
		t.Fatalf("compressed export: %v", err)
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
		t.Fatalf("compressed pull: %v", err)
	}
	if zstManifest.Format != "sqlite+zstd" {
		t.Fatalf("compressed manifest format = %q", zstManifest.Format)
	}
	t.Logf("artifact zstd: %s export=%s pull=%s (%.1f%% of uncompressed)",
		formatBytes(uint64(zstInfo.Size())), zstExport, zstPull,
		100*float64(zstInfo.Size())/float64(info.Size()))

	t.Logf("peak RSS after all measurements: %s", formatBytes(peakRSSBytes()))
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
