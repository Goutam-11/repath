// Package resolve turns per-file references into graph edges. It implements the
// confidence ladder from PLAN.md §5: explicit imports first, then local scope,
// then unique global names, then ambiguity.
package resolve

import (
	"context"
	"path"
	"sort"
	"strings"

	"github.com/Goutam-11/repath/internal/lang/facts"
	"github.com/Goutam-11/repath/internal/store"
)

// maxCandidates caps ambiguity: beyond this the reference is left unresolved
// rather than producing an edge for every same-named method in the repo.
const maxCandidates = 5

// Resolution is the outcome of resolving one reference.
type Resolution struct {
	Dst        int64
	Confidence float64
	Rule       string
	Candidates []int64
	// Kind overrides the edge kind when the resolution changes it: a Python
	// call whose target is a class is stored as REFERENCES, the analogue of
	// TypeScript's `new`.
	Kind     string
	External bool
	Found    bool
}

// Index is the global name index used for resolution.
type Index struct {
	byFile  map[int64]map[string][]store.NodeRow
	global  map[string][]store.NodeRow
	byQName map[string][]store.NodeRow
	exports map[int64][]store.NodeRow
	files   map[string]int64
	paths   map[int64]string
	imports map[int64]map[string]facts.Import
	crates  []rustCrate
	// crateByName maps a workspace member's crate name (underscored, as `use`
	// paths spell it) to its source root. Only local members are listed; a
	// registry dependency has no source here and stays external.
	crateByName map[string]string
	aliases     *Aliases
	root        string

	// Python inheritance. bases holds the resolved superclass links per class
	// node id: fresh links for the files this run re-resolved, persisted
	// EXTENDS edges for every other class. classByID maps a class node back to
	// its symbol, and mroCache memoizes one linearization per class. All three
	// stay nil unless this run resolves Python.
	bases     map[int64][]int64
	classByID map[int64]store.NodeRow
	mroCache  map[int64][]int64

	// Rust trait impls. rustImpls holds the IMPLEMENTS links per type node id:
	// fresh links for the files this run re-resolved, persisted edges for every
	// other type. rustTypeByID maps an id back to its symbol. Both stay nil
	// unless this run resolves Rust, so the other ladders never pay for them.
	rustImpls    map[int64][]int64
	rustTypeByID map[int64]store.NodeRow

	// A targeted run loads imports only for the files it re-resolves. Python,
	// Rust, and TypeScript re-export chains continue through unchanged modules,
	// so the remaining bindings are loaded lazily, at most once per file, from
	// the imports_file index. The TypeScript barrel walk is bounded and only
	// touches the modules a reference actually reaches, which is what keeps the
	// Kibana incremental path proportional to the ten changed files.
	ctx      context.Context
	store    *store.Store
	loads    int
	allLoads bool
	loadErr  error

	// TypeScript barrel re-exports. A named re-export is a public binding keyed
	// by its exported local name; `export *` bindings are kept as an ordered
	// list because one barrel may export-star several modules and the
	// last-wins imports map would silently drop all but the last.
	reexports     map[int64]map[string]facts.Import
	starReexports map[int64][]facts.Import

	// Workspace packages from package.json `workspaces` (local manifests only).
	// The map is built on the first non-relative specifier a TypeScript file
	// resolves, so a run whose imports are all relative pays nothing.
	workspaces       map[string]workspacePackage
	workspacesLoaded bool
}

// lazyImportBulkThreshold bounds the per-file reads a warm run does before one
// bulk read replaces them. The closure a ten-file edit reaches is normally far
// smaller; the cap keeps a pathological chain from issuing thousands of small
// queries.
const lazyImportBulkThreshold = 256

// importsAllThreshold decides when a targeted import read is not worth the
// extra query: a full scan loads 305k rows on the real tree, while the
// imports_file index makes a ten-file read proportional to those ten files.
const importsAllThreshold = 2000

// Build loads every symbol, file, and import binding into the resolution index.
// importsFor limits import bindings to the files whose references this run will
// resolve; nil (or a large set) loads every binding.
func Build(ctx context.Context, st *store.Store, root string, importsFor map[int64]bool) (*Index, error) {
	nodes, err := st.ResolveNodes(ctx)
	if err != nil {
		return nil, err
	}
	paths, err := st.FilePaths(ctx)
	if err != nil {
		return nil, err
	}
	var imports []store.ImportRow
	if importsFor == nil || len(importsFor) > importsAllThreshold {
		imports, err = st.Imports(ctx)
	} else {
		ids := make([]int64, 0, len(importsFor))
		for id := range importsFor {
			ids = append(ids, id)
		}
		imports, err = st.ImportsByFiles(ctx, ids)
	}
	if err != nil {
		return nil, err
	}

	crates := loadRustCrates(root, paths)
	ix := &Index{
		byFile:        make(map[int64]map[string][]store.NodeRow),
		global:        make(map[string][]store.NodeRow),
		byQName:       make(map[string][]store.NodeRow),
		exports:       make(map[int64][]store.NodeRow),
		files:         make(map[string]int64, len(paths)),
		paths:         paths,
		imports:       make(map[int64]map[string]facts.Import),
		reexports:     make(map[int64]map[string]facts.Import),
		starReexports: make(map[int64][]facts.Import),
		crates:        crates,
		crateByName:   rustCratesByName(crates),
		aliases:       LoadAliases(root),
		root:          root,
		ctx:           ctx,
	}
	// Only a targeted import read leaves files unloaded; when every binding is
	// already in memory the lazy loader has nothing to do.
	if importsFor != nil && len(importsFor) <= importsAllThreshold {
		ix.store = st
	}

	for id, p := range paths {
		ix.files[p] = id
	}
	// byFile is only read for the file whose reference is being resolved, so an
	// incremental run needs it for the changed files only. The other maps are
	// global resolution inputs and cover every symbol.
	byFileFor := importsFor
	if importsFor == nil || len(importsFor) > importsAllThreshold {
		byFileFor = nil
	}
	for _, n := range nodes {
		name := n.Name
		if name == "" || n.QName == "" {
			continue
		}
		if byFileFor == nil || byFileFor[n.FileID] {
			if ix.byFile[n.FileID] == nil {
				ix.byFile[n.FileID] = make(map[string][]store.NodeRow)
			}
			ix.byFile[n.FileID][name] = append(ix.byFile[n.FileID][name], n)
		}
		ix.global[name] = append(ix.global[name], n)
		ix.byQName[n.QName] = append(ix.byQName[n.QName], n)
		if n.Exported {
			ix.exports[n.FileID] = append(ix.exports[n.FileID], n)
		}
	}
	for _, imp := range imports {
		ix.addImport(imp)
	}

	// A cold or full run re-resolves every file, so its own extends refs are the
	// complete answer; anything else needs the persisted links for classes it
	// did not touch.
	allFresh := importsFor != nil && len(importsFor) >= len(paths)
	ix.initInheritance(nodes, importsFor)
	if ix.bases != nil && !allFresh {
		if err := ix.loadPersistedBases(ctx, st); err != nil {
			return nil, err
		}
	}
	ix.initRustImpls(nodes, importsFor)
	if ix.rustImpls != nil && !allFresh {
		if err := ix.loadPersistedRustImpls(ctx, st); err != nil {
			return nil, err
		}
	}
	return ix, nil
}

// addImport stores one binding. A plain import creates a local name, so it
// goes in the imports map. A public binding is a re-export: Python and Rust
// `from x import y` / `pub use` also create a local name, but a TypeScript
// `export { y } from "./x"` does not, so its row feeds only the barrel tables.
func (ix *Index) addImport(imp store.ImportRow) {
	binding := facts.Import{
		Local: imp.Local, Imported: imp.Imported, Source: imp.Source,
		Line: imp.Line, Public: imp.Public,
	}
	if !imp.Public {
		if ix.imports[imp.FileID] == nil {
			ix.imports[imp.FileID] = make(map[string]facts.Import)
		}
		ix.imports[imp.FileID][imp.Local] = binding
		return
	}
	if !strings.HasSuffix(ix.paths[imp.FileID], ".py") && !strings.HasSuffix(ix.paths[imp.FileID], ".rs") {
		// TypeScript re-export: resolution input for the barrel walk only.
		if imp.Imported == "*" && imp.Local == "*" {
			// `export * from "./x"`: keep every one, in declaration order.
			ix.starReexports[imp.FileID] = append(ix.starReexports[imp.FileID], binding)
			return
		}
		if ix.reexports[imp.FileID] == nil {
			ix.reexports[imp.FileID] = make(map[string]facts.Import)
		}
		ix.reexports[imp.FileID][imp.Local] = binding
		return
	}
	// Python and Rust re-exports are ordinary bindings too.
	if ix.imports[imp.FileID] == nil {
		ix.imports[imp.FileID] = make(map[string]facts.Import)
	}
	ix.imports[imp.FileID][imp.Local] = binding
	if imp.Imported == "*" && imp.Local == "*" {
		ix.starReexports[imp.FileID] = append(ix.starReexports[imp.FileID], binding)
		return
	}
	if ix.reexports[imp.FileID] == nil {
		ix.reexports[imp.FileID] = make(map[string]facts.Import)
	}
	ix.reexports[imp.FileID][imp.Local] = binding
}

// importsOf returns a file's bindings, loading them on first use. A warm run
// preloads the files it re-resolves, but a re-export chain continues through
// modules it did not touch: without their bindings the chain stops at a module
// boundary and a reference the cold graph classified becomes pending. Each file
// is read at most once, so the extra work is proportional to the chain actually
// walked, not to the tree; after lazyImportBulkThreshold files one full read
// replaces the remaining lookups. The load error is reported through Err so the
// caller can abort before writing a partial result.
func (ix *Index) importsOf(fileID int64) map[string]facts.Import {
	if imports, ok := ix.imports[fileID]; ok {
		return imports
	}
	if ix.store == nil || ix.loadErr != nil {
		return nil
	}
	if ix.allLoads {
		return nil
	}
	if ix.loads >= lazyImportBulkThreshold {
		ix.loadAllImports()
		return ix.imports[fileID]
	}
	rows, err := ix.store.ImportsByFiles(ix.ctx, []int64{fileID})
	if err != nil {
		ix.loadErr = err
		return nil
	}
	ix.loads++
	if len(rows) == 0 {
		// Cache the empty answer so a second lookup does not re-read.
		ix.imports[fileID] = map[string]facts.Import{}
		return ix.imports[fileID]
	}
	for _, imp := range rows {
		ix.addImport(imp)
	}
	return ix.imports[fileID]
}

// loadAllImports is the bounded fallback: one query instead of an unbounded
// number of per-file reads.
func (ix *Index) loadAllImports() {
	if ix.allLoads || ix.loadErr != nil {
		return
	}
	rows, err := ix.store.Imports(ix.ctx)
	if err != nil {
		ix.loadErr = err
		return
	}
	for _, imp := range rows {
		// A file already loaded lazily already has its bindings; adding them
		// twice would duplicate the ordered `export *` list.
		if _, loaded := ix.imports[imp.FileID]; loaded {
			continue
		}
		ix.addImport(imp)
	}
	ix.allLoads = true
}

// Err reports a failed lazy import read. Resolution returns empty results after
// such a failure, so the caller must check before persisting the run.
func (ix *Index) Err() error { return ix.loadErr }

// Resolve returns where ref points. The file's own imports come from the index,
// so a reference in an unchanged file still resolves after an incremental run.
func (ix *Index) Resolve(fromFile int64, fromPath string, ref facts.Ref) Resolution {
	switch ix.language(fromFile) {
	case "python":
		return ix.resolvePython(fromFile, fromPath, ref)
	case "rust":
		return ix.resolveRust(fromFile, fromPath, ref)
	}
	return ix.resolveTS(fromFile, fromPath, ref, ix.imports[fromFile])
}

// resolveTS is the TypeScript/JavaScript ladder.
func (ix *Index) resolveTS(fromFile int64, fromPath string, ref facts.Ref, imports map[string]facts.Import) Resolution {
	// Builtins are not symbols in the repo; treating them as external keeps the
	// unresolved queue meaningful.
	if ref.Form == "ident" && isBuiltin(ref.Name) {
		return Resolution{External: true}
	}
	if ref.Form == "member" && isBuiltin(ref.Receiver) {
		return Resolution{External: true}
	}

	// 1. this.method() inside a class: exact qname, no guessing.
	if ref.Form == "this" {
		qname := classOf(ref.Enclosing) + "." + ref.Name
		for _, n := range ix.byFile[fromFile][ref.Name] {
			if n.QName == qname {
				return Resolution{Dst: n.ID, Confidence: 0.90, Rule: "ts.this.method", Found: true}
			}
		}
		return Resolution{}
	}

	// 2. A local binding from an import statement is the strongest signal.
	if ref.Form == "ident" {
		if binding, ok := imports[ref.Name]; ok {
			return ix.byImport(fromPath, binding, ref)
		}
	}

	// 3. ns.member() where ns is a namespace import, including a static
	// `const ns = require("./x")` binding. The member may itself be a barrel
	// re-export, so the hop rule applies instead of the direct namespace rule.
	if ref.Form == "member" && ref.Receiver != "" {
		if binding, ok := imports[ref.Receiver]; ok && binding.Imported == "*" {
			target, workspace := ix.specifierTarget(fromPath, binding.Source)
			if target == 0 {
				return Resolution{External: true}
			}
			n, hop := ix.tsExport(target, ref.Name, nil, 0)
			switch hop {
			case tsExportDirect:
				if workspace {
					return Resolution{Dst: n.ID, Confidence: 0.95, Rule: "ts.import.workspace", Found: true}
				}
				return Resolution{Dst: n.ID, Confidence: 0.95, Rule: "ts.import.namespace", Found: true}
			case tsExportReexport, tsExportStar:
				return Resolution{Dst: n.ID, Confidence: tsHopConfidence(hop), Rule: tsReexportRule(workspace, hop == tsExportStar), Found: true}
			case tsExportExternal:
				return Resolution{External: true}
			}
		}
	}

	// 4. Typed receivers: cart.total() where cart is "const cart = new Cart()"
	// or a parameter declared as Cart. Without a type there is no honest guess,
	// so a bare obj.m() stays unresolved rather than matching "m" repo-wide.
	if ref.Form == "member" {
		if ref.ReceiverType != "" {
			return ix.byTypedMethod(fromFile, fromPath, ref, imports)
		}
		return Resolution{}
	}

	// 5. Identifiers: local scope, then global uniqueness.
	if nodes := ix.byFile[fromFile][ref.Name]; len(nodes) > 0 {
		if picked := pick(nodes, ref.Arity); len(picked) == 1 {
			return Resolution{Dst: picked[0].ID, Confidence: 0.85, Rule: "ts.local", Found: true}
		}
	}

	return ix.byUnique(ref.Name, ref.Arity, ix.global[ref.Name], "ts.global", tsUniqueConfidence)
}

// resolvePython is the Python ladder. Import bindings are exact (1.00), module
// receivers and relative imports are next, then local scope, then uniqueness.
func (ix *Index) resolvePython(fromFile int64, fromPath string, ref facts.Ref) Resolution {
	if ref.Form == "ident" && isPyBuiltin(ref.Name) {
		return Resolution{External: true}
	}
	if ref.Form == "member" && isPyBuiltin(ref.Receiver) {
		return Resolution{External: true}
	}

	imports := ix.importsOf(fromFile)

	// 1. self.method(), cls.method(), and bounded super().method() inside a
	// class body. The direct method wins; otherwise the base classes are walked
	// in MRO order, which is what Python's attribute lookup does.
	if ref.Form == "this" || ref.Form == "super" {
		return ix.pyByThis(fromFile, ref)
	}

	// 2. A binding created by an import statement.
	if ref.Form == "ident" {
		if binding, ok := imports[ref.Name]; ok {
			return ix.pyByImport(fromPath, binding, ref)
		}
		if r, ok := ix.pyByStar(fromFile, fromPath, ref); ok {
			return r
		}
	}

	// 3. Module receivers (pkg.f(), util.f(), from . import util) and typed
	// receivers (service.f() where service is "Service(...)" or typed).
	if ref.Form == "member" {
		if r, ok := ix.pyPropertyAlias(fromFile, ref); ok {
			return r
		}
		if r, ok := ix.pyByModuleMember(fromFile, fromPath, ref, imports); ok {
			return r
		}
		if r, ok := ix.pyByClassMember(fromFile, fromPath, ref, imports); ok {
			return r
		}
		if ref.ReceiverType != "" {
			if isPyBuiltin(ref.ReceiverType) {
				return Resolution{External: true}
			}
			return ix.pyByTypedMethod(fromFile, fromPath, ref, imports)
		}
		return Resolution{}
	}

	// 4. Local scope, then global uniqueness.
	if nodes := ix.byFile[fromFile][ref.Name]; len(nodes) > 0 {
		if picked := pick(nodes, ref.Arity); len(picked) == 1 {
			return pyTarget(ref, picked[0], 0.85, "py.local")
		}
	}

	// A bare name with one repo-wide definition and no import, scope, or
	// receiver evidence is the weakest rung (#97): 3.1% precision in the django
	// sample, so it stays below the default floor.
	return ix.byUniquePython(ref, ix.global[ref.Name], 0.40)
}

// maxMRODepth bounds the Python inheritance walk. Real hierarchies are shallow,
// and a cycle or a pathological chain must not make resolution unbounded.
const maxMRODepth = 8

// ClassLink is one superclass link resolved in the current run. The indexer
// resolves every extends reference first and hands the links over before any
// other reference is resolved, because a self/cls call may need a base class
// defined in a file this run did not re-resolve.
type ClassLink struct {
	Src int64 // class node
	Dst int64 // base class node
}

// UsesInheritance reports whether this run built the Python MRO inputs. The
// indexer skips its extends pre-pass when it did not.
func (ix *Index) UsesInheritance() bool { return ix.bases != nil }

// initInheritance prepares the Python MRO inputs, and only for a run that will
// resolve a Python file: loading the persisted base links is an edges read, so
// a TypeScript-only incremental run must not pay for it.
func (ix *Index) initInheritance(nodes []store.NodeRow, importsFor map[int64]bool) {
	pythonRun := false
	if importsFor == nil {
		for _, path := range ix.paths {
			if strings.HasSuffix(path, ".py") {
				pythonRun = true
				break
			}
		}
	} else {
		for id := range importsFor {
			if strings.HasSuffix(ix.paths[id], ".py") {
				pythonRun = true
				break
			}
		}
	}
	if !pythonRun {
		return
	}
	ix.bases = make(map[int64][]int64)
	ix.classByID = make(map[int64]store.NodeRow)
	for _, n := range nodes {
		if n.Kind != "class" || !strings.HasSuffix(ix.paths[n.FileID], ".py") {
			continue
		}
		ix.classByID[n.ID] = n
	}
}

// loadPersistedBases fills in the superclass links of every class this run will
// not re-resolve. Without them an incremental run stops at the first base
// defined in an unchanged file and the warm graph disagrees with a cold one.
// Compiler facts are skipped on purpose: the MRO is the tree-sitter answer, so
// an adapter-augmented index stays comparable with a clean rebuild.
func (ix *Index) loadPersistedBases(ctx context.Context, st *store.Store) error {
	pairs, err := st.UnambiguousEdgePairs(ctx, "EXTENDS")
	if err != nil {
		return err
	}
	for _, p := range pairs {
		if strings.HasPrefix(p.Rule, "scip.") {
			continue
		}
		if _, ok := ix.classByID[p.Src]; !ok {
			continue
		}
		if _, ok := ix.classByID[p.Dst]; !ok {
			continue
		}
		ix.bases[p.Src] = append(ix.bases[p.Src], p.Dst)
	}
	return nil
}

// AddInheritance replaces the persisted superclass links of the classes in the
// files this run re-resolved with the links it just resolved. Replacing, not
// merging, is what keeps an incremental graph identical to a cold one: the
// fresh resolution is the answer a full index would produce (#49).
func (ix *Index) AddInheritance(written []int64, links []ClassLink) {
	if ix.bases == nil {
		return
	}
	fresh := make(map[int64]bool, len(written))
	for _, id := range written {
		fresh[id] = true
	}
	for classID, cls := range ix.classByID {
		if fresh[cls.FileID] {
			ix.bases[classID] = nil
		}
	}
	for _, link := range links {
		if _, ok := ix.classByID[link.Src]; !ok {
			continue
		}
		if _, ok := ix.classByID[link.Dst]; !ok {
			continue
		}
		ix.bases[link.Src] = append(ix.bases[link.Src], link.Dst)
	}
	ix.mroCache = nil
}

// UsesRustImpls reports whether this run built the Rust IMPLEMENTS inputs. The
// indexer resolves implements references first when it did.
func (ix *Index) UsesRustImpls() bool { return ix.rustImpls != nil }

// ImplLink is one Type -> Trait link resolved in the current run. It is
// resolved before other references so a typed call can follow a trait's default
// method without depending on edge-write order.
type ImplLink struct {
	Src int64 // implementing type node
	Dst int64 // trait node
}

// initRustImpls prepares the Rust trait lookup inputs, and only for a run that
// will resolve a Rust file: loading persisted links is an edges read.
func (ix *Index) initRustImpls(nodes []store.NodeRow, importsFor map[int64]bool) {
	rustRun := false
	if importsFor == nil {
		for _, p := range ix.paths {
			if strings.HasSuffix(p, ".rs") {
				rustRun = true
				break
			}
		}
	} else {
		for id := range importsFor {
			if strings.HasSuffix(ix.paths[id], ".rs") {
				rustRun = true
				break
			}
		}
	}
	if !rustRun {
		return
	}
	ix.rustImpls = make(map[int64][]int64)
	ix.rustTypeByID = make(map[int64]store.NodeRow)
	for _, n := range nodes {
		if !strings.HasSuffix(ix.paths[n.FileID], ".rs") {
			continue
		}
		switch n.Kind {
		case "struct", "enum", "trait", "type":
			ix.rustTypeByID[n.ID] = n
		}
	}
}

// loadPersistedRustImpls fills in the trait links of types this run will not
// re-resolve, so a warm run can follow a trait default method defined with the
// unchanged type. Compiler facts are skipped: the tree-sitter answer stays
// comparable with a clean rebuild.
func (ix *Index) loadPersistedRustImpls(ctx context.Context, st *store.Store) error {
	pairs, err := st.UnambiguousEdgePairs(ctx, "IMPLEMENTS")
	if err != nil {
		return err
	}
	for _, p := range pairs {
		if strings.HasPrefix(p.Rule, "scip.") {
			continue
		}
		if _, ok := ix.rustTypeByID[p.Src]; !ok {
			continue
		}
		if _, ok := ix.rustTypeByID[p.Dst]; !ok {
			continue
		}
		ix.rustImpls[p.Src] = append(ix.rustImpls[p.Src], p.Dst)
	}
	return nil
}

// AddRustImpls replaces the persisted trait links of the types in the files
// this run re-resolved with the links it just resolved. Replacing, not
// merging, keeps an incremental graph identical to a cold one (#49).
func (ix *Index) AddRustImpls(written []int64, links []ImplLink) {
	if ix.rustImpls == nil {
		return
	}
	fresh := make(map[int64]bool, len(written))
	for _, id := range written {
		fresh[id] = true
	}
	for id, node := range ix.rustTypeByID {
		if fresh[node.FileID] {
			ix.rustImpls[id] = nil
		}
	}
	for _, link := range links {
		if _, ok := ix.rustTypeByID[link.Src]; !ok {
			continue
		}
		if _, ok := ix.rustTypeByID[link.Dst]; !ok {
			continue
		}
		ix.rustImpls[link.Src] = append(ix.rustImpls[link.Src], link.Dst)
	}
}

// mro returns the class linearization used for self/cls/super and typed
// lookups: the class itself first, then its ancestors in C3 order where the
// hierarchy permits it, with a deterministic depth-first fallback for an
// inconsistent or cyclic hierarchy. The result is capped at maxMRODepth.
func (ix *Index) mro(classID int64) []int64 {
	if ix.bases == nil {
		return nil
	}
	if order, ok := ix.mroCache[classID]; ok {
		return order
	}
	order := ix.linearize(classID, make(map[int64]bool))
	if len(order) > maxMRODepth {
		order = order[:maxMRODepth]
	}
	if ix.mroCache == nil {
		ix.mroCache = make(map[int64][]int64, 8)
	}
	ix.mroCache[classID] = order
	return order
}

func (ix *Index) linearize(id int64, active map[int64]bool) []int64 {
	if id == 0 || active[id] {
		return nil
	}
	if order, ok := ix.mroCache[id]; ok {
		return order
	}
	active[id] = true
	direct := make([]int64, 0, len(ix.bases[id]))
	seqs := make([][]int64, 0, len(ix.bases[id])+1)
	for _, base := range ix.bases[id] {
		if base == id || base == 0 {
			continue
		}
		direct = append(direct, base)
		if order := ix.linearize(base, active); len(order) > 0 {
			seqs = append(seqs, order)
		}
	}
	delete(active, id)
	order := make([]int64, 0, len(direct)+1)
	order = append(order, id)
	if len(direct) > 0 {
		seqs = append(seqs, direct)
	}
	order = append(order, c3Merge(seqs)...)
	order = dedupeIDs(order)
	if ix.mroCache == nil {
		ix.mroCache = make(map[int64][]int64, 8)
	}
	ix.mroCache[id] = order
	return order
}

// c3Merge performs the C3 merge. A hierarchy Python itself would reject
// (inconsistent order) falls back to the next declared head, which keeps the
// answer deterministic instead of choosing a base by guesswork.
func c3Merge(seqs [][]int64) []int64 {
	var out []int64
	for {
		empty := true
		for _, seq := range seqs {
			if len(seq) > 0 {
				empty = false
				break
			}
		}
		if empty {
			return out
		}
		var chosen int64
		for _, seq := range seqs {
			if len(seq) == 0 {
				continue
			}
			if !inTails(seq[0], seqs) {
				chosen = seq[0]
				break
			}
		}
		if chosen == 0 {
			for _, seq := range seqs {
				if len(seq) > 0 {
					chosen = seq[0]
					break
				}
			}
		}
		out = append(out, chosen)
		for i := range seqs {
			if len(seqs[i]) > 0 && seqs[i][0] == chosen {
				seqs[i] = seqs[i][1:]
			}
		}
	}
}

// dedupeIDs keeps the first occurrence of each class. A cyclic or inconsistent
// hierarchy can make the merge emit one twice; the linearization must not.
func dedupeIDs(ids []int64) []int64 {
	out := ids[:0]
	seen := make(map[int64]bool, len(ids))
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

func inTails(id int64, seqs [][]int64) bool {
	for _, seq := range seqs {
		if len(seq) < 2 {
			continue
		}
		for _, member := range seq[1:] {
			if member == id {
				return true
			}
		}
	}
	return false
}

// methodInMRO returns the first class in the linearization that defines name,
// with its position: 0 is the class itself, a later position is an inherited
// method. Python attribute lookup has the same order, so this is a deterministic
// lookup rather than a candidate guess. skipFirst starts the walk after the
// class itself, which is what super() means.
func (ix *Index) methodInMRO(classID int64, name string, skipFirst bool) (store.NodeRow, int, bool) {
	for position, id := range ix.mro(classID) {
		if skipFirst && position == 0 {
			continue
		}
		classNode, ok := ix.classByID[id]
		if !ok {
			continue
		}
		for _, n := range ix.byQName[classNode.QName+"."+name] {
			if n.FileID == classNode.FileID {
				return n, position, true
			}
		}
	}
	return store.NodeRow{}, 0, false
}

// classInFile finds the class symbol with this qname in one file. A method body
// always belongs to a class that is indexed, so a miss means the qname could
// not be reconstructed and resolution must not guess.
func (ix *Index) classInFile(fileID int64, qname string) (store.NodeRow, bool) {
	for _, n := range ix.byQName[qname] {
		if n.FileID == fileID && n.Kind == "class" {
			return n, true
		}
	}
	return store.NodeRow{}, false
}

// byImport resolves a name bound by an import statement, following barrel
// re-exports in the target module. A workspace package specifier resolves to
// the package's source entry instead of staying external.
func (ix *Index) byImport(fromPath string, binding facts.Import, ref facts.Ref) Resolution {
	target, workspace := ix.specifierTarget(fromPath, binding.Source)
	if target == 0 {
		return Resolution{External: true}
	}

	if binding.Imported == "default" {
		n, hop := ix.tsExport(target, "default", nil, 0)
		switch hop {
		case tsExportDirect:
			if workspace {
				return Resolution{Dst: n.ID, Confidence: 0.95, Rule: "ts.import.workspace", Found: true}
			}
			return Resolution{Dst: n.ID, Confidence: 0.95, Rule: "ts.import.default", Found: true}
		case tsExportReexport:
			return Resolution{Dst: n.ID, Confidence: 0.95, Rule: tsReexportRule(workspace, false), Found: true}
		case tsExportExternal:
			return Resolution{External: true}
		}
		return Resolution{}
	}

	n, hop := ix.tsExport(target, binding.Imported, nil, 0)
	switch hop {
	case tsExportDirect:
		if workspace {
			return Resolution{Dst: n.ID, Confidence: 0.95, Rule: "ts.import.workspace", Found: true}
		}
		return Resolution{Dst: n.ID, Confidence: 1.0, Rule: "ts.import.named", Found: true}
	case tsExportReexport, tsExportStar:
		return Resolution{Dst: n.ID, Confidence: tsHopConfidence(hop), Rule: tsReexportRule(workspace, hop == tsExportStar), Found: true}
	case tsExportExternal:
		return Resolution{External: true}
	}
	return Resolution{}
}

// tsExport is the barrel walk. It returns the symbol one module exports under
// name, following `export { a } from "./x"` and `export * from "./x"` chains,
// and reports how it got there so the caller can pick an honest rule id. The
// walk is bounded by maxBarrelDepth, cycle-safe through seen, and deterministic:
// a declaration wins over a named re-export, a named re-export over a star, and
// several stars are tried in declaration order.
func (ix *Index) tsExport(fileID int64, name string, seen []int64, depth int) (store.NodeRow, tsExportHop) {
	if fileID == 0 || depth > maxBarrelDepth || containsFile(seen, fileID) {
		return store.NodeRow{}, tsExportMissing
	}
	seen = append(seen, fileID)
	// A warm run loaded only the files it re-resolved. The barrel's own
	// bindings are the hop we are about to take, so load them on first use;
	// importsOf caches per file and the error is checked before any write.
	ix.importsOf(fileID)

	if name == "default" {
		// `export *` never re-exports a default, so only an explicit
		// `export { X as default } from` (or a `export { default } from`)
		// carries one through a barrel.
		if binding, ok := ix.reexports[fileID]["default"]; ok {
			return ix.followTSExport(fileID, binding, seen, depth, tsExportReexport)
		}
		if exports := ix.exports[fileID]; len(exports) == 1 {
			return exports[0], tsExportDirect
		}
		return store.NodeRow{}, tsExportMissing
	}

	if n, ok := ix.exportedByName(fileID, name); ok {
		return n, tsExportDirect
	}

	if binding, ok := ix.reexports[fileID][name]; ok && binding.Imported != "*" {
		return ix.followTSExport(fileID, binding, seen, depth, tsExportReexport)
	}

	external := false
	for _, binding := range ix.starReexports[fileID] {
		target, _ := ix.specifierTarget(ix.paths[fileID], binding.Source)
		if target == 0 {
			if !isRelative(binding.Source) {
				external = true
			}
			continue
		}
		n, hop := ix.tsExport(target, name, seen, depth+1)
		switch hop {
		case tsExportDirect, tsExportReexport, tsExportStar:
			return n, tsExportStar
		case tsExportExternal:
			external = true
		}
	}
	if external {
		return store.NodeRow{}, tsExportExternal
	}
	return store.NodeRow{}, tsExportMissing
}

// followTSExport continues a barrel chain through one named re-export binding.
// A broken relative source stays unresolved; a package source that is not in
// the repo ends the chain as external, which is how a compat barrel over a
// dependency is classified rather than reported as a gap.
func (ix *Index) followTSExport(fileID int64, binding facts.Import, seen []int64, depth int, hop tsExportHop) (store.NodeRow, tsExportHop) {
	target, _ := ix.specifierTarget(ix.paths[fileID], binding.Source)
	if target == 0 {
		if isRelative(binding.Source) {
			return store.NodeRow{}, tsExportMissing
		}
		return store.NodeRow{}, tsExportExternal
	}
	n, child := ix.tsExport(target, binding.Imported, seen, depth+1)
	switch child {
	case tsExportDirect:
		return n, hop
	case tsExportReexport, tsExportStar:
		// A `export *` anywhere in the chain is the weaker link, so the edge
		// reports the star hop rather than the named one that wrapped it.
		if child == tsExportStar {
			return n, tsExportStar
		}
		return n, hop
	}
	return store.NodeRow{}, child
}

// containsFile reports whether the barrel walk already visited fileID. The
// slice holds at most maxBarrelDepth+1 entries, so a linear scan is cheaper
// than the map and the hash it would need.
func containsFile(seen []int64, fileID int64) bool {
	for _, id := range seen {
		if id == fileID {
			return true
		}
	}
	return false
}

// maxBarrelDepth bounds a re-export chain. Real barrels are one or two hops;
// the cap keeps a pathological or cyclic chain from making resolution
// unbounded, and the seen set makes a cycle terminate instead of repeating.
const maxBarrelDepth = 8

type tsExportHop int

const (
	tsExportMissing tsExportHop = iota
	tsExportDirect
	tsExportReexport
	tsExportStar
	tsExportExternal
)

// tsHopConfidence is the confidence of a name found through a barrel hop.
func tsHopConfidence(hop tsExportHop) float64 {
	if hop == tsExportStar {
		return 0.85
	}
	return 0.95
}

// tsReexportRule names the barrel rule. A workspace entry is a different
// resolution input, so it carries its own id instead of hiding behind the
// generic one.
func tsReexportRule(workspace, star bool) string {
	switch {
	case workspace && star:
		return "ts.import.workspace.star"
	case workspace:
		return "ts.import.workspace.reexport"
	case star:
		return "ts.import.star"
	}
	return "ts.import.reexport"
}

// pyByImport resolves a name bound by `import x`, `from x import y`, or an
// alias. A source that is not in the repo is external; a broken relative import
// stays unresolved instead of hiding as external.
func (ix *Index) pyByImport(fromPath string, binding facts.Import, ref facts.Ref) Resolution {
	if binding.Imported == "*" {
		if ix.resolvePyModule(fromPath, binding.Source) != 0 {
			// `import pkg` binds a module, not a symbol; member receivers use
			// pyByModuleMember.
			return Resolution{}
		}
		if isRelative(binding.Source) {
			return Resolution{}
		}
		return Resolution{External: true}
	}

	target := ix.resolvePyModule(fromPath, binding.Source)
	if target == 0 {
		if ix.resolvePyImportedModule(fromPath, binding.Source, binding.Imported) != 0 {
			return Resolution{}
		}
		if isRelative(binding.Source) {
			return Resolution{}
		}
		return Resolution{External: true}
	}

	n, lookup := ix.pyExport(target, binding.Imported, map[int64]bool{}, 0)
	switch lookup {
	case exportExternal:
		return Resolution{External: true}
	case exportFoundStar:
		// A `from x import *` hop is the weakest import link (#102): the
		// name was exported, but only a wildcard tied it to the module.
		return pyTarget(ref, n, 0.80, "py.import.star")
	case exportFound, exportFoundReexport:
		switch {
		case lookup == exportFoundReexport:
			return pyTarget(ref, n, 0.95, "py.import.reexport")
		case binding.Local != binding.Imported:
			return pyTarget(ref, n, 1.0, "py.import.alias")
		default:
			return pyTarget(ref, n, 1.0, "py.import.named")
		}
	}

	// The name may be a submodule of the imported package ("from . import x").
	if ix.resolvePyImportedModule(fromPath, binding.Source, binding.Imported) != 0 {
		return Resolution{}
	}
	return Resolution{}
}

// pyByStar resolves a bare name through a `from x import *` binding. It is
// weaker than a named import because only a wildcard tied the name to the file.
// The imports map keeps the last binding for a local name, so the last star
// import wins when several of them define it — the Python execution order.
func (ix *Index) pyByStar(fromFile int64, fromPath string, ref facts.Ref) (Resolution, bool) {
	for _, binding := range ix.importsOf(fromFile) {
		if binding.Local != "*" {
			continue
		}
		target := ix.resolvePyModule(fromPath, binding.Source)
		if target == 0 {
			continue
		}
		if n, lookup := ix.pyExport(target, ref.Name, map[int64]bool{}, 0); lookup.found() {
			return pyTarget(ref, n, 0.80, "py.import.star"), true
		}
	}
	return Resolution{}, false
}

// pyByModuleMember resolves member.f() when the receiver is a module binding.
// Both an exact binding ("util.f()") and a dotted receiver ("pkg.mod.f()")
// are handled; a receiver with no module binding is left for typed receivers.
func (ix *Index) pyByModuleMember(fromFile int64, fromPath string, ref facts.Ref, imports map[string]facts.Import) (Resolution, bool) {
	receiver := ref.Receiver
	if receiver == "" {
		return Resolution{}, false
	}

	if binding, ok := imports[receiver]; ok {
		spec, isModule := ix.bindingModuleSpec(fromPath, binding)
		if isModule {
			return ix.pyModuleMember(fromPath, ref, spec)
		}
		// A named import whose target file exists but is not a module binding
		// will be handled by the typed-receiver path; do not guess.
		return Resolution{}, false
	}

	if i := strings.Index(receiver, "."); i > 0 {
		prefix := receiver[:i]
		if binding, ok := imports[prefix]; ok {
			if spec, isModule := ix.bindingModuleSpec(fromPath, binding); isModule {
				return ix.pyModuleMember(fromPath, ref, spec+receiver[i:])
			}
		}
	}
	return Resolution{}, false
}

// pyByClassMember resolves Class.method() when the receiver is a class defined
// in this file or imported by name. Classmethod and staticmethod calls are
// common, and the class itself is a real symbol, so this is not name guessing.
func (ix *Index) pyByClassMember(fromFile int64, fromPath string, ref facts.Ref, imports map[string]facts.Import) (Resolution, bool) {
	receiver := ref.Receiver
	if receiver == "" || strings.Contains(receiver, ".") {
		return Resolution{}, false
	}

	for _, n := range ix.byFile[fromFile][receiver] {
		if n.Kind != "class" {
			continue
		}
		if r, ok := ix.pyMethodOn(n, ref); ok {
			return r, true
		}
	}

	binding, ok := imports[receiver]
	if !ok {
		return Resolution{}, false
	}
	target := ix.resolvePyModule(fromPath, binding.Source)
	if target == 0 {
		if isRelative(binding.Source) {
			return Resolution{}, false
		}
		// The class comes from the standard library or a dependency.
		return Resolution{External: true}, true
	}

	classNode, lookup := ix.pyExport(target, binding.Imported, map[int64]bool{}, 0)
	if !lookup.found() {
		return Resolution{}, false
	}
	if classNode.Kind != "class" {
		return Resolution{}, false
	}
	if r, ok := ix.pyMethodOn(classNode, ref); ok {
		return r, true
	}
	return Resolution{}, false
}

// pyModuleMember looks up ref.Name in one module file. The bool reports whether
// the reference was consumed: resolved, external, or the module existed but the
// member did not (which leaves a pending row).
func (ix *Index) pyModuleMember(fromPath string, ref facts.Ref, spec string) (Resolution, bool) {
	target := ix.resolvePyModule(fromPath, spec)
	if target == 0 {
		if isRelative(spec) {
			return Resolution{}, true
		}
		return Resolution{External: true}, true
	}
	n, lookup := ix.pyExport(target, ref.Name, map[int64]bool{}, 0)
	switch lookup {
	case exportExternal:
		return Resolution{External: true}, true
	case exportFoundStar:
		return pyTarget(ref, n, 0.80, "py.import.star"), true
	case exportFound, exportFoundReexport:
		switch {
		case lookup == exportFoundReexport:
			return pyTarget(ref, n, 0.95, "py.import.reexport"), true
		case isRelative(spec):
			return pyTarget(ref, n, 0.90, "py.module.relative"), true
		default:
			return pyTarget(ref, n, 0.95, "py.import.module"), true
		}
	}
	return Resolution{}, true
}

// bindingModuleSpec returns the module specifier a binding refers to, when the
// binding is a module (import pkg, import pkg.mod as m, from . import util).
func (ix *Index) bindingModuleSpec(fromPath string, binding facts.Import) (string, bool) {
	if binding.Imported == "*" {
		// `import a.b` binds the root `a`, not the whole dotted path; the
		// receiver supplies the rest. `import a.b as c` binds the whole path.
		if firstSegment(binding.Source) == binding.Local {
			return binding.Local, true
		}
		return binding.Source, true
	}
	spec := joinPySpec(binding.Source, binding.Imported)
	if ix.resolvePyModule(fromPath, spec) != 0 {
		return spec, true
	}
	return "", false
}

// pyByTypedMethod resolves Type.method when the receiver's type is known from
// an annotation or a Constructor() call. It follows a package __init__ re-export
// to the module that actually defines the class.
func (ix *Index) pyByTypedMethod(fromFile int64, fromPath string, ref facts.Ref, imports map[string]facts.Import) Resolution {
	qname := ref.ReceiverType + "." + ref.Name

	// The receiver type is a class in this file: look the method up directly,
	// then through the class's bases.
	if classNode, ok := ix.classInFile(fromFile, ref.ReceiverType); ok {
		if r, ok := ix.pyTypedOn(classNode, ref); ok {
			return r
		}
	}

	if binding, ok := imports[ref.ReceiverType]; ok {
		if target := ix.resolvePyModule(fromPath, binding.Source); target != 0 {
			for _, n := range ix.byQName[qname] {
				if n.FileID == target {
					return pyTarget(ref, n, 0.85, "py.typed.method")
				}
			}
			if classNode, lookup := ix.pyExport(target, ref.ReceiverType, map[int64]bool{}, 0); lookup.found() {
				if classNode.Kind == "class" {
					if r, ok := ix.pyTypedOn(classNode, ref); ok {
						return r
					}
				}
			}
		}
	}

	if nodes := ix.byQName[qname]; len(nodes) == 1 {
		return pyTarget(ref, nodes[0], 0.70, "py.typed.method.global")
	}
	return Resolution{}
}

// pyByThis resolves self/cls/super member calls. py.self.inherited and
// py.self.super name the base-class walk explicitly, so a reader can tell a
// direct hit from an inherited one.
func (ix *Index) pyByThis(fromFile int64, ref facts.Ref) Resolution {
	classNode, ok := ix.classInFile(fromFile, classOf(ref.Enclosing))
	if !ok {
		return Resolution{}
	}
	method, position, found := ix.methodInMRO(classNode.ID, ref.Name, ref.Form == "super")
	if !found {
		return Resolution{}
	}
	if ref.Form == "super" {
		if position == 0 {
			// super() starts after the enclosing class; a same-class hit is
			// not what super() calls.
			return Resolution{}
		}
		return pyTarget(ref, method, 0.85, "py.self.super")
	}
	if position == 0 {
		return pyTarget(ref, method, 0.90, "py.this.method")
	}
	return pyTarget(ref, method, 0.85, "py.self.inherited")
}

// pyMethodOn resolves Class.method() on a class symbol, direct or inherited.
func (ix *Index) pyMethodOn(classNode store.NodeRow, ref facts.Ref) (Resolution, bool) {
	method, position, found := ix.methodInMRO(classNode.ID, ref.Name, false)
	if !found {
		return Resolution{}, false
	}
	if position == 0 {
		return pyTarget(ref, method, 0.90, "py.class.method"), true
	}
	return pyTarget(ref, method, 0.85, "py.class.inherited"), true
}

// pyTypedOn resolves a typed receiver's method on a known class symbol, direct
// or inherited. An inherited method is one step weaker than an own method
// because it depends on the base chain being complete.
func (ix *Index) pyTypedOn(classNode store.NodeRow, ref facts.Ref) (Resolution, bool) {
	method, position, found := ix.methodInMRO(classNode.ID, ref.Name, false)
	if !found {
		return Resolution{}, false
	}
	if position == 0 {
		return pyTarget(ref, method, 0.85, "py.typed.method"), true
	}
	return pyTarget(ref, method, 0.80, "py.typed.inherited"), true
}

// pyPropertyAlias resolves @name.setter / @name.deleter / @name.getter to the
// property on the enclosing class. The decorator names `setter`, not the
// property, so the ordinary member ladder would leave a bogus pending row. The
// arity-zero definition is the property getter that created the property object.
func (ix *Index) pyPropertyAlias(fromFile int64, ref facts.Ref) (Resolution, bool) {
	if ref.Kind != "decorator" || ref.Receiver == "" {
		return Resolution{}, false
	}
	switch ref.Name {
	case "setter", "deleter", "getter":
	default:
		return Resolution{}, false
	}
	qname := classOf(ref.Enclosing) + "." + ref.Receiver
	var best store.NodeRow
	found := false
	for _, n := range ix.byQName[qname] {
		if n.FileID != fromFile {
			continue
		}
		if !found || betterProperty(n, best) {
			best, found = n, true
		}
	}
	if !found {
		// Not a property of this class: fall through to the ordinary member
		// ladder, which leaves an honest pending row instead of hiding it.
		return Resolution{}, false
	}
	return pyTarget(ref, best, 0.90, "py.property.alias"), true
}

func betterProperty(candidate, best store.NodeRow) bool {
	if candidate.Arity == 0 && best.Arity != 0 {
		return true
	}
	if candidate.Arity != 0 && best.Arity == 0 {
		return false
	}
	return candidate.ID < best.ID
}

// exportLookup is the outcome of looking a name up in a module. Python modules
// re-export imported names, so a name may resolve through another module, or be
// external when the chain ends at the standard library or a dependency.
type exportLookup int

const (
	exportMissing exportLookup = iota
	exportFound
	exportFoundReexport
	// exportFoundStar is a hit whose chain crossed a `from x import *` hop. The
	// name is exported, but only a wildcard connected it to the module, so the
	// caller must report the star rung instead of the exact import one.
	exportFoundStar
	exportExternal
)

// found reports whether the lookup found a local symbol, directly or through a
// re-export, rather than missing or ending outside the repo.
func (l exportLookup) found() bool {
	return l == exportFound || l == exportFoundReexport || l == exportFoundStar
}

// maxPyExportHops bounds a Python re-export chain. Real packages are one or two
// hops deep; the cap keeps a pathological or cyclic wildcard chain bounded, and
// the seen set makes a cycle terminate instead of repeating.
const maxPyExportHops = 8

// pyExport finds an importable name in a module. A module re-exports names it
// imports, which is how `requests.compat` provides stdlib names and how
// `from pkg import Name` works through a package __init__.py. A declaration
// beats a named re-export beats a star re-export, mirroring the TypeScript
// barrel policy: `from .impl import *` in a package __init__ (#102) is followed
// like `export * from`, and the walk reports the weakest hop it crossed.
func (ix *Index) pyExport(fileID int64, name string, seen map[int64]bool, depth int) (store.NodeRow, exportLookup) {
	if fileID == 0 || depth > maxPyExportHops || seen[fileID] {
		return store.NodeRow{}, exportMissing
	}
	seen[fileID] = true

	for _, n := range ix.exports[fileID] {
		if n.Name == name && n.QName == name {
			return n, exportFound
		}
	}

	fromPath := ix.paths[fileID]
	external := false
	for _, imp := range ix.importsOf(fileID) {
		if imp.Local != name || imp.Imported == "*" {
			continue
		}
		target := ix.resolvePyModule(fromPath, imp.Source)
		if target == 0 {
			if !isRelative(imp.Source) {
				external = true
			}
			continue
		}
		n, lookup := ix.pyExport(target, imp.Imported, seen, depth+1)
		switch lookup {
		case exportFound, exportFoundReexport:
			return n, exportFoundReexport
		case exportFoundStar:
			// A wildcard anywhere in the chain is the weaker link.
			return n, exportFoundStar
		case exportExternal:
			external = true
		}
	}
	// A star re-export makes every public name of the source module visible on
	// this one, so it can still find a name the named bindings above did not.
	// Python binds wildcard imports in source order, so the last star that
	// exports the name wins; the walk keeps scanning after a hit for that
	// reason, which is also why the seen set is shared (a second visit to the
	// same file cannot find a different node).
	var starHit store.NodeRow
	starFound := false
	for _, imp := range ix.starReexports[fileID] {
		target := ix.resolvePyModule(fromPath, imp.Source)
		if target == 0 {
			if !isRelative(imp.Source) {
				external = true
			}
			continue
		}
		n, lookup := ix.pyExport(target, name, seen, depth+1)
		switch lookup {
		case exportFound, exportFoundReexport, exportFoundStar:
			starHit, starFound = n, true
		case exportExternal:
			external = true
		}
	}
	if starFound {
		return starHit, exportFoundStar
	}
	if external {
		return store.NodeRow{}, exportExternal
	}
	return store.NodeRow{}, exportMissing
}

// resolvePyModule maps a Python module specifier to a file id. It understands
// leading-dot relative imports and the common src/ and lib/ source layouts for
// absolute imports. Zero means "not in this repo" (stdlib or a dependency).
func (ix *Index) resolvePyModule(fromPath, spec string) int64 {
	if spec == "" {
		return 0
	}

	dots := 0
	for dots < len(spec) && spec[dots] == '.' {
		dots++
	}
	rest := spec[dots:]
	module := strings.ReplaceAll(rest, ".", "/")

	if dots > 0 {
		dir := path.Dir(fromPath)
		for i := 1; i < dots; i++ {
			dir = path.Dir(dir)
		}
		if module != "" {
			dir = path.Join(dir, module)
		}
		return ix.pyFile(dir)
	}

	for _, base := range []string{"", "src", "lib"} {
		if id := ix.pyFile(path.Join(base, module)); id != 0 {
			return id
		}
	}
	return 0
}

// resolvePyImportedModule resolves the module a `from source import name`
// binding may refer to when `name` is a submodule rather than a symbol.
func (ix *Index) resolvePyImportedModule(fromPath, source, imported string) int64 {
	if imported == "" || imported == "*" {
		return 0
	}
	return ix.resolvePyModule(fromPath, joinPySpec(source, imported))
}

func (ix *Index) pyFile(base string) int64 {
	base = strings.TrimSuffix(base, "/")
	if base == "" {
		base = "."
	}
	if base == "." {
		if id, ok := ix.files["__init__.py"]; ok {
			return id
		}
	}
	for _, candidate := range []string{base + ".py", base + "/__init__.py"} {
		if id, ok := ix.files[candidate]; ok {
			return id
		}
	}
	return 0
}

func firstSegment(name string) string {
	if i := strings.Index(name, "."); i >= 0 {
		return name[:i]
	}
	return name
}

func isRelative(spec string) bool { return strings.HasPrefix(spec, ".") }

func joinPySpec(source, name string) string {
	if source == "" {
		return name
	}
	// "." and ".." already carry the separator; appending another dot would
	// change the relative level.
	if strings.Trim(source, ".") == "" {
		return source + name
	}
	return source + "." + name
}

// pyTarget applies the Python instantiation rule: a call that resolves to a
// class is a REFERENCES edge, mirroring `new` in TypeScript.
func pyTarget(ref facts.Ref, n store.NodeRow, confidence float64, rule string) Resolution {
	r := Resolution{Dst: n.ID, Confidence: confidence, Rule: rule, Found: true}
	if ref.Kind == "call" && n.Kind == "class" {
		r.Kind = "REFERENCES"
	}
	return r
}

func (ix *Index) byUniquePython(ref facts.Ref, candidates []store.NodeRow, confidence float64) Resolution {
	picked := pick(candidates, ref.Arity)
	switch len(picked) {
	case 0:
		return Resolution{}
	case 1:
		return pyTarget(ref, picked[0], confidence, "py.global.unique")
	}
	if len(picked) > maxCandidates {
		return Resolution{}
	}
	ids := make([]int64, 0, len(picked))
	allClasses := ref.Kind == "call"
	for _, n := range picked {
		ids = append(ids, n.ID)
		if n.Kind != "class" {
			allClasses = false
		}
	}
	kind := ""
	if allClasses {
		kind = "REFERENCES"
	}
	return Resolution{Confidence: 0.40, Rule: "py.global.ambiguous", Candidates: ids, Found: true, Kind: kind}
}

// tsUniqueConfidence is the ladder rung for a bare TypeScript identifier with
// exactly one repo-wide declaration and no import, scope, or receiver-type
// evidence. A name-only guess stays below the default 0.5 floor: on the pinned
// Kibana security subtree 30 of 61 such edges crossed a package boundary, and
// at least 11 of the 31 same-package edges were prop callbacks, test-local
// helpers, or a same-file local misattributed by arity. Co-location therefore
// does not separate correct from wrong (#82). The edge is still emitted and
// visible with --min-confidence 0.4; Python and Rust keep 0.60 until #77
// calibrates those ladders.
const tsUniqueConfidence = 0.40

func (ix *Index) byUnique(name string, arity int, candidates []store.NodeRow, rulePrefix string, confidence float64) Resolution {
	picked := pick(candidates, arity)
	switch len(picked) {
	case 0:
		return Resolution{}
	case 1:
		return Resolution{Dst: picked[0].ID, Confidence: confidence, Rule: rulePrefix + ".unique", Found: true}
	}
	if len(picked) > maxCandidates {
		return Resolution{}
	}
	ids := make([]int64, 0, len(picked))
	for _, n := range picked {
		ids = append(ids, n.ID)
	}
	return Resolution{Confidence: 0.40, Rule: rulePrefix + ".ambiguous", Candidates: ids, Found: true}
}

// byTypedMethod resolves Type.method when the receiver's type is known. The
// type may have been imported from a barrel or a workspace package, in which
// case the class symbol is looked up through tsExport and its method in the
// defining file; that path is one notch weaker than a direct import.
func (ix *Index) byTypedMethod(fromFile int64, fromPath string, ref facts.Ref, imports map[string]facts.Import) Resolution {
	qname := ref.ReceiverType + "." + ref.Name

	for _, n := range ix.byQName[qname] {
		if n.FileID == fromFile {
			return Resolution{Dst: n.ID, Confidence: 0.85, Rule: "ts.typed.method", Found: true}
		}
	}

	if binding, ok := imports[ref.ReceiverType]; ok {
		if target, _ := ix.specifierTarget(fromPath, binding.Source); target != 0 {
			for _, n := range ix.byQName[qname] {
				if n.FileID == target {
					return Resolution{Dst: n.ID, Confidence: 0.85, Rule: "ts.typed.method", Found: true}
				}
			}
			// The type declaration may live behind a barrel or workspace-entry
			// re-export: resolve the class first, then its method in the file
			// that declares it.
			if classNode, hop := ix.tsExport(target, binding.Imported, nil, 0); hop != tsExportMissing && hop != tsExportExternal {
				for _, n := range ix.byQName[classNode.QName+"."+ref.Name] {
					if n.FileID == classNode.FileID {
						return Resolution{Dst: n.ID, Confidence: 0.80, Rule: "ts.typed.method.reexport", Found: true}
					}
				}
			}
		}
	}

	if nodes := ix.byQName[qname]; len(nodes) == 1 {
		return Resolution{Dst: nodes[0].ID, Confidence: 0.70, Rule: "ts.typed.method.global", Found: true}
	}
	return Resolution{}
}

// UniqueNames returns the symbol names that have exactly one global candidate.
// A pending identifier with any other name cannot be linked by ResolvePending:
// zero candidates have nothing to point at, and 2+ are the documented ambiguity
// case that must stay unresolved.
func (ix *Index) UniqueNames() []string {
	names := make([]string, 0, len(ix.global))
	for name, nodes := range ix.global {
		if len(nodes) == 1 {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// ResolvePending retries a reference that was left pending by an earlier run.
// The language comes from the referring file, so Python and Rust names get
// their own rule ids. Rust uses the plain global ladder rather than
// byUniqueRust: a retry corrects the rule id, not the edge kind, so a struct or
// enum target keeps its CALLS kind.
//
// Python is split by provenance (#97). A pending name with a persisted import
// binding means the import resolved to a module but its export lookup missed
// (typically a `from x import *` re-export the resolver does not follow); the
// unique-name link is then a recovery that was 99.9% correct on django and
// stays at 0.60. A pending name with no import binding is a bare guess that was
// 3.1% correct, so it is emitted at 0.40 and excluded from default answers.
func (ix *Index) ResolvePending(fileID int64, kind, name string) (Resolution, bool) {
	ref := facts.Ref{Kind: kind, Name: name}
	var r Resolution
	switch ix.language(fileID) {
	case "python":
		confidence := 0.40
		if _, imported := ix.importsOf(fileID)[name]; imported {
			confidence = 0.60
		}
		r = ix.byUniquePython(ref, ix.global[name], confidence)
	case "rust":
		r = ix.byUnique(name, 0, ix.global[name], "rs.global", 0.60)
	default:
		r = ix.byUnique(name, 0, ix.global[name], "ts.global", tsUniqueConfidence)
	}
	if r.Found && len(r.Candidates) == 0 {
		return r, true
	}
	return Resolution{}, false
}

func (ix *Index) language(fileID int64) string {
	path := ix.paths[fileID]
	switch {
	case strings.HasSuffix(path, ".py"):
		return "python"
	case strings.HasSuffix(path, ".rs"):
		return "rust"
	}
	return "typescript"
}

// specifierTarget maps an import source to a file id. Zero means external. The
// bool reports that the mapping came from a workspace package (package.json
// `workspaces`), which the barrel rules name explicitly.
func (ix *Index) specifierTarget(fromPath, spec string) (int64, bool) {
	if strings.HasPrefix(spec, ".") {
		return ix.resolveFile(path.Join(path.Dir(fromPath), spec)), false
	}
	if target := ix.aliases.Resolve(spec); target != "" {
		return ix.resolveFile(target), false
	}
	return ix.workspaceTarget(spec)
}

func (ix *Index) resolveFile(base string) int64 {
	base = strings.TrimSuffix(base, "/")

	candidates := []string{base}
	for _, ext := range []string{".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs"} {
		candidates = append(candidates, base+ext)
	}
	// TypeScript ESM convention: import "./x.js" from a "./x.ts" source.
	for _, ext := range []string{".js", ".jsx", ".mjs", ".cjs"} {
		if strings.HasSuffix(base, ext) {
			stem := strings.TrimSuffix(base, ext)
			candidates = append(candidates, stem+".ts", stem+".tsx")
		}
	}
	for _, ext := range []string{".ts", ".tsx", ".js", ".jsx"} {
		candidates = append(candidates, base+"/index"+ext)
	}

	for _, candidate := range candidates {
		if id, ok := ix.files[candidate]; ok {
			return id
		}
	}
	return 0
}

// exportedByName finds a named export in one file.
func (ix *Index) exportedByName(fileID int64, name string) (store.NodeRow, bool) {
	for _, n := range ix.exports[fileID] {
		if n.Name == name {
			return n, true
		}
	}
	return store.NodeRow{}, false
}

// pick prefers candidates with the call's argument count, which splits overloads.
func pick(candidates []store.NodeRow, arity int) []store.NodeRow {
	if arity <= 0 || len(candidates) < 2 {
		return candidates
	}
	matched := make([]store.NodeRow, 0, len(candidates))
	for _, n := range candidates {
		if n.Arity == arity {
			matched = append(matched, n)
		}
	}
	if len(matched) == 0 {
		return candidates
	}
	return matched
}

// classOf turns "Cart.add" into "Cart" for this-call resolution.
func classOf(qname string) string {
	if i := strings.LastIndex(qname, "."); i >= 0 {
		return qname[:i]
	}
	return qname
}

// builtins are JavaScript globals repath must not treat as unresolved project
// symbols. The list is deliberately short: a false negative only costs one
// pending row, while a false positive could hide a real edge.
var builtins = map[string]bool{
	"String": true, "Number": true, "Boolean": true, "Array": true, "Object": true,
	"JSON": true, "Math": true, "Date": true, "RegExp": true, "Promise": true,
	"Map": true, "Set": true, "WeakMap": true, "WeakSet": true, "Symbol": true,
	"Error": true, "TypeError": true, "RangeError": true, "parseInt": true,
	"parseFloat": true, "isNaN": true, "isFinite": true, "encodeURIComponent": true,
	"decodeURIComponent": true, "setTimeout": true, "setInterval": true,
	"clearTimeout": true, "clearInterval": true, "queueMicrotask": true,
	"structuredClone": true, "fetch": true, "console": true, "window": true,
	"document": true, "globalThis": true, "process": true, "require": true,
	"URL": true, "URLSearchParams": true, "TextEncoder": true, "TextDecoder": true,
	"Intl": true, "Reflect": true, "Proxy": true, "BigInt": true,
	"ArrayBuffer": true, "Uint8Array": true, "performance": true,
	"localStorage": true, "sessionStorage": true, "alert": true,
}

// pyBuiltins are Python builtins and decorators that are never project symbols.
var pyBuiltins = map[string]bool{
	"print": true, "len": true, "range": true, "enumerate": true, "zip": true,
	"map": true, "filter": true, "sorted": true, "reversed": true, "sum": true,
	"min": true, "max": true, "abs": true, "round": true, "open": true,
	"input": true, "isinstance": true, "issubclass": true, "getattr": true,
	"setattr": true, "hasattr": true, "delattr": true, "super": true,
	"property": true, "staticmethod": true, "classmethod": true, "object": true,
	"type": true, "str": true, "int": true, "float": true, "bool": true,
	"bytes": true, "bytearray": true, "list": true, "dict": true, "set": true,
	"frozenset": true, "tuple": true, "Exception": true, "BaseException": true,
	"ValueError": true, "TypeError": true, "KeyError": true, "IndexError": true,
	"AttributeError": true, "RuntimeError": true, "StopIteration": true,
	"NotImplementedError": true, "OSError": true, "IOError": true,
	"ImportError": true, "ModuleNotFoundError": true, "ZeroDivisionError": true,
	"AssertionError": true, "id": true, "hash": true, "iter": true, "next": true,
	"vars": true, "dir": true, "callable": true, "format": true, "repr": true,
	"eval": true, "exec": true, "compile": true, "globals": true, "locals": true,
	"__import__": true, "self": true, "cls": true,
}

func isBuiltin(name string) bool { return builtins[name] }

func isPyBuiltin(name string) bool { return pyBuiltins[name] }
