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
	Symbol = facts.Symbol
	Ref    = facts.Ref
	Import = facts.Import
	Result = facts.Result
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

	w := &walker{src: src, res: &res, seen: make(map[string]bool), types: make(map[string]string)}
	w.collectFields(root)
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
		exported = exported
	}

	params := n.ChildByFieldName("parameters")
	arity := w.parameters(params, qname, implType != "" || inTrait)
	exported = hasVisibility(n) || exported
	w.add(kind, name, qname, arity, n, exported)

	// Trait method bodies and impl methods are visited like functions.
	w.children(params, qname, "", false)
	w.children(n.ChildByFieldName("body"), qname, "", false)
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
	if body.IsNull() {
		return
	}
	pub := hasVisibility(n) || exported
	for i := uint32(0); i < body.NamedChildCount(); i++ {
		w.node(body.NamedChild(i), qname, pub, "", false)
	}
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
			if ref.ReceiverType == "Self" && implType != "" {
				ref.ReceiverType = implType
			}
		case "field_expression":
			receiver := pathText(value, w.src)
			ref.Receiver = receiver
			if recvType := w.selfFieldType(receiver, enclosing); recvType != "" {
				ref.ReceiverType = recvType
			}
		default:
			return
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
// resolve when the type is annotated or constructed locally.
func (w *walker) letDeclaration(n sitter.Node, enclosing, implType string) {
	pattern := n.ChildByFieldName("pattern")
	if pattern.IsNull() || pattern.Type() != "identifier" {
		return
	}
	typ := typeNameOf(n.ChildByFieldName("type"), w.src)
	if typ == "" {
		typ = constructedType(n.ChildByFieldName("value"), w.src, implType)
	}
	if typ == "" {
		return
	}
	if typ == "Self" && implType != "" {
		typ = implType
	}
	w.types[scopeKey(enclosing, pattern.Content(w.src))] = typ
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

// constructedType infers the type of `let x = Type::new()` or `Type { .. }`.
func constructedType(value sitter.Node, src []byte, implType string) string {
	if value.IsNull() {
		return ""
	}
	switch value.Type() {
	case "struct_expression":
		if name := value.ChildByFieldName("name"); !name.IsNull() {
			return typeNameOf(name, src)
		}
	case "call_expression":
		fn := ungeneric(value.ChildByFieldName("function"))
		switch fn.Type() {
		case "scoped_identifier", "scoped_type_identifier":
			path := fn.ChildByFieldName("path")
			name := fn.ChildByFieldName("name")
			if path.IsNull() || name.IsNull() {
				return ""
			}
			if pathText(path, src) == "Self" {
				return "Self"
			}
			typ := typeNameOf(path, src)
			if typ == "" {
				typ = pathText(path, src)
			}
			if startsUpper(typ) {
				return typ
			}
		case "identifier", "type_identifier":
			name := fn.Content(src)
			if startsUpper(name) {
				return name
			}
		}
	case "reference_expression":
		return constructedType(value.ChildByFieldName("value"), src, implType)
	}
	return ""
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
	case "scoped_type_identifier", "scoped_identifier":
		if name := n.ChildByFieldName("name"); !name.IsNull() {
			return name.Content(src)
		}
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
