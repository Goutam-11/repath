// Package facts defines the language-neutral output of a source extractor:
// symbol definitions, references, and import bindings.
//
// Extraction is file-local and never touches the database; the indexer writes
// these facts and the resolver turns references into edges.
package facts

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

// Ref is one use of a name: a call, a constructor, a heritage clause, or a
// decorator application.
type Ref struct {
	Kind     string // call | new | extends | implements | decorator
	Name     string
	Form     string // ident | this | member
	Receiver string // identifier (or dotted path) before the dot, when Form is member
	// ReceiverType is the locally inferred type of Receiver ("Cart"), from a
	// variable annotation, a constructor, or a parameter annotation.
	ReceiverType string
	Arity        int
	Line         int
	Enclosing    string // qname of the symbol the reference sits inside
}

// Import is one local binding created by an import statement.
type Import struct {
	Local    string
	Imported string // a name, "default", or "*"
	Source   string
	Line     int
	// Public marks a binding that may be re-exported by its module: Python
	// imports are always eligible, Rust only with `pub use`.
	Public bool
}

// Entrypoint anchors one symbol as an execution entry. It is a dimension over
// symbols, never a new node; Name carries the route or test convention for
// display and debugging.
type Entrypoint struct {
	QName      string
	Name       string
	Rule       string
	Confidence float64
	Line       int
}

// Result is everything one file contributes to the graph.
type Result struct {
	Symbols     []Symbol
	Refs        []Ref
	Imports     []Import
	Entrypoints []Entrypoint
	OK          bool
	Err         string
}
