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

// Symbol is one definition found in a file. QName is scoped to enclosing
// declarations ("Cart.add"), never to the file; the file is a separate column.
type Symbol struct {
	Kind      string
	Name      string
	QName     string
	Arity     int
	StartLine int
	EndLine   int
	Exported  bool
}

// Ref is one use of a name: a call, a constructor, or a heritage clause.
type Ref struct {
	Kind      string // call | new | extends | implements
	Name      string
	Form      string // ident | this | member
	Receiver  string // identifier before the dot, when Form is member
	Arity     int
	Line      int
	Enclosing string // qname of the symbol the reference sits inside
}

// Import is one local binding created by an import statement.
type Import struct {
	Local    string
	Imported string // a name, "default", or "*"
	Source   string
	Line     int
}

// Result is everything one file contributes to the graph.
type Result struct {
	Symbols []Symbol
	Refs    []Ref
	Imports []Import
	OK      bool
	Err     string
}

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

	w := &walker{src: src, res: &res, seen: make(map[string]bool)}
	for i := uint32(0); i < root.NamedChildCount(); i++ {
		w.node(root.NamedChild(i), "", false)
	}
	return res
}

type walker struct {
	src  []byte
	res  *Result
	seen map[string]bool
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
		return

	case "function_declaration", "generator_function_declaration":
		name := nameOf(n, w.src)
		qname := join(enclosing, name)
		w.add("function", name, qname, paramCount(n), n, exported)
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
		switch value.Type() {
		case "arrow_function", "function_expression", "function":
			qname := join(enclosing, name)
			w.add("function", name, qname, paramCount(value), decl, exported)
			w.children(value.ChildByFieldName("parameters"), qname)
			w.children(value.ChildByFieldName("body"), qname)
		default:
			// A plain assignment can still hold references ("const x = helper()").
			w.node(value, enclosing, false)
		}
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
		}
		w.res.Refs = append(w.res.Refs, ref)
	}
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

func (w *walker) importStatement(n sitter.Node) {
	source := n.ChildByFieldName("source")
	line := int(n.StartPoint().Row) + 1
	spec := unquote(source.Content(w.src))

	for i := uint32(0); i < n.NamedChildCount(); i++ {
		clause := n.NamedChild(i)
		if clause.Type() != "import_clause" {
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
