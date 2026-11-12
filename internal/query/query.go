// Package query is the shared answer layer. The CLI renderers and the MCP
// adapter both call it, so neither can drift into its own resolution rules or
// graph defaults.
package query

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Goutam-11/repath/internal/index"
	"github.com/Goutam-11/repath/internal/store"
)

// Defaults are the contract defaults from PLAN.md §6/§7. They live here so the
// CLI and MCP cannot disagree about what "no argument" means.
const (
	DefaultMinConfidence = 0.5
	DefaultDepth         = 8
	DefaultLimit         = 50
	DefaultMaxTokens     = 2000
	DefaultCallersLimit  = 200
)

// NormalizeDepth applies the default depth.
func NormalizeDepth(depth int) int {
	if depth <= 0 {
		return DefaultDepth
	}
	return depth
}

// NormalizeConfidence clamps a caller-supplied threshold into (0,1].
func NormalizeConfidence(c float64) float64 {
	if c <= 0 || c > 1 {
		return DefaultMinConfidence
	}
	return c
}

// Session is one open index and the repository root it belongs to.
type Session struct {
	Root  string
	Store *store.Store
}

// ResolveRoot prefers the explicit flag, then the git work-tree root, then the
// current directory.
func ResolveRoot(flag string) (string, error) {
	if flag != "" {
		return filepath.Abs(flag)
	}

	if out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output(); err == nil {
		if root := strings.TrimSpace(string(out)); root != "" {
			return root, nil
		}
	}

	return os.Getwd()
}

// Open opens the index at root/.repath/index.db and migrates it. Unless noBuild
// is set it also performs the lazy auto-index that makes first use work without
// a separate command (PLAN.md decision 17).
func Open(ctx context.Context, root string, noBuild bool) (*Session, error) {
	st, err := index.Prepare(ctx, root, noBuild)
	if err != nil {
		return nil, err
	}
	return &Session{Root: root, Store: st}, nil
}

// Close closes the underlying index.
func (s *Session) Close() error { return s.Store.Close() }

// Target is what a reference resolves to: one or more symbols, or the
// candidate list that made it ambiguous.
type Target struct {
	Label      string
	Nodes      []store.NodeRow
	Candidates []store.NodeRow
}

// ResolveTarget accepts a symbol, file path, or file:line reference and returns
// the node(s) it names. The int is an exit code from the CLI contract: 0 ok,
// 3 not found, 4 ambiguous.
func ResolveTarget(ctx context.Context, st *store.Store, arg string) (Target, int, error) {
	if path, line, ok := splitFileLine(arg); ok {
		fileID, found, err := st.FileByPath(ctx, path)
		if err != nil {
			return Target{Label: arg}, 1, err
		}
		if found {
			nodeID, ok, err := st.EnclosingNode(ctx, fileID, line)
			if err != nil {
				return Target{Label: arg}, 1, err
			}
			if ok {
				node, err := st.NodeByID(ctx, nodeID)
				if err != nil {
					return Target{Label: arg}, 1, err
				}
				return Target{Label: arg, Nodes: []store.NodeRow{node}}, 0, nil
			}
		}
		return Target{Label: arg}, 3, nil
	}

	if LooksLikeFile(arg) {
		_, found, err := st.FileByPath(ctx, arg)
		if err != nil {
			return Target{Label: arg}, 1, err
		}
		if found {
			nodes, err := st.NodesByFile(ctx, arg)
			if err != nil {
				return Target{Label: arg}, 1, err
			}
			if len(nodes) > 0 {
				return Target{Label: arg, Nodes: nodes}, 0, nil
			}
		}
		return Target{Label: arg}, 3, nil
	}

	return resolveSymbol(ctx, st, arg, "")
}

// ResolveSymbol resolves a name to exactly one symbol. A module narrows the
// candidates to that module's name or root, matching the MCP find_symbol tool.
func ResolveSymbol(ctx context.Context, st *store.Store, name, module string) (Target, int, error) {
	return resolveSymbol(ctx, st, name, module)
}

func resolveSymbol(ctx context.Context, st *store.Store, name, module string) (Target, int, error) {
	matches, err := st.SearchSymbols(ctx, name, 50)
	if err != nil {
		return Target{Label: name}, 1, err
	}
	if module != "" {
		matches, err = filterModule(ctx, st, matches, module)
		if err != nil {
			return Target{Label: name}, 1, err
		}
	}
	if len(matches) == 0 {
		return Target{Label: name}, 3, nil
	}
	chosen, ambiguous := chooseSymbol(matches, name)
	if ambiguous {
		return Target{Label: name, Candidates: matches}, 4, nil
	}
	return Target{Label: name, Nodes: []store.NodeRow{chosen}}, 0, nil
}

// filterModule keeps matches belonging to a module named by name or root.
func filterModule(ctx context.Context, st *store.Store, matches []store.NodeRow, module string) ([]store.NodeRow, error) {
	modules, err := st.Modules(ctx)
	if err != nil {
		return nil, err
	}
	wanted := make(map[int64]bool)
	for _, m := range modules {
		if m.Name == module || m.Root == module {
			wanted[m.ID] = true
		}
	}
	if len(wanted) == 0 {
		return nil, nil
	}
	fileModules, err := st.FileModules(ctx)
	if err != nil {
		return nil, err
	}

	out := matches[:0]
	for _, n := range matches {
		if wanted[fileModules[n.FileID]] {
			out = append(out, n)
		}
	}
	return out, nil
}

// LooksLikeFile reports whether a reference is a path rather than a symbol.
func LooksLikeFile(arg string) bool {
	if strings.Contains(arg, "/") {
		return true
	}
	for _, ext := range []string{".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs", ".py", ".rs"} {
		if strings.HasSuffix(arg, ext) {
			return true
		}
	}
	return false
}

func splitFileLine(arg string) (string, int, bool) {
	index := strings.LastIndex(arg, ":")
	if index <= 0 || index == len(arg)-1 {
		return "", 0, false
	}
	line, err := strconv.Atoi(arg[index+1:])
	if err != nil {
		return "", 0, false
	}
	path := arg[:index]
	if !LooksLikeFile(path) {
		return "", 0, false
	}
	return path, line, true
}

// chooseSymbol prefers an exact qname, then a single name match.
func chooseSymbol(matches []store.NodeRow, name string) (store.NodeRow, bool) {
	var exact []store.NodeRow
	for _, m := range matches {
		if m.QName == name {
			exact = append(exact, m)
		}
	}
	switch len(exact) {
	case 1:
		return exact[0], false
	case 0:
		if len(matches) == 1 {
			return matches[0], false
		}
		return store.NodeRow{}, true
	default:
		return store.NodeRow{}, true
	}
}

// SymbolRef is a symbol reduced to the fields every report shows.
type SymbolRef struct {
	QName    string `json:"qname"`
	Kind     string `json:"kind"`
	File     string `json:"file"`
	Line     int    `json:"line"`
	Exported bool   `json:"exported"`
}

// SymbolRefOf converts a node and a file-path map into a report symbol.
func SymbolRefOf(n store.NodeRow, paths map[int64]string) SymbolRef {
	return SymbolRef{
		QName:    n.QName,
		Kind:     n.Kind,
		File:     paths[n.FileID],
		Line:     n.StartLine,
		Exported: n.Exported,
	}
}
