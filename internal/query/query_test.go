// Package query_test exercises the shared answer layer directly: resolution
// codes, the confidence floor, depth/limit truncation, the module filter, and
// the envelope/provenance rules. It indexes a small hermetic TypeScript repo so
// the answers are produced by the real resolver rather than hand-built rows.
package query_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Goutam-11/repath/internal/index"
	"github.com/Goutam-11/repath/internal/output"
	"github.com/Goutam-11/repath/internal/query"
	"github.com/Goutam-11/repath/internal/store"
)

func writeRepoFile(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", rel, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

// queryFixture is a tiny workspace: a local call chain, a same-file diamond
// (two paths to dest), a two-candidate ambiguous name, and two packages that
// both export `shared` so the module filter has something to narrow.
func queryFixture(t *testing.T) map[string]string {
	return map[string]string{
		"package.json": `{"name":"query-fixture","private":true,"workspaces":["packages/*"]}`,
		"src/app.ts": `export function leaf(): number { return 1; }
export function middle(): number { return leaf(); }
export function root(): number { return middle(); }
export function ambiguousUse(): number { return dup(); }
`,
		"src/chain.ts": `export function gateway(): number { return left() + right(); }
export function left(): number { return dest(); }
export function right(): number { return dest(); }
export function dest(): number { return 0; }
export function cycleA(): number { return cycleB(); }
export function cycleB(): number { return cycleA(); }
`,
		"src/dup-a.ts": `export function dup(): number { return 1; }
`,
		"src/dup-b.ts": `export function dup(): number { return 2; }
`,
		"packages/left/package.json":  `{"name":"@demo/left","main":"index.ts"}`,
		"packages/left/index.ts":      `export function shared(): number { return 1; }`,
		"packages/right/package.json": `{"name":"@demo/right","main":"index.ts"}`,
		"packages/right/index.ts":     `export function shared(): number { return 2; }`,
	}
}

func indexedRepo(t *testing.T) (string, *store.Store) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	for rel, content := range queryFixture(t) {
		writeRepoFile(t, root, rel, content)
	}

	st, err := store.Open(ctx, filepath.Join(root, ".repath", "index.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	result, err := index.Run(ctx, st, root, index.Options{})
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	if result.ParseFailures != 0 {
		t.Fatalf("parse failures = %d, fixture must parse cleanly", result.ParseFailures)
	}
	return root, st
}

func emptyRepo(t *testing.T) (string, *store.Store) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	st, err := store.Open(ctx, filepath.Join(root, ".repath", "index.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return root, st
}

func TestFindResolvesOneSymbolAndCountsEdges(t *testing.T) {
	ctx := context.Background()
	root, st := indexedRepo(t)
	_ = root

	answer, code, err := query.Find(ctx, st, "leaf", "", query.DefaultMinConfidence)
	if err != nil || code != 0 {
		t.Fatalf("find leaf: code=%d err=%v", code, err)
	}
	if answer.Result.Symbol.QName != "leaf" || answer.Result.Symbol.File != "src/app.ts" {
		t.Fatalf("find leaf = %+v", answer.Result.Symbol)
	}
	if answer.Result.Callers != 1 || answer.Result.Callees != 0 {
		t.Fatalf("find leaf callers/callees = %d/%d, want 1/0", answer.Result.Callers, answer.Result.Callees)
	}

	envelope := query.FindEnvelope(answer)
	if envelope.Confidence != 1.0 || len(envelope.Provenance) != 1 {
		t.Fatalf("find envelope = %+v", envelope)
	}
	if envelope.Provenance[0].Rule != "definition" || envelope.Provenance[0].File != "src/app.ts" {
		t.Fatalf("find provenance = %+v", envelope.Provenance[0])
	}
}

func TestCallersConfidenceFloorAndLimit(t *testing.T) {
	ctx := context.Background()
	_, st := indexedRepo(t)

	// `left()` and `right()` both call dest() at ts.local 0.85.
	answer, code, err := query.Callers(ctx, st, "dest", query.DefaultMinConfidence, 0)
	if err != nil || code != 0 {
		t.Fatalf("callers dest: code=%d err=%v", code, err)
	}
	if len(answer.Result.Callers) != 2 || answer.Result.Truncated {
		t.Fatalf("callers dest = %+v, want two untruncated callers", answer.Result)
	}
	for _, caller := range answer.Result.Callers {
		if caller.Confidence < 0.5 || caller.Rule == "" || caller.CallLine == 0 {
			t.Fatalf("caller missing evidence: %+v", caller)
		}
	}

	capped, _, err := query.Callers(ctx, st, "dest", query.DefaultMinConfidence, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(capped.Result.Callers) != 1 || !capped.Result.Truncated {
		t.Fatalf("limited callers = %+v, want one truncated caller", capped.Result)
	}
	if !strings.Contains(query.CallersEnvelope(capped).Summary, "truncated") {
		t.Fatalf("truncated summary = %q", query.CallersEnvelope(capped).Summary)
	}

	// The leaf <- middle edge is 0.85, so a 0.9 floor removes it.
	strict, _, err := query.Callers(ctx, st, "leaf", 0.9, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(strict.Result.Callers) != 0 {
		t.Fatalf("callers at 0.9 = %+v, want none", strict.Result.Callers)
	}
}

func TestImpactDepthLimitAndConfidence(t *testing.T) {
	ctx := context.Background()
	_, st := indexedRepo(t)

	full, code, err := query.Impact(ctx, st, "leaf", 8, query.DefaultMinConfidence, 50)
	if err != nil || code != 0 {
		t.Fatalf("impact leaf: code=%d err=%v", code, err)
	}
	if len(full.Result.Dependents) != 2 {
		t.Fatalf("dependents = %+v, want middle and root", full.Result.Dependents)
	}
	if full.Result.Dependents[0].QName != "middle" || full.Result.Dependents[0].Depth != 1 {
		t.Fatalf("first dependent = %+v, want middle at depth 1", full.Result.Dependents[0])
	}

	shallow, _, err := query.Impact(ctx, st, "leaf", 1, query.DefaultMinConfidence, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(shallow.Result.Dependents) != 1 || shallow.Result.Dependents[0].QName != "middle" {
		t.Fatalf("depth-1 dependents = %+v", shallow.Result.Dependents)
	}

	capped, _, err := query.Impact(ctx, st, "leaf", 8, query.DefaultMinConfidence, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(capped.Result.Dependents) != 1 || !capped.Result.Truncated {
		t.Fatalf("limited impact = %+v, want one truncated dependent", capped.Result)
	}
	if envelope := query.ImpactEnvelope(capped); len(envelope.Provenance) != 1 {
		t.Fatalf("impact provenance = %+v, want one row", envelope.Provenance)
	}

	// An ambiguous 0.40 candidate edge is visible only below the default floor.
	loose, _, err := query.Impact(ctx, st, "src/dup-a.ts:1", 8, 0.4, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(loose.Result.Dependents) != 1 {
		t.Fatalf("0.4 impact dependents = %+v, want the ambiguous caller", loose.Result.Dependents)
	}
	if loose.Result.Dependents[0].Rule != "ts.global.ambiguous" || loose.Result.Dependents[0].Confidence != 0.40 {
		t.Fatalf("ambiguous dependent = %+v", loose.Result.Dependents[0])
	}

	tight, _, err := query.Impact(ctx, st, "src/dup-a.ts:1", 8, query.DefaultMinConfidence, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(tight.Result.Dependents) != 0 {
		t.Fatalf("default impact must exclude the 0.40 candidate: %+v", tight.Result.Dependents)
	}
}

func TestPathFindsShortestChainAndAlternates(t *testing.T) {
	ctx := context.Background()
	_, st := indexedRepo(t)

	answer, code, err := query.Path(ctx, st, "gateway", "dest", 8, query.DefaultMinConfidence, 3)
	if err != nil || code != 0 {
		t.Fatalf("path gateway->dest: code=%d err=%v", code, err)
	}
	if len(answer.Result.Hops) != 2 {
		t.Fatalf("hops = %+v, want 2", answer.Result.Hops)
	}
	if len(answer.Result.Alternate) != 1 {
		t.Fatalf("alternates = %+v, want the other diamond branch", answer.Result.Alternate)
	}
	if answer.Result.Hops[0].Dst.QName == answer.Result.Alternate[0][0].Dst.QName {
		t.Fatalf("alternate must leave gateway by a different branch: %+v", answer.Result)
	}
	if envelope := query.PathEnvelope(answer); len(envelope.Provenance) != 2 {
		t.Fatalf("path provenance = %+v, want both hops", envelope.Provenance)
	}

	missing, code, err := query.Path(ctx, st, "gateway", "nope", 8, query.DefaultMinConfidence, 3)
	if err != nil || code != 3 {
		t.Fatalf("path to missing: code=%d err=%v", code, err)
	}
	if missing.Failed == nil || missing.Failed.Arg != "nope" || missing.Failed.Code != 3 {
		t.Fatalf("failed path = %+v", missing.Failed)
	}

	ambiguous, code, err := query.Path(ctx, st, "dup", "gateway", 8, query.DefaultMinConfidence, 3)
	if err != nil || code != 4 {
		t.Fatalf("path from ambiguous: code=%d err=%v", code, err)
	}
	if ambiguous.Failed == nil || ambiguous.Failed.Code != 4 || len(ambiguous.Failed.Target.Candidates) != 2 {
		t.Fatalf("ambiguous path failure = %+v", ambiguous.Failed)
	}
}

func TestResolveTargetCodesAndCandidates(t *testing.T) {
	ctx := context.Background()
	_, st := indexedRepo(t)

	cases := []struct {
		arg  string
		want int
	}{
		{"leaf", 0},
		{"src/app.ts:2", 0},
		{"src/app.ts", 0},
		{"src/app.ts:9999", 3},
		{"src/missing.ts", 3},
		{"no_such_symbol", 3},
		{"dup", 4},
	}
	codes := make(map[int]bool)
	for _, tc := range cases {
		target, code, err := query.ResolveTarget(ctx, st, tc.arg)
		if err != nil {
			t.Fatalf("resolve %q: %v", tc.arg, err)
		}
		if code != tc.want {
			t.Errorf("resolve %q code = %d, want %d (%+v)", tc.arg, code, tc.want, target)
		}
		codes[code] = true
	}
	if codes[5] {
		t.Fatal("query returned exit code 5, which is check-only")
	}
	if codes[2] {
		t.Fatal("query returned exit code 2; usage is owned by the CLI")
	}

	ambiguous, code, _ := query.Find(ctx, st, "dup", "", query.DefaultMinConfidence)
	if code != 4 || len(ambiguous.Target.Candidates) != 2 {
		t.Fatalf("ambiguous find = code %d, candidates %+v", code, ambiguous.Target.Candidates)
	}
	envelope := query.ErrorEnvelope(4, "ambiguous", []query.ErrorCandidate{{QName: "dup", Kind: "function"}})
	payload, ok := envelope.Answer.(map[string]any)
	if !ok || payload["code"] != 4 {
		t.Fatalf("error envelope payload = %#v", envelope.Answer)
	}
	if _, ok := payload["candidates"]; !ok {
		t.Fatalf("error envelope lost candidates: %#v", payload)
	}
	if envelope.Confidence != 0 || envelope.Truncated || len(envelope.Provenance) != 0 {
		t.Fatalf("error envelope must carry no confidence/provenance: %+v", envelope)
	}
}

func TestModuleFilterNarrowsAmbiguousNames(t *testing.T) {
	ctx := context.Background()
	_, st := indexedRepo(t)

	answer, code, err := query.Find(ctx, st, "shared", "", query.DefaultMinConfidence)
	if err != nil || code != 4 {
		t.Fatalf("find shared without module: code=%d err=%v", code, err)
	}
	if len(answer.Target.Candidates) != 2 {
		t.Fatalf("shared candidates = %+v", answer.Target.Candidates)
	}

	filtered, code, err := query.Find(ctx, st, "shared", "@demo/left", query.DefaultMinConfidence)
	if err != nil || code != 0 {
		t.Fatalf("find shared in @demo/left: code=%d err=%v", code, err)
	}
	if filtered.Result.Symbol.File != "packages/left/index.ts" {
		t.Fatalf("module filter picked %q", filtered.Result.Symbol.File)
	}

	unknownModule, code, err := query.Find(ctx, st, "shared", "no-such-module", query.DefaultMinConfidence)
	if err != nil || code != 3 {
		t.Fatalf("unknown module: code=%d err=%v", code, err)
	}
	if len(unknownModule.Target.Nodes) != 0 {
		t.Fatalf("unknown module returned nodes: %+v", unknownModule.Target)
	}
}

func TestMapAndStatusAnswers(t *testing.T) {
	ctx := context.Background()
	root, st := indexedRepo(t)

	mapped, err := query.Map(ctx, st, 2000, query.DefaultMinConfidence)
	if err != nil {
		t.Fatalf("map: %v", err)
	}
	if len(mapped.Result.Modules) < 3 {
		t.Fatalf("modules = %+v, want the root plus two packages", mapped.Result.Modules)
	}
	if len(mapped.Result.Top) == 0 {
		t.Fatal("map returned no ranked symbols")
	}
	if envelope := query.MapEnvelope(mapped); len(envelope.Provenance) != len(mapped.Result.Top) {
		t.Fatalf("map provenance = %d, want %d", len(envelope.Provenance), len(mapped.Result.Top))
	}

	status, err := query.Status(ctx, st, root, false, false)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !status.Indexed || status.Files != 6 || status.SchemaVersion != store.SchemaVersion {
		t.Fatalf("status = files %d indexed %v schema %d", status.Files, status.Indexed, status.SchemaVersion)
	}
	if status.Nodes == 0 || len(status.Edges) == 0 {
		t.Fatalf("status counters empty: nodes %d edges %+v", status.Nodes, status.Edges)
	}
	if envelope := query.StatusEnvelope(status); envelope.Confidence != 1.0 || !strings.Contains(envelope.Summary, "files") {
		t.Fatalf("status envelope = %+v", envelope)
	}
}

func TestEmptyGraphAnswers(t *testing.T) {
	ctx := context.Background()
	root, st := emptyRepo(t)

	_, code, err := query.Find(ctx, st, "anything", "", query.DefaultMinConfidence)
	if err != nil || code != 3 {
		t.Fatalf("find on empty graph: code=%d err=%v", code, err)
	}
	_, code, err = query.Callers(ctx, st, "anything", query.DefaultMinConfidence, 10)
	if err != nil || code != 3 {
		t.Fatalf("callers on empty graph: code=%d err=%v", code, err)
	}
	_, code, err = query.Impact(ctx, st, "anything", 8, query.DefaultMinConfidence, 10)
	if err != nil || code != 3 {
		t.Fatalf("impact on empty graph: code=%d err=%v", code, err)
	}

	mapped, err := query.Map(ctx, st, 100, query.DefaultMinConfidence)
	if err != nil {
		t.Fatalf("map on empty graph: %v", err)
	}
	if len(mapped.Result.Modules) != 0 || len(mapped.Result.Top) != 0 {
		t.Fatalf("empty map = %+v", mapped.Result)
	}

	status, err := query.Status(ctx, st, root, false, false)
	if err != nil {
		t.Fatalf("status on empty graph: %v", err)
	}
	if status.Indexed || status.Files != 0 || status.Nodes != 0 {
		t.Fatalf("empty status = %+v", status)
	}
}

func TestEnvelopeProvenanceIsCappedAndDeduped(t *testing.T) {
	ctx := context.Background()
	_, st := indexedRepo(t)

	answer, code, err := query.Impact(ctx, st, "leaf", 8, query.DefaultMinConfidence, 50)
	if err != nil || code != 0 {
		t.Fatalf("impact: code=%d err=%v", code, err)
	}
	envelope := query.ImpactEnvelope(answer)

	// Every dependent contributes exactly one (file, line, rule) row here.
	if len(envelope.Provenance) != len(answer.Result.Dependents) {
		t.Fatalf("provenance = %d, want %d", len(envelope.Provenance), len(answer.Result.Dependents))
	}
	for i, item := range envelope.Provenance {
		if item.File == "" || item.Line == 0 || item.Rule == "" {
			t.Fatalf("provenance[%d] incomplete: %+v", i, item)
		}
	}

	// The shared cap is applied by output.New; a hand-built envelope with 60
	// rows keeps only 50.
	items := make([]output.Provenance, 60)
	for i := range items {
		items[i] = output.Provenance{File: "f.ts", Line: i + 1, Rule: "ts.local"}
	}
	capped := output.New(answer.Result, "s", 1, false, items)
	if len(capped.Provenance) != 50 {
		t.Fatalf("capped provenance = %d, want 50", len(capped.Provenance))
	}
}
