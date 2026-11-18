package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Goutam-11/repath/internal/adapter"
)

func copyAdapterFixture(t *testing.T) string {
	t.Helper()
	return copyAdapterFixtureNamed(t, "ts-adapter")
}

func copyAdapterFixtureNamed(t *testing.T, name string) string {
	t.Helper()
	src := filepath.Join("..", "..", "testdata", "fixtures", name)
	dst := t.TempDir()
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
		t.Fatalf("copy fixture: %v", err)
	}
	return dst
}

func TestAdapterCommand(t *testing.T) {
	dir := copyAdapterFixture(t)
	scip := filepath.Join("..", "..", "testdata", "fixtures", "ts-adapter", "index.scip")

	code, beforeOut, errOut := run(t, "status", "--root", dir, "--json")
	if code != 0 {
		t.Fatalf("status before exit = %d: %s", code, errOut)
	}
	var before struct {
		Unresolved int `json:"unresolved"`
	}
	if err := json.Unmarshal([]byte(beforeOut), &before); err != nil {
		t.Fatal(err)
	}
	if before.Unresolved != 1 {
		t.Fatalf("status before unresolved = %d, want 1", before.Unresolved)
	}

	code, out, errOut := run(t, "adapter", "--scip", scip, "--root", dir, "--json")
	if code != 0 {
		t.Fatalf("adapter exit = %d: %s", code, errOut)
	}
	var stats adapter.Stats
	if err := json.Unmarshal([]byte(out), &stats); err != nil {
		t.Fatalf("adapter output is not JSON: %v\n%s", err, out)
	}
	if stats.Matched != 1 || stats.Edges != 1 || stats.Conflicts != 0 {
		t.Fatalf("stats = %+v", stats)
	}

	code, afterOut, errOut := run(t, "status", "--root", dir, "--json")
	if code != 0 {
		t.Fatalf("status after exit = %d: %s", code, errOut)
	}
	var after struct {
		Unresolved int             `json:"unresolved"`
		Adapters   []adapter.Stats `json:"adapters"`
	}
	if err := json.Unmarshal([]byte(afterOut), &after); err != nil {
		t.Fatal(err)
	}
	if after.Unresolved != 0 {
		t.Fatalf("status after unresolved = %d, want 0", after.Unresolved)
	}
	if len(after.Adapters) != 1 || after.Adapters[0].Edges != 1 {
		t.Fatalf("status adapters = %+v", after.Adapters)
	}
}

func TestAdapterCommandErrors(t *testing.T) {
	dir := t.TempDir()
	if code, _, _ := run(t, "adapter", "--root", dir); code != 2 {
		t.Fatalf("missing --scip exit = %d, want 2", code)
	}
	if code, _, _ := run(t, "adapter", "--scip", filepath.Join(dir, "missing.scip"), "--root", dir); code != 3 {
		t.Fatalf("missing file exit = %d, want 3", code)
	}
}

func TestAdapterCommandIndexesLazily(t *testing.T) {
	dir := copyAdapterFixture(t)
	scip := filepath.Join("..", "..", "testdata", "fixtures", "ts-adapter", "index.scip")
	code, out, errOut := run(t, "adapter", "--scip", scip, "--root", dir, "--json")
	if code != 0 {
		t.Fatalf("adapter exit = %d: %s", code, errOut)
	}
	var stats adapter.Stats
	if err := json.Unmarshal([]byte(out), &stats); err != nil {
		t.Fatal(err)
	}
	if stats.Edges != 1 || stats.Matched != 1 {
		t.Fatalf("adapter did not index lazily: %+v", stats)
	}
}

func TestAdapterCommandPython(t *testing.T) {
	dir := copyAdapterFixtureNamed(t, "py-adapter")
	scip := filepath.Join("..", "..", "testdata", "fixtures", "py-adapter", "index.scip")

	code, out, errOut := run(t, "adapter", "--scip", scip, "--root", dir, "--json")
	if code != 0 {
		t.Fatalf("adapter exit = %d: %s", code, errOut)
	}
	var stats adapter.Stats
	if err := json.Unmarshal([]byte(out), &stats); err != nil {
		t.Fatalf("adapter output is not JSON: %v\n%s", err, out)
	}
	if stats.Adapter != "scip-python" || stats.Matched != 2 || stats.Edges != 2 || stats.Conflicts != 0 {
		t.Fatalf("stats = %+v, want scip-python, 2 matched, 2 edges, 0 conflicts", stats)
	}

	code, out, errOut = run(t, "status", "--root", dir, "--json")
	if code != 0 {
		t.Fatalf("status exit = %d: %s", code, errOut)
	}
	var status struct {
		Unresolved int             `json:"unresolved"`
		Adapters   []adapter.Stats `json:"adapters"`
	}
	if err := json.Unmarshal([]byte(out), &status); err != nil {
		t.Fatal(err)
	}
	if status.Unresolved != 0 {
		t.Fatalf("unresolved = %d, want 0", status.Unresolved)
	}
	if len(status.Adapters) != 1 || status.Adapters[0].Adapter != "scip-python" {
		t.Fatalf("status adapters = %+v, want scip-python", status.Adapters)
	}
}

// scipTestEncoder writes the minimal SCIP protobuf the adapter parses. The
// ambiguity fixture is built here instead of committing another producer file,
// so CI still needs no Node/Python toolchain.
type scipTestEncoder struct{ buf []byte }

func (e *scipTestEncoder) varint(v uint64) {
	for v >= 0x80 {
		e.buf = append(e.buf, byte(v)|0x80)
		v >>= 7
	}
	e.buf = append(e.buf, byte(v))
}

func (e *scipTestEncoder) tag(number, wire int) { e.varint(uint64(number<<3 | wire)) }

func (e *scipTestEncoder) bytesField(number int, data []byte) {
	e.tag(number, 2)
	e.varint(uint64(len(data)))
	e.buf = append(e.buf, data...)
}

func (e *scipTestEncoder) varintField(number int, value uint64) {
	e.tag(number, 0)
	e.varint(value)
}

func encodeAmbiguousSCIP(docs []struct {
	path        string
	language    string
	occurrences []struct {
		line   int
		symbol string
		roles  uint64
	}
}) []byte {
	var index scipTestEncoder
	for _, doc := range docs {
		var d scipTestEncoder
		d.bytesField(1, []byte(doc.path))
		d.bytesField(4, []byte(doc.language))
		for _, occ := range doc.occurrences {
			var o scipTestEncoder
			o.varintField(1, uint64(occ.line))
			o.bytesField(2, []byte(occ.symbol))
			o.varintField(3, occ.roles)
			d.bytesField(2, o.buf)
		}
		index.bytesField(2, d.buf)
	}
	return index.buf
}

func TestAdapterStatusSurfacesReplaced(t *testing.T) {
	dir := copyAdapterFixtureNamed(t, "py-adapter-ambiguous")
	scip := filepath.Join(dir, "ambiguous.scip")
	type occ = struct {
		line   int
		symbol string
		roles  uint64
	}
	type doc = struct {
		path        string
		language    string
		occurrences []occ
	}
	const (
		symA = "scip-python python py-amb 0.0.0 models_a/total()."
		symB = "scip-python python py-amb 0.0.0 models_b/total()."
	)
	data := encodeAmbiguousSCIP([]doc{
		{path: "models_a.py", language: "python", occurrences: []occ{{line: 0, symbol: symA, roles: 1}}},
		{path: "models_b.py", language: "python", occurrences: []occ{{line: 0, symbol: symB, roles: 1}}},
		{path: "app.py", language: "python", occurrences: []occ{{line: 1, symbol: symA}}},
	})
	if err := os.WriteFile(scip, data, 0o644); err != nil {
		t.Fatal(err)
	}

	code, out, errOut := run(t, "adapter", "--scip", scip, "--root", dir, "--json")
	if code != 0 {
		t.Fatalf("adapter exit = %d: %s", code, errOut)
	}
	var stats adapter.Stats
	if err := json.Unmarshal([]byte(out), &stats); err != nil {
		t.Fatal(err)
	}
	if stats.Replaced != 1 || stats.ReplacedEdges != 2 || stats.Edges != 1 {
		t.Fatalf("stats = %+v, want replaced=1 replaced_edges=2 edges=1", stats)
	}

	code, out, errOut = run(t, "status", "--root", dir, "--json")
	if code != 0 {
		t.Fatalf("status exit = %d: %s", code, errOut)
	}
	var status struct {
		Ambiguous int             `json:"ambiguous"`
		Adapters  []adapter.Stats `json:"adapters"`
	}
	if err := json.Unmarshal([]byte(out), &status); err != nil {
		t.Fatal(err)
	}
	if len(status.Adapters) != 1 || status.Adapters[0].Replaced != 1 || status.Adapters[0].ReplacedEdges != 2 {
		t.Fatalf("status adapters = %+v, want replaced=1 replaced_edges=2", status.Adapters)
	}
	if status.Ambiguous != 2 {
		t.Fatalf("status ambiguous = %d, want the untouched same-line pair (2 rows)", status.Ambiguous)
	}
}

func TestAdapterCommandLSPFacts(t *testing.T) {
	dir := copyAdapterFixtureNamed(t, "lsp-adapter")
	facts := filepath.Join("..", "..", "testdata", "fixtures", "lsp-adapter", "facts.json")

	code, out, errOut := run(t, "adapter", "--lsp-facts", facts, "--root", dir, "--json")
	if code != 0 {
		t.Fatalf("adapter exit = %d: %s", code, errOut)
	}
	var stats adapter.Stats
	if err := json.Unmarshal([]byte(out), &stats); err != nil {
		t.Fatalf("adapter output is not JSON: %v\n%s", err, out)
	}
	if stats.Adapter != "lsp" || stats.Matched != 2 || stats.Edges != 2 || stats.Unmatched != 1 {
		t.Fatalf("stats = %+v, want lsp, 2 matched, 2 edges, 1 unmatched", stats)
	}

	code, out, errOut = run(t, "status", "--root", dir, "--json")
	if code != 0 {
		t.Fatalf("status exit = %d: %s", code, errOut)
	}
	var status struct {
		Unresolved int             `json:"unresolved"`
		Adapters   []adapter.Stats `json:"adapters"`
	}
	if err := json.Unmarshal([]byte(out), &status); err != nil {
		t.Fatal(err)
	}
	if status.Unresolved != 2 {
		t.Fatalf("unresolved = %d, want the 2-row refused key", status.Unresolved)
	}
	if len(status.Adapters) != 1 || status.Adapters[0].Adapter != "lsp" {
		t.Fatalf("status adapters = %+v, want lsp", status.Adapters)
	}
}

func TestAdapterCommandLSPFactsErrors(t *testing.T) {
	dir := copyAdapterFixtureNamed(t, "lsp-adapter")
	facts := filepath.Join("..", "..", "testdata", "fixtures", "lsp-adapter", "facts.json")

	if code, _, _ := run(t, "adapter", "--root", dir); code != 2 {
		t.Fatalf("no facts flag exit = %d, want 2", code)
	}
	if code, _, _ := run(t, "adapter", "--scip", "index.scip", "--lsp-facts", facts, "--root", dir); code != 2 {
		t.Fatalf("both flags exit = %d, want 2", code)
	}
	if code, _, _ := run(t, "adapter", "--lsp-facts", filepath.Join(dir, "missing.json"), "--root", dir); code != 3 {
		t.Fatalf("missing facts exit = %d, want 3", code)
	}

	tmp := t.TempDir()
	writeFile(t, tmp, "bad.json", `{"schema":2,"language":"ts"}`)
	if code, _, _ := run(t, "adapter", "--lsp-facts", filepath.Join(tmp, "bad.json"), "--root", dir); code != 1 {
		t.Fatalf("malformed facts exit = %d, want 1", code)
	}

	writeFile(t, tmp, "empty.json", `{"schema":1,"language":"python","definitions":[],"references":[]}`)
	code, out, errOut := run(t, "adapter", "--lsp-facts", filepath.Join(tmp, "empty.json"), "--root", dir, "--json")
	if code != 0 {
		t.Fatalf("empty facts exit = %d: %s", code, errOut)
	}
	var stats adapter.Stats
	if err := json.Unmarshal([]byte(out), &stats); err != nil {
		t.Fatal(err)
	}
	if stats.Adapter != "lsp-python" || stats.Definitions != 0 || stats.Edges != 0 {
		t.Fatalf("empty stats = %+v, want a zero-fact lsp-python run", stats)
	}
}
