// Package trace reads an externally produced runtime CPU profile and reports
// the caller→callee calls it actually observed. It never writes to the graph:
// the trace is evidence attached at read time, like SARIF diagnostics, so a
// stale or surprising profile cannot change a default answer.
package trace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Goutam-11/repath/internal/store"
)

// Errors the CLI maps to its documented exit codes: a missing input is 3 and
// malformed input is 2 (the same split as `check --sarif`).
var (
	ErrNotFound  = errors.New("trace file not found")
	ErrMalformed = errors.New("malformed trace")
)

const (
	// RuleDyn is the visible rule id for an observed-only call. It is never
	// persisted; it only labels evidence in the report.
	RuleDyn = "dyn.trace"
	// DynConfidence is the report confidence for an observed call. A sampled
	// profile proves that the call happened at least once, not that it is the
	// only target.
	DynConfidence = 0.60
	// staticFloor is the same default query floor used everywhere else.
	staticFloor = 0.5
)

// Report is the read-only evidence for one profile.
type Report struct {
	File           string         `json:"file"`
	Format         string         `json:"format"`
	Frames         int            `json:"frames"`
	Mapped         int            `json:"mapped"`
	Skipped        int            `json:"skipped"`
	SkippedReasons map[string]int `json:"skipped_reasons,omitempty"`
	ObservedEdges  int            `json:"observed_edges"`
	Confirmed      int            `json:"confirmed"`
	ObservedOnly   int            `json:"observed_only"`
	Truncated      bool           `json:"truncated,omitempty"`
	Edges          []Edge         `json:"edges"`
}

// Endpoint is one symbol in the report.
type Endpoint struct {
	ID    int64  `json:"id"`
	QName string `json:"qname"`
	File  string `json:"file"`
	Line  int    `json:"line"`
}

// Edge is one observed caller→callee pair, with whether the static graph
// already proves the same relation.
type Edge struct {
	Caller     Endpoint `json:"caller"`
	Callee     Endpoint `json:"callee"`
	Count      int      `json:"count"`
	Kind       string   `json:"kind"`
	Rule       string   `json:"rule"`
	Confidence float64  `json:"confidence"`
	Static     bool     `json:"static"`
}

type profile struct {
	Nodes []profileNode `json:"nodes"`
}

type profileNode struct {
	ID        int64     `json:"id"`
	CallFrame callFrame `json:"callFrame"`
	HitCount  int       `json:"hitCount"`
	Children  []int64   `json:"children"`
	Parent    int64     `json:"parent"`
}

type callFrame struct {
	FunctionName string `json:"functionName"`
	URL          string `json:"url"`
	LineNumber   int    `json:"lineNumber"`
}

// Import parses a V8/Chrome CPU profile and reports its observed calls against
// the indexed graph. limit caps the listed edges; the counts are over all of
// them. Nothing is written.
func Import(ctx context.Context, st *store.Store, path string, limit int) (Report, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Report{}, fmt.Errorf("%w: %s", ErrNotFound, path)
		}
		return Report{}, fmt.Errorf("read trace: %w", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return Report{}, fmt.Errorf("%w: invalid JSON: %v", ErrMalformed, err)
	}
	if _, ok := raw["nodes"]; !ok {
		return Report{}, fmt.Errorf("%w: not a V8 CPU profile (missing nodes)", ErrMalformed)
	}
	var prof profile
	if err := json.Unmarshal(data, &prof); err != nil {
		return Report{}, fmt.Errorf("%w: %v", ErrMalformed, err)
	}

	report := Report{File: path, Format: "v8-cpuprofile", SkippedReasons: map[string]int{}}
	if len(prof.Nodes) == 0 {
		return report, nil
	}

	paths, err := st.FilePaths(ctx)
	if err != nil {
		return report, err
	}
	fileIDs := make(map[string]int64, len(paths))
	pathByID := make(map[int64]string, len(paths))
	for id, p := range paths {
		fileIDs[p] = id
		pathByID[id] = p
	}
	nodes, err := st.Nodes(ctx)
	if err != nil {
		return report, err
	}
	nodeByID := make(map[int64]store.NodeRow, len(nodes))
	for _, node := range nodes {
		nodeByID[node.ID] = node
	}
	fileIDFor := func(compilerPath string) (int64, bool) {
		compilerPath = strings.TrimPrefix(filepath.ToSlash(compilerPath), "./")
		if id, ok := fileIDs[compilerPath]; ok {
			return id, true
		}
		var matches []int64
		for p, id := range fileIDs {
			if strings.HasSuffix(p, "/"+compilerPath) {
				matches = append(matches, id)
			}
		}
		if len(matches) == 1 {
			return matches[0], true
		}
		return 0, false
	}

	// Derive parents from the children arrays; V8 omits explicit parent ids in
	// some profile versions.
	nodesByID := make(map[int64]*profileNode, len(prof.Nodes))
	parentOf := make(map[int64]int64, len(prof.Nodes))
	for i := range prof.Nodes {
		node := &prof.Nodes[i]
		nodesByID[node.ID] = node
		if node.Parent > 0 {
			parentOf[node.ID] = node.Parent
		}
	}
	for i := range prof.Nodes {
		for _, child := range prof.Nodes[i].Children {
			if _, ok := parentOf[child]; !ok {
				parentOf[child] = prof.Nodes[i].ID
			}
		}
	}

	type frameKey struct {
		url  string
		line int
		name string
	}
	type frameResult struct {
		nodeID int64
		reason string
	}
	frames := make(map[frameKey]frameResult)
	mappedNode := func(node *profileNode) (int64, string) {
		key := frameKey{url: node.CallFrame.URL, line: node.CallFrame.LineNumber, name: node.CallFrame.FunctionName}
		if cached, ok := frames[key]; ok {
			return cached.nodeID, cached.reason
		}
		nodeID, reason := mapFrame(ctx, st, node.CallFrame, fileIDFor, nodeByID)
		frames[key] = frameResult{nodeID: nodeID, reason: reason}
		return nodeID, reason
	}

	// Map every named frame; count the source frames and skip reasons.
	frameNodes := make(map[int64]int64, len(prof.Nodes))
	for i := range prof.Nodes {
		node := &prof.Nodes[i]
		if node.CallFrame.FunctionName == "" && node.CallFrame.URL == "" {
			continue // (root)/(program): not a source frame
		}
		report.Frames++
		nodeID, reason := mappedNode(node)
		if nodeID == 0 {
			report.Skipped++
			report.SkippedReasons[reason]++
			continue
		}
		report.Mapped++
		frameNodes[node.ID] = nodeID
	}

	type pair struct {
		caller int64
		callee int64
	}
	counts := make(map[pair]int)
	for i := range prof.Nodes {
		node := &prof.Nodes[i]
		callee, ok := frameNodes[node.ID]
		if !ok {
			continue
		}
		parent, ok := parentOf[node.ID]
		if !ok {
			continue
		}
		caller, ok := frameNodes[parent]
		if !ok || caller == callee {
			continue
		}
		count := node.HitCount
		if count < 1 {
			count = 1
		}
		counts[pair{caller: caller, callee: callee}] += count
	}

	staticCache := make(map[int64]map[int64]bool)
	hasStatic := func(caller, callee int64) (bool, error) {
		if cached, ok := staticCache[callee]; ok {
			return cached[caller], nil
		}
		incoming := make(map[int64]bool)
		edges, err := st.Callers(ctx, callee, staticFloor)
		if err != nil {
			return false, err
		}
		for _, edge := range edges {
			incoming[edge.SrcNode.ID] = true
		}
		staticCache[callee] = incoming
		return incoming[caller], nil
	}

	all := make([]Edge, 0, len(counts))
	for key, count := range counts {
		callerNode, ok := nodeByID[key.caller]
		if !ok {
			continue
		}
		calleeNode, ok := nodeByID[key.callee]
		if !ok {
			continue
		}
		static, err := hasStatic(key.caller, key.callee)
		if err != nil {
			return report, err
		}
		all = append(all, Edge{
			Caller: endpointOf(callerNode, pathByID),
			Callee: endpointOf(calleeNode, pathByID),
			Count:  count, Kind: "CALLS", Rule: RuleDyn,
			Confidence: DynConfidence, Static: static,
		})
	}
	report.ObservedEdges = len(all)
	for _, edge := range all {
		if edge.Static {
			report.Confirmed++
		} else {
			report.ObservedOnly++
		}
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].Count != all[j].Count {
			return all[i].Count > all[j].Count
		}
		if all[i].Caller.QName != all[j].Caller.QName {
			return all[i].Caller.QName < all[j].Caller.QName
		}
		return all[i].Callee.QName < all[j].Callee.QName
	})
	if limit > 0 && len(all) > limit {
		all = all[:limit]
		report.Truncated = true
	}
	report.Edges = all
	return report, nil
}

// mapFrame resolves one call frame to an indexed symbol. It returns a skip
// reason instead of guessing when the frame is external, unmapped, or no longer
// names the symbol at that line (the staleness and same-name protection).
func mapFrame(ctx context.Context, st *store.Store, cf callFrame, fileIDFor func(string) (int64, bool),
	nodeByID map[int64]store.NodeRow) (int64, string) {
	if cf.FunctionName == "" || cf.FunctionName == "(anonymous)" {
		return 0, "no-function"
	}
	if cf.URL == "" || strings.HasPrefix(cf.URL, "node:") {
		return 0, "external-file"
	}
	path, ok := framePath(cf.URL)
	if !ok {
		return 0, "external-file"
	}
	fileID, ok := fileIDFor(path)
	if !ok {
		return 0, "unmapped-file"
	}
	line := cf.LineNumber + 1
	if line < 1 {
		return 0, "no-line"
	}
	nodeID, found, err := st.EnclosingNode(ctx, fileID, line)
	if err != nil || !found {
		return 0, "no-enclosing"
	}
	node, ok := nodeByID[nodeID]
	if !ok {
		return 0, "no-enclosing"
	}
	if node.Name != cf.FunctionName {
		return 0, "name-mismatch"
	}
	return nodeID, ""
}

// framePath turns a file:// URL or path into a repo-relative-ish path candidate.
// A non-file scheme (webpack://, node:) is external.
func framePath(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if strings.HasPrefix(raw, "file://") {
		parsed, err := url.Parse(raw)
		if err != nil {
			return "", false
		}
		raw = parsed.Path
	}
	if raw == "" || strings.Contains(raw, "://") {
		return "", false
	}
	return raw, true
}

func endpointOf(node store.NodeRow, paths map[int64]string) Endpoint {
	return Endpoint{ID: node.ID, QName: node.QName, File: paths[node.FileID], Line: node.StartLine}
}
