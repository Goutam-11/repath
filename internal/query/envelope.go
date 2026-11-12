package query

import (
	"fmt"
	"strings"

	"github.com/Goutam-11/repath/internal/graph"
	"github.com/Goutam-11/repath/internal/output"
)

// The envelopes here are the single definition of the MCP/HTTP answer shape
// (PLAN.md §7). The CLI text renderers use the typed results directly; the MCP
// adapter and the read-only HTTP server must not grow separate summary,
// confidence, or provenance logic.

// ErrorCandidate is one ambiguous-match candidate in an error envelope.
type ErrorCandidate struct {
	QName string `json:"qname"`
	Kind  string `json:"kind"`
}

// ErrorEnvelope is the envelope for a not-found, ambiguous, or bad-argument
// answer. Callers map Code to their transport (MCP isError, HTTP status).
func ErrorEnvelope(code int, message string, candidates []ErrorCandidate) output.Envelope {
	answer := map[string]any{"error": message, "code": code}
	if len(candidates) > 0 {
		answer["candidates"] = candidates
	}
	return output.New(answer, message, 0, false, nil)
}

// FindEnvelope is the find_symbol answer card.
func FindEnvelope(answer FindAnswer) output.Envelope {
	result := answer.Result
	provenance := []output.Provenance{{
		File: result.Symbol.File, Line: result.Symbol.Line, Rule: "definition",
	}}
	summary := fmt.Sprintf("%s %s at %s:%d · %d callers · %d callees",
		result.Symbol.QName, result.Symbol.Kind, result.Symbol.File, result.Symbol.Line,
		result.Callers, result.Callees)
	return output.New(result, summary, 1.0, false, provenance)
}

// ImpactEnvelope is the impact answer with min_confidence and edge provenance.
func ImpactEnvelope(answer ImpactAnswer) output.Envelope {
	result := answer.Result
	payload := struct {
		graph.Impact
		MinConfidence float64 `json:"min_confidence"`
	}{result, answer.MinConfidence}

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
		len(result.Dependents), len(result.Files), len(result.Modules), result.Depth, answer.MinConfidence)
	if len(result.Dependents) == 0 {
		summary = fmt.Sprintf("nothing reaches %s at or above confidence %.2f",
			answer.Target.Label, answer.MinConfidence)
	}
	return output.New(payload, summary, weakest(values, 1.0), result.Truncated, provenance)
}

// PathEnvelope is the path answer with both endpoints' depth/confidence.
func PathEnvelope(answer PathAnswer) output.Envelope {
	result := answer.Result
	payload := struct {
		graph.PathResult
		MinConfidence float64 `json:"min_confidence"`
		MaxDepth      int     `json:"max_depth"`
	}{result, answer.MinConfidence, answer.MaxDepth}

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
	return output.New(payload, summary, weakest(values, 0), false, provenance)
}

// CallersEnvelope is the direct-reverse-edge answer.
func CallersEnvelope(answer CallersAnswer) output.Envelope {
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
		summary += " (truncated at " + fmt.Sprint(DefaultCallersLimit) + ")"
	}
	return output.New(result, summary, weakest(values, 1.0), result.Truncated, provenance)
}

// MapEnvelope is the ranked repository overview.
func MapEnvelope(answer MapAnswer) output.Envelope {
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
	return output.New(payload, summary, 1.0, result.Truncated, provenance)
}

// StatusEnvelope is the index-health answer. The caller sets Version.
func StatusEnvelope(report StatusReport) output.Envelope {
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
	return output.New(report, summary, 1.0, false, nil)
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
