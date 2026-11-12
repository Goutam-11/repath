// Package output holds the answer envelope shared by every MCP tool
// (PLAN.md §7) and the small helpers that keep its provenance bounded.
package output

import "encoding/json"

// Provenance is one piece of evidence behind an answer.
type Provenance struct {
	File string `json:"file"`
	Line int    `json:"line"`
	Rule string `json:"rule"`
}

// Envelope is the one shape every MCP tool returns.
type Envelope struct {
	Answer     any          `json:"answer"`
	Summary    string       `json:"summary"`
	Confidence float64      `json:"confidence"`
	Truncated  bool         `json:"truncated"`
	Provenance []Provenance `json:"provenance"`
}

// New builds an envelope, always with a non-nil provenance array.
func New(answer any, summary string, confidence float64, truncated bool, provenance []Provenance) Envelope {
	return Envelope{
		Answer:     answer,
		Summary:    summary,
		Confidence: confidence,
		Truncated:  truncated,
		Provenance: CapProvenance(provenance, 50),
	}
}

// Marshal encodes the envelope compactly: MCP payloads are paid for by the
// client on every call, so indentation is not worth the tokens.
func Marshal(e Envelope) ([]byte, error) {
	if e.Provenance == nil {
		e.Provenance = []Provenance{}
	}
	return json.Marshal(e)
}

// CapProvenance removes duplicate (file, line, rule) evidence and keeps at most
// limit entries, preserving the strongest-first order the caller chose.
func CapProvenance(items []Provenance, limit int) []Provenance {
	out := make([]Provenance, 0, len(items))
	seen := make(map[Provenance]bool, len(items))
	for _, item := range items {
		if seen[item] {
			continue
		}
		seen[item] = true
		out = append(out, item)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}
