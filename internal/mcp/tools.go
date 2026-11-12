package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Goutam-11/repath/internal/graph"
	"github.com/Goutam-11/repath/internal/output"
	"github.com/Goutam-11/repath/internal/query"
)

// argumentError is a bad tool argument: reported to the client as a tool error
// with the CLI's usage exit code (2), not as a JSON-RPC protocol failure.
type argumentError string

func (e argumentError) Error() string { return string(e) }

type content struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type toolResult struct {
	Content []content `json:"content"`
	IsError bool      `json:"isError,omitempty"`
}

type candidate struct {
	QName string `json:"qname"`
	Kind  string `json:"kind"`
}

func resultOf(envelope output.Envelope) toolResult {
	data, err := output.Marshal(envelope)
	if err != nil {
		return toolResult{
			Content: []content{{Type: "text", Text: `{"answer":{"error":"encode envelope"},"summary":"encode envelope","confidence":0,"truncated":false,"provenance":[]}`}},
			IsError: true,
		}
	}
	return toolResult{Content: []content{{Type: "text", Text: string(data)}}}
}

func errorResult(code int, message string, candidates []candidate) toolResult {
	answer := map[string]any{"error": message, "code": code}
	if len(candidates) > 0 {
		answer["candidates"] = candidates
	}
	envelope := output.New(answer, message, 0, false, nil)
	result := resultOf(envelope)
	result.IsError = true
	return result
}

func candidatesOf(target query.Target) []candidate {
	out := make([]candidate, 0, len(target.Candidates))
	for _, n := range target.Candidates {
		out = append(out, candidate{QName: n.QName, Kind: n.Kind})
	}
	return out
}

func targetError(code int, name string, target query.Target) toolResult {
	if code == 3 {
		return errorResult(3, "not found: "+name, nil)
	}
	return errorResult(4, fmt.Sprintf("ambiguous: %s matches %d symbols", name, len(target.Candidates)), candidatesOf(target))
}

// toolDefinitions is the whole MCP surface: exactly six tools (PLAN.md §7).
func toolDefinitions() []map[string]any {
	return []map[string]any{
		{
			"name":        "find_symbol",
			"description": "Find one symbol by name or qualified name and return its location, kind, and direct caller/callee counts.",
			"inputSchema": object(map[string]any{
				"name":   property("string", "Symbol name or qualified name"),
				"module": property("string", "Optional module name or root to narrow the match"),
			}, "name"),
		},
		{
			"name":        "impact",
			"description": "Transitive blast radius: everything that depends on a symbol (or every symbol in a file:line), grouped by depth.",
			"inputSchema": object(map[string]any{
				"symbol":         property("string", "Symbol name, qualified name, or file:line"),
				"depth":          propertyWithDefault("integer", "Maximum reverse depth", query.DefaultDepth),
				"min_confidence": propertyWithDefault("number", "Edge confidence floor, 0-1", query.DefaultMinConfidence),
			}, "symbol"),
		},
		{
			"name":        "path",
			"description": "How one symbol reaches another: the shortest chain with per-edge provenance, plus alternates or the nearest frontier.",
			"inputSchema": object(map[string]any{
				"from":           property("string", "Start symbol"),
				"to":             property("string", "Destination symbol"),
				"max_depth":      propertyWithDefault("integer", "Maximum path length", query.DefaultDepth),
				"min_confidence": propertyWithDefault("number", "Edge confidence floor, 0-1", query.DefaultMinConfidence),
			}, "from", "to"),
		},
		{
			"name":        "callers",
			"description": "Direct reverse edges of one symbol: who calls or references it, with confidence and rule.",
			"inputSchema": object(map[string]any{
				"symbol": property("string", "Symbol name or qualified name"),
				"depth":  propertyWithDefault("integer", "Reverse depth; only 1 is supported (use impact for more)", 1),
			}, "symbol"),
		},
		{
			"name":        "repo_map",
			"description": "Ranked repository overview: modules and their dependencies, module cycles, and the highest-PageRank symbols within a token budget.",
			"inputSchema": object(map[string]any{
				"max_tokens": propertyWithDefault("integer", "Token budget for the symbol list", query.DefaultMaxTokens),
			}),
		},
		{
			"name":        "status",
			"description": "Index health: file/node/edge counts, unresolved and ambiguity rates, parse failures, staleness, and dirty files.",
			"inputSchema": object(map[string]any{}),
		},
	}
}

func object(properties map[string]any, required ...string) map[string]any {
	schema := map[string]any{
		"type":                 "object",
		"properties":           properties,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

func property(typ, description string) map[string]any {
	return map[string]any{"type": typ, "description": description}
}

func propertyWithDefault(typ, description string, def any) map[string]any {
	return map[string]any{"type": typ, "description": description, "default": def}
}

func parseArguments(raw json.RawMessage) (map[string]json.RawMessage, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return map[string]json.RawMessage{}, nil
	}
	var args map[string]json.RawMessage
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, err
	}
	return args, nil
}

func requiredString(args map[string]json.RawMessage, key string) (string, error) {
	raw, ok := args[key]
	if !ok {
		return "", argumentError("missing required argument: " + key)
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || strings.TrimSpace(value) == "" {
		return "", argumentError("argument " + key + " must be a non-empty string")
	}
	return value, nil
}

func optionalString(args map[string]json.RawMessage, key string) (string, error) {
	raw, ok := args[key]
	if !ok {
		return "", nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", argumentError("argument " + key + " must be a string")
	}
	return value, nil
}

func optionalInt(args map[string]json.RawMessage, key string, def int) (int, error) {
	raw, ok := args[key]
	if !ok {
		return def, nil
	}
	var value float64
	if err := json.Unmarshal(raw, &value); err != nil {
		return def, argumentError("argument " + key + " must be a number")
	}
	return int(value), nil
}

func optionalFloat(args map[string]json.RawMessage, key string, def float64) (float64, error) {
	raw, ok := args[key]
	if !ok {
		return def, nil
	}
	var value float64
	if err := json.Unmarshal(raw, &value); err != nil {
		return def, argumentError("argument " + key + " must be a number")
	}
	if value < 0 || value > 1 {
		return def, argumentError("argument " + key + " must be between 0 and 1")
	}
	return value, nil
}

func (s *server) findSymbol(ctx context.Context, args map[string]json.RawMessage) (toolResult, error) {
	name, err := requiredString(args, "name")
	if err != nil {
		return toolResult{}, err
	}
	module, err := optionalString(args, "module")
	if err != nil {
		return toolResult{}, err
	}
	session, err := s.sessionFor(ctx)
	if err != nil {
		return toolResult{}, err
	}

	answer, code, err := query.Find(ctx, session.Store, name, module, query.DefaultMinConfidence)
	if err != nil {
		return toolResult{}, err
	}
	if code != 0 {
		return targetError(code, name, answer.Target), nil
	}

	result := answer.Result
	provenance := []output.Provenance{{
		File: result.Symbol.File, Line: result.Symbol.Line, Rule: "definition",
	}}
	summary := fmt.Sprintf("%s %s at %s:%d · %d callers · %d callees",
		result.Symbol.QName, result.Symbol.Kind, result.Symbol.File, result.Symbol.Line,
		result.Callers, result.Callees)
	return resultOf(output.New(result, summary, 1.0, false, provenance)), nil
}

func (s *server) impact(ctx context.Context, args map[string]json.RawMessage) (toolResult, error) {
	symbol, err := requiredString(args, "symbol")
	if err != nil {
		return toolResult{}, err
	}
	depth, err := optionalInt(args, "depth", query.DefaultDepth)
	if err != nil {
		return toolResult{}, err
	}
	minConfidence, err := optionalFloat(args, "min_confidence", query.DefaultMinConfidence)
	if err != nil {
		return toolResult{}, err
	}
	session, err := s.sessionFor(ctx)
	if err != nil {
		return toolResult{}, err
	}

	answer, code, err := query.Impact(ctx, session.Store, symbol, depth, minConfidence, query.DefaultLimit)
	if err != nil {
		return toolResult{}, err
	}
	if code != 0 {
		return targetError(code, symbol, answer.Target), nil
	}

	result := answer.Result
	payload := struct {
		graph.Impact
		MinConfidence float64 `json:"min_confidence"`
	}{result, minConfidence}

	provenance := make([]output.Provenance, 0, len(result.Dependents))
	values := make([]float64, 0, len(result.Dependents))
	for _, d := range result.Dependents {
		line := d.CallLine
		if line == 0 {
			line = d.Line
		}
		provenance = append(provenance, output.Provenance{File: d.File, Line: line, Rule: d.Rule})
		values = append(values, d.Confidence)
	}

	summary := fmt.Sprintf("%d dependents across %d files and %d modules (deepest depth %d, min confidence %.2f)",
		len(result.Dependents), len(result.Files), len(result.Modules), result.Depth, minConfidence)
	if len(result.Dependents) == 0 {
		summary = fmt.Sprintf("nothing reaches %s at or above confidence %.2f", symbol, minConfidence)
	}
	return resultOf(output.New(payload, summary, weakest(values, 1.0), result.Truncated, provenance)), nil
}

func (s *server) path(ctx context.Context, args map[string]json.RawMessage) (toolResult, error) {
	from, err := requiredString(args, "from")
	if err != nil {
		return toolResult{}, err
	}
	to, err := requiredString(args, "to")
	if err != nil {
		return toolResult{}, err
	}
	maxDepth, err := optionalInt(args, "max_depth", query.DefaultDepth)
	if err != nil {
		return toolResult{}, err
	}
	minConfidence, err := optionalFloat(args, "min_confidence", query.DefaultMinConfidence)
	if err != nil {
		return toolResult{}, err
	}
	session, err := s.sessionFor(ctx)
	if err != nil {
		return toolResult{}, err
	}

	answer, code, err := query.Path(ctx, session.Store, from, to, maxDepth, minConfidence, 3)
	if err != nil {
		return toolResult{}, err
	}
	if code != 0 {
		if answer.Failed != nil {
			return targetError(answer.Failed.Code, answer.Failed.Arg, answer.Failed.Target), nil
		}
		return errorResult(code, "could not resolve path endpoints", nil), nil
	}

	result := answer.Result
	payload := struct {
		graph.PathResult
		MinConfidence float64 `json:"min_confidence"`
		MaxDepth      int     `json:"max_depth"`
	}{result, minConfidence, answer.MaxDepth}

	provenance := make([]output.Provenance, 0, len(result.Hops))
	values := make([]float64, 0, len(result.Hops))
	chain := []string{result.From.QName}
	for _, hop := range result.Hops {
		provenance = append(provenance, output.Provenance{File: hop.Dst.File, Line: hop.Line, Rule: hop.Rule})
		values = append(values, hop.Confidence)
		chain = append(chain, hop.Dst.QName)
	}

	summary := fmt.Sprintf("%d hops: %s; weakest link %.2f",
		len(result.Hops), strings.Join(chain, " → "), weakest(values, 0))
	if len(result.Hops) == 0 {
		summary = fmt.Sprintf("no path from %s to %s within depth %d",
			result.From.QName, result.To.QName, answer.MaxDepth)
	}
	return resultOf(output.New(payload, summary, weakest(values, 0), false, provenance)), nil
}

func (s *server) callers(ctx context.Context, args map[string]json.RawMessage) (toolResult, error) {
	symbol, err := requiredString(args, "symbol")
	if err != nil {
		return toolResult{}, err
	}
	depth, err := optionalInt(args, "depth", 1)
	if err != nil {
		return toolResult{}, err
	}
	if depth != 1 {
		return toolResult{}, argumentError("callers supports depth 1; use impact for transitive reach")
	}
	session, err := s.sessionFor(ctx)
	if err != nil {
		return toolResult{}, err
	}

	answer, code, err := query.Callers(ctx, session.Store, symbol, query.DefaultMinConfidence, query.DefaultCallersLimit)
	if err != nil {
		return toolResult{}, err
	}
	if code != 0 {
		return targetError(code, symbol, answer.Target), nil
	}

	result := answer.Result
	provenance := make([]output.Provenance, 0, len(result.Callers))
	values := make([]float64, 0, len(result.Callers))
	for _, caller := range result.Callers {
		line := caller.CallLine
		if line == 0 {
			line = caller.Line
		}
		provenance = append(provenance, output.Provenance{File: caller.File, Line: line, Rule: caller.Rule})
		values = append(values, caller.Confidence)
	}

	summary := fmt.Sprintf("%d callers of %s at or above confidence %.2f",
		len(result.Callers), result.Symbol.QName, result.MinConfidence)
	if result.Truncated {
		summary += " (truncated at " + fmt.Sprint(query.DefaultCallersLimit) + ")"
	}
	return resultOf(output.New(result, summary, weakest(values, 1.0), result.Truncated, provenance)), nil
}

func (s *server) repoMap(ctx context.Context, args map[string]json.RawMessage) (toolResult, error) {
	maxTokens, err := optionalInt(args, "max_tokens", query.DefaultMaxTokens)
	if err != nil {
		return toolResult{}, err
	}
	session, err := s.sessionFor(ctx)
	if err != nil {
		return toolResult{}, err
	}

	answer, err := query.Map(ctx, session.Store, maxTokens, query.DefaultMinConfidence)
	if err != nil {
		return toolResult{}, err
	}

	result := answer.Result
	payload := struct {
		graph.MapResult
		MaxTokens     int     `json:"max_tokens"`
		MinConfidence float64 `json:"min_confidence"`
	}{result, answer.MaxTokens, answer.MinConfidence}

	provenance := make([]output.Provenance, 0, len(result.Top))
	for _, symbol := range result.Top {
		provenance = append(provenance, output.Provenance{File: symbol.File, Line: symbol.Line, Rule: "pagerank"})
	}

	top := "none"
	if len(result.Top) > 0 {
		top = fmt.Sprintf("%s (rank %.4f)", result.Top[0].QName, result.Top[0].Rank)
	}
	summary := fmt.Sprintf("%d modules, %d cycles, %d ranked symbols; top %s",
		len(result.Modules), len(result.Cycles), len(result.Top), top)
	return resultOf(output.New(payload, summary, 1.0, result.Truncated, provenance)), nil
}

func (s *server) status(ctx context.Context) (toolResult, error) {
	session, err := s.sessionFor(ctx)
	if err != nil {
		return toolResult{}, err
	}

	report, err := query.Status(ctx, session.Store, session.Root, true, false)
	if err != nil {
		return toolResult{}, err
	}
	report.Version = s.version

	totalEdges := 0
	for _, n := range report.Edges {
		totalEdges += n
	}
	summary := fmt.Sprintf("%d files, %d nodes, %d edges; unresolved %d (%.1f%%); %d parse failures",
		report.Files, report.Nodes, totalEdges, report.Unresolved, report.UnresolvedRate*100, report.ParseFailures)
	if report.Stale {
		summary += "; index is stale"
	}
	if report.DirtyFiles > 0 {
		summary += fmt.Sprintf("; %d dirty files", report.DirtyFiles)
	}
	return resultOf(output.New(report, summary, 1.0, false, nil)), nil
}

func weakest(values []float64, fallback float64) float64 {
	if len(values) == 0 {
		return fallback
	}
	min := values[0]
	for _, value := range values[1:] {
		if value < min {
			min = value
		}
	}
	return min
}
