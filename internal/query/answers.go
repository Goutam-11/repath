package query

import (
	"context"

	"github.com/Goutam-11/repath/internal/graph"
	"github.com/Goutam-11/repath/internal/store"
)

// FindResult is the symbol card returned by `find` and the MCP find_symbol tool.
type FindResult struct {
	Symbol        SymbolRef `json:"symbol"`
	Callers       int       `json:"callers"`
	Callees       int       `json:"callees"`
	MinConfidence float64   `json:"min_confidence"`
}

// FindAnswer carries the resolved target alongside the card so callers can
// render a not-found or ambiguous result from the same object.
type FindAnswer struct {
	Target Target
	Result FindResult
}

// Find resolves a symbol (optionally inside one module) and counts its direct
// caller and callee edges.
func Find(ctx context.Context, st *store.Store, name, module string, minConfidence float64) (FindAnswer, int, error) {
	target, code, err := ResolveSymbol(ctx, st, name, module)
	answer := FindAnswer{Target: target}
	if err != nil || code != 0 {
		return answer, code, err
	}

	node := target.Nodes[0]
	callers, err := st.CallerCount(ctx, node.ID, minConfidence)
	if err != nil {
		return answer, 1, err
	}
	callees, err := st.CalleeCount(ctx, node.ID, minConfidence)
	if err != nil {
		return answer, 1, err
	}
	paths, err := st.FilePaths(ctx)
	if err != nil {
		return answer, 1, err
	}

	answer.Result = FindResult{
		Symbol:        SymbolRefOf(node, paths),
		Callers:       callers,
		Callees:       callees,
		MinConfidence: minConfidence,
	}
	return answer, 0, nil
}

// Caller is one reverse edge in a callers answer.
type Caller struct {
	QName      string  `json:"qname"`
	File       string  `json:"file"`
	Line       int     `json:"line"`
	Kind       string  `json:"kind"`
	Confidence float64 `json:"confidence"`
	Rule       string  `json:"rule"`
	CallLine   int     `json:"call_line"`
}

// CallersResult is the `callers` payload.
type CallersResult struct {
	Symbol        SymbolRef `json:"symbol"`
	MinConfidence float64   `json:"min_confidence"`
	Callers       []Caller  `json:"callers"`
	Truncated     bool      `json:"truncated"`
}

// CallersAnswer carries the resolved target alongside the callers list.
type CallersAnswer struct {
	Target Target
	Result CallersResult
}

// Callers resolves one symbol and returns its direct reverse edges. A limit of
// zero means no cap (the CLI); the MCP tool passes DefaultCallersLimit.
func Callers(ctx context.Context, st *store.Store, name string, minConfidence float64, limit int) (CallersAnswer, int, error) {
	// A name, a file, or a file:line target: file:line is how a duplicate qname
	// is disambiguated (for example the two halves of a merged cross-repo
	// index). A whole file maps to several symbols, which is ambiguous for a
	// single-caller answer rather than a silent first pick.
	target, code, err := ResolveTarget(ctx, st, name)
	answer := CallersAnswer{Target: target}
	if err != nil || code != 0 {
		return answer, code, err
	}
	if len(target.Nodes) != 1 {
		target.Candidates = target.Nodes
		target.Nodes = nil
		answer.Target = target
		return answer, 4, nil
	}

	node := target.Nodes[0]
	edges, err := st.Callers(ctx, node.ID, minConfidence)
	if err != nil {
		return answer, 1, err
	}
	paths, err := st.FilePaths(ctx)
	if err != nil {
		return answer, 1, err
	}

	result := CallersResult{
		Symbol:        SymbolRefOf(node, paths),
		MinConfidence: minConfidence,
		Callers:       make([]Caller, 0, len(edges)),
	}
	for _, e := range edges {
		if limit > 0 && len(result.Callers) >= limit {
			result.Truncated = true
			break
		}
		result.Callers = append(result.Callers, Caller{
			QName:      e.SrcNode.QName,
			File:       e.SrcPath,
			Line:       e.SrcNode.StartLine,
			Kind:       e.Kind,
			Confidence: e.Confidence,
			Rule:       e.Rule,
			CallLine:   e.Line,
		})
	}
	answer.Result = result
	return answer, 0, nil
}

// ImpactAnswer is the blast-radius answer plus the resolved target.
type ImpactAnswer struct {
	Target        Target
	Result        graph.Impact
	MinConfidence float64
	Depth         int
	Limit         int
}

// Impact resolves a symbol or file:line and walks its reverse closure.
func Impact(ctx context.Context, st *store.Store, arg string, depth int, minConfidence float64, limit int) (ImpactAnswer, int, error) {
	target, code, err := ResolveTarget(ctx, st, arg)
	answer := ImpactAnswer{
		Target:        target,
		MinConfidence: minConfidence,
		Depth:         NormalizeDepth(depth),
		Limit:         limit,
	}
	if err != nil || code != 0 {
		return answer, code, err
	}
	if answer.Limit <= 0 {
		answer.Limit = DefaultLimit
	}

	result, err := graph.Blast(ctx, st, target.Nodes, answer.Depth, minConfidence, answer.Limit)
	if err != nil {
		return answer, 1, err
	}
	answer.Result = result
	return answer, 0, nil
}

// PathFailure names which end of a path failed to resolve.
type PathFailure struct {
	Arg    string
	Code   int
	Target Target
}

// PathAnswer is the connection answer plus both resolved endpoints.
type PathAnswer struct {
	From          Target
	To            Target
	Result        graph.PathResult
	MinConfidence float64
	MaxDepth      int
	Failed        *PathFailure
}

// Path resolves two symbols and finds the shortest chain between them.
func Path(ctx context.Context, st *store.Store, fromArg, toArg string, maxDepth int, minConfidence float64, alternates int) (PathAnswer, int, error) {
	answer := PathAnswer{MinConfidence: minConfidence, MaxDepth: NormalizeDepth(maxDepth)}
	if alternates <= 0 {
		alternates = 3
	}

	from, code, err := ResolveTarget(ctx, st, fromArg)
	answer.From = from
	if err == nil && code != 0 {
		answer.Failed = &PathFailure{Arg: fromArg, Code: code, Target: from}
	}
	if err != nil || code != 0 {
		return answer, code, err
	}

	to, code, err := ResolveTarget(ctx, st, toArg)
	answer.To = to
	if err == nil && code != 0 {
		answer.Failed = &PathFailure{Arg: toArg, Code: code, Target: to}
	}
	if err != nil || code != 0 {
		return answer, code, err
	}

	result, err := graph.Connect(ctx, st, from.Nodes[0], to.Nodes[0], answer.MaxDepth, minConfidence, alternates)
	if err != nil {
		return answer, 1, err
	}
	answer.Result = result
	return answer, 0, nil
}

// MapAnswer is the ranked repository overview.
type MapAnswer struct {
	Result        graph.MapResult
	MaxTokens     int
	MinConfidence float64
}

// Map builds the PageRank/module overview within the token budget.
func Map(ctx context.Context, st *store.Store, maxTokens int, minConfidence float64) (MapAnswer, error) {
	if maxTokens <= 0 {
		maxTokens = DefaultMaxTokens
	}
	result, err := graph.BuildMap(ctx, st, maxTokens, minConfidence)
	return MapAnswer{Result: result, MaxTokens: maxTokens, MinConfidence: minConfidence}, err
}
