package index

import (
	"path"
	"strings"

	"github.com/Goutam-11/repath/internal/lang/facts"
	"github.com/Goutam-11/repath/internal/store"
)

// augmentPathEntrypoints adds entrypoints that are defined by file convention
// rather than by a language construct. Next.js route handlers are exported
// HTTP-named functions under app/**/route.ts or pages/api/.
func augmentPathEntrypoints(filePath string, result *facts.Result) {
	p := path.Clean(strings.ReplaceAll(filePath, "\\", "/"))
	base := path.Base(p)
	nextRoute := strings.HasSuffix(p, "/route.ts") || strings.HasSuffix(p, "/route.tsx") ||
		strings.HasSuffix(p, "/route.js") || strings.HasSuffix(p, "/route.jsx")
	nextPages := strings.HasPrefix(p, "pages/api/") || strings.Contains(p, "/pages/api/")
	if !nextRoute && !nextPages {
		return
	}

	for _, symbol := range result.Symbols {
		if !symbol.Exported || (symbol.Kind != "function" && symbol.Kind != "method") {
			continue
		}
		rule := ""
		switch symbol.Name {
		case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS":
			rule = "ts.next.route"
		case "handler":
			if nextPages || base == "route.ts" || base == "route.js" {
				rule = "ts.next.route"
			}
		}
		if rule == "" {
			continue
		}
		result.Entrypoints = append(result.Entrypoints, facts.Entrypoint{
			QName: symbol.QName, Name: symbol.Name, Rule: rule,
			Confidence: 0.90, Line: symbol.StartLine,
		})
	}
}

func toEntrypointRows(entries []facts.Entrypoint, ids map[string]int64) []store.EntrypointRow {
	rows := make([]store.EntrypointRow, 0, len(entries))
	for _, entry := range entries {
		nodeID, ok := ids[entry.QName]
		if !ok {
			continue
		}
		rows = append(rows, store.EntrypointRow{
			NodeID: nodeID, Rule: entry.Rule, Name: entry.Name,
			Confidence: entry.Confidence, Line: entry.Line,
		})
	}
	return rows
}
