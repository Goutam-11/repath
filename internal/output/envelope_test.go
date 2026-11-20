package output

import (
	"encoding/json"
	"testing"
)

func TestCapProvenanceDedupesAndBounds(t *testing.T) {
	a := Provenance{File: "a.ts", Line: 1, Rule: "ts.local"}
	b := Provenance{File: "b.ts", Line: 2, Rule: "ts.import.named"}
	c := Provenance{File: "c.ts", Line: 3, Rule: "ts.global.ambiguous"}

	got := CapProvenance([]Provenance{a, a, b, a, c, b}, 2)
	if len(got) != 2 {
		t.Fatalf("capped provenance = %+v, want 2 entries", got)
	}
	if got[0] != a || got[1] != b {
		t.Fatalf("cap must preserve first-seen order: %+v", got)
	}

	// A zero limit means "no cap"; duplicates still collapse.
	all := CapProvenance([]Provenance{a, a, b, c}, 0)
	if len(all) != 3 {
		t.Fatalf("uncapped provenance = %+v, want 3 unique entries", all)
	}
}

func TestNewAlwaysCarriesBoundedProvenance(t *testing.T) {
	items := make([]Provenance, 0, 60)
	for i := 0; i < 60; i++ {
		items = append(items, Provenance{File: "f.ts", Line: i + 1, Rule: "ts.local"})
	}

	envelope := New(map[string]int{"n": 1}, "summary", 0.9, true, items)
	if len(envelope.Provenance) != 50 {
		t.Fatalf("provenance len = %d, want the 50-entry cap", len(envelope.Provenance))
	}
	if !envelope.Truncated || envelope.Confidence != 0.9 || envelope.Summary != "summary" {
		t.Fatalf("envelope fields lost: %+v", envelope)
	}

	empty := New(nil, "nothing", 0, false, nil)
	if empty.Provenance == nil || len(empty.Provenance) != 0 {
		t.Fatalf("empty provenance must be non-nil: %#v", empty.Provenance)
	}
}

func TestMarshalKeepsProvenanceArrayShape(t *testing.T) {
	encoded, err := Marshal(Envelope{Answer: "x", Provenance: nil})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var provenance []Provenance
	if err := json.Unmarshal(raw["provenance"], &provenance); err != nil {
		t.Fatalf("provenance must decode as an array: %v", err)
	}
	if provenance == nil || len(provenance) != 0 {
		t.Fatalf("provenance = %#v, want []", provenance)
	}
	for _, key := range []string{"answer", "summary", "confidence", "truncated"} {
		if _, ok := raw[key]; !ok {
			t.Fatalf("envelope missing %q: %s", key, encoded)
		}
	}
}
