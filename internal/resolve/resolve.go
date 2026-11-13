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
	crates  []string
	aliases *Aliases

	// A targeted run loads imports only for the files it re-resolves. Python
	// and Rust resolution follows re-export chains through unchanged modules,
	// so the remaining bindings are loaded lazily, at most once per file, from
	// the imports_file index. TypeScript never reads another file's bindings,
	// so its incremental path stays targeted for the Kibana budget.
	ctx      context.Context
	store    *store.Store
	loads    int
	allLoads bool
	loadErr  error
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

	ix := &Index{
		byFile:  make(map[int64]map[string][]store.NodeRow),
		global:  make(map[string][]store.NodeRow),
		byQName: make(map[string][]store.NodeRow),
		exports: make(map[int64][]store.NodeRow),
		files:   make(map[string]int64, len(paths)),
		paths:   paths,
		imports: make(map[int64]map[string]facts.Import),
		crates:  loadRustCrates(root, paths),
		aliases: LoadAliases(root),
		ctx:     ctx,
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
	return ix, nil
}

// addImport stores one binding in the local-name map.
func (ix *Index) addImport(imp store.ImportRow) {
	if ix.imports[imp.FileID] == nil {
		ix.imports[imp.FileID] = make(map[string]facts.Import)
	}
	ix.imports[imp.FileID][imp.Local] = facts.Import{
		Local: imp.Local, Imported: imp.Imported, Source: imp.Source,
		Line: imp.Line, Public: imp.Public,
	}
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

	// 3. ns.member() where ns is a namespace import.
	if ref.Form == "member" && ref.Receiver != "" {
		if binding, ok := imports[ref.Receiver]; ok && binding.Imported == "*" {
			if target := ix.resolveSpecifier(fromPath, binding.Source); target != 0 {
				if n, ok := ix.exportedByName(target, ref.Name); ok {
					return Resolution{Dst: n.ID, Confidence: 0.95, Rule: "ts.import.namespace", Found: true}
				}
			} else {
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

	return ix.byUnique(ref.Name, ref.Arity, ix.global[ref.Name], "ts.global")
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

	// 1. self.method() inside a class body.
	if ref.Form == "this" {
		qname := classOf(ref.Enclosing) + "." + ref.Name
		for _, n := range ix.byFile[fromFile][ref.Name] {
			if n.QName == qname {
				return pyTarget(ref, n, 0.90, "py.this.method")
			}
		}
		return Resolution{}
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

	return ix.byUniquePython(ref, ix.global[ref.Name])
}

func (ix *Index) byImport(fromPath string, binding facts.Import, ref facts.Ref) Resolution {
	target := ix.resolveSpecifier(fromPath, binding.Source)
	if target == 0 {
		return Resolution{External: true}
	}

	if binding.Imported == "default" {
		exports := ix.exports[target]
		if len(exports) == 1 {
			return Resolution{Dst: exports[0].ID, Confidence: 0.95, Rule: "ts.import.default", Found: true}
		}
		return Resolution{}
	}

	if n, ok := ix.exportedByName(target, binding.Imported); ok {
		return Resolution{Dst: n.ID, Confidence: 1.0, Rule: "ts.import.named", Found: true}
	}
	return Resolution{}
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

	n, lookup := ix.pyExport(target, binding.Imported, map[int64]bool{})
	switch lookup {
	case exportExternal:
		return Resolution{External: true}
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
		if n, lookup := ix.pyExport(target, ref.Name, map[int64]bool{}); lookup == exportFound || lookup == exportFoundReexport {
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
		for _, method := range ix.byQName[n.QName+"."+ref.Name] {
			if method.FileID == fromFile {
				return pyTarget(ref, method, 0.90, "py.class.method"), true
			}
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

	classNode, lookup := ix.pyExport(target, binding.Imported, map[int64]bool{})
	if lookup != exportFound && lookup != exportFoundReexport {
		return Resolution{}, false
	}
	if classNode.Kind != "class" {
		return Resolution{}, false
	}
	for _, method := range ix.byQName[classNode.QName+"."+ref.Name] {
		if method.FileID == classNode.FileID {
			return pyTarget(ref, method, 0.90, "py.class.method"), true
		}
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
	n, lookup := ix.pyExport(target, ref.Name, map[int64]bool{})
	switch lookup {
	case exportExternal:
		return Resolution{External: true}, true
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

	for _, n := range ix.byQName[qname] {
		if n.FileID == fromFile {
			return pyTarget(ref, n, 0.85, "py.typed.method")
		}
	}

	if binding, ok := imports[ref.ReceiverType]; ok {
		if target := ix.resolvePyModule(fromPath, binding.Source); target != 0 {
			for _, n := range ix.byQName[qname] {
				if n.FileID == target {
					return pyTarget(ref, n, 0.85, "py.typed.method")
				}
			}
			if classNode, lookup := ix.pyExport(target, ref.ReceiverType, map[int64]bool{}); lookup == exportFound || lookup == exportFoundReexport {
				for _, n := range ix.byQName[qname] {
					if n.FileID == classNode.FileID {
						return pyTarget(ref, n, 0.85, "py.typed.method")
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

// exportLookup is the outcome of looking a name up in a module. Python modules
// re-export imported names, so a name may resolve through another module, or be
// external when the chain ends at the standard library or a dependency.
type exportLookup int

const (
	exportMissing exportLookup = iota
	exportFound
	exportFoundReexport
	exportExternal
)

// pyExport finds an importable name in a module. A module re-exports names it
// imports, which is how `requests.compat` provides stdlib names and how
// `from pkg import Name` works through a package __init__.py.
func (ix *Index) pyExport(fileID int64, name string, seen map[int64]bool) (store.NodeRow, exportLookup) {
	if seen[fileID] {
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
		n, lookup := ix.pyExport(target, imp.Imported, seen)
		switch lookup {
		case exportFound, exportFoundReexport:
			return n, exportFoundReexport
		case exportExternal:
			external = true
		}
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

func (ix *Index) byUniquePython(ref facts.Ref, candidates []store.NodeRow) Resolution {
	picked := pick(candidates, ref.Arity)
	switch len(picked) {
	case 0:
		return Resolution{}
	case 1:
		return pyTarget(ref, picked[0], 0.60, "py.global.unique")
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

func (ix *Index) byUnique(name string, arity int, candidates []store.NodeRow, rulePrefix string) Resolution {
	picked := pick(candidates, arity)
	switch len(picked) {
	case 0:
		return Resolution{}
	case 1:
		return Resolution{Dst: picked[0].ID, Confidence: 0.60, Rule: rulePrefix + ".unique", Found: true}
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

// byTypedMethod resolves Type.method when the receiver's type is known.
func (ix *Index) byTypedMethod(fromFile int64, fromPath string, ref facts.Ref, imports map[string]facts.Import) Resolution {
	qname := ref.ReceiverType + "." + ref.Name

	for _, n := range ix.byQName[qname] {
		if n.FileID == fromFile {
			return Resolution{Dst: n.ID, Confidence: 0.85, Rule: "ts.typed.method", Found: true}
		}
	}

	if binding, ok := imports[ref.ReceiverType]; ok {
		if target := ix.resolveSpecifier(fromPath, binding.Source); target != 0 {
			for _, n := range ix.byQName[qname] {
				if n.FileID == target {
					return Resolution{Dst: n.ID, Confidence: 0.85, Rule: "ts.typed.method", Found: true}
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

// ResolvePending retries a reference that was left pending by an earlier run,
// when the import context is no longer available. The language comes from the
// referring file, so Python names get Python rule ids.
func (ix *Index) ResolvePending(fileID int64, kind, name string) (Resolution, bool) {
	ref := facts.Ref{Kind: kind, Name: name}
	var r Resolution
	if ix.language(fileID) == "python" {
		r = ix.byUniquePython(ref, ix.global[name])
	} else {
		r = ix.byUnique(name, 0, ix.global[name], "ts.global")
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

// resolveSpecifier maps an import source to a file id. Zero means external.
func (ix *Index) resolveSpecifier(fromPath, spec string) int64 {
	if strings.HasPrefix(spec, ".") {
		return ix.resolveFile(path.Join(path.Dir(fromPath), spec))
	}
	if target := ix.aliases.Resolve(spec); target != "" {
		return ix.resolveFile(target)
	}
	return 0
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
