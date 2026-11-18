// Package tsx extracts symbol definitions, references, and imports from
// TypeScript and JavaScript sources with a tree-sitter grammar.
package tsx

import (
	"context"
	"fmt"
	"strings"
	"unsafe"

	forestjavascript "github.com/alexaandru/go-sitter-forest/javascript"
	foresttsx "github.com/alexaandru/go-sitter-forest/tsx"
	foresttypescript "github.com/alexaandru/go-sitter-forest/typescript"
	sitter "github.com/alexaandru/go-tree-sitter-bare"

	"github.com/Goutam-11/repath/internal/lang/facts"
)

// Dialect selects the grammar used for a file.
type Dialect int

const (
	// TypeScript parses .ts, .mts and .cts files.
	TypeScript Dialect = iota
	// TSX parses .tsx and .jsx files.
	TSX
	// JavaScript parses .js, .mjs and .cjs files.
	JavaScript
)

// Symbol, Ref, Import, and Result are the shared extraction-fact types. The
// aliases keep tsx's public surface unchanged for existing callers.
type (
	Symbol     = facts.Symbol
	Ref        = facts.Ref
	Import     = facts.Import
	Entrypoint = facts.Entrypoint
	Anchor     = facts.Anchor
	Result     = facts.Result
)

// Extractor holds one parser per dialect. It is not safe for concurrent use;
// index workers each create their own.
type Extractor struct {
	parsers map[Dialect]*sitter.Parser
}

// New builds an extractor with the three grammars repath parses today.
func New() *Extractor {
	e := &Extractor{parsers: make(map[Dialect]*sitter.Parser, 3)}
	e.parsers[TypeScript] = newParser(foresttypescript.GetLanguage())
	e.parsers[TSX] = newParser(foresttsx.GetLanguage())
	e.parsers[JavaScript] = newParser(forestjavascript.GetLanguage())
	return e
}

func newParser(langPointer unsafe.Pointer) *sitter.Parser {
	p := sitter.NewParser()
	p.SetLanguage(sitter.NewLanguage(langPointer))
	return p
}

// Extract parses src and returns its definitions, references, and imports.
func (e *Extractor) Extract(src []byte, dialect Dialect) Result {
	parser, found := e.parsers[dialect]
	if !found {
		return Result{Err: "unsupported dialect"}
	}

	tree, err := parser.ParseString(context.Background(), nil, src)
	if err != nil {
		return Result{Err: err.Error()}
	}
	defer tree.Close()

	root := tree.RootNode()
	res := Result{OK: !root.HasError()}
	if !res.OK {
		if line := errorLine(root); line > 0 {
			res.Err = fmt.Sprintf("syntax error near line %d", line)
		} else {
			res.Err = "syntax error"
		}
	}

	w := &walker{src: src, res: &res, seen: make(map[string]bool), types: make(map[string]string)}
	for i := uint32(0); i < root.NamedChildCount(); i++ {
		w.node(root.NamedChild(i), "", false)
	}
	return res
}

type walker struct {
	src   []byte
	res   *Result
	seen  map[string]bool
	types map[string]string // "enclosing:name" → type name
}

// node visits one declaration or expression. enclosing is the qname of the
// symbol whose body contains it.
func (w *walker) node(n sitter.Node, enclosing string, exported bool) {
	if n.IsNull() {
		return
	}

	switch n.Type() {
	case "import_statement":
		w.importStatement(n)
		return

	case "export_statement":
		if decl := n.ChildByFieldName("declaration"); !decl.IsNull() {
			w.node(decl, enclosing, true)
		}
		w.reExports(n)
		return

	case "function_declaration", "generator_function_declaration":
		name := nameOf(n, w.src)
		qname := join(enclosing, name)
		w.add("function", name, qname, paramCount(n), n, exported)
		w.parameters(n.ChildByFieldName("parameters"), qname)
		w.children(n.ChildByFieldName("parameters"), qname)
		w.children(n.ChildByFieldName("body"), qname)
		return

	case "class_declaration", "abstract_class_declaration":
		name := nameOf(n, w.src)
		qname := join(enclosing, name)
		if name == "" {
			return
		}
		w.add("class", name, qname, 0, n, exported)
		w.heritage(n, qname)
		w.methods(n, qname, exported)
		return

	case "interface_declaration":
		name := nameOf(n, w.src)
		w.add("interface", name, join(enclosing, name), 0, n, exported)
		return

	case "type_alias_declaration":
		name := nameOf(n, w.src)
		w.add("type", name, join(enclosing, name), 0, n, exported)
		return

	case "enum_declaration":
		name := nameOf(n, w.src)
		w.add("enum", name, join(enclosing, name), 0, n, exported)
		return

	case "lexical_declaration", "variable_declaration":
		w.variables(n, enclosing, exported)
		return

	case "internal_module":
		name := nameOf(n, w.src)
		body := n.ChildByFieldName("body")
		if name == "" || body.IsNull() {
			return
		}
		for i := uint32(0); i < body.NamedChildCount(); i++ {
			w.node(body.NamedChild(i), join(enclosing, name), exported)
		}
		return

	case "call_expression":
		w.call(n, enclosing)
		w.routeEntrypoint(n)
		w.httpClient(n, enclosing)
		w.children(n, enclosing)
		return

	case "new_expression":
		w.ctor(n, enclosing)
		w.children(n, enclosing)
		return
	}

	w.children(n, enclosing)
}

// children visits every named child, which is how references inside arbitrary
// expressions are found.
func (w *walker) children(n sitter.Node, enclosing string) {
	if n.IsNull() {
		return
	}
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		w.node(n.NamedChild(i), enclosing, false)
	}
}

func (w *walker) variables(n sitter.Node, enclosing string, exported bool) {
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		decl := n.NamedChild(i)
		if decl.Type() != "variable_declarator" {
			continue
		}
		value := decl.ChildByFieldName("value")
		if value.IsNull() {
			continue
		}
		name := nameOf(decl, w.src)
		w.declaredType(decl, enclosing)
		// A static `require("./x")` / `await import("./x")` binds a module the
		// same way an import statement does. A computed specifier cannot name a
		// file, so it stays external (no binding, no edge).
		if spec, ok := staticModuleSpecifier(value, w.src); ok {
			w.moduleBindings(decl, spec)
		}
		switch value.Type() {
		case "arrow_function", "function_expression", "function":
			qname := join(enclosing, name)
			w.add("function", name, qname, paramCount(value), decl, exported)
			w.parameters(value.ChildByFieldName("parameters"), qname)
			w.children(value.ChildByFieldName("parameters"), qname)
			w.children(value.ChildByFieldName("body"), qname)
		default:
			// A plain assignment can still hold references ("const x = helper()").
			w.node(value, enclosing, false)
		}
	}
}

// staticModuleSpecifier reports the specifier of a `require("x")`,
// `import("x")`, or `await import("x")` expression whose argument is a plain
// string literal. Template literals with substitutions are not static.
func staticModuleSpecifier(value sitter.Node, src []byte) (string, bool) {
	for value.Type() == "await_expression" {
		if value.NamedChildCount() == 0 {
			return "", false
		}
		value = value.NamedChild(0)
	}
	if value.Type() != "call_expression" {
		return "", false
	}
	fn := value.ChildByFieldName("function")
	args := value.ChildByFieldName("arguments")
	if fn.IsNull() || args.IsNull() {
		return "", false
	}
	switch fn.Type() {
	case "import":
	case "identifier":
		if fn.Content(src) != "require" {
			return "", false
		}
	default:
		return "", false
	}
	// A bundler magic comment (`import(/* webpackChunkName: "x" */ "./x")`) is
	// a named node inside the argument list, so count real arguments instead of
	// named children.
	var arg sitter.Node
	count := 0
	for i := uint32(0); i < args.NamedChildCount(); i++ {
		child := args.NamedChild(i)
		if child.Type() == "comment" {
			continue
		}
		count++
		if count > 1 {
			return "", false
		}
		arg = child
	}
	if count != 1 || arg.Type() != "string" {
		return "", false
	}
	return unquote(arg.Content(src)), true
}

// moduleBindings turns the left-hand side of a dynamic require/import into
// import bindings: `const m = require("./x")` binds the module namespace, and
// `const { a, b: c } = require("./x")` binds two names.
func (w *walker) moduleBindings(decl sitter.Node, spec string) {
	name := decl.ChildByFieldName("name")
	if name.IsNull() || spec == "" {
		return
	}
	line := int(decl.StartPoint().Row) + 1
	switch name.Type() {
	case "identifier":
		w.res.Imports = append(w.res.Imports, Import{
			Local: name.Content(w.src), Imported: "*", Source: spec, Line: line,
		})
	case "object_pattern":
		for i := uint32(0); i < name.NamedChildCount(); i++ {
			item := name.NamedChild(i)
			switch item.Type() {
			case "shorthand_property_identifier_pattern":
				local := item.Content(w.src)
				w.res.Imports = append(w.res.Imports, Import{
					Local: local, Imported: local, Source: spec, Line: line,
				})
			case "pair_pattern":
				key := item.ChildByFieldName("key")
				if key.IsNull() {
					continue
				}
				local := key.Content(w.src)
				if value := item.ChildByFieldName("value"); !value.IsNull() && value.Type() == "identifier" {
					local = value.Content(w.src)
				}
				w.res.Imports = append(w.res.Imports, Import{
					Local: local, Imported: key.Content(w.src), Source: spec, Line: line,
				})
			case "object_assignment_pattern":
				// `{ a = 1 }` binds a with a default value.
				left := item.ChildByFieldName("left")
				if left.IsNull() {
					continue
				}
				local := left.Content(w.src)
				if left.Type() == "shorthand_property_identifier_pattern" {
					w.res.Imports = append(w.res.Imports, Import{
						Local: local, Imported: local, Source: spec, Line: line,
					})
				}
			}
		}
	}
}

// reExports records `export * from "./x"`, `export { a } from "./x"`, and
// `export { a as b } from "./x"` as public import bindings. They are the
// barrel hop the resolver follows; `export * as ns` is not followed and stays a
// documented gap.
func (w *walker) reExports(n sitter.Node) {
	source := n.ChildByFieldName("source")
	if source.IsNull() {
		// `export { local }` re-exports a symbol declared in this file, which
		// the declaration already recorded as exported.
		return
	}
	spec := unquote(source.Content(w.src))
	line := int(n.StartPoint().Row) + 1

	hasClause := false
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		child := n.NamedChild(i)
		switch child.Type() {
		case "export_clause":
			hasClause = true
			for j := uint32(0); j < child.NamedChildCount(); j++ {
				specifier := child.NamedChild(j)
				if specifier.Type() != "export_specifier" {
					continue
				}
				name := specifier.ChildByFieldName("name")
				if name.IsNull() {
					continue
				}
				local := name.Content(w.src)
				if alias := specifier.ChildByFieldName("alias"); !alias.IsNull() {
					local = alias.Content(w.src)
				}
				w.res.Imports = append(w.res.Imports, Import{
					Local: local, Imported: name.Content(w.src), Source: spec,
					Line: line, Public: true,
				})
			}
		case "namespace_export":
			hasClause = true
		}
	}
	if !hasClause {
		// `export * from "./x"`: one public binding per source. Several of
		// them are legal and common in a barrel, so the resolver keeps the full
		// list instead of a last-wins map.
		w.res.Imports = append(w.res.Imports, Import{
			Local: "*", Imported: "*", Source: spec, Line: line, Public: true,
		})
	}
}

func (w *walker) methods(class sitter.Node, classQName string, exported bool) {
	body := class.ChildByFieldName("body")
	if body.IsNull() {
		return
	}
	for i := uint32(0); i < body.NamedChildCount(); i++ {
		method := body.NamedChild(i)
		if method.Type() != "method_definition" {
			continue
		}
		name := nameOf(method, w.src)
		if name == "" {
			continue
		}
		qname := classQName + "." + name
		w.add("method", name, qname, paramCount(method), method, exported)
		w.parameters(method.ChildByFieldName("parameters"), qname)
		w.children(method.ChildByFieldName("parameters"), qname)
		w.children(method.ChildByFieldName("body"), qname)
	}
}

// heritage records extends and implements clauses as references to types.
func (w *walker) heritage(class sitter.Node, classQName string) {
	for i := uint32(0); i < class.NamedChildCount(); i++ {
		child := class.NamedChild(i)
		if child.Type() != "class_heritage" {
			continue
		}
		for j := uint32(0); j < child.NamedChildCount(); j++ {
			clause := child.NamedChild(j)
			kind := ""
			switch clause.Type() {
			case "extends_clause":
				kind = "extends"
			case "implements_clause":
				kind = "implements"
			default:
				continue
			}
			value := clause.NamedChild(0)
			if value.IsNull() {
				continue
			}
			name := lastSegment(value.Content(w.src))
			if name == "" {
				continue
			}
			w.res.Refs = append(w.res.Refs, Ref{
				Kind:      kind,
				Name:      name,
				Form:      "ident",
				Line:      int(clause.StartPoint().Row) + 1,
				Enclosing: classQName,
			})
		}
	}
}

func (w *walker) call(n sitter.Node, enclosing string) {
	fn := n.ChildByFieldName("function")
	if fn.IsNull() {
		return
	}
	arity := 0
	if args := n.ChildByFieldName("arguments"); !args.IsNull() {
		arity = int(args.NamedChildCount())
	}
	line := int(n.StartPoint().Row) + 1

	switch fn.Type() {
	case "identifier":
		w.res.Refs = append(w.res.Refs, Ref{
			Kind: "call", Name: fn.Content(w.src), Form: "ident",
			Arity: arity, Line: line, Enclosing: enclosing,
		})
	case "member_expression":
		obj := fn.ChildByFieldName("object")
		prop := fn.ChildByFieldName("property")
		if prop.IsNull() {
			return
		}
		ref := Ref{Kind: "call", Name: prop.Content(w.src), Form: "member", Arity: arity, Line: line, Enclosing: enclosing}
		if obj.Type() == "this" {
			ref.Form = "this"
		} else if obj.Type() == "identifier" {
			ref.Receiver = obj.Content(w.src)
			ref.ReceiverType = w.types[scopeKey(enclosing, ref.Receiver)]
		}
		w.res.Refs = append(w.res.Refs, ref)
	}
}

// routeEntrypoint marks a named handler passed to an Express/Fastify-style
// registration. Inline handlers have no symbol to anchor to; they are skipped
// rather than inventing a symbol.
func (w *walker) routeEntrypoint(n sitter.Node) {
	fn := n.ChildByFieldName("function")
	if fn.IsNull() || fn.Type() != "member_expression" {
		return
	}
	obj := fn.ChildByFieldName("object")
	prop := fn.ChildByFieldName("property")
	if obj.IsNull() || prop.IsNull() || obj.Type() != "identifier" {
		return
	}
	method := prop.Content(w.src)
	if !routeMethod(method) {
		return
	}
	rule := ""
	switch obj.Content(w.src) {
	case "app", "router":
		rule = "ts.route.express"
	case "fastify", "server":
		rule = "ts.route.fastify"
	default:
		return
	}

	args := n.ChildByFieldName("arguments")
	if args.IsNull() {
		return
	}
	name := routePath(args, w.src)
	httpMethod, methodOK := facts.NormalizeHTTPMethod(method)
	httpPath, pathOK := facts.NormalizeHTTPPath(name)
	for i := uint32(0); i < args.NamedChildCount(); i++ {
		arg := args.NamedChild(i)
		if arg.Type() != "identifier" {
			continue
		}
		qname := arg.Content(w.src)
		w.res.Entrypoints = append(w.res.Entrypoints, Entrypoint{
			QName: qname, Name: name, Rule: rule,
			Confidence: 0.90, Line: int(n.StartPoint().Row) + 1,
		})
		if methodOK && pathOK && method != "route" {
			w.res.Anchors = append(w.res.Anchors, facts.Anchor{
				QName: qname, Protocol: "http", Role: "server", Method: httpMethod,
				Path: httpPath, Line: int(n.StartPoint().Row) + 1, Confidence: 0.90,
			})
		}
	}
}

// httpClient records a client-side HTTP call site as a cross-language anchor.
// Only a literal same-origin path is accepted: a computed URL, a template
// string, or a full URL is not a fact the pass can match without guessing.
func (w *walker) httpClient(n sitter.Node, enclosing string) {
	if enclosing == "" {
		return
	}
	fn := n.ChildByFieldName("function")
	args := n.ChildByFieldName("arguments")
	if fn.IsNull() || args.IsNull() {
		return
	}
	method, rawPath := "", ""
	switch fn.Type() {
	case "identifier":
		if fn.Content(w.src) != "fetch" {
			return
		}
		method = "GET"
		rawPath = firstLiteral(args, w.src)
		if option, ok := httpMethodOption(args, w.src); ok {
			method = option
		}
	case "member_expression":
		obj := fn.ChildByFieldName("object")
		prop := fn.ChildByFieldName("property")
		if obj.IsNull() || prop.IsNull() || obj.Type() != "identifier" || obj.Content(w.src) != "axios" {
			return
		}
		method = prop.Content(w.src)
		rawPath = firstLiteral(args, w.src)
	default:
		return
	}
	path, ok := facts.NormalizeHTTPPath(rawPath)
	if !ok {
		return
	}
	httpMethod, ok := facts.NormalizeHTTPMethod(method)
	if !ok {
		return
	}
	w.res.Anchors = append(w.res.Anchors, facts.Anchor{
		QName: enclosing, Protocol: "http", Role: "client", Method: httpMethod,
		Path: path, Line: int(n.StartPoint().Row) + 1, Confidence: 0.90,
	})
}

// firstLiteral returns the first plain string argument, or "" when the call
// uses a template, identifier, or other computed value.
func firstLiteral(args sitter.Node, src []byte) string {
	if args.IsNull() {
		return ""
	}
	for i := uint32(0); i < args.NamedChildCount(); i++ {
		arg := args.NamedChild(i)
		if arg.Type() == "string" {
			return unquote(arg.Content(src))
		}
	}
	return ""
}

// httpMethodOption reads a literal `{ method: "POST" }` option object.
func httpMethodOption(args sitter.Node, src []byte) (string, bool) {
	if args.IsNull() {
		return "", false
	}
	for i := uint32(0); i < args.NamedChildCount(); i++ {
		obj := args.NamedChild(i)
		if obj.Type() != "object" {
			continue
		}
		for j := uint32(0); j < obj.NamedChildCount(); j++ {
			pair := obj.NamedChild(j)
			if pair.Type() != "pair" {
				continue
			}
			key := pair.ChildByFieldName("key")
			value := pair.ChildByFieldName("value")
			if key.IsNull() || value.IsNull() || value.Type() != "string" {
				continue
			}
			if unquote(key.Content(src)) == "method" {
				return unquote(value.Content(src)), true
			}
		}
	}
	return "", false
}

func routeMethod(name string) bool {
	switch name {
	case "get", "post", "put", "patch", "delete", "all", "route":
		return true
	}
	return false
}

func routePath(args sitter.Node, src []byte) string {
	for i := uint32(0); i < args.NamedChildCount(); i++ {
		arg := args.NamedChild(i)
		if arg.Type() == "string" {
			return unquote(arg.Content(src))
		}
	}
	return "route"
}

func (w *walker) ctor(n sitter.Node, enclosing string) {
	value := n.ChildByFieldName("constructor")
	if value.IsNull() {
		return
	}
	name := lastSegment(value.Content(w.src))
	if name == "" {
		return
	}
	w.res.Refs = append(w.res.Refs, Ref{
		Kind: "new", Name: name, Form: "ident",
		Line: int(n.StartPoint().Row) + 1, Enclosing: enclosing,
	})
}

// parameters records parameter type annotations so obj.m() can be resolved when
// obj is a typed parameter ("function f(cart: Cart)").
func (w *walker) parameters(params sitter.Node, enclosing string) {
	if params.IsNull() {
		return
	}
	for i := uint32(0); i < params.NamedChildCount(); i++ {
		param := params.NamedChild(i)
		pattern := param.ChildByFieldName("pattern")
		if pattern.IsNull() || pattern.Type() != "identifier" {
			continue
		}
		typeNode := param.ChildByFieldName("type")
		if typeNode.IsNull() {
			continue
		}
		if name := typeName(typeNode, w.src); name != "" {
			w.types[scopeKey(enclosing, pattern.Content(w.src))] = name
		}
	}
}

// declaredType records "const cart: Cart = …" and "const cart = new Cart()".
func (w *walker) declaredType(decl sitter.Node, enclosing string) {
	name := decl.ChildByFieldName("name")
	if name.IsNull() || name.Type() != "identifier" {
		return
	}
	value := decl.ChildByFieldName("value")

	typ := ""
	if annotation := decl.ChildByFieldName("type"); !annotation.IsNull() {
		typ = typeName(annotation, w.src)
	}
	if typ == "" && !value.IsNull() && value.Type() == "new_expression" {
		if ctor := value.ChildByFieldName("constructor"); !ctor.IsNull() {
			typ = lastSegment(ctor.Content(w.src))
		}
	}
	if typ != "" {
		w.types[scopeKey(enclosing, name.Content(w.src))] = typ
	}
}

func scopeKey(enclosing, name string) string {
	return enclosing + ":" + name
}

// typeName reduces a type annotation to its base name; unions and literals are
// ignored because they cannot identify one receiver type.
func typeName(node sitter.Node, src []byte) string {
	switch node.Type() {
	case "type_annotation":
		return typeName(node.NamedChild(0), src)
	case "type_identifier", "identifier":
		return node.Content(src)
	case "generic_type":
		if name := node.ChildByFieldName("name"); !name.IsNull() {
			return name.Content(src)
		}
	case "predefined_type":
		return node.Content(src)
	}
	return ""
}

func (w *walker) importStatement(n sitter.Node) {
	source := n.ChildByFieldName("source")
	line := int(n.StartPoint().Row) + 1
	spec := ""
	if !source.IsNull() {
		spec = unquote(source.Content(w.src))
	}

	// `import x = require("./x")` is a namespace binding written as an import
	// statement. The parser rejects some of these forms (see AGENTS.md), so a
	// missing clause simply produces no binding.
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		clause := n.NamedChild(i)
		if clause.Type() != "import_require_clause" {
			continue
		}
		id := clause.ChildByFieldName("name")
		if id.IsNull() && clause.NamedChildCount() > 0 {
			id = clause.NamedChild(0)
		}
		if id.Type() == "identifier" {
			clauseSpec := spec
			if s := clause.ChildByFieldName("source"); !s.IsNull() {
				clauseSpec = unquote(s.Content(w.src))
			}
			if clauseSpec == "" {
				continue
			}
			w.res.Imports = append(w.res.Imports, Import{
				Local: id.Content(w.src), Imported: "*", Source: clauseSpec, Line: line,
			})
		}
	}

	for i := uint32(0); i < n.NamedChildCount(); i++ {
		clause := n.NamedChild(i)
		if clause.Type() != "import_clause" || spec == "" {
			continue
		}
		for j := uint32(0); j < clause.NamedChildCount(); j++ {
			item := clause.NamedChild(j)
			switch item.Type() {
			case "identifier":
				w.res.Imports = append(w.res.Imports, Import{
					Local: item.Content(w.src), Imported: "default", Source: spec, Line: line,
				})
			case "namespace_import":
				for k := uint32(0); k < item.NamedChildCount(); k++ {
					if id := item.NamedChild(k); id.Type() == "identifier" {
						w.res.Imports = append(w.res.Imports, Import{
							Local: id.Content(w.src), Imported: "*", Source: spec, Line: line,
						})
					}
				}
			case "named_imports":
				for k := uint32(0); k < item.NamedChildCount(); k++ {
					specifier := item.NamedChild(k)
					if specifier.Type() != "import_specifier" {
						continue
					}
					name := specifier.ChildByFieldName("name")
					if name.IsNull() {
						continue
					}
					local := name.Content(w.src)
					if alias := specifier.ChildByFieldName("alias"); !alias.IsNull() {
						local = alias.Content(w.src)
					}
					w.res.Imports = append(w.res.Imports, Import{
						Local: local, Imported: name.Content(w.src), Source: spec, Line: line,
					})
				}
			}
		}
	}
}

// add records a symbol once per (qname, arity). Overloads with the same arity
// collapse because the schema identifies a symbol by (lang, qname, file, arity).
func (w *walker) add(kind, name, qname string, arity int, n sitter.Node, exported bool) {
	if name == "" || qname == "" {
		return
	}
	key := qname + "/" + fmt.Sprint(arity)
	if w.seen[key] {
		return
	}
	w.seen[key] = true

	w.res.Symbols = append(w.res.Symbols, Symbol{
		Kind:      kind,
		Name:      name,
		QName:     qname,
		Arity:     arity,
		StartLine: int(n.StartPoint().Row) + 1,
		EndLine:   int(n.EndPoint().Row) + 1,
		Exported:  exported,
	})
}

func join(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "." + name
}

// lastSegment turns "A.B.C" into "C" for member expressions in heritage clauses.
func lastSegment(text string) string {
	text = strings.TrimSpace(text)
	if i := strings.LastIndex(text, "."); i >= 0 {
		return text[i+1:]
	}
	return text
}

func unquote(text string) string {
	return strings.Trim(text, `"'`)
}

// nameOf reads the node's name field, ignoring computed names.
func nameOf(n sitter.Node, src []byte) string {
	name := n.ChildByFieldName("name")
	if name.IsNull() {
		return ""
	}
	switch name.Type() {
	case "property_identifier", "private_property_identifier", "identifier",
		"type_identifier", "identifier_pattern", "string":
		return name.Content(src)
	}
	return ""
}

// paramCount counts the declared parameters, which disambiguates overloads.
func paramCount(n sitter.Node) int {
	params := n.ChildByFieldName("parameters")
	if params.IsNull() {
		return 0
	}
	return int(params.NamedChildCount())
}

// errorLine finds the first error node so the message can point at real code.
func errorLine(n sitter.Node) int {
	if n.IsError() || n.IsMissing() {
		return int(n.StartPoint().Row) + 1
	}
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		if line := errorLine(n.NamedChild(i)); line > 0 {
			return line
		}
	}
	return 0
}
