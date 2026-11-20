package adapter

// Compiler ground-truth accuracy for `callers` and `impact` (issue #61).
//
// The adapter's own merge policy is narrow: a SCIP reference only becomes an
// edge when tree-sitter left a matching pending row. This test asks a different
// question — how accurate is the graph itself, before and after the adapter?
//
// Ground truth is the compiler's reference set:
//
//   - definition occurrences give symbol string -> repath node (the same
//     EnclosingNode mapping the adapter uses);
//   - a reference occurrence is attributed to the repath node enclosing its
//     file+line (again the adapter's EnclosingNode, line+1);
//   - expected callers of a target are the enclosing nodes of its references.
//
// It is deliberately opt-in and never a CI gate:
//
//	REPATH_ACCURACY=1 REPATH_ACCURACY_LANG=python \
//	REPATH_ACCURACY_ROOT=/path/to/requests \
//	REPATH_ACCURACY_SCIP=/path/to/requests.scip \
//	go test ./internal/adapter -run '^TestSCIPAccuracy$' -v -count=1
//
// The test reports precision/recall for a deterministic sample of >=60 symbols
// with at least one attributable caller, tree-sitter-only and after
// adapter.ApplySCIPAll on the same store, plus a disagreement list. Facts that
// the graph model cannot attribute are excluded and counted, not silently
// dropped: imports, SCIP-generated occurrences, module/local symbols, external
// definitions, references with no enclosing symbol (repath's documented
// "top-level references are skipped"), recursive references (the adapter skips
// src == target), and same-file+line+name keys that resolve to two different
// targets. Every reference with an enclosing symbol is attributable, including
// a class-body reference and a class-base reference; an earlier revision
// excluded all class-enclosing references and therefore counted correct
// `EXTENDS` edges as false positives (#77). `impact` depth 2 is compared only through Blast (the walker behind
// query.Impact; importing query here would cycle back into this package), at
// the same confidence floor, with truncated results excluded and counted.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Goutam-11/repath/internal/graph"
	"github.com/Goutam-11/repath/internal/index"
	"github.com/Goutam-11/repath/internal/store"
)

const accuracySchema = 1

func TestAccuracyCalibrationBuckets(t *testing.T) {
	counter := newAccuracyCounter()
	counter.add("ts.local", 0.85, true)
	counter.add("ts.local", 0.85, false)
	counter.add("ts.import.named", 1.0, true)
	counter.add("ts.global.ambiguous", 0.40, false)

	cal := counter.result()
	if len(cal.Rules) != 3 {
		t.Fatalf("rules = %+v, want three", cal.Rules)
	}
	wantRules := []struct {
		key       string
		tp, fp    int
		precision float64
	}{
		{"ts.global.ambiguous", 0, 1, 0},
		{"ts.import.named", 1, 0, 1},
		{"ts.local", 1, 1, 0.5},
	}
	for i, want := range wantRules {
		got := cal.Rules[i]
		if got.Key != want.key || got.TruePositives != want.tp || got.FalsePositives != want.fp || got.Precision != want.precision {
			t.Fatalf("rule cell %d = %+v, want %+v", i, got, want)
		}
	}

	wantBuckets := []struct {
		key       string
		tp, fp    int
		precision float64
	}{
		{"0.4", 0, 1, 0},
		{"0.6-0.8", 1, 1, 0.5},
		{"0.9+", 1, 0, 1},
	}
	if len(cal.Buckets) != len(wantBuckets) {
		t.Fatalf("buckets = %+v, want %d", cal.Buckets, len(wantBuckets))
	}
	for i, want := range wantBuckets {
		got := cal.Buckets[i]
		if got.Key != want.key || got.TruePositives != want.tp || got.FalsePositives != want.fp || got.Precision != want.precision {
			t.Fatalf("bucket cell %d = %+v, want %+v", i, got, want)
		}
	}
}

func TestAccuracyBucketBoundaries(t *testing.T) {
	cases := map[float64]string{
		0.40: "0.4", 0.49: "0.4",
		0.50: "0.5", 0.59: "0.5",
		0.60: "0.6-0.8", 0.80: "0.6-0.8", 0.899: "0.6-0.8",
		0.90: "0.9+", 0.99: "0.9+", 1.0: "0.9+",
	}
	for confidence, want := range cases {
		if got := accuracyBucket(confidence); got != want {
			t.Errorf("bucket(%.3f) = %q, want %q", confidence, got, want)
		}
	}
}

func TestSampleEvenlyDeterministic(t *testing.T) {
	gt := &groundTruth{
		nodes:   map[int64]store.NodeRow{},
		files:   map[int64]string{1: "app/a.py"},
		symbols: map[int64]string{},
	}
	for id := int64(1); id <= 5; id++ {
		gt.nodes[id] = store.NodeRow{ID: id, FileID: 1}
		gt.symbols[id] = fmt.Sprintf("sym%d", id)
	}
	first := sampleEvenly([]int64{5, 3, 1, 2, 4}, gt, 3)
	second := sampleEvenly([]int64{4, 2, 5, 1, 3}, gt, 3)
	if len(first) != 3 {
		t.Fatalf("sampled %d targets, want 3", len(first))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("sample order not deterministic: %v vs %v", first, second)
		}
	}
	if all := sampleEvenly([]int64{3, 1, 2}, gt, 99); len(all) != 3 {
		t.Fatalf("want=99 returned %d targets, want all 3", len(all))
	}
}

// TestMeasureRuleEdgesClassifiesAndExcludes pins the exhaustive rung count: a
// compiler-attributed caller is TP, an emitted edge the compiler does not
// attribute is FP, and an edge whose caller file is outside the SCIP document
// set is counted as out-of-scope instead of guessed either way.
func TestMeasureRuleEdgesClassifiesAndExcludes(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}

	tx, err := st.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	prodID, err := tx.UpsertFile(ctx, store.File{Path: "app/a.py", Lang: "python", Size: 1, MTime: 1, Hash: "a", ParseOK: true})
	if err != nil {
		t.Fatal(err)
	}
	testID, err := tx.UpsertFile(ctx, store.File{Path: "tests/test_a.py", Lang: "python", Size: 1, MTime: 1, Hash: "b", ParseOK: true})
	if err != nil {
		t.Fatal(err)
	}
	targetID, err := tx.UpsertFile(ctx, store.File{Path: "app/b.py", Lang: "python", Size: 1, MTime: 1, Hash: "c", ParseOK: true})
	if err != nil {
		t.Fatal(err)
	}
	// The false positive has no import binding; the true positive does, which is
	// the provenance gate #97 adopted.
	fpID, err := tx.UpsertFile(ctx, store.File{Path: "app/c.py", Lang: "python", Size: 1, MTime: 1, Hash: "d", ParseOK: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ReplaceNodes(ctx, prodID, "python", []store.Node{
		{Kind: "function", Name: "a1", QName: "a1", StartLine: 1, EndLine: 2},
		{Kind: "function", Name: "a4", QName: "a4", StartLine: 7, EndLine: 8},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ReplaceNodes(ctx, fpID, "python", []store.Node{
		{Kind: "function", Name: "a2", QName: "a2", StartLine: 4, EndLine: 5},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ReplaceNodes(ctx, testID, "python", []store.Node{
		{Kind: "function", Name: "a3", QName: "a3", StartLine: 1, EndLine: 2},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ReplaceNodes(ctx, targetID, "python", []store.Node{
		{Kind: "function", Name: "b1", QName: "b1", StartLine: 1, EndLine: 2},
	}); err != nil {
		t.Fatal(err)
	}
	if err := tx.ReplaceImports(ctx, prodID, []store.ImportRow{
		{FileID: prodID, Local: "b1", Imported: "b1", Source: "app.b"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	prodIDs, err := st.NodeIDsByFile(ctx, prodID)
	if err != nil {
		t.Fatal(err)
	}
	fpIDs, err := st.NodeIDsByFile(ctx, fpID)
	if err != nil {
		t.Fatal(err)
	}
	testIDs, err := st.NodeIDsByFile(ctx, testID)
	if err != nil {
		t.Fatal(err)
	}
	targetIDs, err := st.NodeIDsByFile(ctx, targetID)
	if err != nil {
		t.Fatal(err)
	}
	const rule = "py.global.unique"
	edges := []store.Edge{
		{Src: prodIDs["a1"], Dst: targetIDs["b1"], Kind: "CALLS", Confidence: 0.6, Rule: rule, Line: 2},
		{Src: fpIDs["a2"], Dst: targetIDs["b1"], Kind: "CALLS", Confidence: 0.6, Rule: rule, Line: 5},
		{Src: testIDs["a3"], Dst: targetIDs["b1"], Kind: "CALLS", Confidence: 0.6, Rule: rule, Line: 2},
		{Src: prodIDs["a4"], Dst: targetIDs["b1"], Kind: "CALLS", Confidence: 1.0, Rule: "py.import.named", Line: 7},
	}
	if _, err := st.InsertEdges(ctx, edges); err != nil {
		t.Fatal(err)
	}

	gt := &groundTruth{
		callers: map[int64]map[int64]bool{targetIDs["b1"]: {prodIDs["a1"]: true}},
		files:   map[int64]string{prodID: "app/a.py", fpID: "app/c.py", testID: "tests/test_a.py", targetID: "app/b.py"},
		covered: map[int64]bool{prodID: true, fpID: true, targetID: true},
		nodes:   map[int64]store.NodeRow{},
		symbols: map[int64]string{},
	}
	cal := measureRuleEdges(t, ctx, st, gt, rule, 0.5)
	if cal.Edges != 3 || cal.InScope != 2 || cal.OutOfScope != 1 {
		t.Fatalf("edges=%d in-scope=%d out-of-scope=%d, want 3/2/1", cal.Edges, cal.InScope, cal.OutOfScope)
	}
	if cal.TruePositives != 1 || cal.FalsePositives != 1 || cal.Precision != 0.5 {
		t.Fatalf("TP=%d FP=%d precision=%v, want 1/1/0.5", cal.TruePositives, cal.FalsePositives, cal.Precision)
	}
	if cal.SameDirTruePositives != 1 || cal.SameDirFalsePositives != 1 {
		t.Fatalf("same-dir TP=%d FP=%d, want 1/1", cal.SameDirTruePositives, cal.SameDirFalsePositives)
	}
	if cal.ImportTruePositives != 1 || cal.ImportFalsePositives != 0 {
		t.Fatalf("import TP=%d FP=%d, want 1/0", cal.ImportTruePositives, cal.ImportFalsePositives)
	}
	if cal.NoImportTruePositives != 0 || cal.NoImportFalsePositives != 1 {
		t.Fatalf("no-import TP=%d FP=%d, want 0/1", cal.NoImportTruePositives, cal.NoImportFalsePositives)
	}
	if len(cal.FalsePositiveExamples) != 1 || !strings.Contains(cal.FalsePositiveExamples[0], "a2") {
		t.Fatalf("false-positive examples = %v, want the a2 edge", cal.FalsePositiveExamples)
	}
}

func TestSCIPAccuracy(t *testing.T) {
	if os.Getenv("REPATH_ACCURACY") != "1" {
		t.Skip("set REPATH_ACCURACY=1 with REPATH_ACCURACY_LANG/ROOT/SCIP (issue #61 evidence)")
	}
	lang := os.Getenv("REPATH_ACCURACY_LANG")
	var family scipFamily
	switch lang {
	case "python":
		family = pySCIPFamily
	case "rust":
		family = rsSCIPFamily
	default:
		t.Fatalf("REPATH_ACCURACY_LANG = %q, want python or rust", lang)
	}
	root := os.Getenv("REPATH_ACCURACY_ROOT")
	scipPath := os.Getenv("REPATH_ACCURACY_SCIP")
	if root == "" || scipPath == "" {
		t.Fatalf("REPATH_ACCURACY_ROOT and REPATH_ACCURACY_SCIP are required")
	}
	root, err := filepath.Abs(root)
	if err != nil {
		t.Fatal(err)
	}
	scipPath, err = filepath.Abs(scipPath)
	if err != nil {
		t.Fatal(err)
	}
	if want := os.Getenv("REPATH_ACCURACY_COMMIT"); want != "" {
		head, err := gitOutput(root, "rev-parse", "HEAD")
		if err != nil {
			t.Fatalf("git rev-parse: %v", err)
		}
		if head != want {
			t.Fatalf("accuracy root HEAD = %s, want pinned %s", head, want)
		}
	}
	// REPATH_ACCURACY_RULE narrows the cohort to the ground-truth targets that
	// receive an edge of one rule. #97 uses it to measure a single confidence
	// rung over its whole population instead of the uniform 60-target sample.
	// A rung run defaults to every candidate target; the uniform run keeps 60.
	rule := os.Getenv("REPATH_ACCURACY_RULE")
	samplesWant, _ := strconv.Atoi(os.Getenv("REPATH_ACCURACY_SAMPLES"))
	if samplesWant <= 0 {
		if rule != "" {
			samplesWant = 1 << 30
		} else {
			samplesWant = 60
		}
	}
	minConfidence := 0.5
	if raw := os.Getenv("REPATH_ACCURACY_MIN_CONFIDENCE"); raw != "" {
		if minConfidence, err = strconv.ParseFloat(raw, 64); err != nil {
			t.Fatalf("REPATH_ACCURACY_MIN_CONFIDENCE: %v", err)
		}
	}
	corpus := os.Getenv("REPATH_ACCURACY_CORPUS")
	if corpus == "" {
		corpus = lang
	}

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	indexed, err := index.Run(ctx, st, root, index.Options{})
	if err != nil {
		t.Fatalf("cold index: %v", err)
	}
	scipIndex, err := LoadSCIP(scipPath)
	if err != nil {
		t.Fatalf("load scip: %v", err)
	}

	gt := buildGroundTruth(t, ctx, st, scipIndex, family)
	var sampled []int64
	var candidates int
	if rule != "" {
		sampled, candidates = sampleRuleTargets(t, ctx, st, gt, rule, samplesWant, minConfidence)
		if len(sampled) == 0 {
			// A corpus with no edge of this rule is a valid zero measurement,
			// not a harness failure; the exhaustive count below still reports
			// every emitted edge (including ones with no attributable target).
			t.Logf("accuracy %s: rule %q: no attributable target at floor %.2f", corpus, rule, minConfidence)
		} else {
			t.Logf("accuracy %s: rule %q cohort, %d of %d targets sampled at floor %.2f",
				corpus, rule, len(sampled), candidates, minConfidence)
		}
	} else {
		sampled, candidates = sampleTargets(gt, samplesWant)
		if len(sampled) < samplesWant {
			t.Fatalf("only %d candidate targets with an attributable caller, want >= %d", len(sampled), samplesWant)
		}
		t.Logf("accuracy %s: %d of %d candidate targets sampled deterministically (min-confidence %.2f)",
			corpus, len(sampled), candidates, minConfidence)
	}
	t.Logf("accuracy %s (%s): %d documents, refs=%d definitions=%d attributable=%d",
		corpus, lang, gt.documents, gt.references, gt.definitions, gt.attributable)
	t.Logf("accuracy %s: excluded %s", corpus, formatCounts(gt.exclusions))

	before := measureAccuracy(t, ctx, st, gt, sampled, "tree-sitter", minConfidence, nil)
	// Re-measure the tree-sitter graph at a 0.4 floor so the calibration table
	// has a populated low-confidence bucket. This runs before ApplySCIPAll,
	// which replaces ambiguous 0.40 groups and would empty the bucket after.
	calibration := measureAccuracy(t, ctx, st, gt, sampled, "tree-sitter 0.4 floor", 0.4, nil)
	// Snapshot the tree-sitter callers before the adapter runs. Ambiguous-group
	// replacement deletes 0.40 candidate rows, so filtering adapter rule ids
	// afterwards no longer reconstructs the pre-adapter graph; the snapshot is
	// the truthful "before" for the adapter-touched cohort.
	snapshot := snapshotCallers(t, ctx, st, minConfidence)
	statsList, err := ApplySCIPAll(ctx, st, scipIndex)
	if err != nil {
		t.Fatalf("apply scip: %v", err)
	}
	after := measureAccuracy(t, ctx, st, gt, sampled, "after-adapter", minConfidence, nil)

	// The uniform sample can miss every target the adapter repaired. Report
	// that cohort separately instead of biasing the sample toward it.
	touched := adapterTouchedTargets(t, ctx, st, gt, family)
	touchedBefore := measureAccuracy(t, ctx, st, gt, touched, "adapter-targets tree-sitter", minConfidence, snapshot)
	touchedAfter := measureAccuracy(t, ctx, st, gt, touched, "adapter-targets after-adapter", minConfidence, nil)

	// An exhaustive edge-level count of the selected rule: every emitted edge at
	// the floor, TP when the compiler attributes that caller to the target. It
	// has no sampling error, which is what a demotion decision needs.
	var ruleCal *ruleEdgeCalibration
	if rule != "" {
		ruleCal = measureRuleEdges(t, ctx, st, gt, rule, minConfidence)
	}

	counts, err := st.Counts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	edges, err := st.CountEdges(ctx)
	if err != nil {
		t.Fatal(err)
	}
	unresolved, err := st.CountPending(ctx)
	if err != nil {
		t.Fatal(err)
	}
	report := accuracyReport{
		Schema:           accuracySchema,
		Corpus:           corpus,
		Language:         lang,
		Commit:           os.Getenv("REPATH_ACCURACY_COMMIT"),
		Root:             root,
		SCIP:             scipPath,
		GeneratedAt:      time.Now().UTC().Format(time.RFC3339),
		References:       gt.references,
		Definitions:      gt.definitions,
		Documents:        gt.documents,
		Attributable:     gt.attributable,
		Exclusions:       gt.exclusions,
		AmbiguousKeys:    gt.ambiguousKeys,
		CandidateTargets: candidates,
		SampledTargets:   len(sampled),
		MinConfidence:    minConfidence,
		Rule:             rule,
		RuleCalibration:  ruleCal,
		Store: accuracyStore{
			Files: indexed.Files, Nodes: counts.Nodes, Edges: edges,
			Unresolved: unresolved, ParseFailures: counts.ParseFailures,
		},
		Adapters:       statsList,
		Before:         before,
		After:          after,
		Calibration04:  calibration,
		TouchedTargets: len(touched),
		TouchedBefore:  touchedBefore,
		TouchedAfter:   touchedAfter,
	}
	for _, phase := range []accuracyPhase{before, after, calibration} {
		t.Logf("accuracy %s %s: callers expected=%d actual=%d TP=%d precision=%.1f%% recall=%.1f%% (out-of-scope actual=%d)",
			corpus, phase.Phase, phase.Expected, phase.Actual, phase.TruePositives,
			phase.Precision*100, phase.Recall*100, phase.OutOfScope)
		t.Logf("accuracy %s %s: impact<=2 expected=%d actual=%d TP=%d precision=%.1f%% recall=%.1f%% (truncated samples=%d, out-of-scope=%d)",
			corpus, phase.Phase, phase.ImpactExpected, phase.ImpactActual, phase.ImpactTruePositives,
			phase.ImpactPrecision*100, phase.ImpactRecall*100, phase.ImpactTruncated, phase.ImpactOutOfScope)
	}
	logCalibration(t, corpus, "tree-sitter", before.CallersCalibration)
	logCalibration(t, corpus, "tree-sitter-impact", before.ImpactCalibration)
	logCalibration(t, corpus, "after-adapter", after.CallersCalibration)
	logCalibration(t, corpus, "after-adapter-impact", after.ImpactCalibration)
	logCalibration(t, corpus, "0.4-floor", calibration.CallersCalibration)
	logCalibration(t, corpus, "0.4-floor-impact", calibration.ImpactCalibration)
	if len(touched) > 0 {
		t.Logf("accuracy %s: %d ground-truth targets repaired by the adapter; callers before expected=%d actual=%d TP=%d precision=%.1f%% recall=%.1f%%; after expected=%d actual=%d TP=%d precision=%.1f%% recall=%.1f%% (+%d TP)",
			corpus, len(touched),
			touchedBefore.Expected, touchedBefore.Actual, touchedBefore.TruePositives,
			touchedBefore.Precision*100, touchedBefore.Recall*100,
			touchedAfter.Expected, touchedAfter.Actual, touchedAfter.TruePositives,
			touchedAfter.Precision*100, touchedAfter.Recall*100,
			touchedAfter.TruePositives-touchedBefore.TruePositives)
	}
	logRuleCalibration(t, corpus, ruleCal)
	for i, line := range before.Disagreements {
		if i >= 12 {
			break
		}
		t.Logf("accuracy %s disagreement (tree-sitter): %s", corpus, line)
	}
	if path := os.Getenv("REPATH_ACCURACY_JSON"); path != "" {
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			t.Fatalf("encode accuracy json: %v", err)
		}
		data = append(data, '\n')
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatalf("write accuracy json: %v", err)
		}
		t.Logf("accuracy json: %s", path)
	}
}

// groundTruth is the compiler reference graph restricted to facts repath can
// attribute, plus the counters for everything excluded.
type groundTruth struct {
	callers       map[int64]map[int64]bool // target -> caller node ids
	symbols       map[int64]string         // target -> representative SCIP symbol
	nodes         map[int64]store.NodeRow
	files         map[int64]string // file id -> repo-relative path
	covered       map[int64]bool   // file ids present in the SCIP document set
	exclusions    map[string]int
	references    int
	definitions   int
	documents     int
	attributable  int
	ambiguousKeys int
}

type occurrenceKey struct {
	fileID int64
	line   int
	name   string
}

type attributedOccurrence struct {
	target int64
	src    int64
	line   int
	symbol string
	path   string
}

func buildGroundTruth(t *testing.T, ctx context.Context, st *store.Store, index SCIPIndex, family scipFamily) *groundTruth {
	t.Helper()
	gt := &groundTruth{
		callers:    map[int64]map[int64]bool{},
		symbols:    map[int64]string{},
		nodes:      map[int64]store.NodeRow{},
		exclusions: map[string]int{},
	}
	nodes, err := st.Nodes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range nodes {
		gt.nodes[node.ID] = node
	}
	paths, err := st.FilePaths(ctx)
	if err != nil {
		t.Fatal(err)
	}
	gt.files = paths
	pathIDs := make(map[string]int64, len(paths))
	for id, path := range paths {
		pathIDs[path] = id
	}
	// Same exact-then-unique-suffix mapping as applySCIPFamily.
	fileIDFor := func(scipPath string) (int64, bool) {
		scipPath = strings.TrimPrefix(filepath.ToSlash(scipPath), "./")
		if id, ok := pathIDs[scipPath]; ok {
			return id, true
		}
		var matches []int64
		for path, id := range pathIDs {
			if strings.HasSuffix(path, "/"+scipPath) {
				matches = append(matches, id)
			}
		}
		if len(matches) == 1 {
			return matches[0], true
		}
		return 0, false
	}
	gt.covered = map[int64]bool{}
	for _, doc := range index.Documents {
		if !family.matches(doc) {
			continue
		}
		gt.documents++
		if id, ok := fileIDFor(doc.Path); ok {
			gt.covered[id] = true
		}
	}

	// Definitions first, exactly like the adapter: first occurrence of a
	// symbol wins and the target is the enclosing repath node.
	definitions := map[string]int64{}
	for _, doc := range index.Documents {
		if !family.matches(doc) {
			continue
		}
		fileID, ok := fileIDFor(doc.Path)
		if !ok {
			continue
		}
		for _, occ := range doc.Occurrences {
			if occ.Roles&1 == 0 {
				continue
			}
			gt.definitions++
			name := family.name(occ.Symbol)
			if name == "" {
				gt.exclusions["definition-unmapped-name"]++
				continue
			}
			target, found, err := st.EnclosingNode(ctx, fileID, occ.Line+1)
			if err != nil {
				t.Fatal(err)
			}
			if !found {
				gt.exclusions["definition-no-enclosing-node"]++
				continue
			}
			if _, exists := definitions[occ.Symbol]; !exists {
				definitions[occ.Symbol] = target
				if _, seen := gt.symbols[target]; !seen {
					gt.symbols[target] = occ.Symbol
				}
			}
		}
	}

	// References, in a second pass so order within a document does not matter.
	// Any reference with an enclosing symbol is attributable: class-body and
	// class-base uses are real edges in the graph too.
	keyTargets := map[occurrenceKey]map[int64]bool{}
	keyOccs := map[occurrenceKey][]attributedOccurrence{}
	for _, doc := range index.Documents {
		if !family.matches(doc) {
			continue
		}
		fileID, ok := fileIDFor(doc.Path)
		if !ok {
			gt.exclusions["document-not-indexed"]++
			continue
		}
		for _, occ := range doc.Occurrences {
			if occ.Roles&1 != 0 {
				continue
			}
			gt.references++
			switch {
			case occ.Roles&2 != 0: // SCIP Import role: not a caller fact
				gt.exclusions["import"]++
				continue
			case occ.Roles&16 != 0: // SCIP Generated role: macro/derived code
				gt.exclusions["generated"]++
				continue
			}
			name := family.name(occ.Symbol)
			if name == "" {
				gt.exclusions["unmapped-symbol"]++
				continue
			}
			target, ok := definitions[occ.Symbol]
			if !ok {
				gt.exclusions["no-definition"]++
				continue
			}
			src, found, err := st.EnclosingNode(ctx, fileID, occ.Line+1)
			if err != nil {
				t.Fatal(err)
			}
			if !found {
				// repath's documented policy: top-level references have no
				// source symbol and are skipped.
				gt.exclusions["no-enclosing-node"]++
				continue
			}
			if src == target {
				// The adapter skips these; a self-reference is not a caller.
				gt.exclusions["recursive"]++
				continue
			}

			key := occurrenceKey{fileID: fileID, line: occ.Line + 1, name: name}
			if keyTargets[key] == nil {
				keyTargets[key] = map[int64]bool{}
			}
			keyTargets[key][target] = true
			keyOccs[key] = append(keyOccs[key], attributedOccurrence{
				target: target, src: src, line: occ.Line + 1, symbol: occ.Symbol, path: doc.Path,
			})
		}
	}
	// A name reduced by symbolName/rustSymbolName can collide on one line: the
	// pending row is keyed by (file, line, name), so two compiler targets at
	// that key are not attributable. Exclude the whole key and count it.
	for key, targets := range keyTargets {
		if len(targets) > 1 {
			gt.exclusions["ambiguous-same-line-name"] += len(keyOccs[key])
			gt.ambiguousKeys++
			delete(keyOccs, key)
			continue
		}
		for _, occ := range keyOccs[key] {
			if gt.callers[occ.target] == nil {
				gt.callers[occ.target] = map[int64]bool{}
			}
			if !gt.callers[occ.target][occ.src] {
				gt.callers[occ.target][occ.src] = true
				gt.attributable++
			}
		}
	}
	return gt
}

// sampleTargets picks up to want targets deterministically: sort by target path
// then SCIP symbol and take evenly spaced entries, so two runs on the same
// corpus report the same symbols.
func sampleTargets(gt *groundTruth, want int) ([]int64, int) {
	var candidates []int64
	for target, callers := range gt.callers {
		if len(callers) == 0 {
			continue
		}
		if _, ok := gt.nodes[target]; !ok {
			continue
		}
		candidates = append(candidates, target)
	}
	return sampleEvenly(candidates, gt, want), len(candidates)
}

// sampleRuleTargets restricts the cohort to ground-truth targets that receive at
// least one edge of rule at the floor. It measures one confidence rung over its
// whole population, not the uniform sample (#97).
func sampleRuleTargets(t *testing.T, ctx context.Context, st *store.Store, gt *groundTruth, rule string, want int, minConfidence float64) ([]int64, int) {
	t.Helper()
	edges, err := st.GraphEdges(ctx, minConfidence)
	if err != nil {
		t.Fatalf("rule cohort edges: %v", err)
	}
	targets := map[int64]bool{}
	for _, edge := range edges {
		if edge.Rule == rule {
			targets[edge.Dst] = true
		}
	}
	var candidates []int64
	for target := range gt.callers {
		if !targets[target] {
			continue
		}
		if _, ok := gt.nodes[target]; !ok {
			continue
		}
		candidates = append(candidates, target)
	}
	return sampleEvenly(candidates, gt, want), len(candidates)
}

// sampleEvenly sorts and evenly spaces candidates so a cohort is reproducible
// from run to run and corpus to corpus.
func sampleEvenly(candidates []int64, gt *groundTruth, want int) []int64 {
	sort.Slice(candidates, func(i, j int) bool {
		a, b := gt.nodes[candidates[i]], gt.nodes[candidates[j]]
		ap, bp := gt.files[a.FileID], gt.files[b.FileID]
		if ap != bp {
			return ap < bp
		}
		if gt.symbols[candidates[i]] != gt.symbols[candidates[j]] {
			return gt.symbols[candidates[i]] < gt.symbols[candidates[j]]
		}
		return candidates[i] < candidates[j]
	})
	if want > len(candidates) {
		want = len(candidates)
	}
	out := make([]int64, 0, want)
	for i := 0; i < want && len(candidates) > 0; i++ {
		out = append(out, candidates[i*len(candidates)/want])
	}
	return out
}

// accuracyCell is one rule or confidence bucket in the calibration table. Only
// TP/FP are attributable: a false negative has no emitted edge to attribute.
type accuracyCell struct {
	Key            string  `json:"key"`
	TruePositives  int     `json:"true_positives"`
	FalsePositives int     `json:"false_positives"`
	Precision      float64 `json:"precision"`
}

// accuracyCalibration groups the same matches by rule id and by confidence
// bucket (0.4 / 0.5 / 0.6-0.8 / 0.9+). Sorted keys keep the JSON stable.
type accuracyCalibration struct {
	Rules   []accuracyCell `json:"rules,omitempty"`
	Buckets []accuracyCell `json:"buckets,omitempty"`
}

// ruleEdgeCalibration is an exhaustive, edge-level view of one rule id: every
// emitted edge at the floor is a TP when the compiler attributes that caller to
// the target, an FP otherwise. Unlike the sampled calibration it covers the
// whole population, which is what a confidence-demotion decision needs.
type ruleEdgeCalibration struct {
	Rule           string  `json:"rule"`
	Floor          float64 `json:"floor"`
	Edges          int     `json:"edges"`
	InScope        int     `json:"in_scope"`
	TruePositives  int     `json:"true_positives"`
	FalsePositives int     `json:"false_positives"`
	Precision      float64 `json:"precision"`
	OutOfScope     int     `json:"out_of_scope"`
	// SameDir counts test the co-location gate option: same directory means
	// same Python package. Option B for #97 stood or fell on these two cells.
	SameDirTruePositives  int `json:"same_dir_true_positives"`
	SameDirFalsePositives int `json:"same_dir_false_positives"`
	// Import provenance is the gate #97 actually adopted: a pending retry that
	// has a file-level import binding for the name is a star-re-export
	// recovery; one without is a bare name guess.
	ImportTruePositives    int      `json:"import_true_positives"`
	ImportFalsePositives   int      `json:"import_false_positives"`
	NoImportTruePositives  int      `json:"no_import_true_positives"`
	NoImportFalsePositives int      `json:"no_import_false_positives"`
	FalsePositiveExamples  []string `json:"false_positive_examples,omitempty"`
}

type accuracyPhase struct {
	Phase               string  `json:"phase"`
	Samples             int     `json:"samples"`
	Expected            int     `json:"expected_callers"`
	Actual              int     `json:"actual_callers"`
	TruePositives       int     `json:"true_positives"`
	Precision           float64 `json:"precision"`
	Recall              float64 `json:"recall"`
	OutOfScope          int     `json:"out_of_scope_actual"`
	ImpactExpected      int     `json:"impact_expected"`
	ImpactActual        int     `json:"impact_actual"`
	ImpactTruePositives int     `json:"impact_true_positives"`
	ImpactPrecision     float64 `json:"impact_precision"`
	ImpactRecall        float64 `json:"impact_recall"`
	ImpactOutOfScope    int     `json:"impact_out_of_scope"`
	ImpactTruncated     int     `json:"impact_truncated"`
	// Calibration is present only when the phase measured live edges (the
	// pre-adapter snapshot has no rule ids to attribute).
	CallersCalibration *accuracyCalibration `json:"callers_calibration,omitempty"`
	ImpactCalibration  *accuracyCalibration `json:"impact_calibration,omitempty"`
	Disagreements      []string             `json:"disagreements,omitempty"`
}

type accuracyReport struct {
	Schema           int            `json:"schema"`
	Corpus           string         `json:"corpus"`
	Language         string         `json:"language"`
	Commit           string         `json:"commit,omitempty"`
	Root             string         `json:"root"`
	SCIP             string         `json:"scip"`
	GeneratedAt      string         `json:"generated_at"`
	Documents        int            `json:"documents"`
	References       int            `json:"references"`
	Definitions      int            `json:"definitions"`
	Attributable     int            `json:"attributable_references"`
	Exclusions       map[string]int `json:"exclusions"`
	AmbiguousKeys    int            `json:"ambiguous_name_keys"`
	CandidateTargets int            `json:"candidate_targets"`
	SampledTargets   int            `json:"sampled_targets"`
	MinConfidence    float64        `json:"min_confidence"`
	// Rule/RuleCalibration are set by a REPATH_ACCURACY_RULE rung run: an
	// exhaustive edge-level count of that one rule over the whole graph.
	Rule            string               `json:"rule,omitempty"`
	RuleCalibration *ruleEdgeCalibration `json:"rule_calibration,omitempty"`
	Store           accuracyStore        `json:"store"`
	Adapters        []Stats              `json:"adapters,omitempty"`
	Before          accuracyPhase        `json:"before"`
	After           accuracyPhase        `json:"after"`
	// Calibration04 re-measures the tree-sitter graph at a 0.4 floor so the
	// low-confidence bucket is populated; headline totals stay at the default.
	Calibration04 accuracyPhase `json:"calibration_04"`
	// AdapterTouched is the cohort of ground-truth targets whose callers the
	// adapter actually repaired, measured with adapter edges filtered out for
	// the "before" row. Separate so the uniform sample stays unbiased.
	TouchedTargets int           `json:"adapter_touched_targets"`
	TouchedBefore  accuracyPhase `json:"adapter_touched_before"`
	TouchedAfter   accuracyPhase `json:"adapter_touched_after"`
}

type accuracyStore struct {
	Files         int `json:"files"`
	Nodes         int `json:"nodes"`
	Edges         int `json:"edges"`
	Unresolved    int `json:"unresolved"`
	ParseFailures int `json:"parse_failures"`
}

// callerRef is one incoming edge reduced to the fields accuracy compares:
// source node and its file path (for SCIP scope).
type callerRef struct {
	src        int64
	path       string
	rule       string
	confidence float64
}

// snapshotCallers reads every edge at the confidence floor before the adapter
// runs, grouped by destination. It is the pre-adapter graph as data, so a
// later adapter pass that deletes ambiguous candidate rows cannot rewrite it.
func snapshotCallers(t *testing.T, ctx context.Context, st *store.Store, minConfidence float64) map[int64][]callerRef {
	t.Helper()
	edges, err := st.GraphEdges(ctx, minConfidence)
	if err != nil {
		t.Fatalf("snapshot callers: %v", err)
	}
	out := make(map[int64][]callerRef)
	for _, edge := range edges {
		out[edge.Dst] = append(out[edge.Dst], callerRef{
			src: edge.Src, path: edge.SrcPath, rule: edge.Rule, confidence: edge.Confidence,
		})
	}
	return out
}

// measureAccuracy compares a cohort against the compiler ground truth. When
// beforeCallers is non-nil it supplies the pre-adapter caller set (the touched
// cohort measured honestly across ambiguous replacement) and the impact
// comparison is skipped because the snapshot has no closure.
func measureAccuracy(t *testing.T, ctx context.Context, st *store.Store, gt *groundTruth, sampled []int64, phase string, minConfidence float64, beforeCallers map[int64][]callerRef) accuracyPhase {
	t.Helper()
	out := accuracyPhase{Phase: phase, Samples: len(sampled)}
	callerCal := newAccuracyCounter()
	impactCal := newAccuracyCounter()
	calibrate := beforeCallers == nil // the pre-adapter snapshot has no rule ids
	coveredPaths := map[string]bool{}
	for id := range gt.covered {
		coveredPaths[gt.files[id]] = true
	}
	addDisagreement := func(line string) {
		if len(out.Disagreements) < 40 {
			out.Disagreements = append(out.Disagreements, line)
		}
	}
	for _, target := range sampled {
		targetNode := gt.nodes[target]
		expected := gt.callers[target]

		var callers []callerRef
		if beforeCallers != nil {
			callers = beforeCallers[target]
		} else {
			edges, err := st.Callers(ctx, target, minConfidence)
			if err != nil {
				t.Fatalf("callers %s: %v", targetNode.QName, err)
			}
			callers = make([]callerRef, 0, len(edges))
			for _, edge := range edges {
				callers = append(callers, callerRef{
					src: edge.SrcNode.ID, path: edge.SrcPath,
					rule: edge.Rule, confidence: edge.Confidence,
				})
			}
		}
		actual := map[int64]bool{}
		strongest := map[int64]callerRef{}
		for _, caller := range callers {
			if !coveredPaths[caller.path] {
				out.OutOfScope++
				continue
			}
			actual[caller.src] = true
			if prev, ok := strongest[caller.src]; !ok || caller.confidence > prev.confidence {
				strongest[caller.src] = caller
			}
		}
		tp := 0
		for id := range expected {
			if actual[id] {
				tp++
			}
		}
		out.Expected += len(expected)
		out.Actual += len(actual)
		out.TruePositives += tp
		if calibrate {
			for id := range actual {
				edge := strongest[id]
				callerCal.add(edge.rule, edge.confidence, expected[id])
			}
		}
		for id := range actual {
			if expected[id] {
				continue
			}
			caller := gt.nodes[id]
			addDisagreement(fmt.Sprintf("%s <- %s (%s:%d) [false-positive/%s/%s]",
				targetNode.QName, caller.QName, gt.files[caller.FileID], caller.StartLine, phase, strongest[id].rule))
		}
		for id := range expected {
			if actual[id] {
				continue
			}
			caller := gt.nodes[id]
			addDisagreement(fmt.Sprintf("%s <- %s (%s:%d) [false-negative/%s]",
				targetNode.QName, caller.QName, gt.files[caller.FileID], caller.StartLine, phase))
		}

		if beforeCallers != nil {
			continue
		}
		// impact depth 2, ground truth closure over the compiler graph.
		impactExpected := gtClosure(gt, target, 2)
		blast, err := graph.Blast(ctx, st, []store.NodeRow{targetNode}, 2, minConfidence, 2000)
		if err != nil {
			t.Fatalf("impact %s: %v", targetNode.QName, err)
		}
		if blast.Truncated {
			out.ImpactTruncated++
		} else {
			impactActual := map[int64]bool{}
			for _, dependent := range blast.Dependents {
				if !coveredPaths[dependent.File] {
					out.ImpactOutOfScope++
					continue
				}
				impactActual[dependent.ID] = true
			}
			tp := 0
			for id := range impactExpected {
				if impactActual[id] {
					tp++
				}
			}
			out.ImpactExpected += len(impactExpected)
			out.ImpactActual += len(impactActual)
			out.ImpactTruePositives += tp
			if calibrate {
				for _, dependent := range blast.Dependents {
					if !coveredPaths[dependent.File] {
						continue
					}
					impactCal.add(dependent.Rule, dependent.Confidence, impactExpected[dependent.ID])
				}
			}
		}
	}
	if calibrate {
		out.CallersCalibration = callerCal.result()
		out.ImpactCalibration = impactCal.result()
	}
	out.Precision = ratio(out.TruePositives, out.Actual)
	out.Recall = ratio(out.TruePositives, out.Expected)
	out.ImpactPrecision = ratio(out.ImpactTruePositives, out.ImpactActual)
	out.ImpactRecall = ratio(out.ImpactTruePositives, out.ImpactExpected)
	return out
}

// measureRuleEdges classifies every emitted edge of one rule at the floor. An
// edge is in scope only when both endpoints are files the compiler indexed; a
// test-file caller outside the production SCIP scope is not attributable, so it
// is counted as out-of-scope instead of guessed either way.
func measureRuleEdges(t *testing.T, ctx context.Context, st *store.Store, gt *groundTruth, rule string, floor float64) *ruleEdgeCalibration {
	t.Helper()
	edges, err := st.GraphEdges(ctx, floor)
	if err != nil {
		t.Fatalf("rule edges: %v", err)
	}
	out := &ruleEdgeCalibration{Rule: rule, Floor: floor}
	coveredPaths := map[string]bool{}
	for id := range gt.covered {
		coveredPaths[gt.files[id]] = true
	}
	// Import bindings are persisted per file; a binding for the target name is
	// the provenance that separates a star-re-export recovery from a bare guess.
	importRows, err := st.Imports(ctx)
	if err != nil {
		t.Fatalf("rule edges imports: %v", err)
	}
	bound := map[string]map[string]bool{}
	for _, imp := range importRows {
		path := gt.files[imp.FileID]
		if bound[path] == nil {
			bound[path] = map[string]bool{}
		}
		bound[path][imp.Local] = true
	}
	var examples []string
	for _, edge := range edges {
		if edge.Rule != rule {
			continue
		}
		out.Edges++
		if !coveredPaths[edge.SrcPath] || !coveredPaths[edge.DstPath] {
			out.OutOfScope++
			continue
		}
		out.InScope++
		sameDir := path.Dir(edge.SrcPath) == path.Dir(edge.DstPath)
		hasImport := bound[edge.SrcPath][edge.DstNode.Name]
		if gt.callers[edge.Dst][edge.Src] {
			out.TruePositives++
			if sameDir {
				out.SameDirTruePositives++
			}
			if hasImport {
				out.ImportTruePositives++
			} else {
				out.NoImportTruePositives++
			}
			continue
		}
		out.FalsePositives++
		if sameDir {
			out.SameDirFalsePositives++
		}
		if hasImport {
			out.ImportFalsePositives++
		} else {
			out.NoImportFalsePositives++
		}
		examples = append(examples, fmt.Sprintf("%s:%d %s -> %s (%s)",
			edge.SrcPath, edge.Line, edge.SrcNode.QName, edge.DstNode.QName, edge.DstPath))
	}
	sort.Strings(examples)
	if len(examples) > 20 {
		examples = examples[:20]
	}
	out.FalsePositiveExamples = examples
	out.Precision = ratio(out.TruePositives, out.InScope)
	return out
}

// accuracyCounter accumulates TP/FP per rule id and per confidence bucket while
// measureAccuracy walks the sample. A false negative has no emitted edge, so it
// cannot be attributed to a rule; the recall denominator stays the phase total.
type accuracyCounter struct {
	rules   map[string]*accuracyCell
	buckets map[string]*accuracyCell
}

func newAccuracyCounter() *accuracyCounter {
	return &accuracyCounter{rules: map[string]*accuracyCell{}, buckets: map[string]*accuracyCell{}}
}

func (c *accuracyCounter) add(rule string, confidence float64, expected bool) {
	if rule == "" {
		rule = "(none)"
	}
	cell, ok := c.rules[rule]
	if !ok {
		cell = &accuracyCell{Key: rule}
		c.rules[rule] = cell
	}
	bucket, ok := c.buckets[accuracyBucket(confidence)]
	if !ok {
		bucket = &accuracyCell{Key: accuracyBucket(confidence)}
		c.buckets[accuracyBucket(confidence)] = bucket
	}
	if expected {
		cell.TruePositives++
		bucket.TruePositives++
	} else {
		cell.FalsePositives++
		bucket.FalsePositives++
	}
}

func (c *accuracyCounter) result() *accuracyCalibration {
	out := &accuracyCalibration{}
	for _, cell := range c.rules {
		cell.Precision = ratio(cell.TruePositives, cell.TruePositives+cell.FalsePositives)
		out.Rules = append(out.Rules, *cell)
	}
	sort.Slice(out.Rules, func(i, j int) bool { return out.Rules[i].Key < out.Rules[j].Key })
	for _, cell := range c.buckets {
		cell.Precision = ratio(cell.TruePositives, cell.TruePositives+cell.FalsePositives)
		out.Buckets = append(out.Buckets, *cell)
	}
	order := map[string]int{"0.4": 0, "0.5": 1, "0.6-0.8": 2, "0.9+": 3}
	sort.Slice(out.Buckets, func(i, j int) bool { return order[out.Buckets[i].Key] < order[out.Buckets[j].Key] })
	return out
}

// accuracyBucket names the calibration band for one edge confidence.
func accuracyBucket(confidence float64) string {
	switch {
	case confidence < 0.5:
		return "0.4"
	case confidence < 0.6:
		return "0.5"
	case confidence < 0.9:
		return "0.6-0.8"
	default:
		return "0.9+"
	}
}

// logRuleCalibration prints one rule's exhaustive edge-level count and the
// first false positives, which is the evidence a demotion decision cites.
func logRuleCalibration(t *testing.T, corpus string, cal *ruleEdgeCalibration) {
	t.Helper()
	if cal == nil {
		return
	}
	t.Logf("accuracy %s rule %s @%.2f: edges=%d in-scope=%d TP=%d FP=%d precision=%.1f%% out-of-scope=%d same-dir TP=%d FP=%d",
		corpus, cal.Rule, cal.Floor, cal.Edges, cal.InScope, cal.TruePositives,
		cal.FalsePositives, cal.Precision*100, cal.OutOfScope,
		cal.SameDirTruePositives, cal.SameDirFalsePositives)
	t.Logf("accuracy %s rule %s provenance: import TP=%d FP=%d precision=%.1f%%; no-import TP=%d FP=%d precision=%.1f%%",
		corpus, cal.Rule,
		cal.ImportTruePositives, cal.ImportFalsePositives,
		ratio(cal.ImportTruePositives, cal.ImportTruePositives+cal.ImportFalsePositives)*100,
		cal.NoImportTruePositives, cal.NoImportFalsePositives,
		ratio(cal.NoImportTruePositives, cal.NoImportTruePositives+cal.NoImportFalsePositives)*100)
	for i, example := range cal.FalsePositiveExamples {
		if i >= 12 {
			break
		}
		t.Logf("accuracy %s rule %s false positive: %s", corpus, cal.Rule, example)
	}
}

func logCalibration(t *testing.T, corpus, phase string, cal *accuracyCalibration) {
	t.Helper()
	if cal == nil {
		return
	}
	for _, cell := range cal.Rules {
		t.Logf("accuracy %s %s rule %-30s TP=%d FP=%d precision=%.1f%%",
			corpus, phase, cell.Key, cell.TruePositives, cell.FalsePositives, cell.Precision*100)
	}
	for _, cell := range cal.Buckets {
		t.Logf("accuracy %s %s bucket %-8s TP=%d FP=%d precision=%.1f%%",
			corpus, phase, cell.Key, cell.TruePositives, cell.FalsePositives, cell.Precision*100)
	}
}

func gtClosure(gt *groundTruth, target int64, depth int) map[int64]bool {
	seen := map[int64]bool{target: true}
	frontier := []int64{target}
	out := map[int64]bool{}
	for d := 0; d < depth; d++ {
		var next []int64
		for _, node := range frontier {
			for caller := range gt.callers[node] {
				if seen[caller] {
					continue
				}
				seen[caller] = true
				out[caller] = true
				next = append(next, caller)
			}
		}
		frontier = next
	}
	return out
}

// accuracyRulePrefix is the adapter rule prefix for the corpus language.
func accuracyRulePrefix(family scipFamily) string {
	if strings.Contains(family.adapter, "python") {
		return "scip.py."
	}
	if strings.Contains(family.adapter, "rust") {
		return "scip.rs."
	}
	return "scip.ts."
}

// adapterTouchedTargets returns the ground-truth targets that have at least one
// adapter edge, sorted for a deterministic cohort order. It runs after
// ApplySCIPAll.
func adapterTouchedTargets(t *testing.T, ctx context.Context, st *store.Store, gt *groundTruth, family scipFamily) []int64 {
	t.Helper()
	prefix := accuracyRulePrefix(family)
	var touched []int64
	for target := range gt.callers {
		edges, err := st.Callers(ctx, target, 0.5)
		if err != nil {
			t.Fatalf("adapter cohort callers: %v", err)
		}
		for _, edge := range edges {
			if strings.HasPrefix(edge.Rule, prefix) {
				touched = append(touched, target)
				break
			}
		}
	}
	sort.Slice(touched, func(i, j int) bool {
		return gt.nodes[touched[i]].QName < gt.nodes[touched[j]].QName
	})
	return touched
}

func ratio(numerator, denominator int) float64 {
	if denominator == 0 {
		return 0
	}
	return float64(numerator) / float64(denominator)
}

func formatCounts(counts map[string]int) string {
	keys := make([]string, 0, len(counts))
	for key := range counts {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", key, counts[key]))
	}
	return strings.Join(parts, " ")
}

func gitOutput(root string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
