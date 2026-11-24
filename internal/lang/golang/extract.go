// Package golang extracts symbol definitions, references, and import bindings
// from Go sources with the pinned tree-sitter-go grammar.
//
// Go is statically typed but its type relation is "declared, then used": a
// method call is resolved from a locally inferred receiver type (a parameter,
// short variable declaration, `var`, composite literal, `new(T)`, or a
// same-file constructor return), never from the method name alone. Interface
// implementation is implicit in the language; v1 resolves calls through an
// interface to the interface's own method set and does not infer which
// concrete types implement it (documented in docs/resolution.md).
package golang

import (
	"context"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	forestgo "github.com/alexaandru/go-sitter-forest/go"
	sitter "github.com/alexaandru/go-tree-sitter-bare"

	"github.com/Goutam-11/repath/internal/lang/facts"
)

// Symbol, Ref, Import, Entrypoint, and Result are the shared extraction facts.
type (
	Symbol     = facts.Symbol
	Ref        = facts.Ref
	Import     = facts.Import
	Entrypoint = facts.Entrypoint
	Result     = facts.Result
)

// Dialect selects the grammar. Go has exactly one.
type Dialect int

// Go parses .go files.
const Go Dialect = iota

// Extractor holds one parser. It is not safe for concurrent use; index workers
// each create their own.
type Extractor struct {
	parser *sitter.Parser
}

// New builds an extractor with the pinned Go grammar.
func New() *Extractor {
	p := sitter.NewParser()
	p.SetLanguage(sitter.NewLanguage(forestgo.GetLanguage()))
	return &Extractor{parser: p}
}

// Extract parses src and returns its definitions, references, and imports.
func (e *Extractor) Extract(src []byte, dialect Dialect) Result {
	tree, err := e.parser.ParseString(context.Background(), nil, src)
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

	w := &walker{
		src:     src,
		res:     &res,
		pkg:     packageName(root, src),
		vars:    make(map[string]string),
		globals: make(map[string]string),
		returns: make(map[string]string),
		fields:  make(map[string]string),
	}
	for i := uint32(0); i < root.NamedChildCount(); i++ {
		w.top(root.NamedChild(i))
	}
	return res
}

type walker struct {
	src []byte
	res *Result
	pkg string

	// vars maps "enclosing:name" to a named type; globals covers package-level
	// declarations. returns maps a function or method qname to its single named
	// result type, which is what makes `u := NewUser(...)` typed.
	vars    map[string]string
	globals map[string]string
	returns map[string]string

	// fields maps "Type.field" to a named field type so a one-hop chained
	// receiver (`x.f.M()`) can be typed when both declarations live in this
	// file. bound is the set of names bound in the current function
	// (parameters, receiver, `:=`, `var`, range and type-switch names); a bare
	// identifier in value position is a reference only when it is not one of
	// them, which keeps local variables out of the graph.
	fields map[string]string
	bound  map[string]bool
}

func scopeKey(enclosing, name string) string { return enclosing + ":" + name }

// top visits a top-level declaration.
func (w *walker) top(n sitter.Node) {
	switch n.Type() {
	case "import_declaration":
		w.imports(n)
	case "type_declaration":
		w.typeDeclaration(n)
	case "function_declaration":
		w.function(n)
	case "method_declaration":
		w.method(n)
	case "const_declaration":
		w.constDeclaration(n)
	case "var_declaration":
		w.varDeclaration(n, "", true)
	case "package_clause":
	default:
		w.walk(n, "")
	}
}

func (w *walker) add(kind, name, qname string, arity int, n sitter.Node, exported bool) {
	if name == "" || name == "_" {
		return
	}
	w.res.Symbols = append(w.res.Symbols, Symbol{
		Kind: kind, Name: name, QName: qname, Arity: arity,
		StartLine: int(n.StartPoint().Row) + 1,
		EndLine:   int(n.EndPoint().Row) + 1,
		Exported:  exported,
	})
}

// imports records one binding per import spec. A dot import and a blank import
// create no binding (dot imports are a documented gap).
func (w *walker) imports(n sitter.Node) {
	var visit func(sitter.Node)
	visit = func(n sitter.Node) {
		if n.Type() == "import_spec" {
			path := n.ChildByFieldName("path")
			if path.IsNull() {
				return
			}
			source := unquote(path.Content(w.src))
			local := lastSegment(source)
			if alias := n.ChildByFieldName("name"); !alias.IsNull() {
				local = alias.Content(w.src)
			}
			if local == "_" || local == "." || source == "" {
				return
			}
			w.res.Imports = append(w.res.Imports, Import{
				Local: local, Imported: "*", Source: source,
				Line: int(n.StartPoint().Row) + 1,
			})
			return
		}
		for i := uint32(0); i < n.NamedChildCount(); i++ {
			visit(n.NamedChild(i))
		}
	}
	visit(n)
}

func (w *walker) typeDeclaration(n sitter.Node) {
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		spec := n.NamedChild(i)
		if spec.Type() != "type_spec" {
			continue
		}
		w.typeSpec(spec, "")
	}
}

func (w *walker) typeSpec(n sitter.Node, scope string) {
	name := n.ChildByFieldName("name")
	if name.IsNull() {
		return
	}
	id := name.Content(w.src)
	qname := id
	if scope != "" {
		qname = scope + "." + id
	}
	typ := n.ChildByFieldName("type")
	kind := "type"
	if !typ.IsNull() {
		switch typ.Type() {
		case "struct_type":
			kind = "struct"
		case "interface_type":
			kind = "interface"
		}
	}
	w.add(kind, id, qname, 0, n, exported(id))
	if typ.IsNull() {
		return
	}
	switch typ.Type() {
	case "struct_type":
		w.structFields(typ, qname, id)
	case "interface_type":
		w.interfaceBody(typ, qname)
	}
}

// structFields records embedded types as composition references. Fields are not
// symbols (there is no field node kind), so a field access stays unresolved
// instead of becoming a guess; a named field's type is remembered for a bounded
// one-hop chained receiver only.
func (w *walker) structFields(typ sitter.Node, owner, name string) {
	for i := uint32(0); i < typ.NamedChildCount(); i++ {
		list := typ.NamedChild(i)
		if list.Type() != "field_declaration_list" {
			continue
		}
		for j := uint32(0); j < list.NamedChildCount(); j++ {
			field := list.NamedChild(j)
			if field.Type() != "field_declaration" {
				continue
			}
			var names []string
			for k := uint32(0); k < field.NamedChildCount(); k++ {
				switch child := field.NamedChild(k); child.Type() {
				case "field_identifier", "identifier":
					names = append(names, child.Content(w.src))
				}
			}
			if len(names) > 0 {
				fieldType := typeName(field.ChildByFieldName("type"), w.src)
				if fieldType != "" {
					for _, fieldName := range names {
						w.fields[name+"."+fieldName] = fieldType
					}
				}
				continue
			}
			typeNode := field.ChildByFieldName("type")
			if typeNode.IsNull() && field.NamedChildCount() > 0 {
				typeNode = field.NamedChild(0)
			}
			w.typeRef(typeNode, "embed", owner, field)
		}
	}
}

// interfaceBody records each method as a symbol scoped to the interface and
// each embedded interface as an EXTENDS reference.
func (w *walker) interfaceBody(typ sitter.Node, owner string) {
	for i := uint32(0); i < typ.NamedChildCount(); i++ {
		child := typ.NamedChild(i)
		switch child.Type() {
		case "method_elem":
			name := child.ChildByFieldName("name")
			if name.IsNull() {
				continue
			}
			m := name.Content(w.src)
			w.add("method", m, owner+"."+m, w.arity(child.ChildByFieldName("parameters")), child, exported(m))
		case "type_identifier", "qualified_type", "generic_type":
			w.typeRef(child, "extends", owner, child)
		}
	}
}

func (w *walker) function(n sitter.Node) {
	name := n.ChildByFieldName("name")
	if name.IsNull() {
		return
	}
	fn := name.Content(w.src)
	w.add("function", fn, fn, w.arity(n.ChildByFieldName("parameters")), n, exported(fn))
	w.bound = make(map[string]bool)
	w.recordParams(n.ChildByFieldName("parameters"), fn)
	w.markBoundList(n.ChildByFieldName("result"))
	if result := namedResult(n.ChildByFieldName("result"), w.src); result != "" {
		w.returns[fn] = result
	}
	if fn == "main" && w.pkg == "main" {
		w.entrypoint(fn, fn, "go.main", 1.00, n)
	} else if isTestName(fn) {
		w.entrypoint(fn, fn, "go.test", 0.90, n)
	}
	w.walk(n.ChildByFieldName("body"), fn)
}

func (w *walker) method(n sitter.Node) {
	name := n.ChildByFieldName("name")
	recv := n.ChildByFieldName("receiver")
	if name.IsNull() || recv.IsNull() {
		return
	}
	recvName, recvType := w.receiver(recv)
	if recvType == "" {
		return
	}
	m := name.Content(w.src)
	qname := recvType + "." + m
	w.add("method", m, qname, w.arity(n.ChildByFieldName("parameters")), n, exported(m))
	w.bound = make(map[string]bool)
	if recvName != "" {
		w.markBound(recvName)
		w.vars[scopeKey(qname, recvName)] = recvType
		w.vars[scopeKey("", recvName)] = recvType
	}
	w.recordParams(n.ChildByFieldName("parameters"), qname)
	w.markBoundList(n.ChildByFieldName("result"))
	if result := namedResult(n.ChildByFieldName("result"), w.src); result != "" {
		w.returns[qname] = result
	}
	w.walk(n.ChildByFieldName("body"), qname)
}

// receiver returns the receiver variable name and its named type, stripping a
// pointer and any generic instantiation.
func (w *walker) receiver(list sitter.Node) (string, string) {
	for i := uint32(0); i < list.NamedChildCount(); i++ {
		param := list.NamedChild(i)
		if param.Type() != "parameter_declaration" {
			continue
		}
		name := ""
		if id := param.ChildByFieldName("name"); !id.IsNull() {
			name = id.Content(w.src)
		}
		return name, typeName(param.ChildByFieldName("type"), w.src)
	}
	return "", ""
}

func (w *walker) recordParams(list sitter.Node, enclosing string) {
	if list.IsNull() {
		return
	}
	for i := uint32(0); i < list.NamedChildCount(); i++ {
		param := list.NamedChild(i)
		if param.Type() != "parameter_declaration" && param.Type() != "variadic_parameter_declaration" {
			continue
		}
		typ := typeName(param.ChildByFieldName("type"), w.src)
		for j := uint32(0); j < param.NamedChildCount(); j++ {
			child := param.NamedChild(j)
			if child.Type() != "identifier" {
				continue
			}
			name := child.Content(w.src)
			// A parameter is bound even when its type is not a named type
			// (`cb func()`): value-position detection must not treat it as a
			// package-level reference.
			w.markBound(name)
			if typ == "" {
				continue
			}
			w.vars[scopeKey(enclosing, name)] = typ
			w.vars[scopeKey("", name)] = typ
		}
	}
}

func (w *walker) constDeclaration(n sitter.Node) {
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		spec := n.NamedChild(i)
		if spec.Type() != "const_spec" {
			continue
		}
		for j := uint32(0); j < spec.NamedChildCount(); j++ {
			child := spec.NamedChild(j)
			if child.Type() != "identifier" {
				continue
			}
			name := child.Content(w.src)
			w.add("const", name, name, 0, spec, exported(name))
		}
		w.walk(spec.ChildByFieldName("value"), "")
	}
}

func (w *walker) varDeclaration(n sitter.Node, enclosing string, topLevel bool) {
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		spec := n.NamedChild(i)
		switch spec.Type() {
		case "var_spec":
			w.varSpec(spec, enclosing, topLevel)
		case "var_spec_list":
			// `var ( … )` wraps its specs; walking only direct children would
			// silently drop every package-level variable in a block.
			for j := uint32(0); j < spec.NamedChildCount(); j++ {
				if inner := spec.NamedChild(j); inner.Type() == "var_spec" {
					w.varSpec(inner, enclosing, topLevel)
				}
			}
		}
	}
}

func (w *walker) varSpec(spec sitter.Node, enclosing string, topLevel bool) {
	typ := typeName(spec.ChildByFieldName("type"), w.src)
	var names []string
	for j := uint32(0); j < spec.NamedChildCount(); j++ {
		child := spec.NamedChild(j)
		if child.Type() == "identifier" {
			names = append(names, child.Content(w.src))
		}
	}
	values := spec.ChildByFieldName("value")
	if typ == "" && !values.IsNull() && values.NamedChildCount() > 0 {
		typ = w.inferredType(values.NamedChild(0), enclosing)
	}
	for _, name := range names {
		if topLevel {
			w.add("var", name, name, 0, spec, exported(name))
			w.globals[name] = typ
			continue
		}
		w.markBound(name)
		if typ != "" {
			w.vars[scopeKey(enclosing, name)] = typ
		}
	}
	w.walk(values, enclosing)
}

// walk visits an expression/statement subtree, emitting calls, conversions,
// composite literals, and local declarations.
func (w *walker) walk(n sitter.Node, enclosing string) {
	if n.IsNull() {
		return
	}
	switch n.Type() {
	case "call_expression":
		w.call(n, enclosing)
		return
	case "composite_literal":
		w.composite(n, enclosing)
		return
	case "short_var_declaration":
		w.shortVar(n, enclosing)
		return
	case "var_declaration":
		w.varDeclaration(n, enclosing, false)
		return
	case "func_literal":
		w.funcLiteral(n, enclosing)
		return
	case "type_declaration":
		// A type declared inside a function is a real symbol for references in
		// that file; it is scoped by the enclosing function so it cannot shadow
		// a package-level name.
		for i := uint32(0); i < n.NamedChildCount(); i++ {
			if spec := n.NamedChild(i); spec.Type() == "type_spec" {
				w.typeSpec(spec, enclosing)
			}
		}
		return
	case "range_clause":
		w.markBoundList(n.ChildByFieldName("left"))
	case "type_switch_statement":
		w.markBoundList(n.ChildByFieldName("alias"))
	case "return_statement":
		w.valueRefs(n, enclosing)
	case "assignment_statement":
		w.valueRefs(n.ChildByFieldName("right"), enclosing)
	case "selector_expression":
		// A selector is either a call target (handled by call_expression) or a
		// field access; fields are not symbols, so it emits no reference.
		return
	}
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		w.walk(n.NamedChild(i), enclosing)
	}
}

func (w *walker) call(n sitter.Node, enclosing string) {
	fn := n.ChildByFieldName("function")
	if !fn.IsNull() {
		switch fn.Type() {
		case "identifier":
			name := fn.Content(w.src)
			if name == "new" {
				// `new(qualified.Type)` keeps the package qualifier: emitting a
				// bare ident here would look for a symbol literally named
				// "pkg.Type".
				if typNode := firstTypeNode(n.ChildByFieldName("arguments")); !typNode.IsNull() {
					w.typeRef(typNode, "new", enclosing, n)
				}
			} else if !isBuiltin(name) {
				w.res.Refs = append(w.res.Refs, Ref{
					Kind: "call", Name: name, Form: "ident",
					Line: int(n.StartPoint().Row) + 1, Enclosing: enclosing,
				})
			}
		case "selector_expression":
			w.callSelector(fn, n, enclosing)
		}
		// Walk the callee subtree: an immediately invoked literal
		// (`func() { … }()`, `defer func() { … }()`) and a chained call
		// (`f(x).m()`) both keep references that the callee node itself does not
		// carry. A selector walk stops at the selector, so its operand is walked
		// instead; an identifier walk is a no-op.
		if fn.Type() == "selector_expression" {
			w.walk(fn.ChildByFieldName("operand"), enclosing)
		} else {
			w.walk(fn, enclosing)
		}
	}
	args := n.ChildByFieldName("arguments")
	w.walk(args, enclosing)
	w.valueRefs(args, enclosing)
}

// callSelector records one member call. A plain `x.M()` is typed from x; a
// one-hop chain `x.f.M()` is typed from field f's declared type when both the
// receiver and the field are declared in this file, and stays unresolved
// otherwise (no name-only guess, no deeper chain).
func (w *walker) callSelector(sel, at sitter.Node, enclosing string) {
	operand := sel.ChildByFieldName("operand")
	field := sel.ChildByFieldName("field")
	if operand.IsNull() || field.IsNull() || field.Type() != "field_identifier" {
		return
	}
	name := field.Content(w.src)
	line := int(at.StartPoint().Row) + 1

	switch operand.Type() {
	case "identifier":
		recv := operand.Content(w.src)
		w.res.Refs = append(w.res.Refs, Ref{
			Kind: "call", Name: name, Form: "member", Receiver: recv,
			ReceiverType: w.lookupType(enclosing, recv),
			Line:         line, Enclosing: enclosing,
		})
	case "selector_expression":
		inner := operand.ChildByFieldName("operand")
		innerField := operand.ChildByFieldName("field")
		if inner.IsNull() || inner.Type() != "identifier" || innerField.IsNull() || innerField.Type() != "field_identifier" {
			return
		}
		base := inner.Content(w.src)
		fieldName := innerField.Content(w.src)
		fieldType := ""
		if baseType := w.lookupType(enclosing, base); baseType != "" {
			fieldType = w.fields[baseType+"."+fieldName]
		}
		w.res.Refs = append(w.res.Refs, Ref{
			Kind: "call", Name: name, Form: "member", Receiver: base + "." + fieldName,
			ReceiverType: fieldType,
			Line:         line, Enclosing: enclosing,
		})
	}
}

func (w *walker) composite(n sitter.Node, enclosing string) {
	typ := n.ChildByFieldName("type")
	if typ.IsNull() && n.NamedChildCount() > 0 {
		typ = n.NamedChild(0)
	}
	w.typeRef(typ, "new", enclosing, n)
	if body := n.ChildByFieldName("body"); !body.IsNull() {
		w.walk(body, enclosing)
		// A keyed field can hold a function value: `sync.Pool{New: newBuf}`.
		w.valueRefs(body, enclosing)
	}
}

// typeRef emits a reference to a type expression: a plain or generic name, or
// a package-qualified name.
func (w *walker) typeRef(typ sitter.Node, kind, enclosing string, at sitter.Node) {
	if typ.IsNull() {
		return
	}
	line := int(at.StartPoint().Row) + 1
	switch typ.Type() {
	case "type_identifier", "identifier":
		w.res.Refs = append(w.res.Refs, Ref{
			Kind: kind, Name: typ.Content(w.src), Form: "ident", Line: line, Enclosing: enclosing,
		})
	case "generic_type":
		if name := typ.ChildByFieldName("type"); !name.IsNull() {
			w.typeRef(name, kind, enclosing, at)
			return
		}
		for i := uint32(0); i < typ.NamedChildCount(); i++ {
			if child := typ.NamedChild(i); child.Type() == "type_identifier" {
				w.res.Refs = append(w.res.Refs, Ref{
					Kind: kind, Name: child.Content(w.src), Form: "ident", Line: line, Enclosing: enclosing,
				})
				break
			}
		}
	case "qualified_type":
		pkg := typ.ChildByFieldName("package")
		name := typ.ChildByFieldName("name")
		if pkg.IsNull() || name.IsNull() {
			return
		}
		w.res.Refs = append(w.res.Refs, Ref{
			Kind: kind, Name: name.Content(w.src), Form: "member",
			Receiver: pkg.Content(w.src), Line: line, Enclosing: enclosing,
		})
	}
}

func (w *walker) shortVar(n sitter.Node, enclosing string) {
	var lists []sitter.Node
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		if child := n.NamedChild(i); child.Type() == "expression_list" {
			lists = append(lists, child)
		}
	}
	if len(lists) == 2 {
		var lefts []string
		for i := uint32(0); i < lists[0].NamedChildCount(); i++ {
			if child := lists[0].NamedChild(i); child.Type() == "identifier" {
				lefts = append(lefts, child.Content(w.src))
			}
		}
		var rights []sitter.Node
		for i := uint32(0); i < lists[1].NamedChildCount(); i++ {
			rights = append(rights, lists[1].NamedChild(i))
		}
		for i, left := range lefts {
			w.markBound(left)
			if i >= len(rights) {
				continue
			}
			if typ := w.inferredType(rights[i], enclosing); typ != "" {
				w.vars[scopeKey(enclosing, left)] = typ
			}
		}
		w.valueRefs(n.ChildByFieldName("right"), enclosing)
	}
	w.walk(n.ChildByFieldName("right"), enclosing)
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		if child := n.NamedChild(i); child.Type() != "expression_list" {
			w.walk(child, enclosing)
		}
	}
}

func (w *walker) funcLiteral(n sitter.Node, enclosing string) {
	// Go has no closure symbols: a reference inside a function literal belongs
	// to the function that lexically contains the literal, so the walk keeps the
	// enclosing qname rather than inventing a "(closure)" owner.
	w.recordParams(n.ChildByFieldName("parameters"), enclosing)
	w.walk(n.ChildByFieldName("body"), enclosing)
}

// valueRef emits a REFERENCES reference for a bare identifier in value position
// (a function value, a package-level var or const). A locally bound name is a
// variable, not a symbol, so it is skipped instead of producing a guessed
// reference; a builtin or a predeclared constant is external by definition.
func (w *walker) valueRef(n sitter.Node, enclosing string) {
	// Top-level references have no source symbol (the same policy as a call
	// outside any function).
	if enclosing == "" {
		return
	}
	name := n.Content(w.src)
	if name == "" || name == "_" || w.bound[name] || isBuiltin(name) || isPredeclaredValue(name) {
		return
	}
	w.res.Refs = append(w.res.Refs, Ref{
		Kind: "ref", Name: name, Form: "ident",
		Line: int(n.StartPoint().Row) + 1, Enclosing: enclosing,
	})
}

// valueRefs visits the direct children of a value list, emitting a reference
// for each bare identifier. It does not recurse into selectors, calls, or
// literals: those are handled by walk and are not bare names.
func (w *walker) valueRefs(n sitter.Node, enclosing string) {
	if n.IsNull() {
		return
	}
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		child := n.NamedChild(i)
		switch child.Type() {
		case "identifier":
			w.valueRef(child, enclosing)
		case "expression_list":
			w.valueRefs(child, enclosing)
		case "keyed_element":
			value := child.ChildByFieldName("value")
			if value.Type() == "identifier" {
				w.valueRef(value, enclosing)
			} else if !value.IsNull() {
				w.valueRefs(value, enclosing)
			}
		}
	}
}

// markBound records a name bound in the current function. A nil set means no
// function is open (a package-level initializer), where every reference is
// top-level and skipped anyway.
func (w *walker) markBound(name string) {
	if w.bound == nil || name == "" || name == "_" {
		return
	}
	w.bound[name] = true
}

// markBoundList records every identifier in a binding site: a parameter list, a
// result list, a range clause's left side, or a type switch's alias.
func (w *walker) markBoundList(n sitter.Node) {
	if n.IsNull() {
		return
	}
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		child := n.NamedChild(i)
		switch child.Type() {
		case "identifier":
			w.markBound(child.Content(w.src))
		case "parameter_declaration", "variadic_parameter_declaration", "expression_list":
			w.markBoundList(child)
		}
	}
}

// inferredType reduces a right-hand expression to a named type the resolver can
// follow: a composite literal, `new(T)`, a same-file constructor's return
// annotation, or another typed variable.
func (w *walker) inferredType(n sitter.Node, enclosing string) string {
	switch n.Type() {
	case "composite_literal":
		typ := n.ChildByFieldName("type")
		if typ.IsNull() && n.NamedChildCount() > 0 {
			typ = n.NamedChild(0)
		}
		return typeName(typ, w.src)
	case "unary_expression":
		for i := uint32(0); i < n.NamedChildCount(); i++ {
			if child := n.NamedChild(i); child.Type() == "composite_literal" {
				return w.inferredType(child, enclosing)
			}
		}
	case "call_expression":
		fn := n.ChildByFieldName("function")
		if fn.IsNull() {
			return ""
		}
		name := fn.Content(w.src)
		if name == "new" {
			return lastSegment(firstTypeArgument(n.ChildByFieldName("arguments"), w.src))
		}
		if typ, ok := w.returns[name]; ok {
			return typ
		}
	case "identifier":
		return w.lookupType(enclosing, n.Content(w.src))
	}
	return ""
}

func (w *walker) lookupType(enclosing, name string) string {
	if typ := w.vars[scopeKey(enclosing, name)]; typ != "" {
		return typ
	}
	if typ := w.vars[scopeKey("", name)]; typ != "" {
		return typ
	}
	return w.globals[name]
}

func (w *walker) entrypoint(name, qname, rule string, confidence float64, n sitter.Node) {
	w.res.Entrypoints = append(w.res.Entrypoints, Entrypoint{
		QName: qname, Name: name, Rule: rule, Confidence: confidence,
		Line: int(n.StartPoint().Row) + 1,
	})
}

func (w *walker) arity(list sitter.Node) int {
	if list.IsNull() {
		return 0
	}
	count := 0
	for i := uint32(0); i < list.NamedChildCount(); i++ {
		switch list.NamedChild(i).Type() {
		case "parameter_declaration", "variadic_parameter_declaration":
			count++
		}
	}
	return count
}

// ---- helpers over the grammar ----

func packageName(root sitter.Node, src []byte) string {
	for i := uint32(0); i < root.NamedChildCount(); i++ {
		n := root.NamedChild(i)
		if n.Type() != "package_clause" {
			continue
		}
		for j := uint32(0); j < n.NamedChildCount(); j++ {
			if id := n.NamedChild(j); id.Type() == "package_identifier" {
				return id.Content(src)
			}
		}
	}
	return ""
}

// typeName reduces a type expression to a Go type name usable for receiver
// lookup. Pointers are stripped (method sets are consulted for both), and a
// package-qualified type keeps its "pkg.Name" spelling.
func typeName(n sitter.Node, src []byte) string {
	if n.IsNull() {
		return ""
	}
	switch n.Type() {
	case "type_identifier", "identifier":
		return n.Content(src)
	case "pointer_type":
		for i := uint32(0); i < n.NamedChildCount(); i++ {
			if child := n.NamedChild(i); child.Type() == "type_identifier" || child.Type() == "qualified_type" || child.Type() == "generic_type" {
				return typeName(child, src)
			}
		}
	case "qualified_type":
		pkg := n.ChildByFieldName("package")
		name := n.ChildByFieldName("name")
		if !pkg.IsNull() && !name.IsNull() {
			return pkg.Content(src) + "." + name.Content(src)
		}
	case "generic_type":
		if name := n.ChildByFieldName("type"); !name.IsNull() {
			return typeName(name, src)
		}
		for i := uint32(0); i < n.NamedChildCount(); i++ {
			if child := n.NamedChild(i); child.Type() == "type_identifier" {
				return child.Content(src)
			}
		}
	}
	return ""
}

func namedResult(n sitter.Node, src []byte) string {
	if n.IsNull() {
		return ""
	}
	if n.Type() == "parameter_list" {
		for i := uint32(0); i < n.NamedChildCount(); i++ {
			if param := n.NamedChild(i); param.Type() == "parameter_declaration" {
				return typeName(param.ChildByFieldName("type"), src)
			}
		}
		return ""
	}
	return typeName(n, src)
}

func firstTypeNode(args sitter.Node) sitter.Node {
	if args.IsNull() {
		return sitter.Node{}
	}
	for i := uint32(0); i < args.NamedChildCount(); i++ {
		child := args.NamedChild(i)
		switch child.Type() {
		case "type_identifier", "qualified_type", "generic_type", "pointer_type":
			return child
		}
	}
	return sitter.Node{}
}

func firstTypeArgument(args sitter.Node, src []byte) string {
	if args.IsNull() {
		return ""
	}
	for i := uint32(0); i < args.NamedChildCount(); i++ {
		child := args.NamedChild(i)
		switch child.Type() {
		case "type_identifier", "qualified_type", "generic_type", "pointer_type":
			return typeName(child, src)
		}
	}
	return ""
}

func errorLine(n sitter.Node) int {
	if n.HasError() {
		if int(n.StartPoint().Row)+1 > 0 && int(n.StartPoint().Row) < int(n.EndPoint().Row)+1 {
			return int(n.StartPoint().Row) + 1
		}
	}
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		if line := errorLine(n.NamedChild(i)); line > 0 {
			return line
		}
	}
	return 0
}

func unquote(s string) string {
	s = strings.TrimSpace(s)
	return strings.Trim(s, "\"`")
}

func lastSegment(s string) string {
	if i := strings.LastIndex(s, "/"); i >= 0 {
		return s[i+1:]
	}
	return s
}

func exported(name string) bool {
	r, _ := utf8.DecodeRuneInString(name)
	return unicode.IsUpper(r)
}

func isTestName(name string) bool {
	for _, prefix := range []string{"Test", "Benchmark", "Example", "Fuzz"} {
		if strings.HasPrefix(name, prefix) && len(name) > len(prefix) {
			r, _ := utf8.DecodeRuneInString(name[len(prefix):])
			if unicode.IsUpper(r) {
				return true
			}
		}
	}
	return false
}

// isBuiltin lists Go's predeclared functions: calls to them are external, not
// unresolved references.
func isBuiltin(name string) bool {
	switch name {
	case "append", "cap", "clear", "close", "complex", "copy", "delete", "imag",
		"len", "make", "max", "min", "new", "panic", "print", "println", "real",
		"recover":
		return true
	}
	return false
}

// isPredeclaredValue lists Go's predeclared constants: a bare `nil` or `true`
// in argument position is not a package-level reference.
func isPredeclaredValue(name string) bool {
	switch name {
	case "nil", "true", "false", "iota":
		return true
	}
	return false
}
