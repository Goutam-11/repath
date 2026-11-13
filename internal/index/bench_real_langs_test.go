package index_test

import (
	"os"
	"path/filepath"
	"testing"
)

// languageBench pins one corpus per language for the issue #48 evidence run.
// The producer, the commit, and the setup commands are recorded in
// docs/benchmarks.md; nothing here downloads or generates anything.
type languageBench struct {
	name        string
	rootEnv     string
	commitEnv   string
	scipEnv     string
	comment     string // line-comment prefix for the incremental touch
	adapterName string // expected stats.adapter
	rulePrefix  string // adapter rules this language produces
}

var languageBenchSpecs = map[string]languageBench{
	"python": {
		name:        "python",
		rootEnv:     "REPATH_BENCH_PY_ROOT",
		commitEnv:   "REPATH_BENCH_PY_COMMIT",
		scipEnv:     "REPATH_BENCH_PY_SCIP",
		comment:     "# ",
		adapterName: "scip-python",
		rulePrefix:  "scip.py.",
	},
	"rust": {
		name:        "rust",
		rootEnv:     "REPATH_BENCH_RS_ROOT",
		commitEnv:   "REPATH_BENCH_RS_COMMIT",
		scipEnv:     "REPATH_BENCH_RS_SCIP",
		comment:     "// ",
		adapterName: "scip-rust",
		rulePrefix:  "scip.rs.",
	},
}

// TestBenchRealLanguages is the opt-in Python/Rust real-repository evidence
// from issue #48. It runs the same cold/incremental/query/map/check/artifact
// matrix as TestBenchReal, then optionally applies a pinned SCIP index and
// reports the before/after unresolved count plus a production/test split of
// the new edges. Each corpus must run in its own process so the Linux VmHWM
// reading belongs to that tree; `make bench-real-langs` does that.
//
//	REPATH_BENCH_LANGS=1 REPATH_BENCH_LANG=rust \
//	REPATH_BENCH_RS_ROOT=/path/to/clone REPATH_BENCH_RS_COMMIT=<sha> \
//	REPATH_BENCH_RS_SCIP=/path/to/rust.scip make bench-real-langs
//
// Like TestBenchReal it removes the clone's `.repath/` and briefly touches ten
// source files (restored from memory). It is evidence, never a CI gate.
func TestBenchRealLanguages(t *testing.T) {
	if os.Getenv("REPATH_BENCH_LANGS") != "1" {
		t.Skip("set REPATH_BENCH_LANGS=1 and REPATH_BENCH_LANG (or run make bench-real-langs)")
	}
	lang := os.Getenv("REPATH_BENCH_LANG")
	spec, ok := languageBenchSpecs[lang]
	if !ok {
		t.Fatalf("REPATH_BENCH_LANG = %q, want python or rust", lang)
	}
	root := os.Getenv(spec.rootEnv)
	if root == "" {
		t.Skipf("%s corpus not configured (%s is empty; see docs/benchmarks.md)", spec.name, spec.rootEnv)
	}
	root, err := filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	scipPath := ""
	if path := os.Getenv(spec.scipEnv); path != "" {
		scipPath, err = filepath.Abs(path)
		if err != nil {
			t.Fatal(err)
		}
	} else {
		t.Logf("%s adapter not configured (%s is empty); measuring tree-sitter only", spec.name, spec.scipEnv)
	}
	runRealBench(t, root, os.Getenv(spec.commitEnv), realBenchOptions{
		label:       spec.name + " ",
		comment:     spec.comment,
		scipPath:    scipPath,
		adapterName: spec.adapterName,
		rulePrefix:  spec.rulePrefix,
	})
}
