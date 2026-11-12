package graph

import (
	"context"
	"sort"

	"github.com/Goutam-11/repath/internal/store"
)

// Endpoint is a symbol reduced to what reports need.
type Endpoint struct {
	ID    int64  `json:"id"`
	QName string `json:"qname"`
	Kind  string `json:"kind"`
	File  string `json:"file"`
	Line  int    `json:"line"`
}

// Dependent is one symbol that transitively depends on the target.
type Dependent struct {
	Endpoint
	Depth      int     `json:"depth"`
	EdgeKind   string  `json:"edge_kind"`
	Confidence float64 `json:"confidence"`
	Rule       string  `json:"rule"`
	CallLine   int     `json:"call_line"`
}

// FileHit is one affected file and how many dependents it holds.
type FileHit struct {
	Path     string `json:"path"`
	Symbols  int    `json:"symbols"`
	MaxDepth int    `json:"max_depth"`
}

// ModuleHit is the module rollup of affected files.
type ModuleHit struct {
	Name    string `json:"name"`
	Symbols int    `json:"symbols"`
}

// Impact is the blast radius of a change.
type Impact struct {
	Targets    []Endpoint  `json:"targets"`
	Dependents []Dependent `json:"dependents"`
	Files      []FileHit   `json:"files"`
	Modules    []ModuleHit `json:"modules"`
	Depth      int         `json:"depth"`
	Truncated  bool        `json:"truncated"`
}

// Blast follows reverse edges from the seeds up to maxDepth, keeping at most
// limit dependents. It never revisits a symbol, so cycles are safe.
func Blast(ctx context.Context, st *store.Store, seeds []store.NodeRow, maxDepth int, minConfidence float64, limit int) (Impact, error) {
	paths, err := st.FilePaths(ctx)
	if err != nil {
		return Impact{}, err
	}
	moduleOf, moduleNames, err := moduleLookup(ctx, st)
	if err != nil {
		return Impact{}, err
	}

	result := Impact{Depth: maxDepth}
	visited := make(map[int64]bool, len(seeds))
	frontier := make([]int64, 0, len(seeds))
	for _, seed := range seeds {
		if visited[seed.ID] {
			continue
		}
		visited[seed.ID] = true
		frontier = append(frontier, seed.ID)
		result.Targets = append(result.Targets, endpointOf(seed, paths))
	}

	fileCounts := make(map[string]*FileHit)
	moduleCounts := make(map[string]int)

	for depth := 1; depth <= maxDepth && len(frontier) > 0; depth++ {
		edges, err := st.Incoming(ctx, frontier, minConfidence)
		if err != nil {
			return result, err
		}

		next := make([]int64, 0, len(edges))
		for _, e := range edges {
			if visited[e.Src] {
				continue
			}
			visited[e.Src] = true

			result.Dependents = append(result.Dependents, Dependent{
				Endpoint:   endpointOf(e.SrcNode, paths),
				Depth:      depth,
				EdgeKind:   e.Kind,
				Confidence: e.Confidence,
				Rule:       e.Rule,
				CallLine:   e.Line,
			})
			next = append(next, e.Src)

			hit, ok := fileCounts[e.SrcPath]
			if !ok {
				hit = &FileHit{Path: e.SrcPath, MaxDepth: depth}
				fileCounts[e.SrcPath] = hit
			}
			hit.Symbols++
			if depth > hit.MaxDepth {
				hit.MaxDepth = depth
			}
			if name := moduleNames[moduleOf[e.SrcNode.FileID]]; name != "" {
				moduleCounts[name]++
			}

			if len(result.Dependents) >= limit {
				result.Truncated = true
				break
			}
		}

		frontier = next
		if result.Truncated {
			break
		}
	}

	for _, hit := range fileCounts {
		result.Files = append(result.Files, *hit)
	}
	sort.Slice(result.Files, func(i, j int) bool {
		if result.Files[i].Symbols != result.Files[j].Symbols {
			return result.Files[i].Symbols > result.Files[j].Symbols
		}
		return result.Files[i].Path < result.Files[j].Path
	})

	for name, count := range moduleCounts {
		result.Modules = append(result.Modules, ModuleHit{Name: name, Symbols: count})
	}
	sort.Slice(result.Modules, func(i, j int) bool {
		if result.Modules[i].Symbols != result.Modules[j].Symbols {
			return result.Modules[i].Symbols > result.Modules[j].Symbols
		}
		return result.Modules[i].Name < result.Modules[j].Name
	})

	return result, nil
}

func endpointOf(n store.NodeRow, paths map[int64]string) Endpoint {
	return Endpoint{ID: n.ID, QName: n.QName, Kind: n.Kind, File: paths[n.FileID], Line: n.StartLine}
}

// moduleLookup returns fileID → moduleID and moduleID → name.
func moduleLookup(ctx context.Context, st *store.Store) (map[int64]int64, map[int64]string, error) {
	fileModules, err := st.FileModules(ctx)
	if err != nil {
		return nil, nil, err
	}
	modules, err := st.Modules(ctx)
	if err != nil {
		return nil, nil, err
	}
	names := make(map[int64]string, len(modules))
	for _, m := range modules {
		names[m.ID] = m.Name
	}
	return fileModules, names, nil
}
