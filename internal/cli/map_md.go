package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/Goutam-11/repath/internal/graph"
)

// mapBundleDir is where `map --md` writes when --out is omitted. It lives under
// .repath/ so a plain run never surprises a git status; pass --out for a
// committable bundle.
const mapBundleDir = ".repath/map"

// mapIndexTopSymbols caps the global top-symbols table in index.md. Module files
// get the rest of the budget, so a large repository still lists every module.
const mapIndexTopSymbols = 20

// mapModuleFileRows caps the per-module file table; the "+N more files" line
// keeps a 3,000-file module from eating the whole budget.
const mapModuleFileRows = 10

const mapTrimNote = "\n… trimmed to the token budget; raise --max-tokens for more.\n"

// symbolTableHeader is shared by index.md and every module page.
const symbolTableHeader = "| rank | symbol | kind | file:line | callers |\n| --- | --- | --- | --- | --- |\n"

// writtenFile is one file a bundle wrote, with its estimated token cost.
type writtenFile struct {
	Path   string
	Tokens int
}

// tokenEstimate is the bundle's budget unit: roughly four bytes per token. It
// is an estimate, not a tokenizer — the point is that --max-tokens bounds the
// whole bundle deterministically.
func tokenEstimate(s string) int { return (len(s) + 3) / 4 }

// moduleDoc is one module's page while it is being built.
type moduleDoc struct {
	Name       string
	Slug       string
	Symbols    int
	Deps       string
	ImportedBy string
	Files      []graph.BundleFile
	Rows       []string
}

// writeMapBundle renders the module rollup as a directory of markdown files and
// trims symbol rows until the whole bundle fits maxTokens. It returns the files
// written and whether anything was trimmed; ordering is fully deterministic, so
// a second run over the same index is byte-identical.
func writeMapBundle(outDir string, bundle graph.MapBundle, maxTokens int, floor float64) ([]writtenFile, bool, error) {
	docs, order := buildModuleDocs(bundle)
	usedSlug := make(map[string]int, len(order))
	for _, name := range order {
		docs[name].Slug = moduleSlug(name, usedSlug)
	}

	// The index tables and every module page's headers are structural: they are
	// written in full before a single symbol row is charged. Charging an empty
	// page (which carries the table header and a placeholder row) makes the
	// budget conservative rather than optimistic.
	indexBase := renderIndexFixed(bundle, docs, order, maxTokens, floor) + symbolTableHeader
	fixed := tokenEstimate(indexBase)
	for _, name := range order {
		empty := *docs[name]
		empty.Rows = nil
		fixed += tokenEstimate(renderModuleDoc(&empty))
	}
	limit := maxTokens - tokenEstimate(mapTrimNote)
	if limit < 0 {
		limit = 0
	}

	// Fill symbol rows in global rank order. The index repeats the first few
	// rows, and that cost is charged too, so the bundle total stays honest.
	indexRows := make([]string, 0, mapIndexTopSymbols)
	trimmed := false
	for _, sym := range bundle.Symbols {
		doc, ok := docs[sym.Module]
		if !ok {
			continue
		}
		row := renderSymbolRow(sym)
		cost := tokenEstimate(row)
		extra := 0
		if len(indexRows) < mapIndexTopSymbols {
			extra = cost
		}
		if fixed+cost+extra > limit {
			trimmed = true
			break
		}
		doc.Rows = append(doc.Rows, row)
		fixed += cost
		if extra > 0 {
			indexRows = append(indexRows, row)
			fixed += extra
		}
	}

	index := renderIndexFixed(bundle, docs, order, maxTokens, floor) + renderSymbolTable(indexRows, trimmed)

	if err := os.MkdirAll(filepath.Join(outDir, "modules"), 0o755); err != nil {
		return nil, false, fmt.Errorf("create bundle directory: %w", err)
	}
	var written []writtenFile
	write := func(rel, content string) error {
		path := filepath.Join(outDir, rel)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
		written = append(written, writtenFile{Path: path, Tokens: tokenEstimate(content)})
		return nil
	}
	if err := write("index.md", index); err != nil {
		return nil, false, err
	}
	for _, name := range order {
		doc := docs[name]
		if err := write(filepath.Join("modules", doc.Slug+".md"), renderModuleDoc(doc)); err != nil {
			return nil, false, err
		}
	}
	return written, trimmed, nil
}

// buildModuleDocs groups files and reverse dependencies per module and returns
// the docs plus the deterministic (name-sorted) render order.
func buildModuleDocs(bundle graph.MapBundle) (map[string]*moduleDoc, []string) {
	docs := make(map[string]*moduleDoc, len(bundle.Modules)+1)
	order := make([]string, 0, len(bundle.Modules)+1)
	for _, m := range bundle.Modules {
		if _, ok := docs[m.Name]; ok {
			continue
		}
		docs[m.Name] = &moduleDoc{Name: m.Name, Symbols: m.Symbols, Deps: depsText(m.Deps)}
		order = append(order, m.Name)
	}
	// A symbol whose file has no module row (the module dimension is missing)
	// still gets a page instead of being dropped from the bundle.
	for _, sym := range bundle.Symbols {
		if _, ok := docs[sym.Module]; !ok {
			docs[sym.Module] = &moduleDoc{Name: sym.Module, Deps: "—"}
			order = append(order, sym.Module)
		}
	}
	sort.Strings(order)

	importedBy := make(map[string][]string, len(order))
	for _, m := range bundle.Modules {
		for _, dep := range m.Deps {
			importedBy[dep] = append(importedBy[dep], m.Name)
		}
	}
	for name, list := range importedBy {
		if doc, ok := docs[name]; ok {
			sort.Strings(list)
			doc.ImportedBy = strings.Join(list, ", ")
		}
	}

	for _, f := range bundle.Files {
		if doc, ok := docs[f.Module]; ok {
			doc.Files = append(doc.Files, f)
		}
	}
	for _, name := range order {
		doc := docs[name]
		sort.Slice(doc.Files, func(i, j int) bool {
			if doc.Files[i].Symbols != doc.Files[j].Symbols {
				return doc.Files[i].Symbols > doc.Files[j].Symbols
			}
			return doc.Files[i].Path < doc.Files[j].Path
		})
	}
	return docs, order
}

func depsText(deps []string) string {
	if len(deps) == 0 {
		return "—"
	}
	return strings.Join(deps, ", ")
}

// displayModule keeps an empty module name readable.
func displayModule(name string) string {
	if name == "" {
		return "(unassigned)"
	}
	return name
}

// moduleSlug turns a module name into a stable file name, suffixing collisions
// in name order so a second run maps the same module to the same file.
func moduleSlug(name string, used map[string]int) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '.' || r == '_' || r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	base := strings.Trim(b.String(), "-.")
	if base == "" {
		base = "module"
	}
	if n := used[base]; n > 0 {
		used[base] = n + 1
		return base + "-" + strconv.Itoa(n+1)
	}
	used[base] = 1
	return base
}

func renderSymbolRow(sym graph.BundleSymbol) string {
	return fmt.Sprintf("| %s | `%s` | %s | `%s:%d` | %d |\n",
		graph.FormatRank(sym.Rank), mdEscape(sym.QName), sym.Kind,
		mdEscape(sym.File), sym.Line, sym.Callers)
}

// mdEscape keeps a table cell intact when a name contains a pipe or backtick.
func mdEscape(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	return strings.ReplaceAll(s, "`", "'")
}

// renderIndexFixed renders everything in index.md except the top-symbol rows.
func renderIndexFixed(bundle graph.MapBundle, docs map[string]*moduleDoc, order []string, maxTokens int, floor float64) string {
	var b strings.Builder
	b.WriteString("# repath map\n\n")
	fmt.Fprintf(&b, "%s · %d symbols · %d entrypoints · min-confidence %.2f\n",
		plural(len(order), "module"), len(bundle.Symbols), bundle.Entrypoints, floor)
	fmt.Fprintf(&b, "Token budget %d. Generated by `repath map --md`; every file here is a\nprojection of the same index, not new data.\n\n", maxTokens)

	b.WriteString("## Modules\n\n")
	b.WriteString("| module | symbols | depends on |\n| --- | --- | --- |\n")
	for _, name := range order {
		doc := docs[name]
		fmt.Fprintf(&b, "| [`%s`](modules/%s.md) | %d | %s |\n",
			mdEscape(displayModule(name)), doc.Slug, doc.Symbols, mdEscape(doc.Deps))
	}

	if len(bundle.Cycles) > 0 {
		b.WriteString("\n## Cycles\n\n")
		for _, cycle := range bundle.Cycles {
			fmt.Fprintf(&b, "- %s\n", strings.Join(cycle, " → "))
		}
	}
	b.WriteString("\n## Top symbols\n\n")
	return b.String()
}

func renderSymbolTable(rows []string, trimmed bool) string {
	var b strings.Builder
	b.WriteString(symbolTableHeader)
	if len(rows) == 0 {
		b.WriteString("| — | — | — | — | — |\n")
	}
	for _, row := range rows {
		b.WriteString(row)
	}
	if trimmed {
		b.WriteString(mapTrimNote)
	}
	return b.String()
}

func renderModuleHeader(doc *moduleDoc) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", displayModule(doc.Name))
	fmt.Fprintf(&b, "%d symbols · %d files · depends on: %s\n",
		doc.Symbols, len(doc.Files), doc.Deps)
	if doc.ImportedBy != "" {
		fmt.Fprintf(&b, "imported by: %s\n", doc.ImportedBy)
	} else {
		b.WriteString("imported by: —\n")
	}

	b.WriteString("\n## Files\n\n")
	if len(doc.Files) == 0 {
		b.WriteString("no indexed files\n")
		return b.String()
	}
	b.WriteString("| file | symbols |\n| --- | --- |\n")
	shown := doc.Files
	if len(shown) > mapModuleFileRows {
		shown = shown[:mapModuleFileRows]
	}
	for _, f := range shown {
		fmt.Fprintf(&b, "| `%s` | %d |\n", mdEscape(f.Path), f.Symbols)
	}
	if more := len(doc.Files) - len(shown); more > 0 {
		fmt.Fprintf(&b, "| +%d more files | |\n", more)
	}
	return b.String()
}

func renderModuleDoc(doc *moduleDoc) string {
	return renderModuleHeader(doc) + "\n## Top symbols\n\n" + renderSymbolTable(doc.Rows, false)
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return strconv.Itoa(n) + " " + word + "s"
}

// printMapBundle reports what was written: one line per file so an agent can
// read the bundle without guessing names, and the budget the trim respected.
func printMapBundle(out io.Writer, files []writtenFile, symbols int, maxTokens int, trimmed bool) {
	total := 0
	for _, f := range files {
		total += f.Tokens
	}
	fmt.Fprintf(out, "repath map --md · %d files · %d symbols · ~%d tokens (budget %d)\n",
		len(files), symbols, total, maxTokens)
	for _, f := range files {
		fmt.Fprintf(out, "  %-52s %d tokens\n", f.Path, f.Tokens)
	}
	if total > maxTokens {
		fmt.Fprintln(out, "  (the structural pages alone exceed the budget; nothing else was written)")
	} else if trimmed {
		fmt.Fprintln(out, "  (symbol rows trimmed to fit the budget; raise --max-tokens for more)")
	}
}
