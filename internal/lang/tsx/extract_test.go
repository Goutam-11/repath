package tsx

import (
	"strings"
	"testing"
)

const sample = `export function greet(name: string, age: number): string {
  return name;
}

class Cart {
  add(item: string) {
    return item;
  }
  total(): number {
    return 0;
  }
}

const handler = async (event: string) => event;

interface Shape {
  area(): number;
}

type ID = string;

enum Color {
  Red,
  Green,
}

export class Exported {
  ping() {}
}
`

func byQName(symbols []Symbol) map[string]Symbol {
	out := make(map[string]Symbol, len(symbols))
	for _, s := range symbols {
		out[s.QName] = s
	}
	return out
}

func TestExtractDefinitions(t *testing.T) {
	symbols, ok, errMsg := New().Extract([]byte(sample), TypeScript)
	if !ok {
		t.Fatalf("parse failed: %s", errMsg)
	}

	got := byQName(symbols)
	cases := []struct {
		qname  string
		kind   string
		arity  int
		export bool
	}{
		{"greet", "function", 2, true},
		{"Cart", "class", 0, false},
		{"Cart.add", "method", 1, false},
		{"Cart.total", "method", 0, false},
		{"handler", "function", 1, false},
		{"Shape", "interface", 0, false},
		{"ID", "type", 0, false},
		{"Color", "enum", 0, false},
		{"Exported", "class", 0, true},
		{"Exported.ping", "method", 0, true},
	}

	for _, c := range cases {
		sym, found := got[c.qname]
		if !found {
			t.Errorf("missing symbol %q (have %v)", c.qname, keys(got))
			continue
		}
		if sym.Kind != c.kind {
			t.Errorf("%s kind = %q, want %q", c.qname, sym.Kind, c.kind)
		}
		if sym.Arity != c.arity {
			t.Errorf("%s arity = %d, want %d", c.qname, sym.Arity, c.arity)
		}
		if sym.Exported != c.export {
			t.Errorf("%s exported = %v, want %v", c.qname, sym.Exported, c.export)
		}
		if sym.StartLine == 0 || sym.EndLine < sym.StartLine {
			t.Errorf("%s has bad lines %d..%d", c.qname, sym.StartLine, sym.EndLine)
		}
	}
}

func TestExtractReportsSyntaxError(t *testing.T) {
	_, ok, errMsg := New().Extract([]byte("function broken( {\n  return 1;\n}\n"), TypeScript)
	if ok {
		t.Fatal("ok = true for a broken file")
	}
	if !strings.Contains(errMsg, "syntax error") {
		t.Fatalf("errMsg = %q, want a syntax error", errMsg)
	}
}

func TestExtractEmptyFile(t *testing.T) {
	symbols, ok, _ := New().Extract(nil, TypeScript)
	if !ok {
		t.Fatal("empty file reported as broken")
	}
	if len(symbols) != 0 {
		t.Fatalf("symbols = %d, want 0", len(symbols))
	}
}

func keys(m map[string]Symbol) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
