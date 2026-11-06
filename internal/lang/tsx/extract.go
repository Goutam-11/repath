// Package tsx extracts symbol definitions from TypeScript and JavaScript
// sources with a tree-sitter grammar.
package tsx

import (
	"context"
	"fmt"
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

// Extract returns the definitions in src. ok is false when the file has syntax
// errors; the message names the first offending line. Definitions found before
// the error are still returned, because a broken file still contains symbols.
func (e *Extractor) Extract(src []byte, dialect Dialect) (symbols []Symbol, ok bool, errMsg string) {
	parser, found := e.parsers[dialect]
	if !found {
		return nil, false, "unsupported dialect"
	}

	tree, err := parser.ParseString(context.Background(), nil, src)
	if err != nil {
		return nil, false, err.Error()
	}
	defer tree.Close()

	root := tree.RootNode()
	ok = !root.HasError()
	if !ok {
		if line := errorLine(root); line > 0 {
			errMsg = fmt.Sprintf("syntax error near line %d", line)
		} else {
			errMsg = "syntax error"
		}
	}

	seen := make(map[string]bool)
	for i := uint32(0); i < root.NamedChildCount(); i++ {
		collect(root.NamedChild(i), src, "", false, seen, &symbols)
	}
	return symbols, ok, errMsg
}

// collect walks one declaration. prefix is the scope path ("Cart.") and exported
// is true when the declaration is re-exported from the module.
func collect(n sitter.Node, src []byte, prefix string, exported bool, seen map[string]bool, out *[]Symbol) {
	if n.IsNull() {
		return
	}

	switch n.Type() {
	case "export_statement":
		if decl := n.ChildByFieldName("declaration"); !decl.IsNull() {
			collect(decl, src, prefix, true, seen, out)
		}

	case "ambient_declaration":
		if inner := n.NamedChild(0); !inner.IsNull() {
			collect(inner, src, prefix, exported, seen, out)
		}

	case "function_declaration", "generator_function_declaration":
		name := nameOf(n, src)
		add(out, seen, "function", name, prefix+name, paramCount(n), n, exported)

	case "class_declaration", "abstract_class_declaration":
		name := nameOf(n, src)
		if name == "" {
			return
		}
		add(out, seen, "class", name, prefix+name, 0, n, exported)

		body := n.ChildByFieldName("body")
		if body.IsNull() {
			return
		}
		for i := uint32(0); i < body.NamedChildCount(); i++ {
			method := body.NamedChild(i)
			if method.Type() != "method_definition" {
				continue
			}
			methodName := nameOf(method, src)
			add(out, seen, "method", methodName, prefix+name+"."+methodName, paramCount(method), method, exported)
		}

	case "interface_declaration":
		name := nameOf(n, src)
		add(out, seen, "interface", name, prefix+name, 0, n, exported)

	case "type_alias_declaration":
		name := nameOf(n, src)
		add(out, seen, "type", name, prefix+name, 0, n, exported)

	case "enum_declaration":
		name := nameOf(n, src)
		add(out, seen, "enum", name, prefix+name, 0, n, exported)

	case "lexical_declaration", "variable_declaration":
		for i := uint32(0); i < n.NamedChildCount(); i++ {
			decl := n.NamedChild(i)
			if decl.Type() != "variable_declarator" {
				continue
			}
			value := decl.ChildByFieldName("value")
			if value.IsNull() {
				continue
			}
			switch value.Type() {
			case "arrow_function", "function_expression", "function":
				name := nameOf(decl, src)
				add(out, seen, "function", name, prefix+name, paramCount(value), decl, exported)
			}
		}

	case "internal_module":
		name := nameOf(n, src)
		body := n.ChildByFieldName("body")
		if name == "" || body.IsNull() {
			return
		}
		for i := uint32(0); i < body.NamedChildCount(); i++ {
			collect(body.NamedChild(i), src, prefix+name+".", exported, seen, out)
		}
	}
}

// add records a symbol once per (qname, arity). Overloads with the same arity
// collapse because the schema identifies a symbol by (lang, qname, file, arity).
func add(out *[]Symbol, seen map[string]bool, kind, name, qname string, arity int, n sitter.Node, exported bool) {
	if name == "" || qname == "" {
		return
	}
	key := qname + "/" + fmt.Sprint(arity)
	if seen[key] {
		return
	}
	seen[key] = true

	*out = append(*out, Symbol{
		Kind:      kind,
		Name:      name,
		QName:     qname,
		Arity:     arity,
		StartLine: int(n.StartPoint().Row) + 1,
		EndLine:   int(n.EndPoint().Row) + 1,
		Exported:  exported,
	})
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
