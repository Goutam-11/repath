package query

import (
	"context"
	"sort"

	"github.com/Goutam-11/repath/internal/graph"
	"github.com/Goutam-11/repath/internal/index"
	"github.com/Goutam-11/repath/internal/store"
)

// ImpactDependent is one affected symbol plus the changed file that reached it.
// When two changed files reach the same symbol, the closest (lowest depth, then
// highest confidence) row wins, so the union has one row per symbol.
type ImpactDependent struct {
	graph.Dependent
	ChangedFile string `json:"changed_file"`
}

// ImpactDiffResult is the union of the per-file impact answers for a change set.
// It reuses the impact walker and adds diff provenance; it is not a second diff
// engine.
type ImpactDiffResult struct {
	Diff          *DiffInfo         `json:"diff"`
	Dependents    []ImpactDependent `json:"dependents"`
	Files         []graph.FileHit   `json:"files"`
	Modules       []graph.ModuleHit `json:"modules"`
	Depth         int               `json:"depth"`
	MinConfidence float64           `json:"min_confidence"`
	Truncated     bool              `json:"truncated"`
}

// EmptyImpactDiff is the clean-tree answer. It opens no index and carries the
// base and an empty change set.
func EmptyImpactDiff(base string, depth int, minConfidence float64) ImpactDiffResult {
	if depth <= 0 {
		depth = DefaultDepth
	}
	return ImpactDiffResult{
		Diff:          &DiffInfo{Base: base, Changed: []ChangedFile{}},
		Dependents:    []ImpactDependent{},
		Files:         []graph.FileHit{},
		Modules:       []graph.ModuleHit{},
		Depth:         depth,
		MinConfidence: minConfidence,
	}
}

// ImpactDiff unions the reverse closure of every changed file. A deleted or
// non-indexed path, or an indexed file with no symbols, goes to Diff.Skipped
// instead of failing the whole answer.
func ImpactDiff(ctx context.Context, st *store.Store, base string, changed []index.ChangedFile,
	depth int, minConfidence float64, limit int) (ImpactDiffResult, error) {

	if depth <= 0 {
		depth = DefaultDepth
	}
	if limit <= 0 {
		limit = DefaultLimit
	}
	result := ImpactDiffResult{
		Diff: &DiffInfo{
			Base:    base,
			Changed: make([]ChangedFile, 0, len(changed)),
		},
		Dependents:    make([]ImpactDependent, 0),
		Files:         make([]graph.FileHit, 0),
		Modules:       make([]graph.ModuleHit, 0),
		Depth:         depth,
		MinConfidence: minConfidence,
	}
	result.Diff.Changed = append(result.Diff.Changed, changedFiles(changed)...)

	best := make(map[int64]ImpactDependent)
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
			result.Diff.Skipped = append(result.Diff.Skipped, ChangedFile(file))
			continue
		}
		answer, code, err := Impact(ctx, st, file.Path, depth, minConfidence, limit)
		if err != nil {
			return result, err
		}
		if code != 0 {
			// Indexed but no symbols (for example a file of only imports).
			result.Diff.Skipped = append(result.Diff.Skipped, ChangedFile(file))
			continue
		}
		if answer.Result.Truncated {
			result.Truncated = true
		}
		for _, dependent := range answer.Result.Dependents {
			candidate := ImpactDependent{Dependent: dependent, ChangedFile: file.Path}
			if previous, ok := best[dependent.ID]; !ok || betterImpactDependent(candidate, previous) {
				best[dependent.ID] = candidate
			}
		}
	}

	for _, dependent := range best {
		result.Dependents = append(result.Dependents, dependent)
	}
	sort.Slice(result.Dependents, func(i, j int) bool {
		return betterImpactDependent(result.Dependents[i], result.Dependents[j])
	})
	if len(result.Dependents) > limit {
		result.Dependents = result.Dependents[:limit]
		result.Truncated = true
	}

	files, modules, err := impactDiffRollups(ctx, st, result.Dependents)
	if err != nil {
		return result, err
	}
	result.Files = files
	result.Modules = modules
	return result, nil
}

// betterImpactDependent is the stable order for the union and the winner rule
// when two changed files reach the same symbol: depth, confidence, the changed
// file, then the dependent's own file and qname.
func betterImpactDependent(a, b ImpactDependent) bool {
	if a.Depth != b.Depth {
		return a.Depth < b.Depth
	}
	if a.Confidence != b.Confidence {
		return a.Confidence > b.Confidence
	}
	if a.ChangedFile != b.ChangedFile {
		return a.ChangedFile < b.ChangedFile
	}
	if a.File != b.File {
		return a.File < b.File
	}
	return a.QName < b.QName
}

// impactDiffRollups rebuilds the file and module rollups from the union, so two
// changed files reaching the same symbol count it once.
func impactDiffRollups(ctx context.Context, st *store.Store, dependents []ImpactDependent) ([]graph.FileHit, []graph.ModuleHit, error) {
	fileHits := make(map[string]*graph.FileHit)
	for _, dependent := range dependents {
		hit, ok := fileHits[dependent.File]
		if !ok {
			hit = &graph.FileHit{Path: dependent.File, MaxDepth: dependent.Depth}
			fileHits[dependent.File] = hit
		}
		hit.Symbols++
		if dependent.Depth > hit.MaxDepth {
			hit.MaxDepth = dependent.Depth
		}
	}
	files := make([]graph.FileHit, 0, len(fileHits))
	for _, hit := range fileHits {
		files = append(files, *hit)
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].Symbols != files[j].Symbols {
			return files[i].Symbols > files[j].Symbols
		}
		return files[i].Path < files[j].Path
	})

	paths, err := st.FilePaths(ctx)
	if err != nil {
		return nil, nil, err
	}
	fileID := make(map[string]int64, len(paths))
	for id, path := range paths {
		fileID[path] = id
	}
	moduleOf, moduleNames, err := graph.ModuleIndex(ctx, st)
	if err != nil {
		return nil, nil, err
	}
	moduleCounts := make(map[string]int)
	for _, dependent := range dependents {
		if name := moduleNames[moduleOf[fileID[dependent.File]]]; name != "" {
			moduleCounts[name]++
		}
	}
	modules := make([]graph.ModuleHit, 0, len(moduleCounts))
	for name, count := range moduleCounts {
		modules = append(modules, graph.ModuleHit{Name: name, Symbols: count})
	}
	sort.Slice(modules, func(i, j int) bool {
		if modules[i].Symbols != modules[j].Symbols {
			return modules[i].Symbols > modules[j].Symbols
		}
		return modules[i].Name < modules[j].Name
	})
	return files, modules, nil
}
