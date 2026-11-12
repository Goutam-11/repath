package check

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Goutam-11/repath/internal/index"
	"github.com/Goutam-11/repath/internal/store"
)

func copyDir(t *testing.T, src, dst string) {
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
		t.Fatalf("copy dir: %v", err)
	}
}

// openSARIFFixture indexes one testdata/fixtures/sarif/<name> tree.
func openSARIFFixture(t *testing.T, name string) (string, *store.Store) {
	t.Helper()
	root := t.TempDir()
	copyDir(t, filepath.Join("..", "..", "testdata", "fixtures", "sarif", name), root)

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(root, ".repath", "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := index.Run(ctx, st, root, index.Options{}); err != nil {
		t.Fatal(err)
	}
	return root, st
}

func TestLoadSARIFFixture(t *testing.T) {
	root := filepath.Join("..", "..", "testdata", "fixtures", "sarif", "ts")
	load, err := LoadSARIF(root, []string{filepath.Join(root, "tsc.sarif")})
	if err != nil {
		t.Fatal(err)
	}
	if load.Summary.Sources != 1 || load.Summary.Results != 5 || load.Summary.Duplicates != 1 {
		t.Fatalf("summary = %+v, want 1 source, 5 results, 1 duplicate", load.Summary)
	}
	if len(load.Diagnostics) != 4 {
		t.Fatalf("diagnostics = %d, want 4 after dedupe: %+v", len(load.Diagnostics), load.Diagnostics)
	}

	byLine := map[int]Diagnostic{}
	for _, diagnostic := range load.Diagnostics {
		byLine[diagnostic.Line] = diagnostic
	}
	errorDiag := byLine[5]
	if errorDiag.Tool != "tsc" || errorDiag.RuleID != "TS2322" ||
		errorDiag.File != "src/cart.ts" || errorDiag.Severity != SeverityError {
		t.Fatalf("error diagnostic = %+v", errorDiag)
	}
	if byLine[10].Severity != SeverityWarning {
		t.Fatalf("warning diagnostic = %+v", byLine[10])
	}
	if byLine[6].Severity != SeverityInfo || byLine[1].Severity != SeverityInfo {
		t.Fatalf("note mapping = %+v / %+v", byLine[6], byLine[1])
	}
}

func TestLoadSARIFDirectoryFindsEveryLog(t *testing.T) {
	root := filepath.Join("..", "..", "testdata", "fixtures", "sarif")
	load, err := LoadSARIF(root, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	if load.Summary.Sources != 2 || load.Summary.Results != 8 || load.Summary.Duplicates != 1 {
		t.Fatalf("summary = %+v, want 2 sources, 8 results, 1 duplicate", load.Summary)
	}
	if len(load.Diagnostics) != 7 {
		t.Fatalf("diagnostics = %d, want 7", len(load.Diagnostics))
	}
}

func TestLoadSARIFRejectsVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.sarif")
	if err := os.WriteFile(path, []byte(`{"version":"2.1.1","runs":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadSARIF(t.TempDir(), []string{path})
	if !errors.Is(err, ErrSARIFInvalid) {
		t.Fatalf("error = %v, want ErrSARIFInvalid", err)
	}
	if !strings.Contains(err.Error(), "only 2.1.0") {
		t.Fatalf("error = %q, want a one-line 2.1.0 hint", err)
	}
}

func TestLoadSARIFMissingAndInvalid(t *testing.T) {
	if _, err := LoadSARIF(t.TempDir(), []string{filepath.Join(t.TempDir(), "missing.sarif")}); !errors.Is(err, ErrSARIFNotFound) {
		t.Fatalf("missing file error = %v, want ErrSARIFNotFound", err)
	}

	empty := t.TempDir()
	if _, err := LoadSARIF(empty, []string{empty}); !errors.Is(err, ErrSARIFNotFound) {
		t.Fatalf("empty directory error = %v, want ErrSARIFNotFound", err)
	}

	bad := filepath.Join(t.TempDir(), "bad.sarif")
	if err := os.WriteFile(bad, []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSARIF(t.TempDir(), []string{bad}); !errors.Is(err, ErrSARIFInvalid) {
		t.Fatalf("bad json error = %v, want ErrSARIFInvalid", err)
	}
}

func TestRunAttachesSARIFDiagnostics(t *testing.T) {
	root, st := openSARIFFixture(t, "ts")
	ctx := context.Background()
	load, err := LoadSARIF(root, []string{filepath.Join(root, "tsc.sarif")})
	if err != nil {
		t.Fatal(err)
	}
	report, err := Run(ctx, st, root, Options{MinConfidence: 0.5, SARIF: &load})
	if err != nil {
		t.Fatal(err)
	}
	if report.Diagnostics == nil {
		t.Fatal("report.Diagnostics is nil")
	}
	summary := *report.Diagnostics
	if summary.Attached != 3 || summary.Skipped != 1 || summary.Duplicates != 1 {
		t.Fatalf("summary = %+v, want 3 attached, 1 skipped, 1 duplicate", summary)
	}
	if summary.Results != summary.Attached+summary.Skipped+summary.Duplicates {
		t.Fatalf("summary does not add up: %+v", summary)
	}

	got := map[string]Finding{}
	for _, finding := range find(report, "diagnostic", "") {
		got[finding.QName+"\x00"+finding.Rule] = finding
	}
	if len(got) != 3 {
		t.Fatalf("diagnostic findings = %d, want 3: %+v", len(got), report.Findings)
	}
	errorFinding, ok := got["Cart.total\x00sarif.tsc.TS2322"]
	if !ok {
		t.Fatalf("missing Cart.total TS2322 diagnostic: %+v", got)
	}
	if errorFinding.Severity != SeverityError || errorFinding.File != "src/cart.ts" || errorFinding.Line != 5 {
		t.Fatalf("error finding = %+v", errorFinding)
	}
	if _, ok := got["run\x00sarif.tsc.TS2532"]; !ok {
		t.Fatalf("missing run TS2532 diagnostic: %+v", got)
	}
	if _, ok := got["Cart.total\x00sarif.tsc.TS6133"]; !ok {
		t.Fatalf("missing Cart.total TS6133 diagnostic: %+v", got)
	}

	if report.Counts.Error != 1 || report.Counts.Warning != 1 || report.Counts.Info != 1 {
		t.Fatalf("counts = %+v, want 1/1/1", report.Counts)
	}
	if ExitCode(report, "error") != ExitFindings {
		t.Fatal("an error diagnostic must fail --fail-on error")
	}
	if ExitCode(report, "none") != 0 {
		t.Fatal("--fail-on none must always pass")
	}
}

func TestRunAttachesPythonSARIF(t *testing.T) {
	root, st := openSARIFFixture(t, "py")
	ctx := context.Background()
	load, err := LoadSARIF(root, []string{filepath.Join(root, "ruff.sarif")})
	if err != nil {
		t.Fatal(err)
	}
	report, err := Run(ctx, st, root, Options{MinConfidence: 0.5, SARIF: &load})
	if err != nil {
		t.Fatal(err)
	}
	if report.Diagnostics == nil || report.Diagnostics.Attached != 2 || report.Diagnostics.Skipped != 1 {
		t.Fatalf("summary = %+v, want 2 attached and 1 skipped", report.Diagnostics)
	}
	found := map[string]string{}
	for _, finding := range find(report, "diagnostic", "") {
		found[finding.QName] = finding.Rule
	}
	if found["Service.total"] != "sarif.ruff.F821" {
		t.Fatalf("Service.total rule = %q, want sarif.ruff.F821 (%+v)", found["Service.total"], found)
	}
	// The second Python result has no ruleId; it must resolve through rule.index.
	if found["run"] != "sarif.ruff.E501" {
		t.Fatalf("run rule = %q, want sarif.ruff.E501 (%+v)", found["run"], found)
	}
}

func TestSARIFSeverityMapping(t *testing.T) {
	cases := map[string]string{
		"error":   SeverityError,
		"warning": SeverityWarning,
		"note":    SeverityInfo,
		"none":    SeverityInfo,
		"":        SeverityWarning,
		"bogus":   SeverityWarning,
	}
	for level, want := range cases {
		if got := sarifSeverity(level); got != want {
			t.Errorf("sarifSeverity(%q) = %q, want %q", level, got, want)
		}
	}
}

func TestNormalizeSARIFPath(t *testing.T) {
	root := "/repo"
	cases := map[string]string{
		"src/cart.ts":              "src/cart.ts",
		"./src/cart.ts":            "src/cart.ts",
		"src/a%20b.ts":             "src/a b.ts",
		"file:///repo/src/cart.ts": "src/cart.ts",
		"/repo/src/cart.ts":        "src/cart.ts",
		"../outside/cart.ts":       "../outside/cart.ts",
		"/somewhere/else/cart.ts":  "/somewhere/else/cart.ts",
	}
	for uri, want := range cases {
		if got := normalizeSARIFPath(root, uri); got != want {
			t.Errorf("normalizeSARIFPath(%q) = %q, want %q", uri, got, want)
		}
	}
}
