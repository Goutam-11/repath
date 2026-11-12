package index_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Goutam-11/repath/internal/artifact"
	"github.com/Goutam-11/repath/internal/check"
	"github.com/Goutam-11/repath/internal/index"
	"github.com/Goutam-11/repath/internal/mcp"
	"github.com/Goutam-11/repath/internal/query"
	"github.com/Goutam-11/repath/internal/store"
)

const perfSampleSize = 20

// perfConfig describes one reproducible synthetic budget run. The tree shape
// is deterministic: `modules` directories, each holding `perModule` TypeScript
// files. Every file defines one exported function; file i>0 imports file i-1
// and calls it, so a module is a 100-node call chain. The 10k tree has 100
// modules and 9,900 CALLS; the 50k tree has 500 modules and 49,900 CALLS.
type perfConfig struct {
	name       string
	modules    int
	perModule  int
	coldBudget time.Duration
	rssBudget  uint64
	artifact   bool
}

// TestPerf10k is the reproducible PLAN §10/M5 budget run. It is opt-in because
// generating and indexing 10k files is not a unit test:
//
//	make perf-10k
func TestPerf10k(t *testing.T) {
	runPerf(t, perfConfig{
		name:       "10k",
		modules:    100,
		perModule:  100,
		coldBudget: 60 * time.Second,
		artifact:   true,
	})
}

// TestPerf50k validates the 50k-file ceiling from PLAN §10 (cold <3 min,
// RSS <1 GB) and measures artifact export/pull at that size. It is opt-in:
//
//	make perf-50k
func TestPerf50k(t *testing.T) {
	runPerf(t, perfConfig{
		name:       "50k",
		modules:    500,
		perModule:  100,
		coldBudget: 3 * time.Minute,
		rssBudget:  1 << 30,
		artifact:   true,
	})
}

func runPerf(t *testing.T, cfg perfConfig) {
	t.Helper()
	if os.Getenv("REPATH_PERF") == "" {
		t.Skip("set REPATH_PERF=1 (or run make perf) to exercise the synthetic budget")
	}

	fileCount := cfg.modules * cfg.perModule
	ctx := context.Background()
	root := t.TempDir()

	start := time.Now()
	generateSynthetic(t, root, cfg.modules, cfg.perModule)
	t.Logf("generated %s tree: %d files in %s", cfg.name, fileCount, time.Since(start))

	if err := os.MkdirAll(filepath.Join(root, ".repath"), 0o755); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, filepath.Join(root, ".repath", "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx); err != nil {
		st.Close()
		t.Fatal(err)
	}

	start = time.Now()
	cold, err := index.Run(ctx, st, root, index.Options{})
	coldDuration := time.Since(start)
	if err != nil {
		st.Close()
		t.Fatalf("cold index: %v", err)
	}
	if cold.Files != fileCount {
		st.Close()
		t.Fatalf("cold files = %d, want %d", cold.Files, fileCount)
	}
	coldRSS := peakRSSBytes()
	t.Logf("cold index: %d files, %d nodes, %d edges in %s; peak RSS %s",
		cold.Files, cold.Nodes, cold.Edges, coldDuration, formatBytes(coldRSS))

	// Ten changed files through the normal incremental path.
	for i := 0; i < 10; i++ {
		path := filepath.Join(root, fmt.Sprintf("m%03d", i), fmt.Sprintf("f_%03d.ts", i))
		file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			st.Close()
			t.Fatal(err)
		}
		if _, err := file.WriteString("\n// touched by the 10-file incremental budget\n"); err != nil {
			file.Close()
			st.Close()
			t.Fatal(err)
		}
		file.Close()
	}
	start = time.Now()
	incremental, err := index.Run(ctx, st, root, index.Options{})
	incrementalDuration := time.Since(start)
	st.Close()
	if err != nil {
		t.Fatalf("incremental index: %v", err)
	}
	if incremental.Indexed != 10 {
		t.Fatalf("incremental indexed = %d, want 10", incremental.Indexed)
	}
	t.Logf("10-file incremental: %s", incrementalDuration)

	// Queries reuse the active database; no re-index.
	session, err := query.Open(ctx, root, true)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	impactSamples := make([]time.Duration, 0, perfSampleSize)
	callerSamples := make([]time.Duration, 0, perfSampleSize)
	pathSamples := make([]time.Duration, 0, perfSampleSize)
	middle := cfg.perModule / 2
	for i := 0; i < perfSampleSize; i++ {
		module := i % cfg.modules
		file := middle + i%20
		if file >= cfg.perModule {
			file = middle
		}
		target := fmt.Sprintf("f_%03d_%03d", module, file)

		start = time.Now()
		answer, code, err := query.Impact(ctx, session.Store, target, 8, 0.5, 50)
		impactSamples = append(impactSamples, time.Since(start))
		if err != nil || code != 0 {
			t.Fatalf("impact %s: code %d err %v", target, code, err)
		}
		if len(answer.Result.Dependents) == 0 {
			t.Fatalf("impact %s returned no dependents", target)
		}

		start = time.Now()
		callers, code, err := query.Callers(ctx, session.Store, target, 0.5, 200)
		callerSamples = append(callerSamples, time.Since(start))
		if err != nil || code != 0 {
			t.Fatalf("callers %s: code %d err %v", target, code, err)
		}
		if len(callers.Result.Callers) == 0 {
			t.Fatalf("callers %s returned none", target)
		}

		from := fmt.Sprintf("f_%03d_%03d", module, cfg.perModule-1)
		to := fmt.Sprintf("f_%03d_%03d", module, cfg.perModule-9)
		start = time.Now()
		pathAnswer, code, err := query.Path(ctx, session.Store, from, to, 8, 0.5, 3)
		pathSamples = append(pathSamples, time.Since(start))
		if err != nil || code != 0 {
			t.Fatalf("path %s -> %s: code %d err %v", from, to, code, err)
		}
		if len(pathAnswer.Result.Hops) == 0 {
			t.Fatalf("path %s -> %s found no hops", from, to)
		}
	}

	mapSamples := make([]time.Duration, 0, 5)
	for i := 0; i < 5; i++ {
		start = time.Now()
		answer, err := query.Map(ctx, session.Store, 2000, 0.5)
		mapSamples = append(mapSamples, time.Since(start))
		if err != nil {
			t.Fatalf("map: %v", err)
		}
		if len(answer.Result.Top) == 0 || len(answer.Result.Modules) == 0 {
			t.Fatal("map returned no symbols or modules")
		}
	}

	impactP50, impactP95 := percentile(impactSamples)
	callerP50, callerP95 := percentile(callerSamples)
	pathP50, pathP95 := percentile(pathSamples)
	mapP50, mapP95 := percentile(mapSamples)
	t.Logf("impact p50=%s p95=%s (budget p95 <150ms)", impactP50, impactP95)
	t.Logf("callers p50=%s p95=%s (budget p95 <150ms)", callerP50, callerP95)
	t.Logf("path p50=%s p95=%s (budget p95 <250ms)", pathP50, pathP95)
	t.Logf("map p50=%s p95=%s (budget p95 <300ms)", mapP50, mapP95)

	// The MCP adapter opens the same active database and answers one call.
	mcpRequest := fmt.Sprintf(
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"impact","arguments":{"symbol":"f_000_%03d","depth":8}}}`+"\n",
		middle)
	var mcpOut bytes.Buffer
	start = time.Now()
	mcpCode := mcp.Run(ctx, root, "perf", strings.NewReader(mcpRequest), &mcpOut, io.Discard)
	mcpDuration := time.Since(start)
	if mcpCode != 0 {
		t.Fatalf("mcp run exit = %d", mcpCode)
	}
	if !strings.Contains(mcpOut.String(), "dependents") {
		t.Fatalf("mcp answer missing dependents: %s", mcpOut.String())
	}
	t.Logf("MCP impact round-trip: %s", mcpDuration)

	// The v1.1 check reads the same graph; report its cost on the full tree.
	start = time.Now()
	checkReport, err := check.Run(ctx, session.Store, root, check.Options{MinConfidence: 0.5, MaxFindings: 200})
	checkDuration := time.Since(start)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	t.Logf("check %s: %s (%d findings)", cfg.name, checkDuration, len(checkReport.Findings))

	// Artifact export/pull at full size. Pull lands in an empty directory so the
	// number measures the snapshot copy, not a 50k-file re-hash overlay.
	if cfg.artifact {
		outDir := t.TempDir()
		start = time.Now()
		exported, err := artifact.Export(ctx, session.Store, root, outDir, "perf")
		exportDuration := time.Since(start)
		if err != nil {
			t.Fatalf("artifact export: %v", err)
		}
		info, err := os.Stat(exported.DBPath)
		if err != nil {
			t.Fatalf("stat artifact: %v", err)
		}
		pullRoot := t.TempDir()
		start = time.Now()
		manifest, err := artifact.Pull(ctx, outDir, pullRoot, "perf")
		pullDuration := time.Since(start)
		if err != nil {
			t.Fatalf("artifact pull: %v", err)
		}
		if manifest.FileCount != cold.Files {
			t.Fatalf("pulled manifest files = %d, want %d", manifest.FileCount, cold.Files)
		}
		t.Logf("artifact: %s export=%s pull=%s (schema v%d, %d files, %d nodes, %d edges)",
			formatBytes(uint64(info.Size())), exportDuration, pullDuration,
			manifest.SchemaVersion, manifest.FileCount, manifest.NodeCount, manifest.EdgeCount)

		zstDir := t.TempDir()
		start = time.Now()
		compressed, err := artifact.ExportWithOptions(ctx, session.Store, root, zstDir, "perf", artifact.Options{Compress: true})
		zstExport := time.Since(start)
		if err != nil {
			t.Fatalf("compressed artifact export: %v", err)
		}
		zstInfo, err := os.Stat(compressed.DBPath)
		if err != nil {
			t.Fatalf("stat compressed artifact: %v", err)
		}
		zstPullRoot := t.TempDir()
		start = time.Now()
		zstManifest, err := artifact.Pull(ctx, zstDir, zstPullRoot, "perf")
		zstPull := time.Since(start)
		if err != nil {
			t.Fatalf("compressed artifact pull: %v", err)
		}
		if zstManifest.Format != "sqlite+zstd" {
			t.Fatalf("compressed manifest format = %q", zstManifest.Format)
		}
		t.Logf("artifact zstd: %s export=%s pull=%s (%.1f%% of uncompressed)",
			formatBytes(uint64(zstInfo.Size())), zstExport, zstPull,
			100*float64(zstInfo.Size())/float64(info.Size()))
	}

	finalRSS := peakRSSBytes()
	if finalRSS > coldRSS {
		t.Logf("peak RSS after all measurements: %s", formatBytes(finalRSS))
	}

	// README quickstart: build is measured separately; this is cold index plus
	// the first answer's graph work.
	t.Logf("cold index + first query: %s (build excluded)", coldDuration+impactP50)

	if coldDuration > cfg.coldBudget {
		t.Errorf("cold %s index = %s, budget <%s", cfg.name, coldDuration, cfg.coldBudget)
	}
	if incrementalDuration > time.Second {
		t.Errorf("10-file incremental = %s, budget <1s", incrementalDuration)
	}
	if impactP95 > 150*time.Millisecond {
		t.Errorf("impact p95 = %s, budget <150ms", impactP95)
	}
	if callerP95 > 150*time.Millisecond {
		t.Errorf("callers p95 = %s, budget <150ms", callerP95)
	}
	if pathP95 > 250*time.Millisecond {
		t.Errorf("path p95 = %s, budget <250ms", pathP95)
	}
	if mapP95 > 300*time.Millisecond {
		t.Errorf("map p95 = %s, budget <300ms", mapP95)
	}
	if cfg.rssBudget > 0 {
		peak := coldRSS
		if finalRSS > peak {
			peak = finalRSS
		}
		if peak > cfg.rssBudget {
			t.Errorf("peak RSS = %s, budget <%s", formatBytes(peak), formatBytes(cfg.rssBudget))
		}
	}
}

func generateSynthetic(t *testing.T, root string, modules, perModule int) {
	t.Helper()
	for m := 0; m < modules; m++ {
		dir := filepath.Join(root, fmt.Sprintf("m%03d", m))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < perModule; i++ {
			name := fmt.Sprintf("f_%03d_%03d", m, i)
			var body string
			if i == 0 {
				body = fmt.Sprintf("export function %s(x: number): number { return x; }\n", name)
			} else {
				previous := fmt.Sprintf("f_%03d_%03d", m, i-1)
				previousFile := fmt.Sprintf("f_%03d", i-1)
				body = fmt.Sprintf("import { %s } from \"./%s\";\nexport function %s(x: number): number { return %s(x); }\n",
					previous, previousFile, name, previous)
			}
			if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f_%03d.ts", i)), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func percentile(samples []time.Duration) (p50, p95 time.Duration) {
	if len(samples) == 0 {
		return 0, 0
	}
	sorted := append([]time.Duration(nil), samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	p50 = sorted[len(sorted)/2]
	index := int(float64(len(sorted)) * 0.95)
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	p95 = sorted[index]
	return p50, p95
}

// peakRSSBytes reads the Linux process high-water mark. Other platforms report
// zero, and the budget check is skipped rather than guessed.
func peakRSSBytes() uint64 {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "VmHWM:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0
		}
		kb, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0
		}
		return kb * 1024
	}
	return 0
}

func formatBytes(n uint64) string {
	if n == 0 {
		return "unavailable"
	}
	const (
		kib = 1024
		mib = 1024 * kib
		gib = 1024 * mib
	)
	switch {
	case n >= gib:
		return fmt.Sprintf("%.2f GiB", float64(n)/gib)
	case n >= mib:
		return fmt.Sprintf("%.1f MiB", float64(n)/mib)
	case n >= kib:
		return fmt.Sprintf("%.1f KiB", float64(n)/kib)
	default:
		return fmt.Sprintf("%d B", n)
	}
}
