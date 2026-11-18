// Package facts defines the language-neutral output of a source extractor:
// symbol definitions, references, and import bindings.
//
// Extraction is file-local and never touches the database; the indexer writes
// these facts and the resolver turns references into edges.
package facts

import "strings"

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
	// ReceiverBound carries the trait bounds of a Rust type-parameter receiver
	// ("Describe", "crate::traits::Describe"), joined by "|" for several bounds.
	// It is empty for a concrete receiver type. Resolution follows a bound only
	// to a trait definition indexed in the same repo; there is no inference.
	ReceiverBound string
	// ReceiverFrom records how ReceiverType was inferred. "return" means a
	// same-file function return type rather than an annotation or constructor.
	ReceiverFrom string
	// ReceiverParam marks ReceiverType as a generic type parameter rather than a
	// concrete type, so resolution never mistakes it for a repo-local symbol.
	ReceiverParam bool
	Arity         int
	Line          int
	Enclosing     string // qname of the symbol the reference sits inside
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

// Anchor is a cross-language fact anchored to the enclosing symbol: an HTTP
// route on the server, an HTTP call site on the client, and later other
// protocols. Anchors are inputs for the cross-language pass; they are never
// nodes or edges on their own.
type Anchor struct {
	QName      string // enclosing symbol
	Protocol   string // "http"
	Role       string // "client" | "server"
	Method     string // upper-case HTTP method, or "ALL" for a wildcard route
	Path       string // normalized literal path
	Line       int
	Confidence float64
}

// Result is everything one file contributes to the graph.
type Result struct {
	Symbols     []Symbol
	Refs        []Ref
	Imports     []Import
	Entrypoints []Entrypoint
	Anchors     []Anchor
	OK          bool
	Err         string
}

// NormalizeHTTPPath accepts a literal same-origin path ("/api/users?x=1") and
// returns the normalized route path. Full URLs, relative paths, and dynamic
// templates return false: the cross-language pass matches exact literals only.
func NormalizeHTTPPath(raw string) (string, bool) {
	path := strings.TrimSpace(raw)
	if !strings.HasPrefix(path, "/") {
		return "", false
	}
	if i := strings.IndexAny(path, "?#"); i >= 0 {
		path = path[:i]
	}
	for strings.Contains(path, "//") {
		path = strings.ReplaceAll(path, "//", "/")
	}
	if path == "" {
		path = "/"
	}
	return path, true
}

// NormalizeHTTPMethod upper-cases a known HTTP method; "ALL" is the wildcard a
// framework registration such as Express `app.all` produces.
func NormalizeHTTPMethod(method string) (string, bool) {
	switch strings.ToUpper(strings.TrimSpace(method)) {
	case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS":
		return strings.ToUpper(strings.TrimSpace(method)), true
	case "ALL":
		return "ALL", true
	}
	return "", false
}
