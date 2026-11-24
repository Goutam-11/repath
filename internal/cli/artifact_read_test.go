package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/Goutam-11/repath/internal/artifact"
)

// artifactReadRepo builds a small TypeScript repo, indexes it, and exports an
// artifact from the same index. The returned root is what the artifact recorded,
// so live and artifact answers can be compared byte-for-byte.
func artifactReadRepo(t *testing.T) (repo, art string) {
	t.Helper()
	repo = t.TempDir()
	writeFile(t, repo, "src/a.ts", "export function add(a: number, b: number): number { return a + b; }\n")
	writeFile(t, repo, "src/b.ts", "import { add } from \"./a\";\nexport function sum(): number { return add(1, 2); }\n")
	art = filepath.Join(t.TempDir(), "artifact")
	if code, _, errOut := run(t, "index", "--root", repo, "--artifact", art, "--json"); code != 0 {
		t.Fatalf("index --artifact exit = %d: %s", code, errOut)
	}
	return repo, art
}

// TestArtifactReadAnswersMatchLiveIndex is the #129 acceptance gate: every read
// command answers byte-identically from the artifact and from the live index the
// artifact was exported from.
func TestArtifactReadAnswersMatchLiveIndex(t *testing.T) {
	repo, art := artifactReadRepo(t)
	cases := [][]string{
		{"find", "add", "--json"},
		{"callers", "add", "--json"},
		{"impact", "add", "--json"},
		{"path", "sum", "add", "--json"},
		{"tests", "add", "--json"},
		{"map", "--json"},
		{"status", "--no-refresh", "--json"},
	}
	for _, args := range cases {
		live := append(append([]string{}, args...), "--root", repo)
		code, liveOut, liveErr := run(t, live...)
		if code != 0 {
			t.Fatalf("%v live exit = %d: %s", args, code, liveErr)
		}
		fromArtifact := append(append([]string{}, args...), "--artifact", art)
		code, artOut, artErr := run(t, fromArtifact...)
		if code != 0 {
			t.Fatalf("%v artifact exit = %d: %s", args, code, artErr)
		}
		if liveOut != artOut {
			t.Errorf("%v: artifact answer differs from the live index\n live: %s\n art:  %s", args, liveOut, artOut)
		}
	}
}

func TestArtifactReadFlowsMatchesLiveIndex(t *testing.T) {
	repo := copyFlowFixture(t, "ts")
	art := filepath.Join(t.TempDir(), "artifact")
	if code, _, errOut := run(t, "index", "--root", repo, "--artifact", art, "--json"); code != 0 {
		t.Fatalf("index --artifact exit = %d: %s", code, errOut)
	}
	code, liveOut, liveErr := run(t, "flows", "start", "--root", repo, "--json")
	if code != 0 {
		t.Fatalf("flows live exit = %d: %s", code, liveErr)
	}
	code, artOut, artErr := run(t, "flows", "start", "--artifact", art, "--json")
	if code != 0 {
		t.Fatalf("flows artifact exit = %d: %s", code, artErr)
	}
	if liveOut != artOut {
		t.Errorf("flows answer differs\n live: %s\n art:  %s", liveOut, artOut)
	}
}

func TestArtifactReadCompressed(t *testing.T) {
	repo, _ := artifactReadRepo(t)
	compressed := filepath.Join(t.TempDir(), "artifact-zstd")
	code, _, errOut := run(t, "index", "--root", repo, "--artifact", compressed, "--compress", "--json")
	if code != 0 {
		t.Fatalf("index --compress exit = %d: %s", code, errOut)
	}
	code, out, errOut := run(t, "find", "add", "--artifact", compressed, "--json")
	if code != 0 {
		t.Fatalf("find on a compressed artifact exit = %d: %s", code, errOut)
	}
	if !strings.Contains(out, `"qname": "add"`) {
		t.Fatalf("find answer = %s", out)
	}
}

func TestArtifactReadDoesNotWriteBesideIt(t *testing.T) {
	_, art := artifactReadRepo(t)
	before := snapshotDir(t, art)
	for _, args := range [][]string{
		{"find", "add", "--artifact", art, "--json"},
		{"callers", "add", "--artifact", art, "--json"},
		{"status", "--artifact", art, "--json"},
		{"map", "--artifact", art, "--json"},
	} {
		if code, _, errOut := run(t, args...); code != 0 {
			t.Fatalf("%v exit = %d: %s", args, code, errOut)
		}
	}
	after := snapshotDir(t, art)
	if strings.Join(before, "\n") != strings.Join(after, "\n") {
		t.Fatalf("artifact directory changed\nbefore: %v\nafter:  %v", before, after)
	}
}

func TestArtifactReadExitCodes(t *testing.T) {
	repo, art := artifactReadRepo(t)

	if code, _, _ := run(t, "find", "add", "--artifact", filepath.Join(t.TempDir(), "missing")); code != 3 {
		t.Fatalf("missing artifact = %d, want 3", code)
	}
	if code, _, _ := run(t, "find", "add", "--artifact="); code != 2 {
		t.Fatalf("empty --artifact = %d, want 2", code)
	}

	// A manifest whose schema is not this binary's is an internal mismatch (1),
	// never "not found".
	stale := filepath.Join(t.TempDir(), "stale")
	copyTreeForTest(t, art, stale)
	mutateManifest(t, stale, func(m map[string]any) { m["schema_version"] = 99 })
	if code, _, _ := run(t, "find", "add", "--artifact", stale); code != 1 {
		t.Fatalf("stale schema = %d, want 1", code)
	}

	// A manifest without its database is missing, not unsupported.
	nodb := filepath.Join(t.TempDir(), "nodb")
	copyTreeForTest(t, art, nodb)
	entries, err := os.ReadDir(nodb)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".sqlite") {
			if err := os.Remove(filepath.Join(nodb, entry.Name())); err != nil {
				t.Fatal(err)
			}
		}
	}
	if code, _, _ := run(t, "find", "add", "--artifact", nodb); code != 3 {
		t.Fatalf("missing database = %d, want 3", code)
	}

	// --artifact on a command that has no read meaning is usage (2).
	for _, args := range [][]string{
		{"check", "--artifact", art},
		{"pull", art, "--root", repo, "--artifact", art},
		{"merge", art, "--out", filepath.Join(t.TempDir(), "merged"), "--artifact", art},
		{"adapter", "--scip", "index.scip", "--artifact", art},
		{"trace", "--file", "cpu.cpuprofile", "--artifact", art},
		{"install", "--artifact", art},
	} {
		if code, _, _ := run(t, args...); code != 2 {
			t.Fatalf("%v = %d, want 2", args, code)
		}
	}

	// A rebuild request cannot be honoured by a read-only artifact.
	if code, _, _ := run(t, "status", "--full", "--artifact", art); code != 2 {
		t.Fatalf("status --full --artifact = %d, want 2", code)
	}
}

// TestArtifactDiffMatchesLiveIndex covers PLAN §4.3: the checkout supplies the
// change set and the artifact supplies the graph, so CI can annotate a PR
// without seeding .repath/.
func TestArtifactDiffMatchesLiveIndex(t *testing.T) {
	dir, base := initImpactDiffRepo(t)
	writeFile(t, dir, "src/a.ts", "export function alpha(): number {\n  return 10;\n}\n")
	art := filepath.Join(t.TempDir(), "artifact")
	if code, _, errOut := run(t, "index", "--root", dir, "--artifact", art, "--json"); code != 0 {
		t.Fatalf("index --artifact exit = %d: %s", code, errOut)
	}

	for _, args := range [][]string{
		{"impact", "--diff", base, "--json"},
		{"tests", "--diff", base, "--json"},
	} {
		live := append(append([]string{}, args...), "--root", dir)
		code, liveOut, liveErr := run(t, live...)
		if code != 0 {
			t.Fatalf("%v live exit = %d: %s", args, code, liveErr)
		}
		fromArtifact := append(append([]string{}, args...), "--root", dir, "--artifact", art)
		code, artOut, artErr := run(t, fromArtifact...)
		if code != 0 {
			t.Fatalf("%v artifact exit = %d: %s", args, code, artErr)
		}
		if liveOut != artOut {
			t.Errorf("%v: artifact diff answer differs\n live: %s\n art:  %s", args, liveOut, artOut)
		}
	}
}

// TestMergedArtifactReadsCrossRepo pins the merged-index motive for #129: the
// xrepo.same_type_as link (0.45) is invisible at the default floor and visible
// with --min-confidence 0.4, from the artifact alone.
func TestMergedArtifactReadsCrossRepo(t *testing.T) {
	tmp := t.TempDir()
	producer := filepath.Join(tmp, "producer")
	writeFile(t, producer, "types.ts", "export interface Widget { id: string; }\n")
	consumer := filepath.Join(tmp, "consumer")
	writeFile(t, consumer, "models.ts", "export interface Widget { label: string; }\n")

	pa := filepath.Join(tmp, "pa")
	ca := filepath.Join(tmp, "ca")
	if code, _, errOut := run(t, "index", "--root", producer, "--artifact", pa, "--json"); code != 0 {
		t.Fatalf("producer artifact exit = %d: %s", code, errOut)
	}
	if code, _, errOut := run(t, "index", "--root", consumer, "--artifact", ca, "--json"); code != 0 {
		t.Fatalf("consumer artifact exit = %d: %s", code, errOut)
	}
	merged := filepath.Join(tmp, "merged")
	if code, _, errOut := run(t, "merge", pa, ca, "--out", merged, "--json"); code != 0 {
		t.Fatalf("merge exit = %d: %s", code, errOut)
	}

	type caller struct {
		QName      string  `json:"qname"`
		File       string  `json:"file"`
		Kind       string  `json:"kind"`
		Confidence float64 `json:"confidence"`
		Rule       string  `json:"rule"`
	}
	var answer struct {
		Symbol struct {
			File string `json:"file"`
		} `json:"symbol"`
		Callers []caller `json:"callers"`
	}
	code, out, errOut := run(t, "callers", "producer/types.ts:1", "--min-confidence", "0.4", "--artifact", merged, "--json")
	if code != 0 {
		t.Fatalf("cross-repo callers exit = %d: %s", code, errOut)
	}
	if err := json.Unmarshal([]byte(out), &answer); err != nil {
		t.Fatalf("callers output: %v\n%s", err, out)
	}
	if answer.Symbol.File != "producer/types.ts" || len(answer.Callers) != 1 {
		t.Fatalf("callers = %+v", answer)
	}
	got := answer.Callers[0]
	if got.File != "consumer/models.ts" || got.Kind != "REFERENCES" || got.Rule != "xrepo.same_type_as" || got.Confidence != 0.45 {
		t.Fatalf("cross-repo caller = %+v", got)
	}

	// The default floor hides the derived link, unchanged.
	code, out, errOut = run(t, "callers", "producer/types.ts:1", "--artifact", merged, "--json")
	if code != 0 {
		t.Fatalf("default-floor callers exit = %d: %s", code, errOut)
	}
	if err := json.Unmarshal([]byte(out), &answer); err != nil {
		t.Fatalf("callers output: %v\n%s", err, out)
	}
	if len(answer.Callers) != 0 {
		t.Fatalf("default floor showed %d callers, want 0", len(answer.Callers))
	}

	var impact struct {
		Dependents []caller `json:"dependents"`
	}
	code, out, errOut = run(t, "impact", "producer/types.ts:1", "--min-confidence", "0.4", "--artifact", merged, "--json")
	if code != 0 {
		t.Fatalf("cross-repo impact exit = %d: %s", code, errOut)
	}
	if err := json.Unmarshal([]byte(out), &impact); err != nil {
		t.Fatalf("impact output: %v\n%s", err, out)
	}
	if len(impact.Dependents) != 1 || impact.Dependents[0].File != "consumer/models.ts" {
		t.Fatalf("impact dependents = %+v", impact.Dependents)
	}
}

// snapshotDir returns a sorted name/size/mtime listing of a directory tree so a
// test can prove a read command did not touch it.
func snapshotDir(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		out = append(out, rel+"|"+info.Mode().String()+"|"+strings.TrimSpace(info.ModTime().UTC().Format("2006-01-02T15:04:05.999999999Z07:00"))+"|"+itoa64(info.Size()))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// mutateManifest rewrites the artifact manifest so a test can make it stale.
func mutateManifest(t *testing.T, dir string, mutate func(map[string]any)) {
	t.Helper()
	path := filepath.Join(dir, artifact.ManifestName)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	mutate(manifest)
	data, err = json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}
