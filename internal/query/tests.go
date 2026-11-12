package query

import (
	"context"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/Goutam-11/repath/internal/graph"
	"github.com/Goutam-11/repath/internal/index"
	"github.com/Goutam-11/repath/internal/store"
	"github.com/Goutam-11/repath/internal/testpath"
)

// TestLink is one piece of evidence connecting a test symbol/file to the
// affected target. It is structural, not runtime coverage.
type TestLink struct {
	TestSymbol     string  `json:"test_symbol,omitempty"`
	TestFile       string  `json:"test_file"`
	AffectedSymbol string  `json:"affected_symbol,omitempty"`
	AffectedFile   string  `json:"affected_file,omitempty"`
	Depth          int     `json:"depth"`
	Confidence     float64 `json:"confidence"`
	Rule           string  `json:"rule"`
	CallLine       int     `json:"call_line,omitempty"`
	// ChangedFile is set only in --diff mode: the changed path whose impact
	// pulled this test file in.
	ChangedFile string `json:"changed_file,omitempty"`
}

// TestFile is one test file and the evidence that reaches it.
type TestFile struct {
	File  string     `json:"file"`
	Links []TestLink `json:"links"`
}

// ChangedFile is one changed path in a diff answer.
type ChangedFile struct {
	Path   string `json:"path"`
	Status string `json:"status"`
}

// DiffInfo is the `tests --diff` provenance: the base, every changed file, and
// the ones that could not be mapped to current symbols (deleted or non-code).
type DiffInfo struct {
	Base    string        `json:"base"`
	Changed []ChangedFile `json:"changed"`
	Skipped []ChangedFile `json:"skipped,omitempty"`
}

// TestsResult is the `tests` payload.
type TestsResult struct {
	Targets       []graph.Endpoint `json:"targets"`
	Tests         []TestFile       `json:"tests"`
	Depth         int              `json:"depth"`
	MinConfidence float64          `json:"min_confidence"`
	Truncated     bool             `json:"truncated"`
	Diff          *DiffInfo        `json:"diff,omitempty"`
}

// TestsAnswer carries the resolved target and the test-impact answer.
type TestsAnswer struct {
	Target Target
	Result TestsResult
}

const testScanLimit = 500

// Tests maps the reverse closure of a target to test files. Graph edges are
// primary; path conventions and test imports are conservative fallbacks.
func Tests(ctx context.Context, st *store.Store, arg string, depth int, minConfidence float64, limit int) (TestsAnswer, int, error) {
	target, code, err := ResolveTarget(ctx, st, arg)
	answer := TestsAnswer{Target: target}
	if err != nil || code != 0 {
		return answer, code, err
	}
	if depth <= 0 {
		depth = DefaultDepth
	}
	if limit <= 0 {
		limit = DefaultLimit
	}

	blast, err := graph.Blast(ctx, st, target.Nodes, depth, minConfidence, testScanLimit)
	if err != nil {
		return answer, 1, err
	}
	paths, err := st.FilePaths(ctx)
	if err != nil {
		return answer, 1, err
	}

	byFile := map[string]*TestFile{}
	graphLinked := map[string]bool{} // test file + affected file
	add := func(link TestLink) {
		file := byFile[link.TestFile]
		if file == nil {
			file = &TestFile{File: link.TestFile}
			byFile[link.TestFile] = file
		}
		file.Links = append(file.Links, link)
	}
	affectedFiles := map[string]bool{}
	for _, node := range target.Nodes {
		if p := paths[node.FileID]; p != "" {
			affectedFiles[p] = true
		}
	}
	for _, dependent := range blast.Dependents {
		if dependent.File == "" {
			continue
		}
		affectedFiles[dependent.File] = true
		if !testpath.IsTest(dependent.File) {
			continue
		}
		targetFile := paths[target.Nodes[0].FileID]
		graphLinked[dependent.File+"\x00"+targetFile] = true
		add(TestLink{
			TestSymbol: dependent.QName, TestFile: dependent.File,
			AffectedSymbol: target.Nodes[0].QName, AffectedFile: targetFile,
			Depth: dependent.Depth, Confidence: dependent.Confidence,
			Rule: dependent.Rule, CallLine: dependent.CallLine,
		})
	}

	// Path conventions and test imports only fill files with no graph evidence.
	known := make(map[string]bool, len(paths))
	for _, p := range paths {
		known[p] = true
	}
	for affected := range affectedFiles {
		if testpath.IsTest(affected) {
			continue
		}
		for _, candidate := range conventionTests(affected) {
			if !known[candidate] || !testpath.IsTest(candidate) {
				continue
			}
			if graphLinked[candidate+"\x00"+affected] || byFile[candidate] != nil {
				continue
			}
			add(TestLink{
				TestFile: candidate, AffectedSymbol: target.Nodes[0].QName,
				AffectedFile: affected, Confidence: 0.50, Rule: "testpath.convention",
			})
		}
	}
	imports, err := st.Imports(ctx)
	if err != nil {
		return answer, 1, err
	}
	for _, imp := range imports {
		testFile := paths[imp.FileID]
		if testFile == "" || !testpath.IsTest(testFile) || byFile[testFile] != nil {
			continue
		}
		affected := resolveImportTarget(testFile, imp.Source, known)
		if affected == "" || !affectedFiles[affected] || testpath.IsTest(affected) {
			continue
		}
		add(TestLink{
			TestFile: testFile, AffectedSymbol: target.Nodes[0].QName,
			AffectedFile: affected, Confidence: 0.50, Rule: "testpath.import",
		})
	}

	files := make([]TestFile, 0, len(byFile))
	for _, file := range byFile {
		sort.Slice(file.Links, func(i, j int) bool {
			if file.Links[i].Depth != file.Links[j].Depth {
				return file.Links[i].Depth < file.Links[j].Depth
			}
			return file.Links[i].Rule < file.Links[j].Rule
		})
		files = append(files, *file)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].File < files[j].File })
	result := TestsResult{
		Targets:       blast.Targets,
		Depth:         depth,
		MinConfidence: minConfidence,
		Tests:         make([]TestFile, 0, len(files)),
	}
	if len(files) > limit {
		files = files[:limit]
		result.Truncated = true
	}
	result.Tests = append(result.Tests, files...)
	if blast.Truncated {
		result.Truncated = true
	}
	answer.Result = result
	return answer, 0, nil
}

// conventionTests lists conservative test-file names for one production file.
func conventionTests(production string) []string {
	dir := path.Dir(production)
	base := path.Base(production)
	ext := path.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	if dir == "." {
		dir = ""
	}
	join := func(parts ...string) string {
		return path.Join(append([]string{dir}, parts...)...)
	}
	candidates := []string{
		join(stem + ".test" + ext),
		join(stem + ".spec" + ext),
		join("test_" + stem + ext),
		join(stem + "_test" + ext),
		join("__tests__", base),
		join("__tests__", stem+".test"+ext),
		join("tests", stem+".test"+ext),
		join("test", stem+".test"+ext),
	}
	if ext == ".py" {
		candidates = append(candidates, join("tests", "test_"+stem+".py"), join("test", "test_"+stem+".py"))
	}
	return candidates
}

// resolveImportTarget maps a test file's import source to a known repo file.
// It covers relative TS/JS imports and Python dotted imports; unresolved
// package imports are left external.
func resolveImportTarget(importer, source string, known map[string]bool) string {
	if source == "" {
		return ""
	}
	if strings.HasPrefix(source, ".") {
		base := path.Clean(path.Join(path.Dir(importer), source))
		for _, candidate := range importCandidates(base) {
			if known[candidate] {
				return candidate
			}
		}
		return ""
	}
	// Python dotted import at root, src/, or lib/.
	dotted := strings.ReplaceAll(source, ".", "/")
	for _, prefix := range []string{"", "src/", "lib/"} {
		base := path.Join(prefix, dotted)
		for _, candidate := range []string{base + ".py", base + "/__init__.py"} {
			if known[candidate] {
				return candidate
			}
		}
	}
	return ""
}

func importCandidates(base string) []string {
	base = strings.TrimSuffix(base, ".js")
	base = strings.TrimSuffix(base, ".jsx")
	out := []string{
		base, base + ".ts", base + ".tsx", base + ".js", base + ".jsx",
		base + ".mjs", base + ".cjs", base + ".py",
		path.Join(base, "index.ts"), path.Join(base, "index.tsx"),
		path.Join(base, "index.js"), path.Join(base, "index.jsx"),
	}
	return out
}

// TestsDiff unions the test-impact answer for every changed file. Changed test
// files include themselves (a test that changed must run); production files go
// through the same Tests walker as `repath tests <file>`. The diff itself is
// discovered once by index.ChangedFiles and passed in, so there is no second
// diff walker and no extra git call.
func TestsDiff(ctx context.Context, st *store.Store, base string, changed []index.ChangedFile,
	depth int, minConfidence float64, limit int) (TestsResult, error) {

	if depth <= 0 {
		depth = DefaultDepth
	}
	if limit <= 0 {
		limit = DefaultLimit
	}

	result := TestsResult{
		Targets:       make([]graph.Endpoint, 0),
		Tests:         make([]TestFile, 0),
		Depth:         depth,
		MinConfidence: minConfidence,
		Diff: &DiffInfo{
			Base:    base,
			Changed: make([]ChangedFile, 0, len(changed)),
		},
	}
	result.Diff.Changed = append(result.Diff.Changed, changedFiles(changed)...)

	byFile := make(map[string]*TestFile)
	seenLinks := make(map[string]bool)
	seenTargets := make(map[int64]bool)

	addLink := func(testFile string, link TestLink) {
		key := strings.Join([]string{
			link.TestFile, link.TestSymbol, link.AffectedSymbol, link.AffectedFile,
			link.ChangedFile, strconv.Itoa(link.Depth),
			strconv.FormatFloat(link.Confidence, 'f', 4, 64),
			link.Rule, strconv.Itoa(link.CallLine),
		}, "\x00")
		if seenLinks[key] {
			return
		}
		seenLinks[key] = true
		file := byFile[testFile]
		if file == nil {
			file = &TestFile{File: testFile}
			byFile[testFile] = file
		}
		file.Links = append(file.Links, link)
	}

	for _, file := range changed {
		if file.Status == "deleted" {
			result.Diff.Skipped = append(result.Diff.Skipped, ChangedFile(file))
			continue
		}
		_, found, err := st.FileByPath(ctx, file.Path)
		if err != nil {
			return result, err
		}
		if !found {
			// Not indexed: a non-code file, a binary, or a generated file.
			result.Diff.Skipped = append(result.Diff.Skipped, ChangedFile(file))
			continue
		}

		if testpath.IsTest(file.Path) {
			// A changed test must run even when no production file reaches it.
			addLink(file.Path, TestLink{
				TestFile: file.Path, AffectedFile: file.Path, ChangedFile: file.Path,
				Depth: 0, Confidence: 1, Rule: "testpath.changed",
			})
			continue
		}

		answer, code, err := Tests(ctx, st, file.Path, depth, minConfidence, limit)
		if err != nil {
			return result, err
		}
		if code != 0 {
			result.Diff.Skipped = append(result.Diff.Skipped, ChangedFile(file))
			continue
		}
		for _, target := range answer.Result.Targets {
			if seenTargets[target.ID] {
				continue
			}
			seenTargets[target.ID] = true
			result.Targets = append(result.Targets, target)
		}
		for _, test := range answer.Result.Tests {
			for _, link := range test.Links {
				link.ChangedFile = file.Path
				addLink(test.File, link)
			}
		}
		if answer.Result.Truncated {
			result.Truncated = true
		}
	}

	files := make([]TestFile, 0, len(byFile))
	for _, file := range byFile {
		sort.Slice(file.Links, func(i, j int) bool {
			if file.Links[i].Depth != file.Links[j].Depth {
				return file.Links[i].Depth < file.Links[j].Depth
			}
			if file.Links[i].Rule != file.Links[j].Rule {
				return file.Links[i].Rule < file.Links[j].Rule
			}
			return file.Links[i].ChangedFile < file.Links[j].ChangedFile
		})
		files = append(files, *file)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].File < files[j].File })
	if len(files) > limit {
		files = files[:limit]
		result.Truncated = true
	}
	result.Tests = append(result.Tests, files...)
	return result, nil
}

func changedFiles(files []index.ChangedFile) []ChangedFile {
	out := make([]ChangedFile, 0, len(files))
	for _, file := range files {
		out = append(out, ChangedFile(file))
	}
	return out
}

// EmptyTestsDiff is the explicit clean-diff answer: no targets and no tests,
// but the base is still named so the caller can tell "nothing changed" from
// "nothing reaches this target".
func EmptyTestsDiff(base string, depth int, minConfidence float64) TestsResult {
	if depth <= 0 {
		depth = DefaultDepth
	}
	return TestsResult{
		Targets:       make([]graph.Endpoint, 0),
		Tests:         make([]TestFile, 0),
		Depth:         depth,
		MinConfidence: minConfidence,
		Diff:          &DiffInfo{Base: base, Changed: make([]ChangedFile, 0)},
	}
}
