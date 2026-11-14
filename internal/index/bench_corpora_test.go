package index_test

// Machine-readable output for the opt-in real-repository evidence and the
// hermetic failure-mode checks added by issue #61.
//
// `runRealBench` accumulates one benchRun per process and, when
// REPATH_BENCH_JSON names a path, writes it as JSON. scripts/bench-corpora.sh
// runs every corpus N times in separate processes, reads those files, and
// aggregates medians/ranges into <outdir>/summary.json. Nothing here is a CI
// gate or runs by default: the corpus tests skip without their env flags.

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Goutam-11/repath/internal/cli"
	"github.com/Goutam-11/repath/internal/query"
	"github.com/Goutam-11/repath/internal/store"
)

// failureCase is one hermetic failure-mode observation, recorded so the shell
// driver can aggregate the exit codes as evidence.
type failureCase struct {
	Name            string `json:"name"`
	ExitCode        int    `json:"exit_code"`
	ExpectedExit    int    `json:"expected_exit"`
	MatchesExpected bool   `json:"matches_expected"`
	Stderr          string `json:"stderr,omitempty"`
}

type failureReport struct {
	Schema int           `json:"schema"`
	Cases  []failureCase `json:"cases"`
}

// benchRunSchema is bumped when a field changes meaning. scripts/bench-corpora.sh
// and bench/baseline.json carry the same number.
const benchRunSchema = 1

// benchRun is one corpus run in one process. Every duration is milliseconds so
// the summary and the checked-in baseline read the same units.
type benchRun struct {
	Schema     int    `json:"schema"`
	Corpus     string `json:"corpus"`
	Language   string `json:"language,omitempty"`
	Commit     string `json:"commit,omitempty"`
	Label      string `json:"label,omitempty"`
	GoVersion  string `json:"go_version"`
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at"`
	Root       string `json:"root"`

	ColdIndexMS        float64 `json:"cold_index_ms"`
	PeakRSSBytes       uint64  `json:"peak_rss_bytes"`
	IncrementalMS      float64 `json:"incremental_ms"`
	IncrementalIndexed int     `json:"incremental_indexed"`

	Queries  benchQueries  `json:"queries"`
	Check    benchCheck    `json:"check"`
	Artifact benchArtifact `json:"artifact"`

	Structure      *benchStructure `json:"structure,omitempty"`
	FinalStructure *benchStructure `json:"final_structure,omitempty"`
	Adapter        *benchAdapter   `json:"adapter,omitempty"`
}

// benchQueries keeps the p50/p95 and sample count of each query family.
type benchQueries struct {
	Impact  benchDuration `json:"impact"`
	Callers benchDuration `json:"callers"`
	Path    benchDuration `json:"path"`
	Map     benchDuration `json:"map"`
}

// benchDuration is one latency distribution in milliseconds.
type benchDuration struct {
	P50MS   float64 `json:"p50_ms"`
	P95MS   float64 `json:"p95_ms"`
	Samples int     `json:"samples"`
}

type benchCheck struct {
	DurationMS float64 `json:"duration_ms"`
	Findings   int     `json:"findings"`
}

type benchArtifact struct {
	UncompressedBytes uint64  `json:"uncompressed_bytes"`
	ExportMS          float64 `json:"export_ms"`
	PullMS            float64 `json:"pull_ms"`
	ZstdBytes         uint64  `json:"zstd_bytes"`
	ZstdExportMS      float64 `json:"zstd_export_ms"`
	ZstdPullMS        float64 `json:"zstd_pull_ms"`
	Ratio             float64 `json:"ratio"` // zstd / uncompressed
	SchemaVersion     int     `json:"schema_version"`
	FileCount         int     `json:"file_count"`
	NodeCount         int     `json:"node_count"`
	EdgeCount         int     `json:"edge_count"`
}

// benchStructure mirrors the graph counters of `status --json`, including the
// read-time unresolved-reason buckets from #60.
type benchStructure struct {
	Files                int                   `json:"files"`
	Nodes                int                   `json:"nodes"`
	NodesByKind          map[string]int        `json:"nodes_by_kind,omitempty"`
	Edges                map[string]int        `json:"edges"`
	EdgesTotal           int                   `json:"edges_total"`
	Ambiguous            int                   `json:"ambiguous"`
	AmbiguityRate        float64               `json:"ambiguity_rate"`
	Unresolved           int                   `json:"unresolved"`
	UnresolvedRate       float64               `json:"unresolved_rate"`
	UnresolvedByReason   map[string]int        `json:"unresolved_by_reason,omitempty"`
	UnresolvedByLang     map[string]int        `json:"unresolved_by_lang,omitempty"`
	UnresolvedTopNames   []store.PendingBucket `json:"unresolved_top_names,omitempty"`
	UnresolvedTopModules []store.PendingBucket `json:"unresolved_top_modules,omitempty"`
	ParseFailures        int                   `json:"parse_failures"`
	Entrypoints          int                   `json:"entrypoints"`
	EntrypointRules      map[string]int        `json:"entrypoint_rules,omitempty"`
	GitHead              string                `json:"git_head,omitempty"`
}

// benchAdapter is the SCIP pass, before and after, on the same cold graph.
type benchAdapter struct {
	Name             string  `json:"name"`
	Definitions      int     `json:"definitions"`
	References       int     `json:"references"`
	Matched          int     `json:"matched"`
	Edges            int     `json:"edges"`
	Conflicts        int     `json:"conflicts"`
	Skipped          int     `json:"skipped"`
	Unmatched        int     `json:"unmatched"`
	WallMS           float64 `json:"wall_ms"`
	UnresolvedBefore int     `json:"unresolved_before"`
	UnresolvedAfter  int     `json:"unresolved_after"`
	EdgesBefore      int     `json:"edges_before"`
	EdgesAfter       int     `json:"edges_after"`

	NewEdgesBySource benchEdgeSource `json:"new_edges_by_source"`
	NewEdgesByRule   map[string]int  `json:"new_edges_by_rule"`
}

// benchEdgeSource splits the adapter's new edges by the source symbol: non-test
// production, test path, and #[test]/pytest entrypoint.
type benchEdgeSource struct {
	Production     int `json:"production"`
	TestPath       int `json:"test_path"`
	TestEntrypoint int `json:"test_entrypoint"`
}

// newBenchRun starts one run record. The corpus/language name comes from
// REPATH_BENCH_CORPUS/REPATH_BENCH_LANGUAGE so the shared harness stays
// language-neutral; scripts/bench-corpora.sh sets both.
func newBenchRun(opts realBenchOptions, wantCommit, root string) *benchRun {
	run := &benchRun{
		Schema:    benchRunSchema,
		Corpus:    os.Getenv("REPATH_BENCH_CORPUS"),
		Language:  os.Getenv("REPATH_BENCH_LANGUAGE"),
		Commit:    wantCommit,
		Label:     strings.TrimSpace(opts.label),
		GoVersion: runtime.Version(),
		StartedAt: time.Now().UTC().Format(time.RFC3339),
		Root:      root,
	}
	if run.Corpus == "" {
		run.Corpus = run.Label
	}
	if run.Corpus == "" {
		run.Corpus = "corpus"
	}
	return run
}

func (r *benchRun) finish() {
	r.FinishedAt = time.Now().UTC().Format(time.RFC3339)
}

// write emits the run JSON when REPATH_BENCH_JSON is set. A write failure fails
// the test so the shell driver never aggregates a missing run.
func (r *benchRun) write(t *testing.T) {
	t.Helper()
	path := os.Getenv("REPATH_BENCH_JSON")
	if path == "" {
		return
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		t.Fatalf("encode bench json: %v", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write bench json %s: %v", path, err)
	}
	t.Logf("bench json: %s", path)
}

// benchStatus snapshots the counters through the same query layer the CLI uses,
// so the JSON matches `status --json` (including unresolved_by_reason) instead
// of re-deriving them.
func benchStatus(t *testing.T, ctx context.Context, st *store.Store, root string) *benchStructure {
	t.Helper()
	report, err := query.Status(ctx, st, root, false, false)
	if err != nil {
		t.Fatalf("status snapshot: %v", err)
	}
	total := 0
	for _, count := range report.Edges {
		total += count
	}
	return &benchStructure{
		Files:                report.Files,
		Nodes:                report.Nodes,
		NodesByKind:          report.NodesByKind,
		Edges:                report.Edges,
		EdgesTotal:           total,
		Ambiguous:            report.Ambiguous,
		AmbiguityRate:        report.AmbiguityRate,
		Unresolved:           report.Unresolved,
		UnresolvedRate:       report.UnresolvedRate,
		UnresolvedByReason:   report.UnresolvedByReason,
		UnresolvedByLang:     report.UnresolvedByLang,
		UnresolvedTopNames:   report.UnresolvedTopNames,
		UnresolvedTopModules: report.UnresolvedTopModules,
		ParseFailures:        report.ParseFailures,
		Entrypoints:          report.Entrypoints,
		EntrypointRules:      report.EntrypointRules,
		GitHead:              report.GitHead,
	}
}

func durationMS(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000
}

// TestCorporaFailureModes is the hermetic failure-mode check from issue #61.
// It calls the CLI in-process, needs no producer, clone, or network, and is
// safe to run under `make check`. The exit codes it asserts are documented in
// docs/benchmarks.md; the corrupt-SCIP row records observed exit 1 rather than
// the issue's suggested 2 because no contract defines malformed adapter input.
func TestCorporaFailureModes(t *testing.T) {
	var cases []failureCase
	record := func(name string, code, expected int, stderr string) {
		cases = append(cases, failureCase{
			Name: name, ExitCode: code, ExpectedExit: expected,
			MatchesExpected: code == expected, Stderr: firstLine(stderr),
		})
	}
	t.Cleanup(func() {
		path := os.Getenv("REPATH_FAILURE_JSON")
		if path == "" {
			return
		}
		report := failureReport{Schema: 1, Cases: cases}
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			t.Errorf("encode failure json: %v", err)
			return
		}
		if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
			t.Errorf("write failure json: %v", err)
		}
	})
	run := func(t *testing.T, args ...string) (int, string, string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		code := cli.Run(args, &stdout, &stderr)
		return code, stdout.String(), stderr.String()
	}

	// writeSource gives every root one indexed file so a successful refresh is
	// observable. Go is not an indexed language; use Python.
	writeSource := func(t *testing.T, root string) {
		t.Helper()
		source := "def target():\n    return 1\n\n\ndef caller():\n    return target()\n"
		if err := os.WriteFile(filepath.Join(root, "sample.py"), []byte(source), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// assertIndexReadable proves a failed producer call did not leave a
	// half-written index: `status --no-refresh` opens the DB and reads counters.
	assertIndexReadable := func(t *testing.T, root string) {
		t.Helper()
		code, stdout, stderr := run(t, "status", "--root", root, "--json", "--no-refresh")
		if code != 0 {
			t.Fatalf("status after failure = %d, want 0 (stderr %q)", code, stderr)
		}
		var payload struct {
			Indexed bool `json:"indexed"`
			Files   int  `json:"files"`
		}
		if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
			t.Fatalf("status JSON after failure: %v\n%s", err, stdout)
		}
		if !payload.Indexed || payload.Files < 1 {
			t.Fatalf("status after failure reports indexed=%v files=%d, want a readable non-empty index",
				payload.Indexed, payload.Files)
		}
	}

	t.Run("missing-scip", func(t *testing.T) {
		root := t.TempDir()
		writeSource(t, root)
		missing := filepath.Join(t.TempDir(), "absent.scip")
		code, _, stderr := run(t, "adapter", "--root", root, "--scip", missing)
		t.Logf("adapter --scip <missing> exit=%d (want 3); stderr=%s", code, firstLine(stderr))
		record("missing-scip", code, 3, stderr)
		if code != 3 {
			t.Fatalf("missing scip exit = %d, want 3", code)
		}
		if !strings.Contains(stderr, "not found") {
			t.Fatalf("missing scip stderr = %q, want a not-found message", stderr)
		}
		assertIndexReadable(t, root)
	})

	t.Run("corrupt-scip", func(t *testing.T) {
		root := t.TempDir()
		writeSource(t, root)
		bad := filepath.Join(t.TempDir(), "bad.scip")
		if err := os.WriteFile(bad, []byte("this is not a scip protobuf\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		code, _, stderr := run(t, "adapter", "--root", root, "--scip", bad)
		t.Logf("adapter --scip <garbage> exit=%d (observed; docs record 1, issue suggested 2); stderr=%s",
			code, firstLine(stderr))
		record("corrupt-scip", code, 1, stderr)
		if code != 1 {
			t.Fatalf("corrupt scip exit = %d, want the observed/documented 1", code)
		}
		if !strings.Contains(stderr, "parse scip") {
			t.Fatalf("corrupt scip stderr = %q, want a parse error", stderr)
		}
		assertIndexReadable(t, root)
	})

	t.Run("empty-scip", func(t *testing.T) {
		root := t.TempDir()
		writeSource(t, root)
		empty := filepath.Join(t.TempDir(), "empty.scip")
		if err := os.WriteFile(empty, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		code, stdout, stderr := run(t, "adapter", "--root", root, "--scip", empty)
		t.Logf("adapter --scip <empty> exit=%d (a zero-document index is not corrupt); stdout=%s stderr=%s",
			code, firstLine(stdout), firstLine(stderr))
		record("empty-scip", code, 0, stderr)
		if code != 0 {
			t.Fatalf("empty scip exit = %d, want 0 (zero documents, zero facts)", code)
		}
		assertIndexReadable(t, root)
	})

	t.Run("non-git-index", func(t *testing.T) {
		root := t.TempDir()
		writeSource(t, root)
		code, stdout, stderr := run(t, "index", "--root", root)
		t.Logf("index in a non-git directory exit=%d (supported fallback); stdout=%s stderr=%s",
			code, firstLine(stdout), firstLine(stderr))
		record("non-git-index", code, 0, stderr)
		if code != 0 {
			t.Fatalf("non-git index exit = %d, want 0 (full discovery fallback)", code)
		}
		assertIndexReadable(t, root)
	})

	t.Run("non-git-tests-diff", func(t *testing.T) {
		root := t.TempDir()
		code, _, stderr := run(t, "tests", "--root", root, "--diff")
		t.Logf("tests --diff in a non-git directory exit=%d (want 3); stderr=%s", code, firstLine(stderr))
		record("non-git-tests-diff", code, 3, stderr)
		if code != 3 {
			t.Fatalf("non-git tests --diff exit = %d, want 3", code)
		}
		if !strings.Contains(stderr, "not a git work tree") {
			t.Fatalf("non-git tests --diff stderr = %q, want a not-a-git-work-tree message", stderr)
		}
	})

	t.Run("read-only-repath", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("running as root: a 0555 directory is still writable")
		}
		root := t.TempDir()
		if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		meta := filepath.Join(root, ".repath")
		if err := os.MkdirAll(meta, 0o555); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(meta, 0o755) })
		code, _, stderr := run(t, "index", "--root", root)
		t.Logf("index with read-only .repath/ exit=%d (want 1); stderr=%s", code, firstLine(stderr))
		record("read-only-repath", code, 1, stderr)
		if code != 1 {
			t.Fatalf("read-only .repath index exit = %d, want 1", code)
		}
		if _, err := os.Stat(filepath.Join(meta, "index.db")); !os.IsNotExist(err) {
			t.Fatalf("read-only .repath/ left an index.db (stat err=%v); want no half-written index", err)
		}
	})
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
