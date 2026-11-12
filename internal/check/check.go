// Package check implements the graph-native repository checks from PLAN §16.2:
// module cycles, dead code, module-boundary violations, fan-in/out hotspots,
// and per-module unresolved rate. Everything is derived from the symbol graph;
// no compiler or config file is consulted. An opt-in SARIF log can add
// `diagnostic` findings (PLAN §3 decision 22, amended in v1.2), but repath still
// never invokes a compiler.
package check

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/Goutam-11/repath/internal/graph"
	"github.com/Goutam-11/repath/internal/store"
	"github.com/Goutam-11/repath/internal/testpath"
)

// Severities and the check-only exit code. Exit codes 0-4 stay reserved for the
// CLI contract; 5 means "findings at or above --fail-on".
const (
	SeverityError   = "error"
	SeverityWarning = "warning"
	SeverityInfo    = "info"

	ExitFindings = 5

	defaultMaxFindings = 200
	// A symbol is a hotspot at this fan-in or fan-out. Fixed so the report is
	// deterministic without a config file.
	hotspotThreshold = 5
	// Only modules with at least this many unresolved refs and this rate are
	// worth reporting; one missing name in a small module is ordinary.
	unresolvedMinPending = 3
	unresolvedRate       = 0.5
)

// Options controls one check run.
type Options struct {
	MinConfidence float64
	MaxFindings   int
	// SARIF is an already-loaded SARIF ingestion. It is opt-in and only adds
	// diagnostic findings; structural findings are unchanged when it is nil.
	SARIF *SARIFLoad
}

// Finding is one check result.
type Finding struct {
	Kind     string   `json:"kind"`
	Severity string   `json:"severity"`
	Rule     string   `json:"rule"`
	Message  string   `json:"message"`
	QName    string   `json:"qname,omitempty"`
	File     string   `json:"file,omitempty"`
	Line     int      `json:"line,omitempty"`
	Module   string   `json:"module,omitempty"`
	Modules  []string `json:"modules,omitempty"`
	Value    float64  `json:"value,omitempty"`
	Count    int      `json:"count,omitempty"`
}

// Counts summarises findings by severity. They count every finding, including
// ones omitted from a truncated list.
type Counts struct {
	Error   int `json:"error"`
	Warning int `json:"warning"`
	Info    int `json:"info"`
}

// Report is the `check --json` contract.
type Report struct {
	Root          string             `json:"root"`
	SchemaVersion int                `json:"schema_version"`
	MinConfidence float64            `json:"min_confidence"`
	MaxFindings   int                `json:"max_findings"`
	Files         int                `json:"files"`
	Nodes         int                `json:"nodes"`
	Edges         int                `json:"edges"`
	Counts        Counts             `json:"counts"`
	Findings      []Finding          `json:"findings"`
	Diagnostics   *DiagnosticSummary `json:"diagnostics,omitempty"`
	Truncated     bool               `json:"truncated"`
}

// Run executes every check against the current index.
func Run(ctx context.Context, st *store.Store, root string, opts Options) (Report, error) {
	if opts.MinConfidence < 0 || opts.MinConfidence > 1 {
		opts.MinConfidence = 0.5
	}
	if opts.MinConfidence == 0 {
		opts.MinConfidence = 0.5
	}
	if opts.MaxFindings <= 0 {
		opts.MaxFindings = defaultMaxFindings
	}

	nodes, err := st.Nodes(ctx)
	if err != nil {
		return Report{}, err
	}
	edges, err := st.GraphEdges(ctx, opts.MinConfidence)
	if err != nil {
		return Report{}, err
	}
	paths, err := st.FilePaths(ctx)
	if err != nil {
		return Report{}, err
	}
	files, err := st.FileMap(ctx)
	if err != nil {
		return Report{}, err
	}
	fileModules, moduleNames, err := graph.ModuleIndex(ctx, st)
	if err != nil {
		return Report{}, err
	}
	pending, err := st.Pending(ctx)
	if err != nil {
		return Report{}, err
	}

	report := Report{
		Root:          root,
		SchemaVersion: store.SchemaVersion,
		MinConfidence: opts.MinConfidence,
		MaxFindings:   opts.MaxFindings,
		Files:         len(files),
		Nodes:         len(nodes),
		Edges:         len(edges),
		Findings:      make([]Finding, 0),
	}

	var findings []Finding
	findings = append(findings, moduleCycleFindings(edges, fileModules, moduleNames)...)
	findings = append(findings, deadCodeFindings(nodes, edges, paths, files)...)
	findings = append(findings, boundaryFindings(edges, fileModules, moduleNames, paths)...)
	findings = append(findings, hotspotFindings(nodes, edges, paths)...)
	findings = append(findings, unresolvedFindings(pending, edges, fileModules, moduleNames)...)
	if opts.SARIF != nil {
		diagnostics, err := diagnosticFindings(ctx, st, opts.SARIF, files)
		if err != nil {
			return Report{}, err
		}
		findings = append(findings, diagnostics...)
		summary := opts.SARIF.Summary
		report.Diagnostics = &summary
	}

	sortFindings(findings)
	for _, finding := range findings {
		switch finding.Severity {
		case SeverityError:
			report.Counts.Error++
		case SeverityWarning:
			report.Counts.Warning++
		case SeverityInfo:
			report.Counts.Info++
		}
	}
	if len(findings) > opts.MaxFindings {
		report.Truncated = true
		findings = findings[:opts.MaxFindings]
	}
	report.Findings = append(report.Findings, findings...)
	return report, nil
}

// ExitCode maps a report and --fail-on value to the process exit code. A
// threshold of "none" never fails; "error", "warning", and "info" include every
// finding at that severity or above.
func ExitCode(report Report, failOn string) int {
	switch failOn {
	case "", "error":
		if report.Counts.Error > 0 {
			return ExitFindings
		}
	case "warning":
		if report.Counts.Error+report.Counts.Warning > 0 {
			return ExitFindings
		}
	case "info":
		if report.Counts.Error+report.Counts.Warning+report.Counts.Info > 0 {
			return ExitFindings
		}
	}
	return 0
}

// ValidFailOn reports whether a --fail-on value is understood.
func ValidFailOn(value string) bool {
	switch value {
	case "none", "error", "warning", "info":
		return true
	}
	return false
}

func moduleCycleFindings(edges []store.EdgeOut, fileModules map[int64]int64, moduleNames map[int64]string) []Finding {
	deps := make(map[string]map[string]bool)
	for _, edge := range edges {
		from := moduleName(fileModules, moduleNames, edge.SrcNode.FileID)
		to := moduleName(fileModules, moduleNames, edge.DstNode.FileID)
		if from == "" || to == "" || from == to {
			continue
		}
		if deps[from] == nil {
			deps[from] = map[string]bool{}
		}
		deps[from][to] = true
	}

	var findings []Finding
	for _, cycle := range graph.Cycles(deps) {
		if len(cycle) < 2 {
			continue
		}
		path := append(append([]string(nil), cycle...), cycle[0])
		findings = append(findings, Finding{
			Kind:     "module-cycle",
			Severity: SeverityWarning,
			Rule:     "module.cycle",
			Message:  "module cycle: " + strings.Join(path, " → "),
			Modules:  cycle,
			Count:    len(cycle),
		})
	}
	return findings
}

func deadCodeFindings(nodes []store.NodeRow, edges []store.EdgeOut, paths map[int64]string, files map[string]store.FileState) []Finding {
	incoming := make(map[int64]int, len(edges))
	for _, edge := range edges {
		incoming[edge.Dst]++
	}

	var findings []Finding
	for _, node := range nodes {
		if node.Kind != "function" && node.Kind != "method" {
			continue
		}
		if !isPrivate(node) || isSpecial(node.Name) || incoming[node.ID] > 0 {
			continue
		}
		path := paths[node.FileID]
		if path == "" || testpath.IsTest(path) {
			continue
		}
		if state, ok := files[path]; ok && !state.ParseOK {
			continue
		}
		findings = append(findings, Finding{
			Kind:     "dead-code",
			Severity: SeverityWarning,
			Rule:     "dead.unreferenced",
			Message:  fmt.Sprintf("unreferenced %s %q", node.Kind, node.QName),
			QName:    node.QName,
			File:     path,
			Line:     node.StartLine,
			Count:    1,
		})
	}
	return findings
}

// boundaryFindings reports two objective boundary violations that need no
// configuration: production code depending on test code, and a cross-module
// reference to a private symbol.
func boundaryFindings(edges []store.EdgeOut, fileModules map[int64]int64, moduleNames map[int64]string, paths map[int64]string) []Finding {
	type key struct {
		rule string
		from string
		to   string
	}
	seen := make(map[key]*Finding)
	order := make([]key, 0)

	add := func(k key, finding Finding) {
		if existing, ok := seen[k]; ok {
			existing.Count++
			return
		}
		finding.Count = 1
		seen[k] = &finding
		order = append(order, k)
	}

	for _, edge := range edges {
		srcPath := paths[edge.SrcNode.FileID]
		dstPath := paths[edge.DstNode.FileID]
		if srcPath == "" || dstPath == "" {
			continue
		}
		srcModule := moduleName(fileModules, moduleNames, edge.SrcNode.FileID)
		dstModule := moduleName(fileModules, moduleNames, edge.DstNode.FileID)
		if srcModule == "" || dstModule == "" || srcModule == dstModule {
			continue
		}
		if testpath.IsTest(srcPath) {
			continue
		}

		if testpath.IsTest(dstPath) {
			k := key{rule: "test-boundary", from: srcModule, to: edge.DstNode.QName}
			add(k, Finding{
				Kind:     "module-boundary",
				Severity: SeverityError,
				Rule:     "boundary.test",
				Message:  fmt.Sprintf("module %q depends on test symbol %q", srcModule, edge.DstNode.QName),
				QName:    edge.DstNode.QName,
				File:     srcPath,
				Line:     edge.Line,
				Module:   srcModule,
				Modules:  []string{srcModule, dstModule},
			})
			continue
		}

		if isPrivate(edge.DstNode) && !isSpecial(edge.DstNode.Name) {
			k := key{rule: "private-crossing", from: srcModule, to: edge.DstNode.QName}
			add(k, Finding{
				Kind:     "module-boundary",
				Severity: SeverityWarning,
				Rule:     "boundary.private",
				Message:  fmt.Sprintf("module %q references private symbol %q in module %q", srcModule, edge.DstNode.QName, dstModule),
				QName:    edge.DstNode.QName,
				File:     srcPath,
				Line:     edge.Line,
				Module:   srcModule,
				Modules:  []string{srcModule, dstModule},
			})
		}
	}

	findings := make([]Finding, 0, len(order))
	for _, k := range order {
		findings = append(findings, *seen[k])
	}
	return findings
}

func hotspotFindings(nodes []store.NodeRow, edges []store.EdgeOut, paths map[int64]string) []Finding {
	incoming := make(map[int64]int)
	outgoing := make(map[int64]int)
	for _, edge := range edges {
		incoming[edge.Dst]++
		outgoing[edge.Src]++
	}

	type ranked struct {
		node  store.NodeRow
		count int
	}
	byID := make(map[int64]store.NodeRow, len(nodes))
	for _, node := range nodes {
		byID[node.ID] = node
	}

	collect := func(counts map[int64]int) []ranked {
		var list []ranked
		for id, count := range counts {
			if count < hotspotThreshold {
				continue
			}
			node, ok := byID[id]
			if !ok {
				continue
			}
			if testpath.IsTest(paths[node.FileID]) {
				continue
			}
			list = append(list, ranked{node: node, count: count})
		}
		sort.Slice(list, func(i, j int) bool {
			if list[i].count != list[j].count {
				return list[i].count > list[j].count
			}
			return list[i].node.QName < list[j].node.QName
		})
		if len(list) > 5 {
			list = list[:5]
		}
		return list
	}

	var findings []Finding
	for _, item := range collect(incoming) {
		findings = append(findings, Finding{
			Kind:     "hotspot",
			Severity: SeverityInfo,
			Rule:     "hotspot.fan-in",
			Message:  fmt.Sprintf("fan-in %d", item.count),
			QName:    item.node.QName,
			File:     paths[item.node.FileID],
			Line:     item.node.StartLine,
			Count:    item.count,
			Value:    float64(item.count),
		})
	}
	for _, item := range collect(outgoing) {
		findings = append(findings, Finding{
			Kind:     "hotspot",
			Severity: SeverityInfo,
			Rule:     "hotspot.fan-out",
			Message:  fmt.Sprintf("fan-out %d", item.count),
			QName:    item.node.QName,
			File:     paths[item.node.FileID],
			Line:     item.node.StartLine,
			Count:    item.count,
			Value:    float64(item.count),
		})
	}
	return findings
}

func unresolvedFindings(pending []store.PendingRef, edges []store.EdgeOut, fileModules map[int64]int64, moduleNames map[int64]string) []Finding {
	pendingByModule := make(map[string]int)
	for _, ref := range pending {
		name := moduleName(fileModules, moduleNames, ref.FileID)
		if name != "" {
			pendingByModule[name]++
		}
	}
	resolvedByModule := make(map[string]int)
	for _, edge := range edges {
		name := moduleName(fileModules, moduleNames, edge.SrcNode.FileID)
		if name != "" {
			resolvedByModule[name]++
		}
	}

	type moduleRate struct {
		name     string
		pending  int
		resolved int
		rate     float64
	}
	var rates []moduleRate
	for name, count := range pendingByModule {
		if count < unresolvedMinPending {
			continue
		}
		resolved := resolvedByModule[name]
		total := count + resolved
		rate := 0.0
		if total > 0 {
			rate = float64(count) / float64(total)
		}
		if rate < unresolvedRate {
			continue
		}
		rates = append(rates, moduleRate{name: name, pending: count, resolved: resolved, rate: rate})
	}
	sort.Slice(rates, func(i, j int) bool {
		if rates[i].rate != rates[j].rate {
			return rates[i].rate > rates[j].rate
		}
		if rates[i].pending != rates[j].pending {
			return rates[i].pending > rates[j].pending
		}
		return rates[i].name < rates[j].name
	})

	var findings []Finding
	for _, item := range rates {
		findings = append(findings, Finding{
			Kind:     "unresolved-rate",
			Severity: SeverityWarning,
			Rule:     "unresolved.module-rate",
			Message: fmt.Sprintf("unresolved %.1f%% (%d/%d references)",
				item.rate*100, item.pending, item.pending+item.resolved),
			Module: item.name,
			Value:  item.rate,
			Count:  item.pending,
		})
	}
	return findings
}

func moduleName(fileModules map[int64]int64, moduleNames map[int64]string, fileID int64) string {
	return moduleNames[fileModules[fileID]]
}

// isPrivate is language-aware: TypeScript and Rust have an explicit public
// marker, while Python's module-level names are all importable and privacy is
// the underscore convention.
func isPrivate(node store.NodeRow) bool {
	if node.Lang == "python" {
		return strings.HasPrefix(node.Name, "_") && !isSpecial(node.Name)
	}
	return !node.Exported
}

// isSpecial excludes framework/entrypoint names that cannot be judged dead from
// the static graph.
func isSpecial(name string) bool {
	switch name {
	case "main", "constructor", "setUp", "tearDown", "setup", "teardown", "__init__":
		return true
	}
	return len(name) > 4 && strings.HasPrefix(name, "__") && strings.HasSuffix(name, "__")
}

func sortFindings(findings []Finding) {
	rank := func(severity string) int {
		switch severity {
		case SeverityError:
			return 0
		case SeverityWarning:
			return 1
		default:
			return 2
		}
	}
	sort.SliceStable(findings, func(i, j int) bool {
		if rank(findings[i].Severity) != rank(findings[j].Severity) {
			return rank(findings[i].Severity) < rank(findings[j].Severity)
		}
		if findings[i].Kind != findings[j].Kind {
			return findings[i].Kind < findings[j].Kind
		}
		if findings[i].Message != findings[j].Message {
			return findings[i].Message < findings[j].Message
		}
		if findings[i].File != findings[j].File {
			return findings[i].File < findings[j].File
		}
		return findings[i].Line < findings[j].Line
	})
}
