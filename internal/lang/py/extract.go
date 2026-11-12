// Package py extracts symbol definitions, references, and imports from Python
// sources with a tree-sitter grammar.
package py

import (
	"context"
	"fmt"
	"strings"

	forestpython "github.com/alexaandru/go-sitter-forest/python"
	sitter "github.com/alexaandru/go-tree-sitter-bare"

	"github.com/Goutam-11/repath/internal/lang/facts"
)

// Dialect selects the grammar used for a file. Python has one grammar today;
// the type exists so callers treat every language frontend the same way.
type Dialect int

const (
	// Python parses .py files.
	Python Dialect = iota
)

// Symbol, Ref, Import, and Result are the shared extraction-fact types.
type (
	Symbol = facts.Symbol
	Ref    = facts.Ref
	Import = facts.Import
	Result = facts.Result
)

// Extractor holds the Python parser. It is not safe for concurrent use; index
// workers each create their own.
type Extractor struct {
	parser *sitter.Parser
}

// New builds an extractor for the pinned Python grammar.
func New() *Extractor {
	p := sitter.NewParser()
	p.SetLanguage(sitter.NewLanguage(forestpython.GetLanguage()))
	return &Extractor{parser: p}
}

// Extract parses src and returns its definitions, references, and imports.
func (e *Extractor) Extract(src []byte, dialect Dialect) Result {
	if dialect != Python {
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
	for i := uint32(0); i < root.NamedChildCount(); i++ {
		w.node(root.NamedChild(i), "", true, false, nil)
	}
	return res
}

type walker struct {
	src   []byte
	res   *Result
	seen  map[string]bool
	types map[string]string // "enclosing:name" → type name
}

// node visits one statement or definition. enclosing is the qname of the symbol
// whose body contains it; inClass marks direct children of a class body so a
// def becomes a method rather than a function.
func (w *walker) node(n sitter.Node, enclosing string, exported, inClass bool, decorators []sitter.Node) {
	if n.IsNull() {
		return
	}

	switch n.Type() {
	case "import_statement":
		w.importStatement(n)
		return

	case "import_from_statement":
		w.importFrom(n)
		return

	case "decorated_definition":
		w.decorated(n, enclosing, exported, inClass)
		return

	case "function_definition":
		w.function(n, enclosing, exported, inClass, decorators)
		return

	case "class_definition":
		w.class(n, enclosing, exported, decorators)
		return

	case "call":
		w.call(n, enclosing)
		w.children(n, enclosing, inClass)
		return

	case "assignment":
		w.assignment(n, enclosing)
		w.children(n, enclosing, inClass)
		return
	}

	w.children(n, enclosing, inClass)
}

func (w *walker) children(n sitter.Node, enclosing string, inClass bool) {
	if n.IsNull() {
		return
	}
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		w.node(n.NamedChild(i), enclosing, false, inClass, nil)
	}
}

// decorated unwraps "@decorator def ..." and passes the decorators to the
// definition, which records them as references to the decorated symbol.
func (w *walker) decorated(n sitter.Node, enclosing string, exported, inClass bool) {
	var decorators []sitter.Node
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		if child := n.NamedChild(i); child.Type() == "decorator" {
			decorators = append(decorators, child)
		}
	}
	w.node(n.ChildByFieldName("definition"), enclosing, exported, inClass, decorators)
}

func (w *walker) function(n sitter.Node, enclosing string, exported, inClass bool, decorators []sitter.Node) {
	name := nameOf(n, w.src)
	if name == "" {
		return
	}
	qname := join(enclosing, name)

	params := n.ChildByFieldName("parameters")
	arity := w.parameters(params, qname, inClass)

	kind := "function"
	if inClass {
		kind = "method"
	}
	w.add(kind, name, qname, arity, n, exported)
	w.decoratorRefs(decorators, qname)
	w.children(params, qname, false)
	w.children(n.ChildByFieldName("body"), qname, false)
}

func (w *walker) class(n sitter.Node, enclosing string, exported bool, decorators []sitter.Node) {
	name := nameOf(n, w.src)
	if name == "" {
		return
	}
	qname := join(enclosing, name)

	w.add("class", name, qname, 0, n, exported)
	w.bases(n, qname)
	w.decoratorRefs(decorators, qname)

	body := n.ChildByFieldName("body")
	if body.IsNull() {
		return
	}
	for i := uint32(0); i < body.NamedChildCount(); i++ {
		w.node(body.NamedChild(i), qname, true, true, nil)
	}
}

// bases records positional superclasses as EXTENDS references. Keyword
// arguments (metaclass=...) are not base types and are skipped.
func (w *walker) bases(class sitter.Node, classQName string) {
	superclasses := class.ChildByFieldName("superclasses")
	if superclasses.IsNull() {
		return
	}
	for i := uint32(0); i < superclasses.NamedChildCount(); i++ {
		w.typeRef(superclasses.NamedChild(i), "extends", classQName)
	}
}

// typeRef turns one base expression into a reference, unwrapping a subscript
// such as Generic[T].
func (w *walker) typeRef(n sitter.Node, kind, enclosing string) {
	switch n.Type() {
	case "identifier":
		w.res.Refs = append(w.res.Refs, Ref{
			Kind: kind, Name: n.Content(w.src), Form: "ident",
			Line: int(n.StartPoint().Row) + 1, Enclosing: enclosing,
		})
	case "attribute":
		object := n.ChildByFieldName("object")
		attr := n.ChildByFieldName("attribute")
		if object.IsNull() || attr.IsNull() || !receiverLike(object) {
			return
		}
		w.res.Refs = append(w.res.Refs, Ref{
			Kind: kind, Name: attr.Content(w.src), Form: "member",
			Receiver: trim(object.Content(w.src)),
			Line:     int(n.StartPoint().Row) + 1, Enclosing: enclosing,
		})
	case "subscript":
		if value := n.ChildByFieldName("value"); !value.IsNull() {
			w.typeRef(value, kind, enclosing)
		}
	}
}

func (w *walker) decoratorRefs(decorators []sitter.Node, enclosing string) {
	for _, decorator := range decorators {
		if decorator.IsNull() || decorator.NamedChildCount() == 0 {
			continue
		}
		expr := decorator.NamedChild(0)
		if expr.Type() == "call" {
			expr = expr.ChildByFieldName("function")
		}
		line := int(decorator.StartPoint().Row) + 1

		switch expr.Type() {
		case "identifier":
			w.res.Refs = append(w.res.Refs, Ref{
				Kind: "decorator", Name: expr.Content(w.src), Form: "ident",
				Line: line, Enclosing: enclosing,
			})
		case "attribute":
			object := expr.ChildByFieldName("object")
			attr := expr.ChildByFieldName("attribute")
			if object.IsNull() || attr.IsNull() || !receiverLike(object) {
				continue
			}
			w.res.Refs = append(w.res.Refs, Ref{
				Kind: "decorator", Name: attr.Content(w.src), Form: "member",
				Receiver: trim(object.Content(w.src)),
				Line:     line, Enclosing: enclosing,
			})
		}
	}
}

func (w *walker) call(n sitter.Node, enclosing string) {
	fn := n.ChildByFieldName("function")
	if fn.IsNull() {
		return
	}
	arguments := n.ChildByFieldName("arguments")
	arity := positionalArgs(arguments)
	line := int(n.StartPoint().Row) + 1

	switch fn.Type() {
	case "identifier":
		w.res.Refs = append(w.res.Refs, Ref{
			Kind: "call", Name: fn.Content(w.src), Form: "ident",
			Arity: arity, Line: line, Enclosing: enclosing,
		})
	case "attribute":
		object := fn.ChildByFieldName("object")
		attr := fn.ChildByFieldName("attribute")
		if object.IsNull() || attr.IsNull() {
			return
		}
		ref := Ref{Kind: "call", Name: attr.Content(w.src), Form: "member", Arity: arity, Line: line, Enclosing: enclosing}

		switch object.Type() {
		case "identifier":
			receiver := object.Content(w.src)
			switch receiver {
			case "self", "cls":
				ref.Form = "this"
			default:
				ref.Receiver = receiver
				ref.ReceiverType = w.types[scopeKey(enclosing, receiver)]
			}
		case "attribute":
			// "self.cart.total()" is not a guessed type: only a field whose
			// type was recorded from an annotation or a Constructor() call is
			// typed.
			receiver := trim(object.Content(w.src))
			ref.Receiver = receiver
			if field := selfField(receiver); field != "" {
				ref.ReceiverType = w.types[scopeKey(classOf(enclosing), "attr."+field)]
			}
		default:
			return
		}
		w.res.Refs = append(w.res.Refs, ref)
	}
}

// assignment records local variable types from annotations and Constructor()
// calls so obj.method() can resolve without guessing. It then lets children be
// visited for calls on either side.
func (w *walker) assignment(n sitter.Node, enclosing string) {
	left := n.ChildByFieldName("left")
	right := n.ChildByFieldName("right")

	typ := ""
	if annotation := n.ChildByFieldName("type"); !annotation.IsNull() {
		typ = typeName(annotation, w.src)
	}
	if typ == "" {
		typ = callType(right, w.src)
	}
	if typ == "" {
		return
	}

	switch left.Type() {
	case "identifier":
		w.types[scopeKey(enclosing, left.Content(w.src))] = typ
	case "attribute":
		object := left.ChildByFieldName("object")
		attr := left.ChildByFieldName("attribute")
		if object.IsNull() || attr.IsNull() {
			return
		}
		if object.Type() == "identifier" && (object.Content(w.src) == "self" || object.Content(w.src) == "cls") {
			w.types[scopeKey(classOf(enclosing), "attr."+attr.Content(w.src))] = typ
		}
	}
}

// parameters records parameter annotations and returns the declared arity.
// self/cls is not part of a bound method's call arity; *args/**kwargs do not
// add a positional parameter.
func (w *walker) parameters(params sitter.Node, enclosing string, method bool) int {
	if params.IsNull() {
		return 0
	}

	count := 0
	for i := uint32(0); i < params.NamedChildCount(); i++ {
		param := params.NamedChild(i)
		name := ""
		var typNode sitter.Node

		switch param.Type() {
		case "identifier":
			name = param.Content(w.src)
		case "typed_parameter":
			name = firstIdentifier(param, w.src)
			typNode = param.ChildByFieldName("type")
		case "typed_default_parameter":
			name = fieldIdentifier(param, "name", w.src)
			typNode = param.ChildByFieldName("type")
		case "default_parameter":
			name = fieldIdentifier(param, "name", w.src)
		case "list_splat_pattern", "dictionary_splat_pattern":
			continue
		default:
			continue
		}
		if name == "" {
			continue
		}

		if method && count == 0 && (name == "self" || name == "cls") {
			continue
		}
		if !typNode.IsNull() {
			if typ := typeName(typNode, w.src); typ != "" {
				w.types[scopeKey(enclosing, name)] = typ
			}
		}
		count++
	}
	return count
}

func (w *walker) importStatement(n sitter.Node) {
	line := int(n.StartPoint().Row) + 1
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		item := n.NamedChild(i)
		switch item.Type() {
		case "dotted_name":
			name := item.Content(w.src)
			w.res.Imports = append(w.res.Imports, Import{
				Local: firstSegment(name), Imported: "*", Source: name, Line: line,
			})
		case "aliased_import":
			name := item.ChildByFieldName("name")
			alias := item.ChildByFieldName("alias")
			if name.IsNull() || alias.IsNull() {
				continue
			}
			w.res.Imports = append(w.res.Imports, Import{
				Local: alias.Content(w.src), Imported: "*", Source: name.Content(w.src), Line: line,
			})
		}
	}
}

func (w *walker) importFrom(n sitter.Node) {
	line := int(n.StartPoint().Row) + 1
	source := w.moduleSource(n.ChildByFieldName("module_name"))

	for i := uint32(0); i < n.NamedChildCount(); i++ {
		if n.FieldNameForNamedChild(i) == "module_name" {
			continue
		}
		item := n.NamedChild(i)
		switch item.Type() {
		case "dotted_name":
			name := item.Content(w.src)
			w.res.Imports = append(w.res.Imports, Import{
				Local: firstSegment(name), Imported: name, Source: source, Line: line,
			})
		case "aliased_import":
			name := item.ChildByFieldName("name")
			alias := item.ChildByFieldName("alias")
			if name.IsNull() || alias.IsNull() {
				continue
			}
			w.res.Imports = append(w.res.Imports, Import{
				Local: alias.Content(w.src), Imported: name.Content(w.src), Source: source, Line: line,
			})
		case "wildcard_import":
			w.res.Imports = append(w.res.Imports, Import{
				Local: "*", Imported: "*", Source: source, Line: line,
			})
		}
	}
}

// moduleSource keeps the leading dots of a relative import, because resolution
// depends on the dot count ("." = current package, ".." = parent).
func (w *walker) moduleSource(node sitter.Node) string {
	if node.IsNull() {
		return ""
	}
	if node.Type() != "relative_import" {
		return node.Content(w.src)
	}

	dots := ""
	var rest string
	for i := uint32(0); i < node.NamedChildCount(); i++ {
		child := node.NamedChild(i)
		switch child.Type() {
		case "import_prefix":
			dots = strings.Repeat(".", strings.Count(child.Content(w.src), "."))
		case "dotted_name":
			rest = child.Content(w.src)
		}
	}
	return dots + rest
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

	// Any module-level def is importable in Python, including a private-looking
	// name and one defined under an `if`/`try` block.
	if !strings.Contains(qname, ".") {
		exported = true
	}

	w.res.Symbols = append(w.res.Symbols, Symbol{
		Kind: kind, Name: name, QName: qname, Arity: arity,
		StartLine: int(n.StartPoint().Row) + 1,
		EndLine:   int(n.EndPoint().Row) + 1,
		Exported:  exported,
	})
}

func positionalArgs(arguments sitter.Node) int {
	if arguments.IsNull() {
		return 0
	}
	count := 0
	for i := uint32(0); i < arguments.NamedChildCount(); i++ {
		if arguments.NamedChild(i).Type() == "keyword_argument" {
			continue
		}
		count++
	}
	return count
}

// callType returns the type a call expression assigns: Foo() and pkg.Foo()
// infer Foo. Only capitalized callees are treated as constructors, because a
// lowercase factory call (make()) does not tell us the returned type.
func callType(n sitter.Node, src []byte) string {
	if n.IsNull() || n.Type() != "call" {
		return ""
	}
	fn := n.ChildByFieldName("function")
	var name string
	switch fn.Type() {
	case "identifier":
		name = fn.Content(src)
	case "attribute":
		if attr := fn.ChildByFieldName("attribute"); !attr.IsNull() {
			name = attr.Content(src)
		}
	}
	if name == "" || !startsUpper(name) {
		return ""
	}
	return name
}

func startsUpper(name string) bool {
	if name == "" {
		return false
	}
	c := name[0]
	return c >= 'A' && c <= 'Z'
}

// typeName reduces a type expression to its base name; unions and literals are
// ignored because they cannot identify one receiver type.
func typeName(node sitter.Node, src []byte) string {
	switch node.Type() {
	case "type":
		if node.NamedChildCount() == 0 {
			return ""
		}
		return typeName(node.NamedChild(0), src)
	case "identifier":
		return node.Content(src)
	case "subscript":
		if value := node.ChildByFieldName("value"); !value.IsNull() {
			return typeName(value, src)
		}
	case "attribute":
		if attr := node.ChildByFieldName("attribute"); !attr.IsNull() {
			return attr.Content(src)
		}
	}
	return ""
}

func nameOf(n sitter.Node, src []byte) string {
	name := n.ChildByFieldName("name")
	if name.IsNull() {
		return ""
	}
	if name.Type() == "identifier" {
		return name.Content(src)
	}
	return ""
}

func fieldIdentifier(n sitter.Node, field string, src []byte) string {
	id := n.ChildByFieldName(field)
	if id.IsNull() || id.Type() != "identifier" {
		return ""
	}
	return id.Content(src)
}

func firstIdentifier(n sitter.Node, src []byte) string {
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		if child := n.NamedChild(i); child.Type() == "identifier" {
			return child.Content(src)
		}
	}
	return ""
}

// receiverLike keeps member references honest: a call such as
// get().method() or items[0].method() has no identifier receiver to resolve.
func receiverLike(n sitter.Node) bool {
	switch n.Type() {
	case "identifier", "attribute":
		return true
	}
	return false
}

// selfField turns "self.cart" into "cart"; longer chains are not field types.
func selfField(receiver string) string {
	parts := strings.Split(receiver, ".")
	if len(parts) != 2 || (parts[0] != "self" && parts[0] != "cls") {
		return ""
	}
	return parts[1]
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

func firstSegment(name string) string {
	if i := strings.Index(name, "."); i >= 0 {
		return name[:i]
	}
	return name
}

func trim(text string) string {
	return strings.TrimSpace(text)
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
