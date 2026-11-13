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

// loadRustCrates finds the source root of every Cargo package in the repo. A
// crate root is the directory containing lib.rs or main.rs.
//
// The indexed file list already names every src/lib.rs and src/main.rs, so the
// roots are derived from it instead of walking every directory on each run.
// The walk remains as a fallback for a Rust tree whose crate roots are not
// indexable (for example an oversized or skipped lib.rs).
func loadRustCrates(root string, paths map[int64]string) []string {
	seen := map[string]bool{}
	var crates []string
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
			crates = append(crates, src)
		}
	}

	if len(crates) == 0 && hasRust {
		return walkRustCrates(root)
	}
	sortRustCrates(crates)
	return crates
}

// walkRustCrates is the filesystem fallback used when the indexed paths do not
// describe the crate roots.
func walkRustCrates(root string) []string {
	seen := map[string]bool{}
	var crates []string

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
			crates = append(crates, rel)
		}
		return nil
	})

	sortRustCrates(crates)
	return crates
}

// sortRustCrates puts the longest prefix first so a nested crate wins over the
// repo root.
func sortRustCrates(crates []string) {
	sort.Slice(crates, func(i, j int) bool {
		if len(crates[i]) != len(crates[j]) {
			return len(crates[i]) > len(crates[j])
		}
		return crates[i] < crates[j]
	})
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// rustModuleOf maps a file to its crate source root and module segments.
func (ix *Index) rustModuleOf(filePath string) (string, []string, bool) {
	for _, crate := range ix.crates {
		if filePath != crate && !strings.HasPrefix(filePath, crate+"/") {
			continue
		}
		rel := strings.TrimPrefix(strings.TrimPrefix(filePath, crate), "/")
		rel = strings.TrimSuffix(rel, ".rs")
		if rel == "lib" || rel == "main" {
			return crate, nil, true
		}
		rel = strings.TrimSuffix(rel, "/mod")
		if rel == "" {
			return crate, nil, true
		}
		return crate, strings.Split(rel, "/"), true
	}
	return "", nil, false
}

// resolveRustModule maps a Rust path specifier to a file. It understands
// crate/self/super, module files and mod.rs. Missing unqualified paths are
// external; missing relative paths are broken and stay unresolved.
func (ix *Index) resolveRustModule(fromPath, spec string) (int64, rustModuleState) {
	if spec == "" {
		return 0, rustModuleExternal
	}
	crate, current, ok := ix.rustModuleOf(fromPath)
	if !ok {
		return 0, rustModuleExternal
	}

	parts := splitRustPath(spec)
	if len(parts) == 0 {
		return 0, rustModuleExternal
	}

	var base []string
	index := 0
	switch parts[0] {
	case "crate":
		index = 1
	case "self":
		base = append(base, current...)
		index = 1
	case "super":
		base = append(base, current...)
		for index < len(parts) && parts[index] == "super" {
			if len(base) > 0 {
				base = base[:len(base)-1]
			}
			index++
		}
	default:
		if id := ix.rustModuleFile(crate, parts); id != 0 {
			return id, rustModuleLocal
		}
		return 0, rustModuleExternal
	}

	segments := append(base, parts[index:]...)
	if id := ix.rustModuleFile(crate, segments); id != 0 {
		return id, rustModuleLocal
	}
	return 0, rustModuleLocal
}

func (ix *Index) rustModuleFile(crate string, segments []string) int64 {
	if len(segments) == 0 {
		for _, name := range []string{"lib.rs", "main.rs"} {
			if id, ok := ix.files[path.Join(crate, name)]; ok {
				return id
			}
		}
		return 0
	}
	base := path.Join(crate, strings.Join(segments, "/"))
	for _, candidate := range []string{base + ".rs", base + "/mod.rs"} {
		if id, ok := ix.files[candidate]; ok {
			return id
		}
	}
	return 0
}

// rustExport finds a name in a module, following public re-exports.
func (ix *Index) rustExport(fileID int64, name string, seen map[int64]bool) (store.NodeRow, exportLookup) {
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
	for _, imp := range ix.imports[fileID] {
		if !imp.Public || imp.Local != name || imp.Imported == "*" {
			continue
		}
		target, state := ix.resolveRustModule(fromPath, imp.Source)
		if state != rustModuleLocal || target == 0 {
			if state == rustModuleExternal {
				external = true
			}
			continue
		}
		n, lookup := ix.rustExport(target, imp.Imported, seen)
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
	if ref.Form == "member" && ref.ReceiverType != "" && isRustBuiltin(ref.ReceiverType) {
		return Resolution{External: true}
	}

	imports := ix.imports[fromFile]

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
			return ix.rustByImport(fromFile, fromPath, binding, ref)
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
		if r, ok := ix.rustScoped(fromFile, fromPath, ref, imports); ok {
			return r
		}
		return Resolution{}
	}

	if ref.Form == "member" {
		if ref.ReceiverType != "" {
			return ix.rustTypedMethod(fromFile, fromPath, ref, imports)
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
		target, state := ix.resolveRustModule(fromPath, binding.Source)
		if state == rustModuleLocal {
			_ = target
			return Resolution{}
		}
		return Resolution{External: true}
	}

	target, state := ix.resolveRustModule(fromPath, binding.Source)
	if state != rustModuleLocal || target == 0 {
		if state == rustModuleExternal {
			return Resolution{External: true}
		}
		return Resolution{}
	}

	n, lookup := ix.rustExport(target, binding.Imported, map[int64]bool{})
	switch lookup {
	case exportExternal:
		return Resolution{External: true}
	case exportFound, exportFoundReexport:
		switch {
		case lookup == exportFoundReexport:
			return rustTarget(ref, n, 0.95, "rs.use.reexport")
		case binding.Local != binding.Imported:
			return rustTarget(ref, n, 1.0, "rs.use.alias")
		default:
			return rustTarget(ref, n, 1.0, "rs.use.named")
		}
	}

	// The imported name may be a submodule (`use crate::foo;` / `use crate::foo as f`).
	if id, subState := ix.resolveRustModule(fromPath, joinRustPath(binding.Source, binding.Imported)); subState == rustModuleLocal && id != 0 {
		return Resolution{}
	}
	return Resolution{}
}

func (ix *Index) rustByStar(fromFile int64, fromPath string, ref facts.Ref) (Resolution, bool) {
	for _, binding := range ix.imports[fromFile] {
		if binding.Local != "*" || binding.Source == "" {
			continue
		}
		target, state := ix.resolveRustModule(fromPath, binding.Source)
		if state != rustModuleLocal || target == 0 {
			continue
		}
		if n, lookup := ix.rustExport(target, ref.Name, map[int64]bool{}); lookup == exportFound || lookup == exportFoundReexport {
			return rustTarget(ref, n, 0.80, "rs.use.glob"), true
		}
	}
	return Resolution{}, false
}

// rustScoped resolves `a::b::c()` and `Type::method()`.
func (ix *Index) rustScoped(fromFile int64, fromPath string, ref facts.Ref, imports map[string]facts.Import) (Resolution, bool) {
	if ref.Receiver == "Self" {
		qname := classOf(ref.Enclosing) + "." + ref.Name
		for _, n := range ix.byQName[qname] {
			if n.FileID == fromFile {
				return rustTarget(ref, n, 0.90, "rs.self.assoc"), true
			}
		}
	}

	spec, _ := ix.rustReceiverSpec(fromPath, ref.Receiver, imports)

	// A type path with a known associated function (`Type::new`).
	if r, ok := ix.rustAssoc(fromFile, fromPath, spec, ref, imports); ok {
		return r, true
	}

	// A module path (`crate::foo::bar`, `store::open`).
	target, state := ix.resolveRustModule(fromPath, spec)
	if state == rustModuleLocal && target != 0 {
		if n, lookup := ix.rustExport(target, ref.Name, map[int64]bool{}); lookup == exportFound || lookup == exportFoundReexport {
			rule := "rs.module.path"
			if lookup == exportFoundReexport {
				rule = "rs.use.reexport"
			}
			return rustTarget(ref, n, 0.95, rule), true
		}
		return Resolution{}, true
	}
	if state == rustModuleExternal && strings.Contains(spec, "::") {
		return Resolution{External: true}, true
	}
	return Resolution{}, false
}

// rustAssoc resolves Type::method, including a type reached through a module
// path (`crate::models::User::new`).
func (ix *Index) rustAssoc(fromFile int64, fromPath, spec string, ref facts.Ref, imports map[string]facts.Import) (Resolution, bool) {
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
		for _, method := range ix.byQName[n.QName+"."+ref.Name] {
			if method.FileID == fromFile {
				return rustTarget(ref, method, 0.95, "rs.assoc.fn"), true
			}
		}
		if n.Kind == "enum" && startsUpper(ref.Name) {
			return rustTarget(ref, n, 0.90, "rs.enum.variant"), true
		}
		if rustExternalMethods[ref.Name] {
			return Resolution{External: true}, true
		}
	}

	if moduleSpec != "" {
		target, state := ix.resolveRustModule(fromPath, moduleSpec)
		if state == rustModuleExternal {
			return Resolution{External: true}, true
		}
		if state == rustModuleLocal && target != 0 {
			return ix.rustTypeAssoc(target, typeName, ref)
		}
		return Resolution{}, false
	}

	// Imported type.
	if binding, ok := imports[typeName]; ok {
		target, state := ix.resolveRustModule(fromPath, binding.Source)
		if state == rustModuleExternal {
			return Resolution{External: true}, true
		}
		if state == rustModuleLocal && target != 0 {
			return ix.rustTypeAssoc(target, binding.Imported, ref)
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

func (ix *Index) rustTypeAssoc(moduleFile int64, typeName string, ref facts.Ref) (Resolution, bool) {
	typeNode, lookup := ix.rustExport(moduleFile, typeName, map[int64]bool{})
	if lookup == exportExternal {
		return Resolution{External: true}, true
	}
	if lookup != exportFound && lookup != exportFoundReexport {
		return Resolution{}, false
	}
	if !isRustTypeKind(typeNode.Kind) {
		return Resolution{}, false
	}
	for _, method := range ix.byQName[typeNode.QName+"."+ref.Name] {
		if method.FileID == typeNode.FileID {
			return rustTarget(ref, method, 0.95, "rs.assoc.fn"), true
		}
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
	nodes := ix.byQName[typeName+"."+ref.Name]
	switch len(nodes) {
	case 0:
		return Resolution{}, false
	case 1:
		return rustTarget(ref, nodes[0], 0.70, "rs.assoc.global"), true
	}
	if len(nodes) > maxCandidates {
		return Resolution{}, false
	}
	ids := make([]int64, 0, len(nodes))
	for _, n := range nodes {
		ids = append(ids, n.ID)
	}
	return Resolution{Confidence: 0.40, Rule: "rs.typed.method.ambiguous", Candidates: ids, Found: true}, true
}

func (ix *Index) rustTypedMethod(fromFile int64, fromPath string, ref facts.Ref, imports map[string]facts.Import) Resolution {
	prefix, base := splitRustType(ref.ReceiverType)
	if base == "" {
		base = ref.ReceiverType
	}
	if isRustBuiltin(base) {
		return Resolution{External: true}
	}
	qname := base + "." + ref.Name

	for _, n := range ix.byQName[qname] {
		if n.FileID == fromFile {
			return rustTarget(ref, n, 0.85, "rs.typed.method")
		}
	}

	if prefix != "" && !isRustRelative(prefix) {
		if _, state := ix.resolveRustModule(fromPath, prefix); state == rustModuleExternal {
			return Resolution{External: true}
		}
	}
	if prefix != "" {
		if target, state := ix.resolveRustModule(fromPath, prefix); state == rustModuleLocal && target != 0 {
			if r, ok := ix.rustTypeAssoc(target, base, ref); ok {
				return r
			}
		}
	}

	if binding, ok := imports[base]; ok {
		if target, state := ix.resolveRustModule(fromPath, binding.Source); state == rustModuleLocal && target != 0 {
			for _, n := range ix.byQName[qname] {
				if n.FileID == target {
					return rustTarget(ref, n, 0.85, "rs.typed.method")
				}
			}
			if r, ok := ix.rustTypeAssoc(target, binding.Imported, ref); ok {
				return r
			}
		} else if state == rustModuleExternal {
			return Resolution{External: true}
		}
	}

	nodes := ix.byQName[qname]
	switch len(nodes) {
	case 1:
		return rustTarget(ref, nodes[0], 0.70, "rs.typed.method.global")
	case 0:
		if rustExternalMethods[ref.Name] {
			return Resolution{External: true}
		}
		return Resolution{}
	}
	if len(nodes) > maxCandidates {
		return Resolution{}
	}
	ids := make([]int64, 0, len(nodes))
	for _, n := range nodes {
		ids = append(ids, n.ID)
	}
	return Resolution{Confidence: 0.40, Rule: "rs.typed.method.ambiguous", Candidates: ids, Found: true}
}

// rustReceiverSpec turns a receiver into a path, following module aliases.
func (ix *Index) rustReceiverSpec(fromPath, receiver string, imports map[string]facts.Import) (string, bool) {
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

	_, state := ix.resolveRustModule(fromPath, receiver)
	return receiver, state == rustModuleLocal
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
	if id, state := ix.resolveRustModule(fromPath, spec); state == rustModuleLocal && id != 0 {
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
