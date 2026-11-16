// Package rs extracts symbol definitions, references, and imports from Rust
// sources with a tree-sitter grammar.
package rs

import (
	"context"
	"fmt"
	"strings"

	forestrust "github.com/alexaandru/go-sitter-forest/rust"
	sitter "github.com/alexaandru/go-tree-sitter-bare"

	"github.com/Goutam-11/repath/internal/lang/facts"
)

// Dialect selects the grammar used for a file. Rust has one grammar today;
// the type exists so callers treat every language frontend the same way.
type Dialect int

const (
	// Rust parses .rs files.
	Rust Dialect = iota
)

// Symbol, Ref, Import, and Result are the shared extraction-fact types.
type (
	Symbol     = facts.Symbol
	Ref        = facts.Ref
	Import     = facts.Import
	Entrypoint = facts.Entrypoint
	Result     = facts.Result
)

// Extractor holds the Rust parser. It is not safe for concurrent use; index
// workers each create their own.
type Extractor struct {
	parser *sitter.Parser
}

// New builds an extractor for the pinned Rust grammar.
func New() *Extractor {
	p := sitter.NewParser()
	p.SetLanguage(sitter.NewLanguage(forestrust.GetLanguage()))
	return &Extractor{parser: p}
}

// Extract parses src and returns its definitions, references, and imports.
func (e *Extractor) Extract(src []byte, dialect Dialect) Result {
	if dialect != Rust {
		return Result{Err: "unsupported dialect"}
	}

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
		src:        src,
		res:        &res,
		seen:       make(map[string]bool),
		types:      make(map[string]string),
		typeFrom:   make(map[string]string),
		bounds:     make(map[string]string),
		params:     make(map[string]bool),
		returns:    make(map[string]string),
		assocTypes: make(map[string]string),
	}
	w.collectFields(root)
	w.collectAssocTypes(root, "")
	w.collectReturns(root, "", "")
	w.resolveReturnProjections()
	for i := uint32(0); i < root.NamedChildCount(); i++ {
		w.node(root.NamedChild(i), "", false, "", false)
	}
	return res
}

type walker struct {
	src   []byte
	res   *Result
	seen  map[string]bool
	types map[string]string // "enclosing:name" → type, "field:Type.field" → type
	// typeFrom records how a local binding's type was inferred ("return" for a
	// same-file function return type), so the resolver can pick an honest rule.
	typeFrom map[string]string
	// bounds holds the trait bounds of a generic type parameter, keyed by
	// "scope:T" where scope is a function qname or an impl/trait type name.
	bounds map[string]string
	// params records type parameter names, keyed like bounds, so a receiver can
	// be marked as generic even when it has no bound.
	params map[string]bool
	// returns maps a same-file function's qname to its return type base name. It
	// is a local definition only: a receiver typed by it never crosses a file.
	returns map[string]string
	// assocTypes maps "Type.Assoc" to the concrete local type an impl binds it
	// to (`impl Trait for Type { type Assoc = Other; }`). It resolves the
	// associated-type projections a same-file return type can name.
	assocTypes map[string]string
}

// boundPath renders one trait bound as a module path plus its last segment.
func boundPath(n sitter.Node, src []byte) string {
	if n.IsNull() {
		return ""
	}
	prefix, name := typePath(n, src)
	if name == "" {
		return ""
	}
	if prefix == "" {
		return name
	}
	return prefix + "::" + name
}

// collectBounds reads a type_parameters / where_clause pair into the walker's
// bound map. Bounds are kept verbatim as paths; the resolver decides whether a
// path names a local trait.
// childOfType returns the first named child of a type. The Rust grammar
// exposes type_parameters as a field but where_clause only as a child.
func childOfType(n sitter.Node, typ string) sitter.Node {
	if n.IsNull() {
		return sitter.Node{}
	}
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		if child := n.NamedChild(i); child.Type() == typ {
			return child
		}
	}
	return sitter.Node{}
}

func (w *walker) collectBounds(scope string, typeParams, whereClause sitter.Node) {
	if scope != "" && !typeParams.IsNull() {
		for i := uint32(0); i < typeParams.NamedChildCount(); i++ {
			param := typeParams.NamedChild(i)
			if param.Type() != "type_parameter" {
				continue
			}
			name := nameOf(param, w.src)
			if name == "" {
				continue
			}
			w.params[scopeKey(scope, name)] = true
			w.addBounds(scopeKey(scope, name), boundsOf(param.ChildByFieldName("bounds"), w.src))
		}
	}
	if scope == "" || whereClause.IsNull() {
		return
	}
	for i := uint32(0); i < whereClause.NamedChildCount(); i++ {
		pred := whereClause.NamedChild(i)
		if pred.Type() != "where_predicate" {
			continue
		}
		name := typeNameOf(pred.ChildByFieldName("left"), w.src)
		if name == "" {
			continue
		}
		w.addBounds(scopeKey(scope, name), boundsOf(pred.ChildByFieldName("bounds"), w.src))
	}
}

func (w *walker) addBounds(key string, paths []string) {
	if len(paths) == 0 {
		return
	}
	if existing := w.bounds[key]; existing != "" {
		paths = append(strings.Split(existing, "|"), paths...)
	}
	w.bounds[key] = strings.Join(paths, "|")
}

// boundsOf lists the trait paths in a trait_bounds node, skipping lifetimes.
func boundsOf(n sitter.Node, src []byte) []string {
	if n.IsNull() {
		return nil
	}
	var out []string
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		child := n.NamedChild(i)
		switch child.Type() {
		case "lifetime", "removed_trait_bound":
			continue
		}
		if path := boundPath(child, src); path != "" {
			out = append(out, path)
		}
	}
	return out
}

// boundsFor returns the bound paths of a receiver type, looking in the
// enclosing function's scope first and then in its enclosing impl/trait type.
func (w *walker) boundsFor(enclosing, typ string) string {
	if typ == "" {
		return ""
	}
	if b := w.bounds[scopeKey(enclosing, typ)]; b != "" {
		return b
	}
	if parent := classOf(enclosing); parent != enclosing {
		return w.bounds[scopeKey(parent, typ)]
	}
	return ""
}

// isTypeParam reports whether a receiver type names a generic type parameter of
// the enclosing function or its impl/trait.
func (w *walker) isTypeParam(enclosing, typ string) bool {
	if typ == "" {
		return false
	}
	if w.params[scopeKey(enclosing, typ)] {
		return true
	}
	if parent := classOf(enclosing); parent != enclosing {
		return w.params[scopeKey(parent, typ)]
	}
	return false
}

// collectAssocTypes records `type Assoc = Concrete` from impl blocks before
// return types are collected, because a return type may name Self::Assoc.
func (w *walker) collectAssocTypes(n sitter.Node, implType string) {
	switch n.Type() {
	case "impl_item":
		base := typeNameOf(n.ChildByFieldName("type"), w.src)
		body := n.ChildByFieldName("body")
		if base == "" || body.IsNull() {
			return
		}
		for i := uint32(0); i < body.NamedChildCount(); i++ {
			child := body.NamedChild(i)
			if child.Type() != "type_item" {
				continue
			}
			name := nameOf(child, w.src)
			value := typeNameOf(child.ChildByFieldName("type"), w.src)
			if name != "" && value != "" {
				w.assocTypes[base+"."+name] = value
			}
		}
		return
	case "mod_item":
		name := nameOf(n, w.src)
		body := n.ChildByFieldName("body")
		if name == "" || body.IsNull() {
			return
		}
		for i := uint32(0); i < body.NamedChildCount(); i++ {
			w.collectAssocTypes(body.NamedChild(i), implType)
		}
		return
	}
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		w.collectAssocTypes(n.NamedChild(i), implType)
	}
}

// resolveReturnProjections replaces `Self::Assoc` return types with the concrete
// local type the same file's impl binds. No impl, no answer: the projection
// stays unresolved rather than guessed.
func (w *walker) resolveReturnProjections() {
	for qname, ret := range w.returns {
		owner := qname
		if i := strings.LastIndex(qname, "."); i >= 0 {
			owner = qname[:i]
		}
		projection := ""
		switch {
		case strings.HasPrefix(ret, "Self::"):
			projection = strings.TrimPrefix(ret, "Self::")
		default:
			if i := strings.Index(ret, "::"); i >= 0 {
				owner = ret[:i]
				projection = ret[i+2:]
			}
		}
		if projection == "" {
			continue
		}
		if concrete := w.assocTypes[owner+"."+projection]; concrete != "" {
			w.returns[qname] = concrete
		}
	}
}

// collectReturns is a pre-pass: a call may precede the definition of the
// function whose return type types its receiver.
func (w *walker) collectReturns(n sitter.Node, enclosing, implType string) {
	switch n.Type() {
	case "function_item":
		name := nameOf(n, w.src)
		ret := typeNameOf(n.ChildByFieldName("return_type"), w.src)
		if name != "" && ret != "" {
			qname := join(enclosing, name)
			if implType != "" {
				qname = implType + "." + name
			}
			w.returns[qname] = ret
		}
		return
	case "impl_item":
		base := typeNameOf(n.ChildByFieldName("type"), w.src)
		body := n.ChildByFieldName("body")
		if base == "" || body.IsNull() {
			return
		}
		for i := uint32(0); i < body.NamedChildCount(); i++ {
			w.collectReturns(body.NamedChild(i), base, base)
		}
		return
	case "trait_item":
		name := nameOf(n, w.src)
		body := n.ChildByFieldName("body")
		if name == "" || body.IsNull() {
			return
		}
		qname := join(enclosing, name)
		for i := uint32(0); i < body.NamedChildCount(); i++ {
			w.collectReturns(body.NamedChild(i), qname, "")
		}
		return
	case "mod_item":
		name := nameOf(n, w.src)
		body := n.ChildByFieldName("body")
		if name == "" || body.IsNull() {
			return
		}
		qname := join(enclosing, name)
		for i := uint32(0); i < body.NamedChildCount(); i++ {
			w.collectReturns(body.NamedChild(i), qname, "")
		}
		return
	}
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		w.collectReturns(n.NamedChild(i), enclosing, implType)
	}
}

// node visits one item. enclosing is the qname prefix; implType is set inside
// an impl block so methods become Type.method; inTrait marks a trait body.
func (w *walker) node(n sitter.Node, enclosing string, exported bool, implType string, inTrait bool) {
	if n.IsNull() {
		return
	}

	switch n.Type() {
	case "use_declaration":
		w.useDeclaration(n)
		return

	case "function_item":
		w.function(n, enclosing, exported, implType, inTrait)
		return

	case "function_signature_item":
		w.functionSignature(n, enclosing, exported, implType, inTrait)
		return

	case "struct_item":
		w.item("struct", n, enclosing, exported)
		w.structFields(n)
		return

	case "enum_item":
		w.item("enum", n, enclosing, exported)
		return

	case "trait_item":
		w.trait(n, enclosing, exported)
		return

	case "impl_item":
		w.implBlock(n, enclosing)
		return

	case "mod_item":
		w.module(n, enclosing, exported)
		return

	case "type_item":
		w.item("type", n, enclosing, exported)
		return

	case "const_item", "static_item":
		w.item("const", n, enclosing, exported)
		return

	case "macro_definition":
		// Macro bodies are token trees. repath does not invent symbols or edges
		// from them; the blind spot is documented rather than guessed.
		return

	case "macro_invocation":
		return

	case "call_expression":
		w.call(n, enclosing, implType)
		w.children(n, enclosing, implType, inTrait)
		return

	case "struct_expression":
		w.structExpression(n, enclosing)
		w.children(n, enclosing, implType, inTrait)
		return

	case "let_declaration":
		w.letDeclaration(n, enclosing, implType)
		w.children(n, enclosing, implType, inTrait)
		return
	}

	w.children(n, enclosing, implType, inTrait)
}

func (w *walker) children(n sitter.Node, enclosing, implType string, inTrait bool) {
	if n.IsNull() {
		return
	}
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		w.node(n.NamedChild(i), enclosing, false, implType, inTrait)
	}
}

func (w *walker) function(n sitter.Node, enclosing string, exported bool, implType string, inTrait bool) {
	name := nameOf(n, w.src)
	if name == "" {
		return
	}

	qname := join(enclosing, name)
	kind := "function"
	switch {
	case implType != "":
		qname = implType + "." + name
		kind = "method"
		exported = hasVisibility(n) || inTrait || exported
	case inTrait:
		kind = "method"
	}

	params := n.ChildByFieldName("parameters")
	arity := w.parameters(params, qname, implType != "" || inTrait)
	exported = hasVisibility(n) || exported
	w.add(kind, name, qname, arity, n, exported)
	w.collectBounds(qname, n.ChildByFieldName("type_parameters"), childOfType(n, "where_clause"))
	w.entrypointsFor(n, name, qname)
	if kind == "function" && name == "main" {
		w.addEntrypoint(name, qname, "rs.main", 0.95, n)
	}

	// Trait method bodies and impl methods are visited like functions.
	w.children(params, qname, "", false)
	w.children(n.ChildByFieldName("body"), qname, "", false)
}

// entrypointsFor anchors the two Rust entry conventions the graph can judge:
// a `#[test]` function and a crate-root `main`. Attributes are previous named
// siblings in the tree-sitter grammar.
func (w *walker) entrypointsFor(n sitter.Node, name, qname string) {
	if !hasAttribute(n, w.src, "test") {
		return
	}
	w.addEntrypoint(name, qname, "rs.test", 0.90, n)
}

func (w *walker) addEntrypoint(name, qname, rule string, confidence float64, n sitter.Node) {
	if name == "" || qname == "" {
		return
	}
	w.res.Entrypoints = append(w.res.Entrypoints, Entrypoint{
		QName: qname, Name: name, Rule: rule, Confidence: confidence,
		Line: int(n.StartPoint().Row) + 1,
	})
}

func hasAttribute(n sitter.Node, src []byte, want string) bool {
	for prev := n.PrevNamedSibling(); !prev.IsNull() && prev.Type() == "attribute_item"; prev = prev.PrevNamedSibling() {
		if strings.Contains(prev.Content(src), want) {
			return true
		}
	}
	return false
}

func (w *walker) functionSignature(n sitter.Node, enclosing string, exported bool, implType string, inTrait bool) {
	name := nameOf(n, w.src)
	if name == "" {
		return
	}
	qname := join(enclosing, name)
	kind := "function"
	if implType != "" {
		qname = implType + "." + name
		kind = "method"
	} else if inTrait {
		kind = "method"
	}
	params := n.ChildByFieldName("parameters")
	arity := w.parameters(params, qname, implType != "" || inTrait)
	exported = hasVisibility(n) || exported || inTrait
	w.add(kind, name, qname, arity, n, exported)
	w.children(params, qname, "", false)
}

func (w *walker) item(kind string, n sitter.Node, enclosing string, exported bool) {
	name := nameOf(n, w.src)
	if name == "" {
		return
	}
	w.add(kind, name, join(enclosing, name), 0, n, hasVisibility(n) || exported)
}

func (w *walker) trait(n sitter.Node, enclosing string, exported bool) {
	name := nameOf(n, w.src)
	if name == "" {
		return
	}
	qname := join(enclosing, name)
	pub := hasVisibility(n) || exported
	w.add("trait", name, qname, 0, n, pub)
	w.collectBounds(qname, n.ChildByFieldName("type_parameters"), childOfType(n, "where_clause"))

	// Supertrait paths are stored as reserved import bindings so a later run can
	// follow `T: Child` to `Child: Root` without the trait's AST. They carry a
	// "@trait:" local name, which no Rust identifier can collide with.
	if bounds := n.ChildByFieldName("bounds"); !bounds.IsNull() {
		line := int(n.StartPoint().Row) + 1
		for i := uint32(0); i < bounds.NamedChildCount(); i++ {
			if path := boundPath(bounds.NamedChild(i), w.src); path != "" {
				w.res.Imports = append(w.res.Imports, Import{
					Local:    "@trait:" + qname + "#" + fmt.Sprint(i),
					Imported: path, Source: "", Line: line, Public: pub,
				})
			}
		}
	}

	body := n.ChildByFieldName("body")
	if body.IsNull() {
		return
	}
	for i := uint32(0); i < body.NamedChildCount(); i++ {
		w.node(body.NamedChild(i), qname, pub, "", true)
	}
}

func (w *walker) implBlock(n sitter.Node, enclosing string) {
	typeNode := n.ChildByFieldName("type")
	baseType := typeNameOf(typeNode, w.src)
	if baseType == "" {
		return
	}
	w.collectBounds(baseType, n.ChildByFieldName("type_parameters"), childOfType(n, "where_clause"))

	if traitNode := n.ChildByFieldName("trait"); !traitNode.IsNull() {
		w.implTraitRef(traitNode, baseType)
	}

	traitImpl := !n.ChildByFieldName("trait").IsNull()
	body := n.ChildByFieldName("body")
	if body.IsNull() {
		return
	}
	for i := uint32(0); i < body.NamedChildCount(); i++ {
		w.node(body.NamedChild(i), baseType, traitImpl, baseType, false)
	}
}

// implTraitRef records Type -> Trait as an IMPLEMENTS reference. The source is
// the type symbol, so it is attached only when the type is a symbol in this
// file (the common case); an external type has no source node.
func (w *walker) implTraitRef(traitNode sitter.Node, baseType string) {
	line := int(traitNode.StartPoint().Row) + 1
	switch traitNode.Type() {
	case "type_identifier", "identifier":
		w.res.Refs = append(w.res.Refs, Ref{
			Kind: "implements", Name: traitNode.Content(w.src), Form: "ident",
			Line: line, Enclosing: baseType,
		})
	case "scoped_type_identifier", "scoped_identifier", "generic_type":
		path, name := typePath(traitNode, w.src)
		if name == "" {
			return
		}
		form := "ident"
		if path != "" {
			form = "member"
		}
		w.res.Refs = append(w.res.Refs, Ref{
			Kind: "implements", Name: name, Form: form, Receiver: path,
			Line: line, Enclosing: baseType,
		})
	}
}

func (w *walker) module(n sitter.Node, enclosing string, exported bool) {
	name := nameOf(n, w.src)
	if name == "" {
		return
	}
	qname := join(enclosing, name)
	body := n.ChildByFieldName("body")
	pub := hasVisibility(n) || exported
	line := int(n.StartPoint().Row) + 1

	// Module layout is persisted as a reserved import binding: `mod x { .. }`
	// carries an empty source (the module lives in this file), and
	// `#[path = "..."] mod x;` carries the attribute value. The "@mod:" local
	// name cannot collide with a Rust identifier, so the use tree stays
	// untouched. Conventional `mod x;` needs no row: its file is derived from
	// the parent module's directory.
	if body.IsNull() {
		if attr := pathAttribute(n, w.src); attr != "" {
			w.res.Imports = append(w.res.Imports, Import{
				Local: "@mod:" + qname, Imported: "@mod", Source: attr, Line: line, Public: pub,
			})
		}
		return
	}
	w.res.Imports = append(w.res.Imports, Import{
		Local: "@mod:" + qname, Imported: "@mod", Source: "", Line: line, Public: pub,
	})
	for i := uint32(0); i < body.NamedChildCount(); i++ {
		w.node(body.NamedChild(i), qname, pub, "", false)
	}
}

// pathAttribute returns the value of a #[path = "..."] attribute immediately
// above a module declaration.
func pathAttribute(n sitter.Node, src []byte) string {
	for prev := n.PrevNamedSibling(); !prev.IsNull() && prev.Type() == "attribute_item"; prev = prev.PrevNamedSibling() {
		text := prev.Content(src)
		if !strings.Contains(text, "path") {
			continue
		}
		start := strings.Index(text, `"`)
		if start < 0 {
			continue
		}
		end := strings.Index(text[start+1:], `"`)
		if end < 0 {
			continue
		}
		return text[start+1 : start+1+end]
	}
	return ""
}

func (w *walker) structFields(n sitter.Node) {
	body := n.ChildByFieldName("body")
	if body.IsNull() || body.Type() != "field_declaration_list" {
		return
	}
	typeName := nameOf(n, w.src)
	for i := uint32(0); i < body.NamedChildCount(); i++ {
		field := body.NamedChild(i)
		if field.Type() != "field_declaration" {
			continue
		}
		name := fieldIdentifier(field, "name", w.src)
		typ := typeNameOf(field.ChildByFieldName("type"), w.src)
		if name != "" && typ != "" {
			w.types["field:"+typeName+"."+name] = typ
		}
	}
}

// collectFields records struct field types before walking, because an impl may
// appear before the struct in the file.
func (w *walker) collectFields(n sitter.Node) {
	if n.Type() == "struct_item" {
		w.structFields(n)
		return
	}
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		w.collectFields(n.NamedChild(i))
	}
}

func (w *walker) call(n sitter.Node, enclosing, implType string) {
	fn := ungeneric(n.ChildByFieldName("function"))
	if fn.IsNull() {
		return
	}
	arguments := n.ChildByFieldName("arguments")
	arity := 0
	if !arguments.IsNull() {
		arity = int(arguments.NamedChildCount())
	}
	line := int(n.StartPoint().Row) + 1

	switch fn.Type() {
	case "identifier", "type_identifier":
		w.res.Refs = append(w.res.Refs, Ref{
			Kind: "call", Name: fn.Content(w.src), Form: "ident",
			Arity: arity, Line: line, Enclosing: enclosing,
		})
	case "scoped_identifier", "scoped_type_identifier":
		pathNode := fn.ChildByFieldName("path")
		nameNode := fn.ChildByFieldName("name")
		if pathNode.IsNull() || nameNode.IsNull() {
			return
		}
		w.res.Refs = append(w.res.Refs, Ref{
			Kind: "call", Name: nameNode.Content(w.src), Form: "scoped",
			Receiver: pathText(pathNode, w.src), Arity: arity, Line: line, Enclosing: enclosing,
		})
	case "field_expression":
		value := fn.ChildByFieldName("value")
		field := fn.ChildByFieldName("field")
		if value.IsNull() || field.IsNull() {
			return
		}
		ref := Ref{Kind: "call", Name: field.Content(w.src), Form: "member", Arity: arity, Line: line, Enclosing: enclosing}
		switch value.Type() {
		case "self":
			ref.Form = "this"
		case "identifier":
			receiver := value.Content(w.src)
			ref.Receiver = receiver
			ref.ReceiverType = w.types[scopeKey(enclosing, receiver)]
			ref.ReceiverFrom = w.typeFrom[scopeKey(enclosing, receiver)]
			if ref.ReceiverType == "Self" && implType != "" {
				ref.ReceiverType = implType
			}
		case "field_expression":
			receiver := pathText(value, w.src)
			ref.Receiver = receiver
			if recvType := w.selfFieldType(receiver, enclosing); recvType != "" {
				ref.ReceiverType = recvType
			}
		case "call_expression":
			// make().method(): only a same-file return definition types the
			// chained receiver. A bare `Type::new()` heuristic (no local return
			// type) is not trustworthy here, so the chain is not walked.
			typ, from := w.constructedType(value, enclosing, implType)
			if typ == "" || from == "" {
				return
			}
			ref.ReceiverType = typ
			ref.ReceiverFrom = from
		default:
			return
		}
		if ref.ReceiverType != "" {
			if ref.ReceiverBound == "" {
				ref.ReceiverBound = w.boundsFor(enclosing, ref.ReceiverType)
			}
			ref.ReceiverParam = w.isTypeParam(enclosing, ref.ReceiverType)
		}
		w.res.Refs = append(w.res.Refs, ref)
	}
}

// selfFieldType resolves self.field.method() using the struct's field types.
func (w *walker) selfFieldType(receiver, enclosing string) string {
	parts := strings.Split(receiver, ".")
	if len(parts) != 2 || parts[0] != "self" {
		return ""
	}
	return w.types["field:"+classOf(enclosing)+"."+parts[1]]
}

func (w *walker) structExpression(n sitter.Node, enclosing string) {
	name := n.ChildByFieldName("name")
	if name.IsNull() {
		return
	}
	form := "ident"
	receiver := ""
	if name.Type() == "scoped_type_identifier" {
		path, leaf := typePath(name, w.src)
		if leaf == "" {
			return
		}
		form = "member"
		receiver = path
		w.res.Refs = append(w.res.Refs, Ref{
			Kind: "new", Name: leaf, Form: form, Receiver: receiver,
			Line: int(n.StartPoint().Row) + 1, Enclosing: enclosing,
		})
		return
	}
	w.res.Refs = append(w.res.Refs, Ref{
		Kind: "new", Name: name.Content(w.src), Form: form, Receiver: receiver,
		Line: int(n.StartPoint().Row) + 1, Enclosing: enclosing,
	})
}

// letDeclaration records the type of local bindings so value.method() can
// resolve when the type is annotated, constructed locally, or returned by a
// same-file function whose return type is a local definition.
func (w *walker) letDeclaration(n sitter.Node, enclosing, implType string) {
	pattern := n.ChildByFieldName("pattern")
	if pattern.IsNull() || pattern.Type() != "identifier" {
		return
	}
	typ := typeNameOf(n.ChildByFieldName("type"), w.src)
	from := ""
	if typ == "" {
		typ, from = w.constructedType(n.ChildByFieldName("value"), enclosing, implType)
	}
	if typ == "" {
		return
	}
	if typ == "Self" && implType != "" {
		typ = implType
	}
	key := scopeKey(enclosing, pattern.Content(w.src))
	w.types[key] = typ
	if from != "" {
		w.typeFrom[key] = from
	}
}

// parameters records parameter types and returns the declared arity, excluding
// Rust's self parameter.
func (w *walker) parameters(params sitter.Node, enclosing string, method bool) int {
	if params.IsNull() {
		return 0
	}
	count := 0
	for i := uint32(0); i < params.NamedChildCount(); i++ {
		param := params.NamedChild(i)
		if param.Type() == "self_parameter" {
			continue
		}
		if param.Type() != "parameter" {
			continue
		}
		name := fieldIdentifier(param, "pattern", w.src)
		typ := typeNameOf(param.ChildByFieldName("type"), w.src)
		if name != "" && typ != "" {
			w.types[scopeKey(enclosing, name)] = typ
		}
		count++
	}
	return count
}

// useDeclaration flattens Rust's use tree into local bindings.
func (w *walker) useDeclaration(n sitter.Node) {
	argument := n.ChildByFieldName("argument")
	if argument.IsNull() {
		return
	}
	line := int(n.StartPoint().Row) + 1
	public := hasVisibility(n)
	w.flattenUse(argument, "", public, line)
}

func (w *walker) flattenUse(n sitter.Node, prefix string, public bool, line int) {
	if n.IsNull() {
		return
	}
	switch n.Type() {
	case "scoped_use_list":
		pathNode := n.ChildByFieldName("path")
		next := prefix
		if !pathNode.IsNull() {
			next = joinPath(prefix, pathText(pathNode, w.src))
		}
		list := n.ChildByFieldName("list")
		for i := uint32(0); i < list.NamedChildCount(); i++ {
			w.flattenUse(list.NamedChild(i), next, public, line)
		}

	case "use_list":
		for i := uint32(0); i < n.NamedChildCount(); i++ {
			w.flattenUse(n.NamedChild(i), prefix, public, line)
		}

	case "scoped_identifier", "scoped_type_identifier":
		pathNode := n.ChildByFieldName("path")
		nameNode := n.ChildByFieldName("name")
		if pathNode.IsNull() || nameNode.IsNull() {
			return
		}
		w.addImport(nameNode.Content(w.src), nameNode.Content(w.src), pathText(pathNode, w.src), line, public)

	case "identifier", "type_identifier":
		name := n.Content(w.src)
		w.addImport(name, name, prefix, line, public)

	case "use_as_clause":
		pathNode := n.ChildByFieldName("path")
		alias := n.ChildByFieldName("alias")
		if pathNode.IsNull() || alias.IsNull() {
			return
		}
		source, imported := pathParts(pathNode, prefix, w.src)
		if imported == "" {
			return
		}
		w.addImport(alias.Content(w.src), imported, source, line, public)

	case "use_wildcard":
		source := ""
		for i := uint32(0); i < n.NamedChildCount(); i++ {
			source = pathText(n.NamedChild(i), w.src)
			break
		}
		w.addImport("*", "*", source, line, public)

	case "self":
		if prefix != "" {
			w.addImport(lastSegment(prefix), "*", prefix, line, public)
		}
	}
}

func (w *walker) addImport(local, imported, source string, line int, public bool) {
	if local == "" || imported == "" {
		return
	}
	w.res.Imports = append(w.res.Imports, Import{
		Local: local, Imported: imported, Source: source, Line: line, Public: public,
	})
}

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
		Kind: kind, Name: name, QName: qname, Arity: arity,
		StartLine: int(n.StartPoint().Row) + 1,
		EndLine:   int(n.EndPoint().Row) + 1,
		Exported:  exported,
	})
}

// ungeneric unwraps `foo::<T>` and `foo::<T>()` to the underlying callable.
func ungeneric(n sitter.Node) sitter.Node {
	for !n.IsNull() {
		switch n.Type() {
		case "generic_function":
			n = n.ChildByFieldName("function")
		case "parenthesized_expression":
			if n.NamedChildCount() == 0 {
				return sitter.Node{}
			}
			n = n.NamedChild(0)
		default:
			return n
		}
	}
	return n
}

// constructedType infers the type of `let x = Type::new()`, `Type { .. }`, or a
// call to a same-file function whose return type is a local definition. The
// second result is the inference source: "return" for a return type.
func (w *walker) constructedType(value sitter.Node, enclosing, implType string) (string, string) {
	if value.IsNull() {
		return "", ""
	}
	switch value.Type() {
	case "struct_expression":
		if name := value.ChildByFieldName("name"); !name.IsNull() {
			return typeNameOf(name, w.src), ""
		}
	case "call_expression":
		fn := ungeneric(value.ChildByFieldName("function"))
		if fn.Type() == "field_expression" {
			// value.method(): type the result only from the same file's declared
			// return type for that receiver type.
			recv := fn.ChildByFieldName("value")
			field := fn.ChildByFieldName("field")
			if !recv.IsNull() && !field.IsNull() && recv.Type() == "identifier" {
				recvType := w.types[scopeKey(enclosing, recv.Content(w.src))]
				if recvType != "" {
					if ret := w.returns[recvType+"."+field.Content(w.src)]; ret != "" && ret != "Self" {
						return ret, "return"
					}
				}
			}
			return "", ""
		}
		switch fn.Type() {
		case "scoped_identifier", "scoped_type_identifier":
			path := fn.ChildByFieldName("path")
			name := fn.ChildByFieldName("name")
			if path.IsNull() || name.IsNull() {
				return "", ""
			}
			if pathText(path, w.src) == "Self" {
				return "Self", ""
			}
			typ := typeNameOf(path, w.src)
			if typ == "" {
				typ = pathText(path, w.src)
			}
			last := lastSegment(typ)
			// Only a same-file definition types the call's result. `Type::new()`
			// returning Self is the one associated-function convention on top of
			// that, and an explicit `-> Other` wins over the path's own name.
			if ret := w.returns[last+"."+name.Content(w.src)]; ret != "" {
				if ret == "Self" {
					return typ, "return"
				}
				return ret, "return"
			}
			if startsUpper(last) {
				return typ, ""
			}
		case "identifier", "type_identifier":
			name := fn.Content(w.src)
			if ret := w.returns[name]; ret != "" && ret != "Self" {
				return ret, "return"
			}
			if startsUpper(name) {
				return name, ""
			}
		}
	case "reference_expression":
		return w.constructedType(value.ChildByFieldName("value"), enclosing, implType)
	}
	return "", ""
}

func hasVisibility(n sitter.Node) bool {
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		if n.NamedChild(i).Type() == "visibility_modifier" {
			return true
		}
	}
	return false
}

func nameOf(n sitter.Node, src []byte) string {
	name := n.ChildByFieldName("name")
	if name.IsNull() {
		return ""
	}
	switch name.Type() {
	case "identifier", "type_identifier":
		return name.Content(src)
	}
	return ""
}

func fieldIdentifier(n sitter.Node, field string, src []byte) string {
	id := n.ChildByFieldName(field)
	if id.IsNull() || (id.Type() != "identifier" && id.Type() != "field_identifier") {
		return ""
	}
	return id.Content(src)
}

// typeNameOf reduces a type expression to its base symbol name.
func typeNameOf(n sitter.Node, src []byte) string {
	if n.IsNull() {
		return ""
	}
	switch n.Type() {
	case "type_identifier", "primitive_type":
		return n.Content(src)
	case "generic_type":
		return typeNameOf(n.ChildByFieldName("type"), src)
	case "reference_type":
		return typeNameOf(n.ChildByFieldName("type"), src)
	case "slice_type":
		return "slice"
	case "array_type":
		return "array"
	case "dynamic_type":
		if n.NamedChildCount() > 0 {
			return typeNameOf(n.NamedChild(0), src)
		}
	case "scoped_type_identifier", "scoped_identifier":
		return pathText(n, src)
	}
	return ""
}

// typePath returns the module prefix and last segment of a type/path node.
func typePath(n sitter.Node, src []byte) (string, string) {
	switch n.Type() {
	case "type_identifier", "identifier":
		return "", n.Content(src)
	case "scoped_type_identifier", "scoped_identifier":
		pathNode := n.ChildByFieldName("path")
		nameNode := n.ChildByFieldName("name")
		if pathNode.IsNull() || nameNode.IsNull() {
			return "", ""
		}
		return pathText(pathNode, src), nameNode.Content(src)
	case "generic_type":
		return typePath(n.ChildByFieldName("type"), src)
	}
	return "", ""
}

// pathText renders a path node with :: separators, stripping generic arguments.
func pathText(n sitter.Node, src []byte) string {
	if n.IsNull() {
		return ""
	}
	switch n.Type() {
	case "identifier", "type_identifier", "crate", "super", "self", "primitive_type":
		return n.Content(src)
	case "scoped_identifier", "scoped_type_identifier":
		pathNode := n.ChildByFieldName("path")
		nameNode := n.ChildByFieldName("name")
		if pathNode.IsNull() || nameNode.IsNull() {
			return strings.TrimSpace(n.Content(src))
		}
		return joinPath(pathText(pathNode, src), nameNode.Content(src))
	case "generic_type":
		return pathText(n.ChildByFieldName("type"), src)
	}
	text := strings.TrimSpace(n.Content(src))
	if i := strings.Index(text, "::<"); i >= 0 {
		text = text[:i]
	}
	return text
}

// pathParts splits a use path into (module source, imported name).
func pathParts(n sitter.Node, prefix string, src []byte) (string, string) {
	switch n.Type() {
	case "scoped_identifier", "scoped_type_identifier":
		pathNode := n.ChildByFieldName("path")
		nameNode := n.ChildByFieldName("name")
		if pathNode.IsNull() || nameNode.IsNull() {
			return "", ""
		}
		return pathText(pathNode, src), nameNode.Content(src)
	case "identifier", "type_identifier":
		return prefix, n.Content(src)
	}
	return prefix, ""
}

func joinPath(prefix, name string) string {
	if name == "" {
		return prefix
	}
	if prefix == "" || strings.HasPrefix(name, "crate") || strings.HasPrefix(name, "super") || strings.HasPrefix(name, "self") {
		return name
	}
	return prefix + "::" + name
}

func lastSegment(path string) string {
	if i := strings.LastIndex(path, "::"); i >= 0 {
		return path[i+2:]
	}
	return path
}

func join(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "." + name
}

func classOf(qname string) string {
	if i := strings.LastIndex(qname, "."); i >= 0 {
		return qname[:i]
	}
	return qname
}

func scopeKey(enclosing, name string) string {
	return enclosing + ":" + name
}

func startsUpper(name string) bool {
	if name == "" {
		return false
	}
	c := name[0]
	return c >= 'A' && c <= 'Z'
}

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
