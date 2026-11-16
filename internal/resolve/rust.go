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

// rustModuleState says whether a Rust path pointed at a file, a dependency, or
// a broken relative path (which must stay unresolved rather than external).
type rustModuleState int

const (
	rustModuleMissing rustModuleState = iota
	rustModuleLocal
	rustModuleExternal
)

// rustCrate is one Cargo package source root. Name is the crate name as a
// Rust `use` path spells it (underscored); it is empty when the manifest could
// not be read, in which case only convention-based resolution works.
type rustCrate struct {
	Dir  string // repo-relative source root ("src", "crates/core/src")
	Name string
}

// loadRustCrates finds the source root of every local Cargo package in the
// repo. A crate root is the directory containing lib.rs or main.rs.
//
// The indexed file list already names every src/lib.rs and src/main.rs, so the
// roots are derived from it instead of walking every directory on each run.
// The walk remains as a fallback for a Rust tree whose crate roots are not
// indexable (for example an oversized or skipped lib.rs).
func loadRustCrates(root string, paths map[int64]string) []rustCrate {
	seen := map[string]bool{}
	var crates []rustCrate
	hasRust := false

	for _, p := range paths {
		if !strings.HasSuffix(p, ".rs") {
			continue
		}
		hasRust = true
		base := path.Base(p)
		if base != "lib.rs" && base != "main.rs" {
			continue
		}
		src := path.Dir(p)
		if path.Base(src) != "src" {
			continue
		}
		manifest := "Cargo.toml"
		if crate := path.Dir(src); crate != "." && crate != "" {
			manifest = crate + "/Cargo.toml"
		}
		if !fileExists(filepath.Join(root, filepath.FromSlash(manifest))) {
			continue
		}
		if !seen[src] {
			seen[src] = true
			crates = append(crates, rustCrate{Dir: src, Name: rustCrateName(root, src)})
		}
	}

	if len(crates) == 0 && hasRust {
		return walkRustCrates(root)
	}
	sortRustCrates(crates)
	return crates
}

// rustCrateName reads a package's crate name from its Cargo.toml. This is a
// narrow TOML subset ([package] and [lib] name keys), never a TOML parser; a
// workspace member's package name is required by Cargo, and [lib] may rename
// the crate. Names are normalized the way `use` paths spell them (hyphens
// become underscores). A missing or unreadable manifest falls back to the
// package directory name.
func rustCrateName(root, srcDir string) string {
	pkgDir := path.Dir(srcDir)
	manifest := path.Join(pkgDir, "Cargo.toml")
	if pkgDir == "." || pkgDir == "" {
		manifest = "Cargo.toml"
	}
	if data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(manifest))); err == nil {
		if name := cargoCrateName(data); name != "" {
			return name
		}
	}
	if pkgDir == "." || pkgDir == "" {
		return ""
	}
	return normalizeCrateName(path.Base(pkgDir))
}

// cargoCrateName extracts [package].name, preferring [lib].name when present.
func cargoCrateName(data []byte) string {
	section, pkg, lib := "", "", ""
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			section = strings.Trim(strings.TrimSpace(line), "[]")
			continue
		}
		if !strings.HasPrefix(line, "name") {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(line, "name"))
		if !strings.HasPrefix(rest, "=") {
			continue
		}
		val := strings.TrimSpace(strings.TrimPrefix(rest, "="))
		start := strings.IndexByte(val, '"')
		if start < 0 {
			continue
		}
		val = val[start+1:]
		end := strings.IndexByte(val, '"')
		if end < 0 {
			continue
		}
		val = val[:end]
		switch section {
		case "package":
			if pkg == "" {
				pkg = val
			}
		case "lib":
			if lib == "" {
				lib = val
			}
		}
	}
	if lib != "" {
		return normalizeCrateName(lib)
	}
	return normalizeCrateName(pkg)
}

func normalizeCrateName(name string) string {
	return strings.ReplaceAll(strings.TrimSpace(name), "-", "_")
}

// rustCratesByName maps a use path's first segment to a local crate source
// root. A duplicate name is invalid in a Cargo workspace; the first entry in
// the deterministic (longest directory first) order wins.
func rustCratesByName(crates []rustCrate) map[string]string {
	out := make(map[string]string, len(crates))
	for _, crate := range crates {
		if crate.Name == "" {
			continue
		}
		if _, ok := out[crate.Name]; !ok {
			out[crate.Name] = crate.Dir
		}
	}
	return out
}

// walkRustCrates is the filesystem fallback used when the indexed paths do not
// describe the crate roots.
func walkRustCrates(root string) []rustCrate {
	seen := map[string]bool{}
	var crates []rustCrate

	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if p != root {
				switch d.Name() {
				case ".git", ".cache", "target", "vendor", "node_modules", ".repath":
					return filepath.SkipDir
				}
			}
			return nil
		}
		if d.Name() != "Cargo.toml" {
			return nil
		}
		src := filepath.Join(filepath.Dir(p), "src")
		if !fileExists(filepath.Join(src, "lib.rs")) && !fileExists(filepath.Join(src, "main.rs")) {
			return nil
		}
		rel, err := filepath.Rel(root, src)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if !seen[rel] {
			seen[rel] = true
			crates = append(crates, rustCrate{Dir: rel, Name: rustCrateName(root, rel)})
		}
		return nil
	})

	sortRustCrates(crates)
	return crates
}

// sortRustCrates puts the longest prefix first so a nested crate wins over the
// repo root.
func sortRustCrates(crates []rustCrate) {
	sort.Slice(crates, func(i, j int) bool {
		if len(crates[i].Dir) != len(crates[j].Dir) {
			return len(crates[i].Dir) > len(crates[j].Dir)
		}
		return crates[i].Dir < crates[j].Dir
	})
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// rustModuleOf maps a file to its crate source root and module segments.
func (ix *Index) rustModuleOf(filePath string) (string, []string, bool) {
	for _, crate := range ix.crates {
		if filePath != crate.Dir && !strings.HasPrefix(filePath, crate.Dir+"/") {
			continue
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(filePath, crate.Dir), "/")
		rel = strings.TrimSuffix(rel, ".rs")
		if rel == "lib" || rel == "main" {
			return crate.Dir, nil, true
		}
		rel = strings.TrimSuffix(rel, "/mod")
		if rel == "" {
			return crate.Dir, nil, true
		}
		return crate.Dir, strings.Split(rel, "/"), true
	}
	return "", nil, false
}

// rustModuleTarget is one resolved Rust module path. Prefix is the qname prefix
// of an inline module's items inside File ("inner" for `mod inner { fn f }`);
// it is empty for a file-backed module.
type rustModuleTarget struct {
	File   int64
	Prefix string
	// Layout names the module-layout feature the path crossed, so the resolver
	// can emit an honest rule: "workspace", "attr" (#[path]), "inline", or "".
	Layout string
	State  rustModuleState
}

// resolveRustModule resolves a module path with the referencing file's
// top-level module scope. It understands crate/self/super, inline modules,
// #[path] declarations, conventional module files, and local workspace members.
func (ix *Index) resolveRustModule(fromPath, spec string) rustModuleTarget {
	return ix.resolveRustModuleScoped(fromPath, "", spec)
}

// resolveRustModuleScoped is resolveRustModule with the referencing symbol's
// inline-module prefix, so `self::x` and `super::x` inside `mod outer { .. }`
// resolve from `outer` rather than from the file root.
func (ix *Index) resolveRustModuleScoped(fromPath, scope, spec string) rustModuleTarget {
	if spec == "" {
		return rustModuleTarget{State: rustModuleExternal}
	}
	parts := splitRustPath(spec)
	if len(parts) == 0 {
		return rustModuleTarget{State: rustModuleExternal}
	}

	crateDir, _, inCrate := ix.rustModuleOf(fromPath)
	fromFile := ix.files[fromPath]

	switch parts[0] {
	case "crate":
		if !inCrate {
			return rustModuleTarget{State: rustModuleExternal}
		}
		root := ix.rustCrateRoot(crateDir)
		if root == 0 {
			return rustModuleTarget{State: rustModuleLocal}
		}
		return ix.walkRustModule(root, "", parts[1:])
	case "self":
		if !inCrate || fromFile == 0 {
			return rustModuleTarget{State: rustModuleExternal}
		}
		return ix.walkRustModule(fromFile, scope, parts[1:])
	case "super":
		if !inCrate || fromFile == 0 {
			return rustModuleTarget{State: rustModuleExternal}
		}
		file, prefix := fromFile, scope
		i := 0
		for i < len(parts) && parts[i] == "super" {
			if prefix != "" {
				prefix = dropRustScope(prefix)
			} else {
				parent := ix.rustParentModuleFile(file)
				if parent == 0 {
					// No parent module is derivable (a #[path] file, or the crate
					// root): stay at the current module rather than guess.
					break
				}
				file = parent
			}
			i++
		}
		return ix.walkRustModule(file, prefix, parts[i:])
	}

	// An unqualified path is resolved like a Rust 2018 uniform path: first as a
	// child of the current module, then at the crate root (2015-edition style),
	// then as a local workspace member. A workspace member shadows only when
	// neither local reading found the module.
	if inCrate && fromFile != 0 {
		if target := ix.walkRustModule(fromFile, scope, parts); target.State == rustModuleLocal {
			return target
		}
	}
	if inCrate {
		if root := ix.rustCrateRoot(crateDir); root != 0 {
			if target := ix.walkRustModule(root, "", parts); target.State == rustModuleLocal {
				return target
			}
		}
	}
	if dir, ok := ix.crateByName[parts[0]]; ok && dir != crateDir {
		if root := ix.rustCrateRoot(dir); root != 0 {
			target := ix.walkRustModule(root, "", parts[1:])
			if target.State == rustModuleLocal {
				target.Layout = mergeRustLayout(target.Layout, "workspace")
				return target
			}
		}
	}
	return rustModuleTarget{State: rustModuleExternal}
}

// rustCrateRoot returns the file id of a crate's root (lib.rs preferred).
func (ix *Index) rustCrateRoot(crateDir string) int64 {
	for _, name := range []string{"lib.rs", "main.rs"} {
		if id, ok := ix.files[path.Join(crateDir, name)]; ok {
			return id
		}
	}
	return 0
}

// walkRustModule walks segments from a module whose declarations live in file
// and whose inline-module qname prefix is prefix.
func (ix *Index) walkRustModule(file int64, prefix string, segments []string) rustModuleTarget {
	layout := ""
	for _, segment := range segments {
		key := joinRustDot(prefix, segment)
		if attr, ok := ix.rustModuleDecl(file, key); ok {
			if attr == "" {
				prefix = key
				layout = mergeRustLayout(layout, "inline")
				continue
			}
			target, ok := ix.rustPathModule(file, key, attr)
			if !ok {
				// A broken #[path] is a local gap, never external.
				return rustModuleTarget{State: rustModuleLocal}
			}
			file, prefix = target, ""
			layout = mergeRustLayout(layout, "attr")
			continue
		}
		next := ix.rustModuleFileIn(ix.rustSubmoduleDir(file), segment)
		if next == 0 {
			return rustModuleTarget{State: rustModuleMissing}
		}
		file, prefix = next, ""
	}
	return rustModuleTarget{File: file, Prefix: prefix, Layout: layout, State: rustModuleLocal}
}

// rustModuleDecl reads a module declaration's layout from the reserved
// "@mod:" import binding. An empty attribute means an inline module; a
// non-empty one is the #[path] value.
func (ix *Index) rustModuleDecl(file int64, qnamePrefix string) (string, bool) {
	imp, ok := ix.importsOf(file)["@mod:"+qnamePrefix]
	if !ok {
		return "", false
	}
	return imp.Source, true
}

// rustInlineScope returns the deepest inline-module prefix of an enclosing
// qname ("outer.inner" for a symbol inside nested inline modules).
func (ix *Index) rustInlineScope(file int64, enclosing string) string {
	if enclosing == "" {
		return ""
	}
	segments := strings.Split(enclosing, ".")
	for i := len(segments) - 1; i >= 1; i-- {
		prefix := strings.Join(segments[:i], ".")
		if _, ok := ix.rustModuleDecl(file, prefix); ok {
			return prefix
		}
	}
	return ""
}

// rustSubmoduleDir returns the directory where a file's `mod x;` children live:
// the crate root directory for lib.rs/main.rs, the file's own directory for
// mod.rs, and the file's stem directory otherwise.
func (ix *Index) rustSubmoduleDir(file int64) string {
	p := ix.paths[file]
	crateDir, segments, ok := ix.rustModuleOf(p)
	if !ok {
		return path.Dir(p)
	}
	if len(segments) == 0 {
		return crateDir
	}
	if path.Base(p) == "mod.rs" {
		return path.Dir(p)
	}
	return strings.TrimSuffix(p, ".rs")
}

// rustParentModuleFile returns the file of a module's parent, derived from the
// conventional module path. It cannot see through a #[path] declaration, so a
// pseudo-module has no derivable parent and returns 0.
func (ix *Index) rustParentModuleFile(file int64) int64 {
	p := ix.paths[file]
	crateDir, segments, ok := ix.rustModuleOf(p)
	if !ok || len(segments) == 0 {
		return 0
	}
	parent := segments[:len(segments)-1]
	if len(parent) == 0 {
		return ix.rustCrateRoot(crateDir)
	}
	base := path.Join(crateDir, strings.Join(parent, "/"))
	for _, candidate := range []string{base + ".rs", base + "/mod.rs"} {
		if id, ok := ix.files[candidate]; ok {
			return id
		}
	}
	return 0
}

func (ix *Index) rustModuleFileIn(dir, segment string) int64 {
	for _, candidate := range []string{path.Join(dir, segment) + ".rs", path.Join(dir, segment, "mod.rs")} {
		if id, ok := ix.files[candidate]; ok {
			return id
		}
	}
	return 0
}

// rustPathModule resolves a #[path = "..."] value. rustc makes the value
// relative to the declaring file's directory, plus the inline-module path when
// the declaration sits inside an inline module (never the file-stem directory
// used by convention for `mod x;`).
func (ix *Index) rustPathModule(file int64, qnamePrefix, attr string) (int64, bool) {
	base := path.Dir(ix.paths[file])
	if segments := strings.Split(qnamePrefix, "."); len(segments) > 1 {
		base = path.Join(base, path.Join(segments[:len(segments)-1]...))
	}
	base = path.Join(base, attr)
	candidates := []string{base}
	if !strings.HasSuffix(attr, ".rs") {
		candidates = append(candidates, base+".rs", base+"/mod.rs")
	}
	for _, candidate := range candidates {
		if id, ok := ix.files[candidate]; ok {
			return id, true
		}
	}
	return 0, false
}

func mergeRustLayout(current, next string) string {
	rank := func(layout string) int {
		switch layout {
		case "workspace":
			return 3
		case "attr":
			return 2
		case "inline":
			return 1
		}
		return 0
	}
	if rank(next) > rank(current) {
		return next
	}
	return current
}

func joinRustDot(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "." + name
}

func dropRustScope(prefix string) string {
	if i := strings.LastIndex(prefix, "."); i >= 0 {
		return prefix[:i]
	}
	return ""
}

// rustExportPrefix finds a name in a module, following public re-exports.
// prefix is the inline-module qname prefix of the target module ("" for a
// file-backed module).
func (ix *Index) rustExportPrefix(fileID int64, prefix, name string, seen map[int64]bool) (store.NodeRow, exportLookup) {
	if seen[fileID] {
		return store.NodeRow{}, exportMissing
	}
	seen[fileID] = true

	qname := name
	if prefix != "" {
		qname = prefix + "." + name
	}
	for _, n := range ix.exports[fileID] {
		if n.Name == name && n.QName == qname {
			return n, exportFound
		}
	}

	fromPath := ix.paths[fileID]
	external := false
	for _, imp := range ix.importsOf(fileID) {
		if !imp.Public || imp.Local != name || imp.Imported == "*" {
			continue
		}
		target := ix.resolveRustModule(fromPath, imp.Source)
		if target.State != rustModuleLocal || target.File == 0 {
			if target.State == rustModuleExternal {
				external = true
			}
			continue
		}
		n, lookup := ix.rustExportPrefix(target.File, target.Prefix, imp.Imported, seen)
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

// rustExport is rustExportPrefix for a file-backed module.
func (ix *Index) rustExport(fileID int64, name string, seen map[int64]bool) (store.NodeRow, exportLookup) {
	return ix.rustExportPrefix(fileID, "", name, seen)
}

// resolveRust is the Rust ladder.
func (ix *Index) resolveRust(fromFile int64, fromPath string, ref facts.Ref) Resolution {
	// `Self { .. }` in an impl is the enclosing type, not a builtin.
	if ref.Name == "Self" && ref.Form == "ident" {
		typeName := classOf(ref.Enclosing)
		for _, n := range ix.byFile[fromFile][typeName] {
			if n.QName == typeName && isRustTypeKind(n.Kind) {
				return rustTarget(ref, n, 0.90, "rs.self.type")
			}
		}
	}

	if ref.Form == "ident" && isRustBuiltin(ref.Name) {
		return Resolution{External: true}
	}
	if ref.Form == "member" && ref.ReceiverType != "" && isRustBuiltin(rustTypeBase(ref.ReceiverType)) {
		return Resolution{External: true}
	}

	imports := ix.importsOf(fromFile)
	scope := ix.rustInlineScope(fromFile, ref.Enclosing)

	if ref.Form == "this" {
		enclosingType := classOf(ref.Enclosing)
		qname := enclosingType + "." + ref.Name
		for _, n := range ix.byFile[fromFile][ref.Name] {
			if n.QName == qname {
				return rustTarget(ref, n, 0.90, "rs.self.method")
			}
		}
		if isRustBuiltin(enclosingType) {
			return Resolution{External: true}
		}
		return Resolution{}
	}

	if ref.Form == "ident" {
		if binding, ok := imports[ref.Name]; ok {
			// A binding that names a module but not the symbol (for example an
			// import inside an inline test module, which our file-scoped import
			// table cannot place) must not hide a definition in this file. Only a
			// real target or an external source ends the ladder here.
			if r := ix.rustByImport(fromFile, fromPath, binding, ref); r.Found || r.External {
				return r
			}
		}
		if r, ok := ix.rustByStar(fromFile, fromPath, ref); ok {
			return r
		}
		if nodes := ix.byFile[fromFile][ref.Name]; len(nodes) > 0 {
			if picked := pick(nodes, ref.Arity); len(picked) == 1 {
				return rustTarget(ref, picked[0], 0.85, "rs.local")
			}
		}
		return ix.byUniqueRust(ref, ix.global[ref.Name])
	}

	if ref.Form == "scoped" {
		if r, ok := ix.rustScoped(fromFile, fromPath, ref, imports, scope); ok {
			return r
		}
		return Resolution{}
	}

	if ref.Form == "member" {
		if ref.ReceiverType != "" {
			return ix.rustTypedMethod(fromFile, fromPath, ref, imports, scope)
		}
		return Resolution{}
	}

	return Resolution{}
}

func (ix *Index) rustByImport(fromFile int64, fromPath string, binding facts.Import, ref facts.Ref) Resolution {
	if binding.Source == "" {
		return Resolution{External: true}
	}

	if binding.Imported == "*" {
		target := ix.resolveRustModule(fromPath, binding.Source)
		if target.State == rustModuleLocal {
			return Resolution{}
		}
		return Resolution{External: true}
	}

	target := ix.resolveRustModule(fromPath, binding.Source)
	if target.State != rustModuleLocal || target.File == 0 {
		if target.State == rustModuleExternal {
			return Resolution{External: true}
		}
		return Resolution{}
	}

	n, lookup := ix.rustExportPrefix(target.File, target.Prefix, binding.Imported, map[int64]bool{})
	switch lookup {
	case exportExternal:
		return Resolution{External: true}
	case exportFound, exportFoundReexport:
		switch {
		case lookup == exportFoundReexport:
			return rustTarget(ref, n, 0.95, "rs.use.reexport")
		case binding.Local != binding.Imported:
			return rustTarget(ref, n, 1.0, "rs.use.alias")
		case target.Layout == "workspace":
			return rustTarget(ref, n, 0.95, "rs.use.workspace")
		default:
			return rustTarget(ref, n, 1.0, "rs.use.named")
		}
	}

	// The imported name may be a submodule (`use crate::foo;` / `use crate::foo as f`).
	if target := ix.resolveRustModule(fromPath, joinRustPath(binding.Source, binding.Imported)); target.State == rustModuleLocal && target.File != 0 {
		return Resolution{}
	}
	return Resolution{}
}

func (ix *Index) rustByStar(fromFile int64, fromPath string, ref facts.Ref) (Resolution, bool) {
	for _, binding := range ix.importsOf(fromFile) {
		if binding.Local != "*" || binding.Source == "" {
			continue
		}
		target := ix.resolveRustModule(fromPath, binding.Source)
		if target.State != rustModuleLocal || target.File == 0 {
			continue
		}
		if n, lookup := ix.rustExportPrefix(target.File, target.Prefix, ref.Name, map[int64]bool{}); lookup == exportFound || lookup == exportFoundReexport {
			return rustTarget(ref, n, 0.80, "rs.use.glob"), true
		}
	}
	return Resolution{}, false
}

// rustScoped resolves `a::b::c()` and `Type::method()`.
func (ix *Index) rustScoped(fromFile int64, fromPath string, ref facts.Ref, imports map[string]facts.Import, scope string) (Resolution, bool) {
	if ref.Receiver == "Self" {
		qname := classOf(ref.Enclosing) + "." + ref.Name
		for _, n := range ix.byQName[qname] {
			if n.FileID == fromFile {
				return rustTarget(ref, n, 0.90, "rs.self.assoc"), true
			}
		}
	}

	spec, _ := ix.rustReceiverSpec(fromPath, ref.Receiver, imports, scope)

	// A type path with a known associated function (`Type::new`).
	if r, ok := ix.rustAssoc(fromFile, fromPath, spec, ref, imports, scope); ok {
		return r, true
	}

	// A module path (`crate::foo::bar`, `store::open`).
	target := ix.resolveRustModuleScoped(fromPath, scope, spec)
	if target.State == rustModuleLocal && target.File != 0 {
		if n, lookup := ix.rustExportPrefix(target.File, target.Prefix, ref.Name, map[int64]bool{}); lookup == exportFound || lookup == exportFoundReexport {
			rule := rustModuleRule(target.Layout)
			if lookup == exportFoundReexport {
				rule = "rs.use.reexport"
			}
			return rustTarget(ref, n, 0.95, rule), true
		}
		return Resolution{}, true
	}
	if target.State == rustModuleExternal && strings.Contains(spec, "::") {
		return Resolution{External: true}, true
	}
	return Resolution{}, false
}

// rustModuleRule names the module-layout feature a scoped path exercised.
func rustModuleRule(layout string) string {
	switch layout {
	case "workspace":
		return "rs.module.workspace"
	case "attr":
		return "rs.module.path.attr"
	case "inline":
		return "rs.module.inline"
	}
	return "rs.module.path"
}

// rustAssoc resolves Type::method, including a type reached through a module
// path (`crate::models::User::new`).
func (ix *Index) rustAssoc(fromFile int64, fromPath, spec string, ref facts.Ref, imports map[string]facts.Import, scope string) (Resolution, bool) {
	typeName := spec
	moduleSpec := ""
	if i := strings.LastIndex(spec, "::"); i >= 0 {
		moduleSpec = spec[:i]
		typeName = spec[i+2:]
	}

	if typeName == "" {
		return Resolution{}, false
	}

	// Same-file type.
	for _, n := range ix.byFile[fromFile][typeName] {
		if !isRustTypeKind(n.Kind) {
			continue
		}
		if r, ok := ix.rustAssocOnType(n, ref); ok {
			return r, true
		}
	}

	if moduleSpec != "" {
		target := ix.resolveRustModuleScoped(fromPath, scope, moduleSpec)
		switch target.State {
		case rustModuleExternal:
			return Resolution{External: true}, true
		case rustModuleLocal:
			if target.File == 0 {
				return Resolution{}, false
			}
			return ix.rustTypeAssoc(target.File, target.Prefix, typeName, ref)
		}
		return Resolution{}, false
	}

	// Imported type.
	if binding, ok := imports[typeName]; ok {
		target := ix.resolveRustModule(fromPath, binding.Source)
		switch target.State {
		case rustModuleExternal:
			return Resolution{External: true}, true
		case rustModuleLocal:
			if target.File != 0 {
				return ix.rustTypeAssoc(target.File, target.Prefix, binding.Imported, ref)
			}
		}
	}

	if isRustBuiltin(typeName) {
		return Resolution{External: true}, true
	}
	if rustExternalMethods[ref.Name] {
		return Resolution{External: true}, true
	}

	return ix.rustGlobalAssoc(typeName, ref)
}

// rustTypeAssoc resolves Type::method against the module that exports Type.
// prefix is the inline-module qname prefix of that module.
func (ix *Index) rustTypeAssoc(moduleFile int64, prefix, typeName string, ref facts.Ref) (Resolution, bool) {
	typeNode, lookup := ix.rustExportPrefix(moduleFile, prefix, typeName, map[int64]bool{})
	if lookup == exportExternal {
		return Resolution{External: true}, true
	}
	if lookup != exportFound && lookup != exportFoundReexport {
		// The use binding proves the name is in scope; a private item (visible
		// to a descendant module) has no export row.
		if n, ok := ix.rustTypeInModule(moduleFile, prefix, typeName); ok {
			typeNode = n
		} else {
			return Resolution{}, false
		}
	}
	if !isRustTypeKind(typeNode.Kind) {
		return Resolution{}, false
	}
	return ix.rustAssocOnType(typeNode, ref)
}

// rustTypeInModule finds a type declaration by name in one module, ignoring
// the export flag. Callers use it only after an import already put the name in
// scope, so a private item is a legitimate answer.
func (ix *Index) rustTypeInModule(file int64, prefix, name string) (store.NodeRow, bool) {
	qname := name
	if prefix != "" {
		qname = prefix + "." + name
	}
	for _, n := range ix.byFile[file][name] {
		if !isRustTypeKind(n.Kind) {
			continue
		}
		if n.QName == qname {
			return n, true
		}
	}
	return store.NodeRow{}, false
}

// rustAssocOnType resolves an associated function on a known type node. Inherent
// and trait impl blocks share the Type.method qname, so two to five candidates
// stay a 0.40 ambiguous group and more than five are dropped; one candidate is
// never picked out of several.
func (ix *Index) rustAssocOnType(typeNode store.NodeRow, ref facts.Ref) (Resolution, bool) {
	qname := typeNode.QName + "." + ref.Name
	if nodes := ix.rustMethods(qname, func(file int64) bool { return file == typeNode.FileID }); len(nodes) > 0 {
		return ix.rustCandidateResolution(ref, nodes, "rs.assoc.fn", 0.95, "rs.assoc.ambiguous"), true
	}
	if nodes := ix.rustMethods(qname, func(file int64) bool { return ix.rustSameCrate(typeNode.FileID, file) }); len(nodes) > 0 {
		return ix.rustCandidateResolution(ref, nodes, "rs.assoc.fn.crate", 0.90, "rs.assoc.ambiguous"), true
	}
	if nodes := ix.rustTraitMethods(typeNode, ref.Name); len(nodes) > 0 {
		return ix.rustCandidateResolution(ref, nodes, "rs.trait.method", 0.80, "rs.trait.method.ambiguous"), true
	}
	if typeNode.Kind == "enum" && startsUpper(ref.Name) {
		return rustTarget(ref, typeNode, 0.90, "rs.enum.variant"), true
	}
	if rustExternalMethods[ref.Name] {
		return Resolution{External: true}, true
	}
	return Resolution{}, false
}

// rustGlobalAssoc is the fallback when the type is not in scope: a unique
// Type.method qname is still a real answer, at lower confidence.
func (ix *Index) rustGlobalAssoc(typeName string, ref facts.Ref) (Resolution, bool) {
	nodes := ix.rustMethods(typeName+"."+ref.Name, nil)
	if len(nodes) == 0 {
		return Resolution{}, false
	}
	return ix.rustCandidateResolution(ref, nodes, "rs.assoc.global", 0.70, "rs.assoc.global.ambiguous"), true
}

func (ix *Index) rustTypedMethod(fromFile int64, fromPath string, ref facts.Ref, imports map[string]facts.Import, scope string) Resolution {
	prefix, base := splitRustType(ref.ReceiverType)
	if base == "" {
		base = ref.ReceiverType
	}
	if isRustBuiltin(base) {
		return Resolution{External: true}
	}
	if prefix != "" && !isRustRelative(prefix) {
		if target := ix.resolveRustModuleScoped(fromPath, scope, prefix); target.State == rustModuleExternal {
			return Resolution{External: true}
		}
	}

	// A generic receiver follows its declared bounds to the trait's local
	// method definitions. An unbounded type parameter can only call methods
	// from a dependency or macro-generated impl, so it is external rather than
	// pending.
	if ref.ReceiverBound != "" {
		if r, ok := ix.rustBoundMethod(fromFile, fromPath, ref, imports, scope); ok {
			return r
		}
	} else if ref.ReceiverParam {
		return Resolution{External: true}
	}

	// The receiver's type, when the repo defines it. A type imported from a
	// dependency is external, exactly like a builtin receiver.
	typeNode, external := ix.rustReceiverType(fromFile, fromPath, ref.ReceiverType, base, imports, scope)
	if external {
		return Resolution{External: true}
	}

	qname := base + "." + ref.Name

	// 1. A method defined in the same file as the call.
	if nodes := ix.rustMethods(qname, func(file int64) bool { return file == fromFile }); len(nodes) > 0 {
		rule, confidence := "rs.typed.method", 0.85
		if ref.ReceiverFrom == "return" {
			rule, confidence = "rs.typed.method.return", 0.80
		}
		return ix.rustCandidateResolution(ref, nodes, rule, confidence, "rs.typed.method.ambiguous")
	}

	// 2. The receiver's type is a known local type. A method in the type's own
	// file is the imported-type answer; an impl block in another file of the
	// same crate is slightly weaker because the type was not defined there.
	if typeNode.ID != 0 {
		if nodes := ix.rustMethods(qname, func(file int64) bool { return file == typeNode.FileID }); len(nodes) > 0 {
			rule, confidence := "rs.typed.method", 0.85
			if ref.ReceiverFrom == "return" {
				rule, confidence = "rs.typed.method.return", 0.80
			}
			return ix.rustCandidateResolution(ref, nodes, rule, confidence, "rs.typed.method.ambiguous")
		}
		if nodes := ix.rustMethods(qname, func(file int64) bool { return ix.rustSameCrate(typeNode.FileID, file) }); len(nodes) > 0 {
			return ix.rustCandidateResolution(ref, nodes, "rs.typed.method.crate", 0.80, "rs.typed.method.ambiguous")
		}
		// 3. No local inherent method: follow the type's IMPLEMENTS links to a
		// trait default method. Ambiguity across traits stays visible.
		if nodes := ix.rustTraitMethods(typeNode, ref.Name); len(nodes) > 0 {
			return ix.rustCandidateResolution(ref, nodes, "rs.trait.method", 0.80, "rs.trait.method.ambiguous")
		}
	}

	// 4. Global uniqueness, then ambiguity.
	nodes := ix.rustMethods(qname, nil)
	switch len(nodes) {
	case 0:
		if rustExternalMethods[ref.Name] {
			return Resolution{External: true}
		}
		return Resolution{}
	case 1:
		return rustTarget(ref, nodes[0], 0.70, "rs.typed.method.global")
	}
	return ix.rustCandidateResolution(ref, nodes, "rs.typed.method.global", 0.70, "rs.typed.method.ambiguous")
}

// rustMethods returns the method nodes for a qname, optionally filtered by
// file.
func (ix *Index) rustMethods(qname string, keep func(file int64) bool) []store.NodeRow {
	var out []store.NodeRow
	for _, n := range ix.byQName[qname] {
		if n.Kind != "method" {
			continue
		}
		if keep != nil && !keep(n.FileID) {
			continue
		}
		out = append(out, n)
	}
	return out
}

// rustCandidateResolution applies the ambiguity policy to a method tier: one
// candidate keeps the tier's confidence, two to five become a 0.40 group with
// every candidate visible, and more than five are dropped.
func (ix *Index) rustCandidateResolution(ref facts.Ref, nodes []store.NodeRow, rule string, confidence float64, ambiguousRule string) Resolution {
	nodes = uniqueNodes(nodes)
	switch len(nodes) {
	case 0:
		return Resolution{}
	case 1:
		return rustTarget(ref, nodes[0], confidence, rule)
	}
	if len(nodes) > maxCandidates {
		return Resolution{}
	}
	ids := make([]int64, 0, len(nodes))
	for _, n := range nodes {
		ids = append(ids, n.ID)
	}
	return Resolution{Confidence: 0.40, Rule: ambiguousRule, Candidates: ids, Found: true}
}

// uniqueNodes dedupes by node id and orders by id so candidate lists are
// deterministic across runs and index orderings.
func uniqueNodes(nodes []store.NodeRow) []store.NodeRow {
	seen := make(map[int64]bool, len(nodes))
	out := make([]store.NodeRow, 0, len(nodes))
	for _, n := range nodes {
		if seen[n.ID] {
			continue
		}
		seen[n.ID] = true
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// rustReceiverType returns the type node that defines the receiver's type, and
// whether the name resolves to a dependency instead (its methods are external).
func (ix *Index) rustReceiverType(fromFile int64, fromPath, receiverType, base string, imports map[string]facts.Import, scope string) (store.NodeRow, bool) {
	prefix, _ := splitRustType(receiverType)
	if prefix != "" {
		target := ix.resolveRustModuleScoped(fromPath, scope, prefix)
		switch target.State {
		case rustModuleExternal:
			return store.NodeRow{}, !isRustRelative(prefix)
		case rustModuleLocal:
			if target.File == 0 {
				return store.NodeRow{}, false
			}
			if n, lookup := ix.rustExportPrefix(target.File, target.Prefix, base, map[int64]bool{}); lookup == exportFound || lookup == exportFoundReexport {
				return n, false
			}
			if n, ok := ix.rustTypeInModule(target.File, target.Prefix, base); ok {
				return n, false
			}
		}
		return store.NodeRow{}, false
	}
	for _, n := range ix.byFile[fromFile][base] {
		if isRustTypeKind(n.Kind) {
			return n, false
		}
	}
	if binding, ok := imports[base]; ok {
		if binding.Source == "" {
			return store.NodeRow{}, true
		}
		target := ix.resolveRustModule(fromPath, binding.Source)
		switch target.State {
		case rustModuleExternal:
			return store.NodeRow{}, true
		case rustModuleLocal:
			if target.File == 0 {
				return store.NodeRow{}, false
			}
			if n, lookup := ix.rustExportPrefix(target.File, target.Prefix, binding.Imported, map[int64]bool{}); lookup == exportFound || lookup == exportFoundReexport {
				return n, false
			}
			if n, ok := ix.rustTypeInModule(target.File, target.Prefix, binding.Imported); ok {
				return n, false
			}
		}
	}
	return store.NodeRow{}, false
}

// rustTraitMethods collects the trait method declarations a local type receives
// through its IMPLEMENTS links, including inherited supertraits.
func (ix *Index) rustTraitMethods(typeNode store.NodeRow, name string) []store.NodeRow {
	if ix.rustImpls == nil {
		return nil
	}
	var out []store.NodeRow
	seen := map[int64]bool{}
	sawExternal := false
	for _, traitID := range ix.rustImpls[typeNode.ID] {
		trait, ok := ix.rustTypeByID[traitID]
		if !ok || trait.Kind != "trait" {
			continue
		}
		ix.collectTraitMethods(trait, name, 0, map[int64]bool{}, seen, &out, &sawExternal)
	}
	return out
}

// rustSameCrate reports whether two files belong to the same Cargo package.
func (ix *Index) rustSameCrate(a, b int64) bool {
	if a == b {
		return true
	}
	crateA, _, okA := ix.rustModuleOf(ix.paths[a])
	crateB, _, okB := ix.rustModuleOf(ix.paths[b])
	return okA && okB && crateA == crateB
}

// maxTraitDepth bounds the supertrait walk, like Python's MRO cap.
const maxTraitDepth = 4

// rustBoundMethod follows a type parameter's bounds to local trait method
// definitions. It is bounded: a local trait, its same-repo supertraits, and
// nothing else.
func (ix *Index) rustBoundMethod(fromFile int64, fromPath string, ref facts.Ref, imports map[string]facts.Import, scope string) (Resolution, bool) {
	var traits []store.NodeRow
	external := false
	for _, bound := range strings.Split(ref.ReceiverBound, "|") {
		n, found, ext := ix.rustBoundTrait(fromFile, fromPath, bound, imports, scope)
		switch {
		case found:
			traits = append(traits, n)
		case ext:
			external = true
		}
	}
	if len(traits) == 0 {
		if external {
			return Resolution{External: true}, true
		}
		return Resolution{}, false
	}

	var candidates []store.NodeRow
	seen := map[int64]bool{}
	sawExternal := false
	for _, trait := range traits {
		ix.collectTraitMethods(trait, ref.Name, 0, map[int64]bool{}, seen, &candidates, &sawExternal)
	}
	if len(candidates) == 0 {
		if sawExternal {
			return Resolution{External: true}, true
		}
		return Resolution{}, true
	}
	return ix.rustCandidateResolution(ref, candidates, "rs.generic.bound", 0.85, "rs.generic.bound.ambiguous"), true
}

// collectTraitMethods gathers a trait's method declarations and walks its
// supertraits, following the reserved "@trait:" bindings recorded at extraction
// time. A supertrait with no local definition ends that branch as external.
func (ix *Index) collectTraitMethods(trait store.NodeRow, name string, depth int, visited map[int64]bool, seen map[int64]bool, out *[]store.NodeRow, sawExternal *bool) {
	if visited[trait.ID] || depth > maxTraitDepth {
		return
	}
	visited[trait.ID] = true
	for _, n := range ix.byQName[trait.QName+"."+name] {
		if n.Kind != "method" || n.FileID != trait.FileID || seen[n.ID] {
			continue
		}
		seen[n.ID] = true
		*out = append(*out, n)
	}

	fileImports := ix.importsOf(trait.FileID)
	prefix := "@trait:" + trait.QName + "#"
	var locals []string
	for local := range fileImports {
		if strings.HasPrefix(local, prefix) {
			locals = append(locals, local)
		}
	}
	sort.Strings(locals)
	scope := ix.rustInlineScope(trait.FileID, trait.QName)
	for _, local := range locals {
		next, found, ext := ix.rustBoundTrait(trait.FileID, ix.paths[trait.FileID], fileImports[local].Imported, fileImports, scope)
		if !found {
			if ext {
				*sawExternal = true
			}
			continue
		}
		ix.collectTraitMethods(next, name, depth+1, visited, seen, out, sawExternal)
	}
}

// rustBoundTrait resolves one bound path to a local trait definition. The
// second result reports a local definition; the third reports that the bound is
// external (a prelude or dependency trait, or an unresolved module), which
// makes the call external rather than pending.
func (ix *Index) rustBoundTrait(fromFile int64, fromPath, bound string, imports map[string]facts.Import, scope string) (store.NodeRow, bool, bool) {
	prefix, name := splitRustType(bound)
	if name == "" {
		return store.NodeRow{}, false, false
	}
	if prefix != "" {
		target := ix.resolveRustModuleScoped(fromPath, scope, prefix)
		switch target.State {
		case rustModuleExternal:
			return store.NodeRow{}, false, true
		case rustModuleLocal:
			if target.File == 0 {
				return store.NodeRow{}, false, false
			}
			n, lookup := ix.rustExportPrefix(target.File, target.Prefix, name, map[int64]bool{})
			if r, found, ext := rustTraitLookup(n, lookup); found || ext {
				return r, found, ext
			}
			if n, ok := ix.rustTypeInModule(target.File, target.Prefix, name); ok && n.Kind == "trait" {
				return n, true, false
			}
			return store.NodeRow{}, false, false
		}
	}
	if binding, ok := imports[name]; ok && binding.Imported != "*" && binding.Local == name {
		target := ix.resolveRustModule(fromPath, binding.Source)
		switch target.State {
		case rustModuleExternal:
			return store.NodeRow{}, false, true
		case rustModuleLocal:
			if target.File == 0 {
				return store.NodeRow{}, false, false
			}
			n, lookup := ix.rustExportPrefix(target.File, target.Prefix, binding.Imported, map[int64]bool{})
			if r, found, ext := rustTraitLookup(n, lookup); found || ext {
				return r, found, ext
			}
			if n, ok := ix.rustTypeInModule(target.File, target.Prefix, binding.Imported); ok && n.Kind == "trait" {
				return n, true, false
			}
		}
	}
	// A trait in the same module: the file root, or the inline module the call
	// itself sits in.
	for _, n := range ix.byFile[fromFile][name] {
		if n.Kind != "trait" {
			continue
		}
		if n.QName == name || (scope != "" && n.QName == scope+"."+name) {
			return n, true, false
		}
	}
	// A glob import can bring a local trait into scope.
	var globs []string
	for local := range imports {
		if local == "*" {
			globs = append(globs, local)
		}
	}
	sort.Strings(globs)
	for _, local := range globs {
		target := ix.resolveRustModule(fromPath, imports[local].Source)
		if target.State != rustModuleLocal || target.File == 0 {
			continue
		}
		n, lookup := ix.rustExportPrefix(target.File, target.Prefix, name, map[int64]bool{})
		if (lookup == exportFound || lookup == exportFoundReexport) && n.Kind == "trait" {
			return n, true, false
		}
	}
	// Rust resolves a bound that is not local from the prelude or a dependency;
	// treating it as external keeps the unresolved queue honest.
	return store.NodeRow{}, false, true
}

func rustTraitLookup(n store.NodeRow, lookup exportLookup) (store.NodeRow, bool, bool) {
	switch lookup {
	case exportFound, exportFoundReexport:
		if n.Kind == "trait" {
			return n, true, false
		}
		return store.NodeRow{}, false, false
	case exportExternal:
		return store.NodeRow{}, false, true
	}
	return store.NodeRow{}, false, false
}

// rustReceiverSpec turns a receiver into a path, following module aliases.
func (ix *Index) rustReceiverSpec(fromPath, receiver string, imports map[string]facts.Import, scope string) (string, bool) {
	if receiver == "" {
		return "", false
	}
	if binding, ok := imports[receiver]; ok {
		if isModule, spec := ix.rustBindingModule(fromPath, binding); isModule {
			return spec, true
		}
	}

	for i := 0; i < len(receiver); {
		next := strings.Index(receiver[i:], "::")
		if next < 0 {
			break
		}
		prefix := receiver[:i+next]
		if binding, ok := imports[prefix]; ok {
			if isModule, spec := ix.rustBindingModule(fromPath, binding); isModule {
				return spec + receiver[i+next:], true
			}
		}
		i += next + 2
	}

	target := ix.resolveRustModuleScoped(fromPath, scope, receiver)
	return receiver, target.State == rustModuleLocal
}

// rustBindingModule reports whether a use binding names a module rather than a
// symbol, and returns the module path. `use crate::util;` and `use crate::foo as
// f;` both bind modules.
func (ix *Index) rustBindingModule(fromPath string, binding facts.Import) (bool, string) {
	if binding.Source == "" {
		return false, ""
	}
	if binding.Imported == "*" {
		return true, binding.Source
	}
	spec := joinRustPath(binding.Source, binding.Imported)
	if target := ix.resolveRustModule(fromPath, spec); target.State == rustModuleLocal && target.File != 0 {
		return true, spec
	}
	return false, ""
}

func (ix *Index) byUniqueRust(ref facts.Ref, candidates []store.NodeRow) Resolution {
	picked := pick(candidates, ref.Arity)
	switch len(picked) {
	case 0:
		return Resolution{}
	case 1:
		return rustTarget(ref, picked[0], 0.60, "rs.global.unique")
	}
	if len(picked) > maxCandidates {
		return Resolution{}
	}
	ids := make([]int64, 0, len(picked))
	for _, n := range picked {
		ids = append(ids, n.ID)
	}
	return Resolution{Confidence: 0.40, Rule: "rs.global.ambiguous", Candidates: ids, Found: true}
}

// rustTarget applies the tuple-struct rule: a call resolving to a struct or
// enum is construction, stored as REFERENCES.
func rustTarget(ref facts.Ref, n store.NodeRow, confidence float64, rule string) Resolution {
	r := Resolution{Dst: n.ID, Confidence: confidence, Rule: rule, Found: true}
	if ref.Kind == "call" && (n.Kind == "struct" || n.Kind == "enum") {
		r.Kind = "REFERENCES"
	}
	return r
}

// rustTypeBase strips a trailing generic/associated-type suffix for builtin
// checks ("T::Item" stays a projection, not a builtin).
func rustTypeBase(typ string) string {
	_, base := splitRustType(typ)
	if base == "" {
		return typ
	}
	return base
}

func isRustTypeKind(kind string) bool {
	switch kind {
	case "struct", "enum", "trait", "type":
		return true
	}
	return false
}

func startsUpper(name string) bool {
	if name == "" {
		return false
	}
	c := name[0]
	return c >= 'A' && c <= 'Z'
}

func splitRustPath(spec string) []string {
	parts := strings.Split(spec, "::")
	out := parts[:0]
	for _, part := range parts {
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func joinRustPath(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "::" + name
}

// splitRustType splits a Rust path type into its module prefix and last
// segment ("crate::models::User" -> "crate::models", "User").
func splitRustType(typ string) (string, string) {
	if i := strings.LastIndex(typ, "::"); i >= 0 {
		return typ[:i], typ[i+2:]
	}
	return "", typ
}

func isRustRelative(spec string) bool {
	first := spec
	if i := strings.Index(spec, "::"); i >= 0 {
		first = spec[:i]
	}
	return first == "crate" || first == "self" || first == "super"
}

// rustExternalMethods are trait methods with no local definition on a known
// receiver type. The trait itself may be a dependency or macro-generated, so
// the call is external rather than a pending name. Inherent methods are still
// resolved by qname first.
var rustExternalMethods = map[string]bool{
	"to_string": true, "to_owned": true, "clone": true, "fmt": true,
	"serialize": true, "deserialize": true, "eq": true, "ne": true,
	"cmp": true, "partial_cmp": true, "hash": true, "next": true,
	"iter": true, "iter_mut": true, "into_iter": true, "len": true,
	"is_empty": true, "get": true, "get_mut": true, "insert": true,
	"remove": true, "push": true, "pop": true, "write_str": true,
	"write_fmt": true, "write_all": true, "flush": true, "as_ref": true,
	"as_mut": true, "into": true, "from": true, "try_from": true,
	"as_bytes": true, "as_str": true, "extend_from_slice": true,
	"contains": true, "starts_with": true, "ends_with": true,
	"split": true, "trim": true, "parse": true,
}

var rustBuiltins = map[string]bool{
	"String": true, "Vec": true, "Option": true, "Result": true, "Box": true,
	"Rc": true, "Arc": true, "RefCell": true, "Cell": true, "HashMap": true,
	"HashSet": true, "BTreeMap": true, "BTreeSet": true, "VecDeque": true,
	"Cow": true, "Path": true, "PathBuf": true, "OsStr": true, "OsString": true,
	"str": true, "slice": true, "array": true, "usize": true, "isize": true, "bool": true, "char": true,
	"f32": true, "f64": true, "u8": true, "u16": true, "u32": true, "u64": true,
	"u128": true, "i8": true, "i16": true, "i32": true, "i64": true, "i128": true,
	"Some": true, "None": true, "Ok": true, "Err": true, "Self": true,
	"Default": true, "Clone": true, "Copy": true, "Debug": true, "Display": true,
	"Iterator": true, "IntoIterator": true, "From": true, "Into": true,
	"TryFrom": true, "TryInto": true, "AsRef": true, "AsMut": true,
	"PartialEq": true, "Eq": true, "PartialOrd": true, "Ord": true, "Hash": true,
	"Send": true, "Sync": true, "Sized": true, "Drop": true,
	"Fn": true, "FnMut": true, "FnOnce": true, "ToString": true, "ToOwned": true,
	"println": true, "format": true, "vec": true, "panic": true,
	"assert": true, "assert_eq": true, "assert_ne": true, "todo": true,
	"unimplemented": true, "unreachable": true, "write": true, "writeln": true,
}

func isRustBuiltin(name string) bool { return rustBuiltins[name] }
