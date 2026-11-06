// Package lang maps file paths to the dialect needed to parse them.
package lang

import (
	"path/filepath"
	"strings"

	"github.com/Goutam-11/repath/internal/lang/tsx"
)

// For reports the dialect and language name for path. ok is false for files
// repath does not parse yet, and for declaration-only or minified output that
// would only add noise to the graph.
func For(path string) (dialect tsx.Dialect, name string, ok bool) {
	lower := strings.ToLower(filepath.ToSlash(path))

	for _, suffix := range []string{".d.ts", ".min.js", ".min.mjs", ".min.cjs", ".min.jsx"} {
		if strings.HasSuffix(lower, suffix) {
			return 0, "", false
		}
	}

	switch strings.ToLower(filepath.Ext(lower)) {
	case ".ts", ".mts", ".cts":
		return tsx.TypeScript, "typescript", true
	case ".tsx":
		return tsx.TSX, "typescript", true
	case ".js", ".mjs", ".cjs":
		return tsx.JavaScript, "javascript", true
	case ".jsx":
		return tsx.TSX, "javascript", true
	}

	return 0, "", false
}
