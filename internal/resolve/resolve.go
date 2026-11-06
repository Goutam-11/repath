// Package resolve turns per-file references into graph edges. It implements the
// confidence ladder from PLAN.md §5: explicit imports first, then local scope,
// then unique global names, then ambiguity.
package resolve

import (
	"context"
	"path"
	"strings"

	"github.com/Goutam-11/repath/internal/lang/tsx"
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
	External   bool
	Found      bool
}

// Index is the global name index used for resolution.
type Index struct {
	byFile  map[int64]map[string][]store.NodeRow
	global  map[string][]store.NodeRow
	methods map[string][]store.NodeRow
	exports map[int64][]store.NodeRow
	files   map[string]int64
	aliases *Aliases
}

// Build loads every symbol and file into the resolution index.
func Build(ctx context.Context, st *store.Store, root string) (*Index, error) {
	nodes, err := st.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	paths, err := st.FilePaths(ctx)
	if err != nil {
		return nil, err
	}

	ix := &Index{
		byFile:  make(map[int64]map[string][]store.NodeRow),
		global:  make(map[string][]store.NodeRow),
		methods: make(map[string][]store.NodeRow),
		exports: make(map[int64][]store.NodeRow),
		files:   make(map[string]int64, len(paths)),
		aliases: LoadAliases(root),
	}

	for id, p := range paths {
		ix.files[p] = id
	}
	for _, n := range nodes {
		name := n.Name
		if name == "" || n.QName == "" {
			continue
		}
		if ix.byFile[n.FileID] == nil {
			ix.byFile[n.FileID] = make(map[string][]store.NodeRow)
		}
		ix.byFile[n.FileID][name] = append(ix.byFile[n.FileID][name], n)
		ix.global[name] = append(ix.global[name], n)
		if n.Kind == "method" {
			ix.methods[name] = append(ix.methods[name], n)
		}
		if n.Exported {
			ix.exports[n.FileID] = append(ix.exports[n.FileID], n)
		}
	}
	return ix, nil
}

// Resolve returns where ref points. imports is the referring file's binding map.
func (ix *Index) Resolve(fromFile int64, fromPath string, ref tsx.Ref, imports map[string]tsx.Import) Resolution {
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

	// 4. Bare member calls can only be guessed by unique method name.
	if ref.Form == "member" {
		return ix.byUnique(ref.Name, ref.Arity, ix.methods[ref.Name], "ts.method")
	}

	// 5. Identifiers: local scope, then global uniqueness.
	if nodes := ix.byFile[fromFile][ref.Name]; len(nodes) > 0 {
		if picked := pick(nodes, ref.Arity); len(picked) == 1 {
			return Resolution{Dst: picked[0].ID, Confidence: 0.85, Rule: "ts.local", Found: true}
		}
	}

	return ix.byUnique(ref.Name, ref.Arity, ix.global[ref.Name], "ts.global")
}

func (ix *Index) byImport(fromPath string, binding tsx.Import, ref tsx.Ref) Resolution {
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

// ResolveName resolves a bare name by global uniqueness. It is used when
// retrying references that were left pending by an earlier run, where the
// import context is no longer available.
func (ix *Index) ResolveName(name string) (int64, bool) {
	r := ix.byUnique(name, 0, ix.global[name], "ts.global")
	if r.Found && len(r.Candidates) == 0 {
		return r.Dst, true
	}
	return 0, false
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

func isBuiltin(name string) bool { return builtins[name] }
