// Package lang maps file paths to the dialect needed to parse them and owns the
// extractor used by index workers.
package lang

import (
	"path/filepath"
	"strings"

	"github.com/Goutam-11/repath/internal/lang/facts"
	"github.com/Goutam-11/repath/internal/lang/golang"
	"github.com/Goutam-11/repath/internal/lang/py"
	"github.com/Goutam-11/repath/internal/lang/rs"
	"github.com/Goutam-11/repath/internal/lang/tsx"
)

// Dialect is the grammar a file needs. It is intentionally separate from any
// grammar package's dialect so index does not depend on a language frontend.
type Dialect int

const (
	// TypeScript parses .ts, .mts and .cts files.
	TypeScript Dialect = iota
	// TSX parses .tsx and .jsx files.
	TSX
	// JavaScript parses .js, .mjs and .cjs files.
	JavaScript
	// Python parses .py files.
	Python
	// Rust parses .rs files.
	Rust
	// Go parses .go files.
	Go
)

// For reports the dialect and language name for path. ok is false for files
// repath does not parse yet, and for declaration-only or minified output that
// would only add noise to the graph.
func For(path string) (dialect Dialect, name string, ok bool) {
	lower := strings.ToLower(filepath.ToSlash(path))

	for _, suffix := range []string{".d.ts", ".min.js", ".min.mjs", ".min.cjs", ".min.jsx"} {
		if strings.HasSuffix(lower, suffix) {
			return 0, "", false
		}
	}

	switch strings.ToLower(filepath.Ext(lower)) {
	case ".ts", ".mts", ".cts":
		return TypeScript, "typescript", true
	case ".tsx":
		return TSX, "typescript", true
	case ".js", ".mjs", ".cjs":
		return JavaScript, "javascript", true
	case ".jsx":
		return TSX, "javascript", true
	case ".py":
		return Python, "python", true
	case ".rs":
		return Rust, "rust", true
	case ".go":
		return Go, "go", true
	}

	return 0, "", false
}

// Extractor parses one file with the grammar its dialect names. It is not safe
// for concurrent use; index workers each create their own.
type Extractor struct {
	tsx *tsx.Extractor
	py  *py.Extractor
	rs  *rs.Extractor
	go_ *golang.Extractor
}

// NewExtractor builds an extractor for every registered language.
func NewExtractor() *Extractor {
	return &Extractor{tsx: tsx.New(), py: py.New(), rs: rs.New(), go_: golang.New()}
}

// Extract parses src and returns its definitions, references, and imports.
func (e *Extractor) Extract(src []byte, dialect Dialect) facts.Result {
	switch dialect {
	case TypeScript:
		return e.tsx.Extract(src, tsx.TypeScript)
	case TSX:
		return e.tsx.Extract(src, tsx.TSX)
	case JavaScript:
		return e.tsx.Extract(src, tsx.JavaScript)
	case Python:
		return e.py.Extract(src, py.Python)
	case Rust:
		return e.rs.Extract(src, rs.Rust)
	case Go:
		return e.go_.Extract(src, golang.Go)
	}
	return facts.Result{Err: "unsupported dialect"}
}
