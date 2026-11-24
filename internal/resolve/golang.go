package resolve

import (
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Goutam-11/repath/internal/lang/facts"
	"github.com/Goutam-11/repath/internal/store"
)

// goModule is the root go.mod's module path plus `replace` directives that
// point at a local directory. A replaced module resolves like a package of the
// main module, which is how a Go workspace with local forks stays in the graph.
type goModule struct {
	path     string
	replaces map[string]string
}

// goPredeclared lists Go's predeclared types and constants. They are external,
// never pending: there is no source file in the repo to point at.
var goPredeclared = map[string]bool{
	"bool": true, "byte": true, "complex64": true, "complex128": true,
	"error": true, "float32": true, "float64": true, "int": true, "int8": true,
	"int16": true, "int32": true, "int64": true, "rune": true, "string": true,
	"uint": true, "uint8": true, "uint16": true, "uint32": true, "uint64": true,
	"uintptr": true, "any": true, "comparable": true,
	"true": true, "false": true, "nil": true, "iota": true,
}

// resolveGo is the Go ladder. A package is a directory of .go files, so a
// package-level name is looked up across every file of that directory; a
// member call needs either an imported package alias (exact) or a locally
// inferred receiver type (typed method). Without one it stays unresolved: Go
// has no receiver-less method call to guess from.
func (ix *Index) resolveGo(fromFile int64, fromPath string, ref facts.Ref, imports map[string]facts.Import) Resolution {
	if ref.Form == "ident" && goPredeclared[ref.Name] {
		return Resolution{External: true}
	}
	dir := path.Dir(fromPath)
	pkgs := ix.goPackages()

	switch ref.Form {
	case "member":
		if binding, ok := imports[ref.Receiver]; ok {
			target, local := ix.goImportTarget(binding.Source)
			if !local {
				return Resolution{External: true}
			}
			if n, ok := ix.goPackageSymbol(pkgs, target, ref.Name, ref.Arity); ok {
				return goEdge(ref.Kind, "go.import.member", 0.95, n.ID)
			}
			return Resolution{}
		}
		if ref.ReceiverType != "" {
			return ix.goTypedMethod(pkgs, imports, dir, ref)
		}
		return Resolution{}

	case "ident":
		if rows := pick(ix.byFile[fromFile][ref.Name], ref.Arity); len(rows) == 1 {
			return goEdge(ref.Kind, "go.local", 0.95, rows[0].ID)
		}
		if rows := pick(pkgs[dir][ref.Name], ref.Arity); len(rows) == 1 {
			return goEdge(ref.Kind, "go.package", 0.90, rows[0].ID)
		} else if len(rows) > 1 {
			return goAmbiguous(rows, "go.package.ambiguous")
		}
		if ref.Kind == "ref" {
			// A bare identifier in value position (a function value, a var, a
			// const) can only name something visible in this file or package
			// without a qualifier. A repo-wide unique name is not evidence, so
			// the reference stays pending instead of becoming a guessed edge.
			return Resolution{}
		}
		return ix.byUnique(ref.Name, ref.Arity, ix.global[ref.Name], "go.global", 0.40)
	}
	return Resolution{}
}

// goTypedMethod resolves `x.M()` once x's type is known. A package-qualified
// type ("pkg.T") follows the import alias; a bare type is searched in the
// current package. An interface receiver resolves to the interface's own
// method, which is the static target of the call.
func (ix *Index) goTypedMethod(pkgs map[string]map[string][]store.NodeRow, imports map[string]facts.Import, dir string, ref facts.Ref) Resolution {
	typ := ref.ReceiverType
	if i := strings.LastIndex(typ, "."); i > 0 {
		alias, name := typ[:i], typ[i+1:]
		binding, ok := imports[alias]
		if !ok {
			return Resolution{}
		}
		target, local := ix.goImportTarget(binding.Source)
		if !local {
			return Resolution{External: true}
		}
		return ix.goMethodOn(pkgs, target, name, ref)
	}
	return ix.goMethodOn(pkgs, dir, typ, ref)
}

// goMethodOn looks a method up on one type in one package. The type itself must
// be declared there; a receiver type that is not a repo symbol stays
// unresolved rather than matching a same-named method elsewhere.
func (ix *Index) goMethodOn(pkgs map[string]map[string][]store.NodeRow, dir, typeName string, ref facts.Ref) Resolution {
	typeNode, ok := ix.goTypeNode(pkgs, dir, typeName)
	if !ok {
		return Resolution{}
	}
	rows := pick(pkgs[dir][typeName+"."+ref.Name], ref.Arity)
	if len(rows) == 0 {
		return Resolution{}
	}
	if len(rows) > 1 {
		return goAmbiguous(rows, "go.typed.method.ambiguous")
	}
	if typeNode.Kind == "interface" {
		return Resolution{Dst: rows[0].ID, Confidence: 0.85, Rule: "go.interface.method", Found: true}
	}
	return Resolution{Dst: rows[0].ID, Confidence: 0.90, Rule: "go.typed.method", Found: true}
}

// goPackageSymbol finds a package-level name (function, type, const, var) or a
// method by its scoped qname. Only an exported name is visible across packages.
func (ix *Index) goPackageSymbol(pkgs map[string]map[string][]store.NodeRow, dir, name string, arity int) (store.NodeRow, bool) {
	rows := pick(pkgs[dir][name], arity)
	if len(rows) == 0 {
		return store.NodeRow{}, false
	}
	return rows[0], true
}

// goTypeNode returns the single type declaration named typeName in dir.
func (ix *Index) goTypeNode(pkgs map[string]map[string][]store.NodeRow, dir, typeName string) (store.NodeRow, bool) {
	for _, row := range pkgs[dir][typeName] {
		switch row.Kind {
		case "struct", "interface", "type":
			return row, true
		}
	}
	return store.NodeRow{}, false
}

// goImportTarget maps an import path to a repo-relative package directory.
// Only the main module and a local `replace` target are local; everything else
// is a dependency and therefore external.
func (ix *Index) goImportTarget(spec string) (string, bool) {
	if spec == "" {
		return "", false
	}
	mod := ix.goModuleInfo()
	if mod.path != "" {
		if spec == mod.path {
			return ix.goDirIfIndexed(".")
		}
		if strings.HasPrefix(spec, mod.path+"/") {
			return ix.goDirIfIndexed(strings.TrimPrefix(spec, mod.path+"/"))
		}
	}
	if dir, ok := mod.replaces[spec]; ok {
		return ix.goDirIfIndexed(dir)
	}
	return "", false
}

func (ix *Index) goDirIfIndexed(dir string) (string, bool) {
	dir = path.Clean(dir)
	ix.goPackages()
	if ix.goDirs[dir] {
		return dir, true
	}
	return "", false
}

// goPackages builds two lazy views once per run: every package directory with
// its symbols keyed by both name and qname, and the set of indexed directories.
// The resolver index already holds every node, so this is a map rebuild, not a
// database read; a non-Go run never pays for it.
func (ix *Index) goPackages() map[string]map[string][]store.NodeRow {
	if ix.goPkgsReady {
		return ix.goPkgs
	}
	ix.goPkgsReady = true
	ix.goPkgs = make(map[string]map[string][]store.NodeRow)
	ix.goDirs = make(map[string]bool)
	// byQName covers every node on every run; byFile is deliberately partial on
	// an incremental run, so a package built from it would lose the unchanged
	// files of the same package and cold/incremental parity (#49).
	for qname, rows := range ix.byQName {
		for _, row := range rows {
			p := ix.paths[row.FileID]
			if !strings.HasSuffix(p, ".go") {
				continue
			}
			dir := path.Dir(p)
			ix.goDirs[dir] = true
			pkg := ix.goPkgs[dir]
			if pkg == nil {
				pkg = make(map[string][]store.NodeRow)
				ix.goPkgs[dir] = pkg
			}
			// Package-level symbols have QName == Name, so keying by qname
			// covers both. Methods are only reachable by "Type.Method": a bare
			// method name must never satisfy a package-level lookup.
			pkg[qname] = append(pkg[qname], row)
		}
	}
	for _, pkg := range ix.goPkgs {
		for key := range pkg {
			sort.Slice(pkg[key], func(i, j int) bool { return pkg[key][i].ID < pkg[key][j].ID })
		}
	}
	return ix.goPkgs
}

// goModuleInfo reads go.mod once. Parsing is deliberately minimal: the module
// path and local replace targets, which is all package resolution needs.
func (ix *Index) goModuleInfo() goModule {
	if ix.goModReady {
		return ix.goMod
	}
	ix.goModReady = true
	ix.goMod.replaces = make(map[string]string)

	data, err := os.ReadFile(filepath.Join(ix.root, "go.mod"))
	if err != nil {
		return ix.goMod
	}
	lines := strings.Split(string(data), "\n")
	inReplace := false
	for _, raw := range lines {
		line := strings.TrimSpace(stripGoModComment(raw))
		if line == "" {
			continue
		}
		if inReplace {
			if line == ")" {
				inReplace = false
				continue
			}
			ix.addGoReplace(line)
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		switch fields[0] {
		case "module":
			if len(fields) > 1 {
				ix.goMod.path = strings.Trim(fields[1], `"`)
			}
		case "replace":
			if len(fields) >= 2 && fields[1] == "(" {
				inReplace = true
				continue
			}
			ix.addGoReplace(strings.TrimSpace(strings.TrimPrefix(line, "replace")))
		}
	}
	return ix.goMod
}

// addGoReplace records `old => ./local`; a module-to-module replacement is a
// dependency and stays external.
func (ix *Index) addGoReplace(line string) {
	fields := strings.Fields(line)
	if len(fields) < 3 || fields[1] != "=>" {
		return
	}
	old, target := fields[0], fields[2]
	if len(fields) > 3 {
		return // `=> path version` is a module replacement
	}
	if !strings.HasPrefix(target, ".") && !filepath.IsAbs(target) {
		return
	}
	dir := filepath.ToSlash(filepath.Clean(target))
	if filepath.IsAbs(target) {
		rel, err := filepath.Rel(ix.root, target)
		if err != nil || strings.HasPrefix(rel, "..") {
			return
		}
		dir = filepath.ToSlash(rel)
	}
	ix.goMod.replaces[old] = dir
}

// stripGoModComment removes a `//` comment from one go.mod line.
func stripGoModComment(line string) string {
	if i := strings.Index(line, "//"); i >= 0 {
		return line[:i]
	}
	return line
}

// goAmbiguous emits a 0.40 candidate group, or nothing when more than five
// candidates exist (the shared ambiguity policy, decision 12).
func goAmbiguous(rows []store.NodeRow, rule string) Resolution {
	if len(rows) > maxCandidates {
		return Resolution{}
	}
	ids := make([]int64, 0, len(rows))
	for _, n := range rows {
		ids = append(ids, n.ID)
	}
	return Resolution{Confidence: 0.40, Rule: rule, Candidates: ids, Found: true}
}

// goEdge names the relation. A composite literal, an embedded struct, and an
// embedded interface are type references rather than calls, so they get their
// own ladder rows even when the lookup itself was exact.
func goEdge(refKind, rule string, confidence float64, id int64) Resolution {
	switch refKind {
	case "new":
		return Resolution{Dst: id, Confidence: 0.90, Rule: "go.type.ref", Found: true}
	case "embed":
		return Resolution{Dst: id, Confidence: 0.80, Rule: "go.embed", Found: true}
	case "extends":
		return Resolution{Dst: id, Confidence: 0.90, Rule: "go.embed.interface", Found: true}
	}
	return Resolution{Dst: id, Confidence: confidence, Rule: rule, Found: true}
}
