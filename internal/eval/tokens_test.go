package eval

// The #87 measurement: how many tokens does an answer cost compared with
// reading the files that answer names?
//
// This is opt-in evidence, never a CI gate:
//
//	REPATH_TOKENS=1 \
//	REPATH_TOKENS_CORPORA="requests=.cache/requests,django=.cache/django-60" \
//	go test ./internal/eval -run '^TestAnswerTokens$' -v -count=1
//
// A corpus may carry a scope after a `|` (`kibana=.cache/kibana|x-pack/...`),
// which uses the #86 feature. Token counts are an estimator: both the answer and
// the file side are measured in bytes and converted with the same rule
// (4 bytes per token, rounded up), so the ratio compares the same unit. The
// answers are JSON payloads (the structs the CLI/MCP emit), not the text
// renderer, and the file side is the sum of the named files' on-disk bytes —
// the answer deliberately does not include file bodies, which is the bias the
// docs state.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Goutam-11/repath/internal/index"
	"github.com/Goutam-11/repath/internal/query"
	"github.com/Goutam-11/repath/internal/store"
)

const (
	tokenAnswerBudget = 1200
	tokenImpactDepth  = 2
	tokenImpactLimit  = 50
	tokenTopSymbols   = 3
)

// tokenQuestion is one answer and the files it points at.
type tokenQuestion struct {
	Kind       string   `json:"kind"`
	Arg        string   `json:"arg"`
	Answer     int      `json:"answer_tokens"`
	Files      []string `json:"files"`
	FileTokens int      `json:"file_tokens"`
	Ratio      float64  `json:"ratio"`
	Note       string   `json:"note,omitempty"`
}

type tokenCorpusReport struct {
	Name        string          `json:"name"`
	Root        string          `json:"root"`
	Scope       []string        `json:"scope,omitempty"`
	Files       int             `json:"files"`
	Nodes       int             `json:"nodes"`
	Edges       int             `json:"edges"`
	Pending     int             `json:"pending"`
	Questions   []tokenQuestion `json:"questions"`
	Answer      int             `json:"answer_tokens"`
	FileTokens  int             `json:"file_tokens"`
	Ratio       float64         `json:"ratio"`
	TotalMedian int             `json:"total_median"`
	TotalMin    int             `json:"total_min"`
	TotalMax    int             `json:"total_max"`
	Runs        int             `json:"runs"`
	Identical   bool            `json:"answers_identical_across_runs"`
}

type tokenReport struct {
	Schema         int                 `json:"schema"`
	GeneratedAt    string              `json:"generated_at"`
	Estimator      string              `json:"estimator"`
	AnswerBudget   int                 `json:"answer_budget"`
	Corpora        []tokenCorpusReport `json:"corpora"`
	AnswerTotal    int                 `json:"answer_tokens_total"`
	FileTotal      int                 `json:"file_tokens_total"`
	OverallRatio   float64             `json:"overall_ratio"`
	Method         string              `json:"method"`
	KnownBias      string              `json:"known_bias"`
	UnverifiedNote string              `json:"unverified,omitempty"`
}

type tokenCorpus struct {
	name  string
	root  string
	scope []string
}

func parseTokenCorpora(raw string) []tokenCorpus {
	var out []tokenCorpus
	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		name, rest, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		root, scopeRaw, _ := strings.Cut(rest, "|")
		var scope []string
		if scopeRaw != "" {
			scope = strings.Split(scopeRaw, "|")
		}
		out = append(out, tokenCorpus{name: strings.TrimSpace(name), root: strings.TrimSpace(root), scope: scope})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// answerTokens is the estimator both sides use.
func answerTokens(bytes int) int { return (bytes + 3) / 4 }

func marshalAnswer(t *testing.T, value any) int {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal answer: %v", err)
	}
	return answerTokens(len(data))
}

// fileBytes sums the on-disk sizes of the named files, de-duplicated. A file
// that the answer names but that is no longer on disk contributes nothing and
// is reported as a note.
func fileBytes(root string, paths []string) (int, []string) {
	seen := make(map[string]bool, len(paths))
	total := 0
	var missing []string
	sorted := append([]string(nil), paths...)
	sort.Strings(sorted)
	for _, p := range sorted {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		info, err := os.Stat(filepath.Join(root, filepath.FromSlash(p)))
		if err != nil {
			missing = append(missing, p)
			continue
		}
		total += int(info.Size())
	}
	return total, missing
}

func ratioOf(fileTokens, answer int) float64 {
	if answer == 0 {
		return 0
	}
	return float64(fileTokens) / float64(answer)
}

func TestAnswerTokens(t *testing.T) {
	if os.Getenv("REPATH_TOKENS") != "1" {
		t.Skip("set REPATH_TOKENS=1 and REPATH_TOKENS_CORPORA (issue #87 evidence)")
	}
	corpora := parseTokenCorpora(os.Getenv("REPATH_TOKENS_CORPORA"))
	if len(corpora) == 0 {
		t.Skip("REPATH_TOKENS_CORPORA is empty (name=path[,name=path|scope])")
	}
	runs := 3
	if raw := os.Getenv("REPATH_TOKENS_RUNS"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 {
			t.Fatalf("REPATH_TOKENS_RUNS = %q, want a positive integer", raw)
		}
		runs = n
	}

	report := tokenReport{
		Schema:       1,
		GeneratedAt:  time.Now().UTC().Format(time.RFC3339),
		Estimator:    "ceil(bytes/4); the same rule on both the answer and the file side",
		AnswerBudget: tokenAnswerBudget,
		Method: "for each corpus, index into a temporary store and ask a fixed set of " +
			"questions: map (budget 1200), find/impact(depth 2) for the top symbols, and " +
			"the shortest path over the first resolvable edge. The answer side is the JSON " +
			"payload; the file side is the summed on-disk bytes of the files the answer names.",
		KnownBias: "answers never contain file bodies, so the ratio compares a location-list " +
			"with full source text; it measures what an agent skips reading, not what the " +
			"answer explains.",
	}

	for _, corpus := range corpora {
		root, err := filepath.Abs(corpus.root)
		if err != nil {
			t.Fatalf("%s: %v", corpus.name, err)
		}
		if _, err := os.Stat(root); err != nil {
			t.Logf("skip corpus %s: %v", corpus.name, err)
			report.UnverifiedNote += fmt.Sprintf("%s absent; ", corpus.name)
			continue
		}
		entries, err := runTokenCorpus(t, corpus.name, root, corpus.scope, runs)
		if err != nil {
			t.Fatalf("%s: %v", corpus.name, err)
		}
		report.Corpora = append(report.Corpora, entries)
		report.AnswerTotal += entries.Answer
		report.FileTotal += entries.FileTokens
	}
	report.OverallRatio = ratioOf(report.FileTotal, report.AnswerTotal)

	for _, corpus := range report.Corpora {
		label := "whole repo"
		if len(corpus.Scope) > 0 {
			label = strings.Join(corpus.Scope, ",")
		}
		t.Logf("tokens %s: %s · %d files · %d answer tokens vs %d file tokens (%.1fx) over %d runs",
			corpus.Name, label, corpus.Files,
			corpus.Answer, corpus.FileTokens, corpus.Ratio, corpus.Runs)
		for _, q := range corpus.Questions {
			t.Logf("  %-6s %-46s answer %6d · files %7d (%d) · %6.1fx %s",
				q.Kind, truncateArg(q.Arg), q.Answer, q.FileTokens, len(q.Files), q.Ratio, q.Note)
		}
	}
	t.Logf("tokens total: %d answer tokens vs %d file tokens (%.1fx)",
		report.AnswerTotal, report.FileTotal, report.OverallRatio)

	if path := os.Getenv("REPATH_TOKENS_JSON"); path != "" {
		data, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func truncateArg(s string) string {
	if len(s) <= 46 {
		return s
	}
	return s[:43] + "..."
}

func runTokenCorpus(t *testing.T, name, root string, scope []string, runs int) (tokenCorpusReport, error) {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "tokens.db"))
	if err != nil {
		return tokenCorpusReport{}, err
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		return tokenCorpusReport{}, err
	}
	if _, err := index.Run(ctx, st, root, index.Options{Scope: scope, WholeRepo: len(scope) == 0}); err != nil {
		return tokenCorpusReport{}, err
	}
	counts, err := st.Counts(ctx)
	if err != nil {
		return tokenCorpusReport{}, err
	}
	pending, err := st.CountPending(ctx)
	if err != nil {
		return tokenCorpusReport{}, err
	}
	edges, err := st.EdgeCounts(ctx)
	if err != nil {
		return tokenCorpusReport{}, err
	}
	totalEdges := 0
	for _, n := range edges {
		totalEdges += n
	}

	questions, err := buildTokenQuestions(t, ctx, st, root)
	if err != nil {
		return tokenCorpusReport{}, err
	}
	// Every run recomputes the answers; the bytes must be identical, which is
	// the determinism half of the measurement.
	baseline, err := json.Marshal(questions)
	if err != nil {
		return tokenCorpusReport{}, err
	}
	report := tokenCorpusReport{Name: name, Root: root, Scope: scope,
		Files: counts.Files, Nodes: counts.Nodes, Pending: pending, Edges: totalEdges,
		Questions: questions, Runs: runs}
	report.Answer = sumQuestionTokens(questions)
	report.FileTokens = sumFileTokens(questions)
	report.Ratio = ratioOf(report.FileTokens, report.Answer)
	report.TotalMin, report.TotalMax = report.Answer, report.Answer
	report.Identical = true
	for i := 1; i < runs; i++ {
		again, err := buildTokenQuestions(t, ctx, st, root)
		if err != nil {
			return tokenCorpusReport{}, err
		}
		data, err := json.Marshal(again)
		if err != nil {
			return tokenCorpusReport{}, err
		}
		if string(data) != string(baseline) {
			report.Identical = false
		}
		total := sumQuestionTokens(again)
		if total < report.TotalMin {
			report.TotalMin = total
		}
		if total > report.TotalMax {
			report.TotalMax = total
		}
	}
	report.TotalMedian = report.Answer
	return report, nil
}

func sumQuestionTokens(questions []tokenQuestion) int {
	total := 0
	for _, q := range questions {
		total += q.Answer
	}
	return total
}

func sumFileTokens(questions []tokenQuestion) int {
	total := 0
	for _, q := range questions {
		total += q.FileTokens
	}
	return total
}

// buildTokenQuestions derives a deterministic question set from the graph
// itself: no corpus-specific symbol lists, so the same harness runs on any
// pinned tree.
func buildTokenQuestions(t *testing.T, ctx context.Context, st *store.Store, root string) ([]tokenQuestion, error) {
	t.Helper()
	var questions []tokenQuestion
	add := func(kind, arg string, answer any, files []string, note string) {
		bytes, missing := fileBytes(root, files)
		if len(missing) > 0 {
			note = strings.TrimSpace(note + " missing-on-disk:" + strings.Join(missing, ","))
		}
		answerBytes := marshalAnswer(t, answer)
		fileTokens := answerTokens(bytes)
		questions = append(questions, tokenQuestion{
			Kind: kind, Arg: arg, Answer: answerBytes,
			Files: uniqueSorted(files), FileTokens: fileTokens,
			Ratio: ratioOf(fileTokens, answerBytes), Note: note,
		})
	}

	mapAnswer, err := query.Map(ctx, st, tokenAnswerBudget, 0.5)
	if err != nil {
		return nil, err
	}
	topFiles := make([]string, 0, len(mapAnswer.Result.Top))
	for _, sym := range mapAnswer.Result.Top {
		topFiles = append(topFiles, sym.File)
	}
	add("map", fmt.Sprintf("--max-tokens %d", tokenAnswerBudget), mapAnswer.Result, topFiles, "")

	// The top symbols make the find/impact questions; only uniquely resolvable
	// qnames are usable as CLI arguments.
	used := 0
	for _, sym := range mapAnswer.Result.Top {
		if used >= tokenTopSymbols {
			break
		}
		target, code, err := query.ResolveSymbol(ctx, st, sym.QName, "")
		if err != nil || code != 0 || len(target.Nodes) != 1 {
			continue
		}
		used++

		findAnswer, code, err := query.Find(ctx, st, sym.QName, "", 0.5)
		if err != nil {
			return nil, err
		}
		if code == 0 {
			add("find", sym.QName, findAnswer.Result, []string{sym.File}, "")
		}

		impactAnswer, code, err := query.Impact(ctx, st, sym.QName, tokenImpactDepth, 0.5, tokenImpactLimit)
		if err != nil {
			return nil, err
		}
		if code == 0 {
			files := make([]string, 0, len(impactAnswer.Result.Files))
			for _, hit := range impactAnswer.Result.Files {
				files = append(files, hit.Path)
			}
			note := ""
			if impactAnswer.Result.Truncated {
				note = "truncated"
			}
			add("impact", sym.QName+" --depth "+strconv.Itoa(tokenImpactDepth),
				impactAnswer.Result, files, note)
		}
	}

	// One path question: the first edge (sorted) whose endpoints both resolve to
	// a single symbol, so the argument is not ambiguous.
	if q, ok, err := pathQuestion(t, ctx, st, root); err != nil {
		return nil, err
	} else if ok {
		questions = append(questions, q)
	}
	return questions, nil
}

func pathQuestion(t *testing.T, ctx context.Context, st *store.Store, root string) (tokenQuestion, bool, error) {
	t.Helper()
	edges, err := st.GraphEdges(ctx, 0.5)
	if err != nil {
		return tokenQuestion{}, false, err
	}
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].SrcNode.QName != edges[j].SrcNode.QName {
			return edges[i].SrcNode.QName < edges[j].SrcNode.QName
		}
		return edges[i].DstNode.QName < edges[j].DstNode.QName
	})
	for _, edge := range edges {
		from, to := edge.SrcNode.QName, edge.DstNode.QName
		if from == "" || to == "" || from == to {
			continue
		}
		if !resolvesUniquely(ctx, st, from) || !resolvesUniquely(ctx, st, to) {
			continue
		}
		answer, code, err := query.Path(ctx, st, from, to, query.DefaultDepth, 0.5, 3)
		if err != nil || code != 0 || len(answer.Result.Hops) == 0 {
			continue
		}
		files := []string{answer.Result.From.File}
		for _, hop := range answer.Result.Hops {
			files = append(files, hop.Dst.File)
		}
		bytes, missing := fileBytes(root, files)
		note := ""
		if len(missing) > 0 {
			note = "missing-on-disk:" + strings.Join(missing, ",")
		}
		answerBytes := marshalAnswer(t, answer.Result)
		fileTokens := answerTokens(bytes)
		return tokenQuestion{
			Kind: "path", Arg: from + " -> " + to,
			Answer: answerBytes,
			Files:  uniqueSorted(files), FileTokens: fileTokens,
			Ratio: ratioOf(fileTokens, answerBytes),
			Note:  note,
		}, true, nil
	}
	return tokenQuestion{}, false, nil
}

func resolvesUniquely(ctx context.Context, st *store.Store, qname string) bool {
	target, code, err := query.ResolveSymbol(ctx, st, qname, "")
	return err == nil && code == 0 && len(target.Nodes) == 1
}

func uniqueSorted(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, v := range values {
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}
