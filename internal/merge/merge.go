// Package merge builds one read-only index from several single-repo artifacts.
//
// Identity stays per repo: every input file is namespaced under its repo name,
// so two repos that both declare `User` keep two distinct nodes. Only after all
// parts are copied does the merge link what it can prove: type-like symbols with
// the same qname that are unique inside each part become cross-repo REFERENCES
// at 0.45 (`xrepo.same_type_as`), below the default query floor like xlang links
// (decision 23). Nothing is merged, renamed, or guessed.
package merge

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/Goutam-11/repath/internal/artifact"
	"github.com/Goutam-11/repath/internal/store"
)

// ErrExists means the output directory already carries an artifact; the merge
// never overwrites an existing snapshot.
var ErrExists = errors.New("merge output already contains an artifact")

const (
	// CrossRepoRule names the derived link between same-named types.
	CrossRepoRule = "xrepo.same_type_as"
	// CrossRepoConfidence keeps the link below the default 0.5 query floor, so
	// default answers on a merged index are unchanged (like xlang, decision 23).
	CrossRepoConfidence = 0.45
)

// typeKinds are the node kinds considered a "type" for cross-repo drift links.
var typeKinds = map[string]bool{
	"class": true, "interface": true, "struct": true, "trait": true, "type": true,
}

// Result reports what one merge produced.
type Result struct {
	OutDir         string               `json:"out"`
	DBPath         string               `json:"db"`
	ManifestPath   string               `json:"manifest"`
	Parts          []artifact.MergePart `json:"parts"`
	Files          int                  `json:"files"`
	Nodes          int                  `json:"nodes"`
	Edges          int                  `json:"edges"`
	Pending        int                  `json:"pending"`
	Entrypoints    int                  `json:"entrypoints"`
	CrossRepoEdges int                  `json:"cross_repo_edges"`
}

type nodeKey struct {
	path  string
	qname string
	arity int
}

type part struct {
	info        artifact.MergePart
	st          *store.Store
	cleanup     func()
	files       []store.FileRow
	nodes       []store.NodeRow
	edges       []store.Edge
	pending     []store.PendingRef
	entrypoints []store.Entrypoint
	modules     []store.ModuleRow
	pathByID    map[int64]string
	keyByID     map[int64]nodeKey
	nodesByFile map[int64][]store.NodeRow
	fileIDs     map[int64]int64
	types       map[string]store.NodeRow
}

// Run merges the artifact directories in sources into a new artifact at outDir.
// Inputs are read-only and never modified; the output is a new snapshot with a
// manifest listing each part.
func Run(ctx context.Context, sources []string, outDir, version string) (Result, error) {
	if strings.TrimSpace(outDir) == "" {
		return Result{}, errors.New("merge needs --out DIR")
	}
	if len(sources) == 0 {
		return Result{}, errors.New("merge needs at least one artifact directory")
	}
	if _, err := os.Stat(filepath.Join(outDir, artifact.ManifestName)); err == nil {
		return Result{}, fmt.Errorf("%w: %s", ErrExists, outDir)
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return Result{}, fmt.Errorf("create merge output: %w", err)
	}

	parts, err := openParts(ctx, sources)
	if err != nil {
		return Result{}, err
	}
	defer func() {
		for _, p := range parts {
			if p.cleanup != nil {
				p.cleanup()
			}
		}
	}()
	sort.Slice(parts, func(i, j int) bool { return parts[i].info.Name < parts[j].info.Name })

	dbPath := filepath.Join(outDir, artifact.ArtifactName("merged"))
	st, err := store.Open(ctx, dbPath)
	if err != nil {
		return Result{}, fmt.Errorf("open merged index: %w", err)
	}
	if err := st.Migrate(ctx); err != nil {
		st.Close()
		return Result{}, err
	}

	newRoot := func(prefix, root string) string {
		if root == "" || root == "." {
			return prefix
		}
		return prefix + "/" + root
	}

	var modules []store.Module
	for _, p := range parts {
		for _, m := range p.modules {
			modules = append(modules, store.Module{Name: m.Name, Kind: m.Kind, Root: newRoot(p.info.Name, m.Root)})
		}
	}
	moduleIDs, err := st.UpsertModules(ctx, modules)
	if err != nil {
		st.Close()
		return Result{}, err
	}

	// Copy files, nodes, and pending rows in one transaction. Node ids are
	// reassigned by ReplaceNodes, so edges are inserted after this commit once
	// the new ids are known.
	tx, err := st.Begin(ctx)
	if err != nil {
		st.Close()
		return Result{}, err
	}
	var allFileIDs []int64
	var pendingRefs []store.PendingRef
	for pi := range parts {
		p := &parts[pi]
		rootByModule := make(map[int64]string, len(p.modules))
		for _, m := range p.modules {
			rootByModule[m.ID] = m.Root
		}
		p.fileIDs = make(map[int64]int64, len(p.files))
		for _, f := range p.files {
			var moduleID int64
			if oldRoot, ok := rootByModule[f.ModuleID]; ok {
				moduleID = moduleIDs[newRoot(p.info.Name, oldRoot)]
			}
			id, err := tx.UpsertFile(ctx, store.File{
				Path: p.info.Name + "/" + f.Path, Lang: f.Lang, ModuleID: moduleID,
				Hash: f.Hash, ParseOK: f.ParseOK, Err: f.Err,
			})
			if err != nil {
				tx.Rollback()
				st.Close()
				return Result{}, err
			}
			p.fileIDs[f.ID] = id
			allFileIDs = append(allFileIDs, id)

			var nodes []store.Node
			for _, n := range p.nodesByFile[f.ID] {
				nodes = append(nodes, store.Node{
					Kind: n.Kind, Name: n.Name, QName: n.QName, Arity: n.Arity,
					StartLine: n.StartLine, EndLine: n.EndLine, Exported: n.Exported,
				})
			}
			sort.SliceStable(nodes, func(i, j int) bool {
				if nodes[i].QName != nodes[j].QName {
					return nodes[i].QName < nodes[j].QName
				}
				if nodes[i].Arity != nodes[j].Arity {
					return nodes[i].Arity < nodes[j].Arity
				}
				return nodes[i].StartLine < nodes[j].StartLine
			})
			if _, err := tx.ReplaceNodes(ctx, id, f.Lang, nodes); err != nil {
				tx.Rollback()
				st.Close()
				return Result{}, err
			}
		}
		for _, ref := range p.pending {
			if id, ok := p.fileIDs[ref.FileID]; ok {
				ref.FileID = id
				pendingRefs = append(pendingRefs, ref)
			}
		}
	}
	if err := tx.Commit(); err != nil {
		st.Close()
		return Result{}, err
	}
	// Pending rows are replaced through the store (they are not written by a
	// Tx); every file id is now committed.
	if len(allFileIDs) > 0 {
		sort.SliceStable(pendingRefs, func(i, j int) bool {
			if pendingRefs[i].FileID != pendingRefs[j].FileID {
				return pendingRefs[i].FileID < pendingRefs[j].FileID
			}
			if pendingRefs[i].Name != pendingRefs[j].Name {
				return pendingRefs[i].Name < pendingRefs[j].Name
			}
			if pendingRefs[i].Form != pendingRefs[j].Form {
				return pendingRefs[i].Form < pendingRefs[j].Form
			}
			return pendingRefs[i].Line < pendingRefs[j].Line
		})
		if err := st.ReplacePendingFiles(ctx, allFileIDs, pendingRefs); err != nil {
			st.Close()
			return Result{}, err
		}
	}

	paths, err := st.FilePaths(ctx)
	if err != nil {
		st.Close()
		return Result{}, err
	}
	newNodes, err := st.Nodes(ctx)
	if err != nil {
		st.Close()
		return Result{}, err
	}
	newIDs := make(map[nodeKey]int64, len(newNodes))
	for _, n := range newNodes {
		newIDs[nodeKey{path: paths[n.FileID], qname: n.QName, arity: n.Arity}] = n.ID
	}

	if err := copyEntrypoints(ctx, st, parts, newIDs); err != nil {
		st.Close()
		return Result{}, err
	}

	edges, crossRepo, err := mergedEdges(parts, newIDs)
	if err != nil {
		st.Close()
		return Result{}, err
	}
	if _, err := st.InsertEdges(ctx, edges); err != nil {
		st.Close()
		return Result{}, err
	}

	for _, p := range parts {
		key := "merge.parts." + p.info.Name
		_ = st.SetMeta(ctx, key+".root", p.info.Root)
		_ = st.SetMeta(ctx, key+".commit", p.info.Commit)
	}
	names := make([]string, 0, len(parts))
	for _, p := range parts {
		names = append(names, p.info.Name)
	}
	_ = st.SetMeta(ctx, "repo", "merged")
	_ = st.SetMeta(ctx, "root", "merged")
	_ = st.SetMeta(ctx, "merge.parts", strings.Join(names, ","))
	_ = st.SetMeta(ctx, "merge.cross_repo_edges", strconv.Itoa(crossRepo))

	counts, err := st.Counts(ctx)
	if err != nil {
		st.Close()
		return Result{}, err
	}
	edgeCounts, err := st.EdgeCounts(ctx)
	if err != nil {
		st.Close()
		return Result{}, err
	}
	pending, err := st.CountPending(ctx)
	if err != nil {
		st.Close()
		return Result{}, err
	}
	entryCount, err := st.EntrypointCount(ctx)
	if err != nil {
		st.Close()
		return Result{}, err
	}
	totalEdges := 0
	for _, n := range edgeCounts {
		totalEdges += n
	}
	unresolvedRate := 0.0
	if totalEdges+pending > 0 {
		unresolvedRate = float64(pending) / float64(totalEdges+pending)
	}

	if err := st.Checkpoint(ctx); err != nil {
		st.Close()
		return Result{}, err
	}
	tmp := dbPath + ".tmp"
	_ = os.Remove(tmp)
	if err := st.VacuumInto(ctx, tmp); err != nil {
		st.Close()
		return Result{}, err
	}
	if err := st.Close(); err != nil {
		return Result{}, err
	}
	if err := os.Chmod(tmp, 0o444); err != nil {
		return Result{}, fmt.Errorf("chmod merged artifact: %w", err)
	}
	if err := os.Remove(dbPath); err != nil && !os.IsNotExist(err) {
		return Result{}, err
	}
	if err := os.Rename(tmp, dbPath); err != nil {
		return Result{}, fmt.Errorf("write merged artifact: %w", err)
	}

	manifest := artifact.Manifest{
		SchemaVersion:  store.SchemaVersion,
		ToolVersion:    version,
		Commit:         "merged",
		FileCount:      counts.Files,
		NodeCount:      counts.Nodes,
		EdgeCount:      totalEdges,
		UnresolvedRate: unresolvedRate,
		Format:         "sqlite",
		DBFile:         filepath.Base(dbPath),
		Parts:          partInfos(parts),
	}
	manifestPath := filepath.Join(outDir, artifact.ManifestName)
	if err := artifact.WriteManifest(manifestPath, manifest); err != nil {
		return Result{}, err
	}

	return Result{
		OutDir: outDir, DBPath: dbPath, ManifestPath: manifestPath,
		Parts: manifest.Parts, Files: counts.Files, Nodes: counts.Nodes,
		Edges: totalEdges, Pending: pending, Entrypoints: entryCount,
		CrossRepoEdges: crossRepo,
	}, nil
}

// openParts reads every input artifact and assigns a unique repo name. A name
// comes from the part's recorded repo root, so a merged index stays readable;
// collisions are broken deterministically with the commit prefix.
func openParts(ctx context.Context, sources []string) ([]part, error) {
	used := map[string]bool{}
	var parts []part
	for _, source := range sources {
		st, cleanup, manifest, err := artifact.OpenReadOnly(ctx, source)
		if err != nil {
			for _, p := range parts {
				p.cleanup()
			}
			return nil, err
		}
		meta, err := st.Meta(ctx)
		if err != nil {
			cleanup()
			for _, p := range parts {
				p.cleanup()
			}
			return nil, fmt.Errorf("read %s meta: %w", source, err)
		}
		commit := manifest.Commit
		if commit == "" {
			commit = meta["git_head"]
		}
		name := uniqueName(partBaseName(meta["root"], source), commit, used)
		used[name] = true

		p := part{
			info:    artifact.MergePart{Name: name, Root: meta["root"], Commit: commit},
			st:      st,
			cleanup: cleanup,
		}
		if p.files, err = st.Files(ctx); err != nil {
			cleanup()
			return nil, err
		}
		if p.nodes, err = st.Nodes(ctx); err != nil {
			cleanup()
			return nil, err
		}
		if p.edges, err = st.AllEdges(ctx); err != nil {
			cleanup()
			return nil, err
		}
		if p.pending, err = st.Pending(ctx); err != nil {
			cleanup()
			return nil, err
		}
		if p.entrypoints, err = st.Entrypoints(ctx); err != nil {
			cleanup()
			return nil, err
		}
		if p.modules, err = st.Modules(ctx); err != nil {
			cleanup()
			return nil, err
		}
		if p.pathByID, err = st.FilePaths(ctx); err != nil {
			cleanup()
			return nil, err
		}
		p.keyByID = make(map[int64]nodeKey, len(p.nodes))
		p.nodesByFile = make(map[int64][]store.NodeRow)
		for _, n := range p.nodes {
			p.keyByID[n.ID] = nodeKey{path: p.pathByID[n.FileID], qname: n.QName, arity: n.Arity}
			p.nodesByFile[n.FileID] = append(p.nodesByFile[n.FileID], n)
		}
		// Type-like qnames unique inside the part are the only cross-repo
		// candidates; an ambiguous name is never linked.
		counts := make(map[string]int)
		for _, n := range p.nodes {
			if typeKinds[n.Kind] {
				counts[n.QName]++
			}
		}
		p.types = make(map[string]store.NodeRow)
		for _, n := range p.nodes {
			if typeKinds[n.Kind] && counts[n.QName] == 1 {
				p.types[n.QName] = n
			}
		}
		p.info.Files = len(p.files)
		p.info.Nodes = len(p.nodes)
		p.info.Edges = len(p.edges)
		p.info.Pending = len(p.pending)
		parts = append(parts, p)
	}
	return parts, nil
}

// copyEntrypoints re-anchors each part's entrypoints to the new node ids.
func copyEntrypoints(ctx context.Context, st *store.Store, parts []part, newIDs map[nodeKey]int64) error {
	tx, err := st.Begin(ctx)
	if err != nil {
		return err
	}
	for _, p := range parts {
		byFile := make(map[int64][]store.EntrypointRow)
		eps := append([]store.Entrypoint(nil), p.entrypoints...)
		sort.SliceStable(eps, func(i, j int) bool {
			if eps[i].FileID != eps[j].FileID {
				return eps[i].FileID < eps[j].FileID
			}
			if eps[i].Rule != eps[j].Rule {
				return eps[i].Rule < eps[j].Rule
			}
			return eps[i].Line < eps[j].Line
		})
		for _, ep := range eps {
			key := p.keyByID[ep.NodeID]
			id, ok := newIDs[nodeKey{path: p.info.Name + "/" + key.path, qname: key.qname, arity: key.arity}]
			if !ok {
				continue
			}
			newFile, ok := p.fileIDs[ep.EntrypointRow.FileID]
			if !ok {
				continue
			}
			row := ep.EntrypointRow
			row.NodeID = id
			row.FileID = newFile
			byFile[newFile] = append(byFile[newFile], row)
		}
		fileIDs := make([]int64, 0, len(byFile))
		for fileID := range byFile {
			fileIDs = append(fileIDs, fileID)
		}
		sort.Slice(fileIDs, func(i, j int) bool { return fileIDs[i] < fileIDs[j] })
		for _, fileID := range fileIDs {
			rows := byFile[fileID]
			sort.SliceStable(rows, func(i, j int) bool {
				if rows[i].Rule != rows[j].Rule {
					return rows[i].Rule < rows[j].Rule
				}
				return rows[i].Line < rows[j].Line
			})
			if err := tx.ReplaceEntrypoints(ctx, fileID, rows); err != nil {
				tx.Rollback()
				return err
			}
		}
	}
	return tx.Commit()
}

// mergedEdges remaps every part's edges and adds the cross-repo type links.
func mergedEdges(parts []part, newIDs map[nodeKey]int64) ([]store.Edge, int, error) {
	var out []store.Edge
	for _, p := range parts {
		for _, e := range p.edges {
			sk, ok := p.keyByID[e.Src]
			if !ok {
				continue
			}
			dk, ok := p.keyByID[e.Dst]
			if !ok {
				continue
			}
			src, ok := newIDs[nodeKey{path: p.info.Name + "/" + sk.path, qname: sk.qname, arity: sk.arity}]
			if !ok {
				continue
			}
			dst, ok := newIDs[nodeKey{path: p.info.Name + "/" + dk.path, qname: dk.qname, arity: dk.arity}]
			if !ok {
				continue
			}
			e.Src, e.Dst = src, dst
			e.Candidates = remapCandidates(e.Candidates, p, newIDs)
			out = append(out, e)
		}
	}

	// Cross-repo links: for each ordered pair of parts, a qname that is a
	// unique type in both becomes one REFERENCES edge from the earlier part to
	// the later one. It is below the default floor and never replaces an edge.
	crossRepo := 0
	for i := 0; i < len(parts); i++ {
		for j := i + 1; j < len(parts); j++ {
			qnames := make([]string, 0, len(parts[i].types))
			for qname := range parts[i].types {
				if _, ok := parts[j].types[qname]; ok {
					qnames = append(qnames, qname)
				}
			}
			sort.Strings(qnames)
			for _, qname := range qnames {
				a := parts[i].types[qname]
				b := parts[j].types[qname]
				src, ok := newIDs[nodeKey{path: parts[i].info.Name + "/" + parts[i].pathByID[a.FileID], qname: a.QName, arity: a.Arity}]
				if !ok {
					continue
				}
				dst, ok := newIDs[nodeKey{path: parts[j].info.Name + "/" + parts[j].pathByID[b.FileID], qname: b.QName, arity: b.Arity}]
				if !ok {
					continue
				}
				if src == dst {
					continue
				}
				out = append(out, store.Edge{
					Src: src, Dst: dst, Kind: "REFERENCES",
					Confidence: CrossRepoConfidence, Rule: CrossRepoRule,
				})
				crossRepo++
			}
		}
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Src != out[j].Src {
			return out[i].Src < out[j].Src
		}
		if out[i].Dst != out[j].Dst {
			return out[i].Dst < out[j].Dst
		}
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		if out[i].Rule != out[j].Rule {
			return out[i].Rule < out[j].Rule
		}
		return out[i].Line < out[j].Line
	})
	return out, crossRepo, nil
}

// remapCandidates rewrites an ambiguous group's candidate ids; ids that did not
// survive the merge are dropped, and an empty group is omitted.
func remapCandidates(candidates string, p part, newIDs map[nodeKey]int64) string {
	if candidates == "" {
		return ""
	}
	var kept []string
	for _, field := range strings.Split(candidates, ",") {
		old, err := strconv.ParseInt(strings.TrimSpace(field), 10, 64)
		if err != nil {
			continue
		}
		key, ok := p.keyByID[old]
		if !ok {
			continue
		}
		if id, ok := newIDs[nodeKey{path: p.info.Name + "/" + key.path, qname: key.qname, arity: key.arity}]; ok {
			kept = append(kept, strconv.FormatInt(id, 10))
		}
	}
	return strings.Join(kept, ",")
}

func partInfos(parts []part) []artifact.MergePart {
	out := make([]artifact.MergePart, 0, len(parts))
	for _, p := range parts {
		out = append(out, p.info)
	}
	return out
}

func partBaseName(root, source string) string {
	name := ""
	if root != "" {
		name = filepath.Base(filepath.Clean(root))
		if name == "." || name == string(filepath.Separator) {
			name = ""
		}
	}
	if name == "" {
		name = filepath.Base(filepath.Clean(source))
		if name == "." || name == string(filepath.Separator) || name == "" {
			name = "part"
		}
	}
	name = strings.Map(func(r rune) rune {
		switch r {
		case '/', '\\', ':', ' ':
			return '-'
		}
		return r
	}, name)
	return name
}

func uniqueName(base, commit string, used map[string]bool) string {
	if base == "" {
		base = "part"
	}
	if !used[base] {
		return base
	}
	short := commit
	if len(short) > 7 {
		short = short[:7]
	}
	if short != "" && !used[base+"-"+short] {
		return base + "-" + short
	}
	for n := 2; ; n++ {
		candidate := base + "-" + strconv.Itoa(n)
		if !used[candidate] {
			return candidate
		}
	}
}
