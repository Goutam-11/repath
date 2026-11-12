package index_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/Goutam-11/repath/internal/index"
	"github.com/Goutam-11/repath/internal/mcp"
	"github.com/Goutam-11/repath/internal/query"
	"github.com/Goutam-11/repath/internal/store"
)

const (
	perfModules    = 100
	perfPerModule  = 100
	perfFileCount  = perfModules * perfPerModule
	perfSampleSize = 20
)

// TestPerf10k is the reproducible PLAN §10/M5 budget run. It is opt-in because
// generating and indexing 10k files is not a unit test:
//
//	make perf
func TestPerf10k(t *testing.T) {
	if os.Getenv("REPATH_PERF") == "" {
		t.Skip("set REPATH_PERF=1 (or run make perf) to exercise the 10k budget")
	}

	ctx := context.Background()
	root := t.TempDir()
	generateSynthetic(t, root, perfModules, perfPerModule)

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

	start := time.Now()
	cold, err := index.Run(ctx, st, root, index.Options{})
	coldDuration := time.Since(start)
	if err != nil {
		st.Close()
		t.Fatalf("cold index: %v", err)
	}
	if cold.Files != perfFileCount {
		st.Close()
		t.Fatalf("cold files = %d, want %d", cold.Files, perfFileCount)
	}
	t.Logf("cold index: %d files, %d nodes, %d edges in %s", cold.Files, cold.Nodes, cold.Edges, coldDuration)

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
	for i := 0; i < perfSampleSize; i++ {
		module := i % perfModules
		target := fmt.Sprintf("f_%03d_%03d", module, 40+i%20)

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

		from := fmt.Sprintf("f_%03d_%03d", module, 49)
		to := fmt.Sprintf("f_%03d_%03d", module, 41)
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
	mcpRequest := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"impact","arguments":{"symbol":"f_000_040","depth":8}}}` + "\n"
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

	// README quickstart: build is measured separately; this is cold index plus
	// the first answer's graph work.
	t.Logf("cold index + first query: %s (build excluded)", coldDuration+impactP50)

	if coldDuration > 60*time.Second {
		t.Errorf("cold 10k index = %s, budget <60s", coldDuration)
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
