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
	Symbol     = facts.Symbol
	Ref        = facts.Ref
	Import     = facts.Import
	Entrypoint = facts.Entrypoint
	Result     = facts.Result
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

	w := &walker{
		src: src, res: &res, seen: make(map[string]bool), types: make(map[string]string),
		classes: make(map[string]bool), returns: make(map[string]string),
		baseNames: make(map[string][]string), params: make(map[string]bool),
	}
	w.prewalk(root, "")
	for i := uint32(0); i < root.NamedChildCount(); i++ {
		w.node(root.NamedChild(i), "", true, false, nil)
	}
	if w.mainGuard {
		w.mainEntrypoint()
	}
	return res
}

type walker struct {
	src       []byte
	res       *Result
	seen      map[string]bool
	types     map[string]string   // "enclosing:name" → type name
	classes   map[string]bool     // class qnames defined in this file
	returns   map[string]string   // def qname → return annotation type, same file
	baseNames map[string][]string // class qname → plain identifier bases, same file
	params    map[string]bool     // "enclosing:name" → declared parameter
	mainGuard bool
}

// maxAttrInheritanceDepth bounds the same-file walk that types `self.attr` from
// a base class's class attribute. It mirrors the resolver's MRO cap.
const maxAttrInheritanceDepth = 8

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

	case "if_statement":
		if isMainGuard(n, w.src) {
			w.mainGuard = true
		}
		w.children(n, enclosing, inClass)
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

// prewalk records the file-local facts the single-pass walk needs before it
// reaches the references that use them: class qnames, return annotations of
// local factories, and the plain identifier bases of every class. A call can
// appear textually before the class or def it names, so collecting these during
// the main walk would miss them.
func (w *walker) prewalk(n sitter.Node, enclosing string) {
	if n.IsNull() {
		return
	}
	switch n.Type() {
	case "decorated_definition":
		w.prewalk(n.ChildByFieldName("definition"), enclosing)
		return
	case "class_definition":
		name := nameOf(n, w.src)
		if name == "" {
			return
		}
		qname := join(enclosing, name)
		w.classes[qname] = true
		w.baseNames[qname] = plainBaseNames(n, w.src)
		body := n.ChildByFieldName("body")
		w.prewalkClassAttrs(body, qname)
		w.prewalk(body, qname)
		return
	case "function_definition":
		name := nameOf(n, w.src)
		if name == "" {
			return
		}
		qname := join(enclosing, name)
		if rt := n.ChildByFieldName("return_type"); !rt.IsNull() {
			if typ := typeName(rt, w.src); typ != "" {
				w.returns[qname] = typ
			}
		}
		w.prewalk(n.ChildByFieldName("body"), qname)
		return
	}
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		w.prewalk(n.NamedChild(i), enclosing)
	}
}

// baseNames returns the plain identifier superclasses of a class. Keyword
// arguments (metaclass=...) are not bases and a subscripted or dotted base
// (Generic[T], pkg.Base) cannot be mapped to a same-file class.
func plainBaseNames(class sitter.Node, src []byte) []string {
	superclasses := class.ChildByFieldName("superclasses")
	if superclasses.IsNull() {
		return nil
	}
	var out []string
	for i := uint32(0); i < superclasses.NamedChildCount(); i++ {
		child := superclasses.NamedChild(i)
		if child.Type() == "identifier" {
			out = append(out, child.Content(src))
		}
	}
	return out
}

// prewalkClassAttrs records the annotations of class-body assignments before
// any method body is walked, so a method that uses `self.cache` is typed even
// when the annotation is written after it.
func (w *walker) prewalkClassAttrs(body sitter.Node, classQName string) {
	if body.IsNull() {
		return
	}
	for i := uint32(0); i < body.NamedChildCount(); i++ {
		statement := body.NamedChild(i)
		if statement.Type() == "expression_statement" && statement.NamedChildCount() == 1 {
			statement = statement.NamedChild(0)
		}
		if statement.Type() != "assignment" {
			continue
		}
		left := statement.ChildByFieldName("left")
		annotation := statement.ChildByFieldName("type")
		if left.IsNull() || left.Type() != "identifier" || annotation.IsNull() {
			continue
		}
		if typ := typeName(annotation, w.src); typ != "" {
			w.types[scopeKey(classQName, "attr."+left.Content(w.src))] = typ
		}
	}
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
	// self/cls is not part of a bound method's call arity, but a staticmethod
	// has no binding parameter: its first argument is a real one.
	arity := w.parameters(params, qname, inClass && !hasDecorator(decorators, "staticmethod", w.src))

	kind := "function"
	if inClass {
		kind = "method"
	}
	w.add(kind, name, qname, arity, n, exported)
	w.decoratorRefs(decorators, qname)
	w.decoratorEntries(decorators, qname)
	if strings.HasPrefix(name, "test_") {
		w.res.Entrypoints = append(w.res.Entrypoints, Entrypoint{
			QName: qname, Name: name, Rule: "py.test", Confidence: 0.90,
			Line: int(n.StartPoint().Row) + 1,
		})
	}
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

// decoratorEntries marks FastAPI/Flask route handlers. The decorator's first
// string argument is kept as the display name; the route itself is not a node.
func (w *walker) decoratorEntries(decorators []sitter.Node, enclosing string) {
	for _, decorator := range decorators {
		if decorator.IsNull() || decorator.NamedChildCount() == 0 {
			continue
		}
		expr := decorator.NamedChild(0)
		args := sitter.Node{}
		if expr.Type() == "call" {
			args = expr.ChildByFieldName("arguments")
			expr = expr.ChildByFieldName("function")
		}

		name := ""
		receiver := ""
		switch expr.Type() {
		case "identifier":
			name = expr.Content(w.src)
		case "attribute":
			object := expr.ChildByFieldName("object")
			attr := expr.ChildByFieldName("attribute")
			if object.IsNull() || attr.IsNull() || object.Type() != "identifier" {
				continue
			}
			name = attr.Content(w.src)
			receiver = object.Content(w.src)
		default:
			continue
		}

		rule := ""
		switch name {
		case "route":
			rule = "py.route.flask"
		case "get", "post", "put", "patch", "delete", "websocket":
			rule = "py.route.fastapi"
		default:
			continue
		}
		switch receiver {
		case "app", "router", "bp", "blueprint", "api":
		default:
			continue
		}

		w.res.Entrypoints = append(w.res.Entrypoints, Entrypoint{
			QName: enclosing, Name: firstString(args, w.src), Rule: rule,
			Confidence: 0.90, Line: int(decorator.StartPoint().Row) + 1,
		})
	}
}

func firstString(n sitter.Node, src []byte) string {
	if n.IsNull() {
		return "route"
	}
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		child := n.NamedChild(i)
		if child.Type() == "string" {
			return strings.Trim(child.Content(src), `"'`)
		}
	}
	return "route"
}

func isMainGuard(n sitter.Node, src []byte) bool {
	condition := n.ChildByFieldName("condition")
	if condition.IsNull() {
		return false
	}
	text := condition.Content(src)
	return strings.Contains(text, "__name__") && strings.Contains(text, "__main__")
}

func (w *walker) mainEntrypoint() {
	for _, symbol := range w.res.Symbols {
		if symbol.Name != "main" || strings.Contains(symbol.QName, ".") {
			continue
		}
		w.res.Entrypoints = append(w.res.Entrypoints, Entrypoint{
			QName: symbol.QName, Name: "__main__", Rule: "py.main",
			Confidence: 0.90, Line: symbol.StartLine,
		})
		return
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
				ref.ReceiverType = w.attrType(classOf(enclosing), field, make(map[string]bool), 0)
			}
		case "call":
			// super().m() is bounded to the zero-argument form. An explicit
			// super(Child, self) names its starting class, which the reference
			// shape does not carry, so it stays unresolved rather than guessed.
			if !isZeroArgCall(object, "super", w.src) {
				return
			}
			ref.Form = "super"
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
		typ = w.returnType(right, enclosing)
	}
	if typ == "" {
		return
	}

	switch left.Type() {
	case "identifier":
		name := left.Content(w.src)
		w.types[scopeKey(enclosing, name)] = typ
		if w.classes[enclosing] {
			// A class-body binding is also an attribute: `cache: Cache` or
			// `cache = Cache()` types `self.cache` in every method.
			w.types[scopeKey(enclosing, "attr."+name)] = typ
		}
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
func (w *walker) parameters(params sitter.Node, enclosing string, skipFirst bool) int {
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
		w.params[scopeKey(enclosing, name)] = true

		if skipFirst && count == 0 && (name == "self" || name == "cls") {
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
				Local: firstSegment(name), Imported: "*", Source: name, Line: line, Public: true,
			})
		case "aliased_import":
			name := item.ChildByFieldName("name")
			alias := item.ChildByFieldName("alias")
			if name.IsNull() || alias.IsNull() {
				continue
			}
			w.res.Imports = append(w.res.Imports, Import{
				Local: alias.Content(w.src), Imported: "*", Source: name.Content(w.src),
				Line: line, Public: true,
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
				Local: firstSegment(name), Imported: name, Source: source, Line: line, Public: true,
			})
		case "aliased_import":
			name := item.ChildByFieldName("name")
			alias := item.ChildByFieldName("alias")
			if name.IsNull() || alias.IsNull() {
				continue
			}
			w.res.Imports = append(w.res.Imports, Import{
				Local: alias.Content(w.src), Imported: name.Content(w.src),
				Source: source, Line: line, Public: true,
			})
		case "wildcard_import":
			w.res.Imports = append(w.res.Imports, Import{
				Local: "*", Imported: "*", Source: source, Line: line, Public: true,
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

// typeName reduces a type expression to the one receiver type it names.
// A subscripted type keeps its outer name (`list[Service]` is a list, and list
// is a builtin, not a local class), while Optional/Union and PEP 604 `X | None`
// unwrap to their single non-None member. Two distinct local members stay
// untyped: picking one would be a guess.
func typeName(node sitter.Node, src []byte) string {
	switch node.Type() {
	case "type":
		if node.NamedChildCount() == 0 {
			return ""
		}
		return typeName(node.NamedChild(0), src)
	case "identifier":
		return node.Content(src)
	case "string":
		// A forward reference: "Service", 'pkg.Service', or "Service | None".
		// The union is split textually because the quoted form is not a type
		// expression the grammar can hand back.
		text := strings.Trim(node.Content(src), `"'`)
		if strings.Contains(text, "|") {
			var members []string
			for _, part := range strings.Split(text, "|") {
				members = append(members, scalarName(strings.TrimSpace(part)))
			}
			return singleMember(members)
		}
		return scalarName(text)
	case "generic_type", "subscript":
		value := node.ChildByFieldName("value")
		if value.IsNull() && node.NamedChildCount() > 0 {
			value = node.NamedChild(0)
		}
		if value.IsNull() {
			return ""
		}
		name := typeName(value, src)
		switch name {
		case "Optional", "Union":
			return singleUnionMember(node, src)
		}
		return name
	case "binary_operator":
		return singleBinaryUnionMember(node, src)
	case "attribute":
		if attr := node.ChildByFieldName("attribute"); !attr.IsNull() {
			return attr.Content(src)
		}
	}
	return ""
}

// typeArguments collects the type expressions nested in a subscripted type.
func typeArguments(n sitter.Node, src []byte, out *[]string) {
	for i := uint32(0); i < n.NamedChildCount(); i++ {
		child := n.NamedChild(i)
		switch child.Type() {
		case "type":
			if typ := typeName(child, src); typ != "" {
				*out = append(*out, typ)
			}
		case "type_parameter", "generic_type":
			typeArguments(child, src, out)
		}
	}
}

// singleUnionMember returns the one type an Optional/Union argument list names,
// ignoring None/NoneType. Zero or several distinct members stay untyped.
func singleUnionMember(n sitter.Node, src []byte) string {
	var members []string
	typeArguments(n, src, &members)
	return singleMember(members)
}

// singleBinaryUnionMember handles PEP 604 unions (`Service | None`).
func singleBinaryUnionMember(n sitter.Node, src []byte) string {
	var members []string
	var walk func(sitter.Node)
	walk = func(node sitter.Node) {
		switch node.Type() {
		case "binary_operator":
			for i := uint32(0); i < node.NamedChildCount(); i++ {
				walk(node.NamedChild(i))
			}
		case "none":
			return
		default:
			if typ := typeName(node, src); typ != "" {
				members = append(members, typ)
			}
		}
	}
	walk(n)
	return singleMember(members)
}

// scalarName keeps the last dotted segment of a type name.
func scalarName(text string) string {
	if i := strings.LastIndex(text, "."); i >= 0 {
		text = text[i+1:]
	}
	return text
}

func singleMember(members []string) string {
	only := ""
	for _, member := range members {
		if member == "None" || member == "NoneType" {
			continue
		}
		if only != "" && only != member {
			return ""
		}
		only = member
	}
	return only
}

// returnType infers the type a local factory call produces: `x = make()` where
// `def make() -> Service`. Only functions defined in this file with a return
// annotation are consulted, so a lowercase call into another module is never
// guessed.
func (w *walker) returnType(call sitter.Node, enclosing string) string {
	if call.IsNull() || call.Type() != "call" {
		return ""
	}
	fn := call.ChildByFieldName("function")
	switch fn.Type() {
	case "identifier":
		name := fn.Content(w.src)
		for scope := enclosing; ; scope = scopeOf(scope) {
			if w.params[scopeKey(scope, name)] {
				// A parameter (or a local binding named like the def) shadows
				// the factory; the call is not the local def.
				return ""
			}
			if typ, ok := w.returns[join(scope, name)]; ok {
				return typ
			}
			if scope == "" {
				return ""
			}
		}
	case "attribute":
		object := fn.ChildByFieldName("object")
		attr := fn.ChildByFieldName("attribute")
		if object.IsNull() || attr.IsNull() || object.Type() != "identifier" {
			return ""
		}
		name := attr.Content(w.src)
		switch object.Content(w.src) {
		case "self", "cls":
			// self.create() / cls.create(): the factory lives on the class.
			for scope := enclosing; scope != ""; scope = scopeOf(scope) {
				if w.classes[scope] {
					return w.returns[join(scope, name)]
				}
			}
			return ""
		default:
			// Class.create(): the callee names the class explicitly.
			return w.returns[join(object.Content(w.src), name)]
		}
	}
	return ""
}

// attrType looks up the type recorded for a class attribute, following the
// plain identifier bases declared in this file. A base from another module is
// not followed: its annotation lives in a file this extractor never sees.
func (w *walker) attrType(classQName, field string, seen map[string]bool, depth int) string {
	if classQName == "" || depth > maxAttrInheritanceDepth || seen[classQName] {
		return ""
	}
	seen[classQName] = true
	if typ, ok := w.types[scopeKey(classQName, "attr."+field)]; ok {
		return typ
	}
	parent := scopeOf(classQName)
	for _, base := range w.baseNames[classQName] {
		baseQName := ""
		if candidate := join(parent, base); w.classes[candidate] {
			baseQName = candidate
		} else if w.classes[base] {
			baseQName = base
		}
		if baseQName == "" {
			continue
		}
		if typ := w.attrType(baseQName, field, seen, depth+1); typ != "" {
			return typ
		}
	}
	return ""
}

func isZeroArgCall(n sitter.Node, name string, src []byte) bool {
	if n.IsNull() || n.Type() != "call" {
		return false
	}
	fn := n.ChildByFieldName("function")
	if fn.IsNull() || fn.Type() != "identifier" || fn.Content(src) != name {
		return false
	}
	return positionalArgs(n.ChildByFieldName("arguments")) == 0
}

// hasDecorator reports whether one of the decorators is the bare name given.
func hasDecorator(decorators []sitter.Node, name string, src []byte) bool {
	for _, decorator := range decorators {
		if decorator.IsNull() || decorator.NamedChildCount() == 0 {
			continue
		}
		expr := decorator.NamedChild(0)
		if expr.Type() == "call" {
			expr = expr.ChildByFieldName("function")
		}
		if expr.Type() == "identifier" && expr.Content(src) == name {
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

// scopeOf is the enclosing scope of a qname; unlike classOf it is empty for a
// module-level symbol instead of repeating the name.
func scopeOf(qname string) string {
	if i := strings.LastIndex(qname, "."); i >= 0 {
		return qname[:i]
	}
	return ""
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
