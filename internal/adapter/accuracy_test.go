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
// The test reports precision/recall for a deterministic sample of >=20 symbols
// with at least one attributable caller, tree-sitter-only and after
// adapter.ApplySCIPAll on the same store, plus a disagreement list. Facts that
// the graph model cannot attribute are excluded and counted, not silently
// dropped: imports, SCIP-generated occurrences, module/local symbols, external
// definitions, references outside a function/method (repath's documented
// "top-level references are skipped"), recursive references (the adapter skips
// src == target), and same-file+line+name keys that resolve to two different
// targets. `impact` depth 2 is compared only through Blast (the walker behind
// query.Impact; importing query here would cycle back into this package), at
// the same confidence floor, with truncated results excluded and counted.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
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
	samplesWant, _ := strconv.Atoi(os.Getenv("REPATH_ACCURACY_SAMPLES"))
	if samplesWant <= 0 {
		samplesWant = 20
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
	sampled, candidates := sampleTargets(gt, samplesWant)
	if len(sampled) < samplesWant {
		t.Fatalf("only %d candidate targets with an attributable caller, want >= %d", len(sampled), samplesWant)
	}
	t.Logf("accuracy %s (%s): %d documents, refs=%d definitions=%d attributable=%d",
		corpus, lang, gt.documents, gt.references, gt.definitions, gt.attributable)
	t.Logf("accuracy %s: %d of %d candidate targets sampled deterministically (min-confidence %.2f)",
		corpus, len(sampled), candidates, minConfidence)
	t.Logf("accuracy %s: excluded %s", corpus, formatCounts(gt.exclusions))

	before := measureAccuracy(t, ctx, st, gt, sampled, "tree-sitter", minConfidence, nil)
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
		Store: accuracyStore{
			Files: indexed.Files, Nodes: counts.Nodes, Edges: edges,
			Unresolved: unresolved, ParseFailures: counts.ParseFailures,
		},
		Adapters:       statsList,
		Before:         before,
		After:          after,
		TouchedTargets: len(touched),
		TouchedBefore:  touchedBefore,
		TouchedAfter:   touchedAfter,
	}
	for _, phase := range []accuracyPhase{before, after} {
		t.Logf("accuracy %s %s: callers expected=%d actual=%d TP=%d precision=%.1f%% recall=%.1f%% (out-of-scope actual=%d)",
			corpus, phase.Phase, phase.Expected, phase.Actual, phase.TruePositives,
			phase.Precision*100, phase.Recall*100, phase.OutOfScope)
		t.Logf("accuracy %s %s: impact<=2 expected=%d actual=%d TP=%d precision=%.1f%% recall=%.1f%% (truncated samples=%d, out-of-scope=%d)",
			corpus, phase.Phase, phase.ImpactExpected, phase.ImpactActual, phase.ImpactTruePositives,
			phase.ImpactPrecision*100, phase.ImpactRecall*100, phase.ImpactTruncated, phase.ImpactOutOfScope)
	}
	if len(touched) > 0 {
		t.Logf("accuracy %s: %d ground-truth targets repaired by the adapter; callers before expected=%d actual=%d TP=%d precision=%.1f%% recall=%.1f%%; after expected=%d actual=%d TP=%d precision=%.1f%% recall=%.1f%% (+%d TP)",
			corpus, len(touched),
			touchedBefore.Expected, touchedBefore.Actual, touchedBefore.TruePositives,
			touchedBefore.Precision*100, touchedBefore.Recall*100,
			touchedAfter.Expected, touchedAfter.Actual, touchedAfter.TruePositives,
			touchedAfter.Precision*100, touchedAfter.Recall*100,
			touchedAfter.TruePositives-touchedBefore.TruePositives)
	}
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
			if kind := gt.nodes[src].Kind; kind != "function" && kind != "method" {
				// Class/module-level reference; the extraction policy is to
				// skip it, so it is not attributable as a caller.
				gt.exclusions["enclosing-not-function"]++
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
	return out, len(candidates)
}

type accuracyPhase struct {
	Phase               string   `json:"phase"`
	Samples             int      `json:"samples"`
	Expected            int      `json:"expected_callers"`
	Actual              int      `json:"actual_callers"`
	TruePositives       int      `json:"true_positives"`
	Precision           float64  `json:"precision"`
	Recall              float64  `json:"recall"`
	OutOfScope          int      `json:"out_of_scope_actual"`
	ImpactExpected      int      `json:"impact_expected"`
	ImpactActual        int      `json:"impact_actual"`
	ImpactTruePositives int      `json:"impact_true_positives"`
	ImpactPrecision     float64  `json:"impact_precision"`
	ImpactRecall        float64  `json:"impact_recall"`
	ImpactOutOfScope    int      `json:"impact_out_of_scope"`
	ImpactTruncated     int      `json:"impact_truncated"`
	Disagreements       []string `json:"disagreements,omitempty"`
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
	Store            accuracyStore  `json:"store"`
	Adapters         []Stats        `json:"adapters,omitempty"`
	Before           accuracyPhase  `json:"before"`
	After            accuracyPhase  `json:"after"`
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
	src  int64
	path string
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
		out[edge.Dst] = append(out[edge.Dst], callerRef{src: edge.Src, path: edge.SrcPath})
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
				callers = append(callers, callerRef{src: edge.SrcNode.ID, path: edge.SrcPath})
			}
		}
		actual := map[int64]bool{}
		for _, caller := range callers {
			if !coveredPaths[caller.path] {
				out.OutOfScope++
				continue
			}
			actual[caller.src] = true
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
		for id := range actual {
			if expected[id] {
				continue
			}
			caller := gt.nodes[id]
			addDisagreement(fmt.Sprintf("%s <- %s (%s:%d) [false-positive/%s]",
				targetNode.QName, caller.QName, gt.files[caller.FileID], caller.StartLine, phase))
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
		}
	}
	out.Precision = ratio(out.TruePositives, out.Actual)
	out.Recall = ratio(out.TruePositives, out.Expected)
	out.ImpactPrecision = ratio(out.ImpactTruePositives, out.ImpactActual)
	out.ImpactRecall = ratio(out.ImpactTruePositives, out.ImpactExpected)
	return out
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
