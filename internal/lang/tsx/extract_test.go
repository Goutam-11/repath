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
	result := New().Extract([]byte(sample), TypeScript)
	if !result.OK {
		t.Fatalf("parse failed: %s", result.Err)
	}

	got := byQName(result.Symbols)
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
	result := New().Extract([]byte("function broken( {\n  return 1;\n}\n"), TypeScript)
	if result.OK {
		t.Fatal("OK = true for a broken file")
	}
	if !strings.Contains(result.Err, "syntax error") {
		t.Fatalf("Err = %q, want a syntax error", result.Err)
	}
}

func TestExtractEmptyFile(t *testing.T) {
	result := New().Extract(nil, TypeScript)
	if !result.OK {
		t.Fatal("empty file reported as broken")
	}
	if len(result.Symbols) != 0 || len(result.Refs) != 0 || len(result.Imports) != 0 {
		t.Fatalf("empty file produced %d symbols, %d refs, %d imports",
			len(result.Symbols), len(result.Refs), len(result.Imports))
	}
}

func TestExtractRefsAndImports(t *testing.T) {
	src := `import { add } from "./util";
import * as helpers from "./helpers";

export class Calc {
  run(a: number) {
    return add(a, helpers.clamp(a));
  }
}

export function total(n: number) {
  return new Calc().run(n);
}
`
	result := New().Extract([]byte(src), TypeScript)
	if !result.OK {
		t.Fatalf("parse failed: %s", result.Err)
	}

	if len(result.Imports) != 2 {
		t.Fatalf("imports = %d, want 2: %+v", len(result.Imports), result.Imports)
	}
	if result.Imports[0].Local != "add" || result.Imports[0].Imported != "add" || result.Imports[0].Source != "./util" {
		t.Fatalf("named import = %+v", result.Imports[0])
	}
	if result.Imports[1].Imported != "*" || result.Imports[1].Local != "helpers" {
		t.Fatalf("namespace import = %+v", result.Imports[1])
	}

	found := map[string]Ref{}
	for _, ref := range result.Refs {
		found[ref.Name] = ref
	}
	if ref, ok := found["add"]; !ok || ref.Form != "ident" || ref.Enclosing != "Calc.run" {
		t.Fatalf("add ref = %+v", found["add"])
	}
	if ref, ok := found["clamp"]; !ok || ref.Form != "member" || ref.Receiver != "helpers" {
		t.Fatalf("clamp ref = %+v", found["clamp"])
	}
	if ref, ok := found["Calc"]; !ok || ref.Kind != "new" {
		t.Fatalf("Calc ref = %+v", found["Calc"])
	}
}

func keys(m map[string]Symbol) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestExtractBarrelAndDynamicImports pins the #56 extraction facts: `export *`,
// named/aliased/default re-exports, and static require()/import() bindings.
func TestExtractBarrelAndDynamicImports(t *testing.T) {
	src := `export * from "./util";
export * from "./more";
export { clamp, add as plus } from "./math";
export { default as fmt, default } from "./format";
export * as ns from "./ns";
export { localName };
import eq = require("./eq");

const tools = require("./util");
const { a, b: renamed, c = 1, ...rest } = require("./destructured");
const later = await import("./later");
const webpack = await import(/* webpackChunkName: "chunk" */ "./webpack");
const required = require(/* comment */ "./required");
const computed = await import(` + "`./${name}`" + `);
`
	result := New().Extract([]byte(src), TypeScript)
	if !result.OK {
		t.Fatalf("parse failed: %s", result.Err)
	}

	type binding struct {
		local, imported, source string
		public                  bool
	}
	var got []binding
	for _, imp := range result.Imports {
		got = append(got, binding{imp.Local, imp.Imported, imp.Source, imp.Public})
	}

	want := []binding{
		// `export *` keeps one binding per source: two of them must survive.
		{"*", "*", "./util", true},
		{"*", "*", "./more", true},
		{"clamp", "clamp", "./math", true},
		{"plus", "add", "./math", true},
		{"fmt", "default", "./format", true},
		{"default", "default", "./format", true},
		// `export * as ns` is deliberately not followed, and a sourceless
		// `export { localName }` creates no binding.
		{"eq", "*", "./eq", false},
		{"tools", "*", "./util", false},
		{"a", "a", "./destructured", false},
		{"renamed", "b", "./destructured", false},
		{"c", "c", "./destructured", false},
		{"later", "*", "./later", false},
		{"webpack", "*", "./webpack", false},
		{"required", "*", "./required", false},
	}
	if len(got) != len(want) {
		t.Fatalf("imports = %d, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("import %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	// A computed specifier must not create a binding.
	for _, imp := range result.Imports {
		if imp.Source == "./${name}" || imp.Source == "" {
			t.Errorf("computed specifier produced a binding: %+v", imp)
		}
	}
}

func TestExtractHTTPAnchors(t *testing.T) {
	src := []byte(`export async function load() { return fetch("/api/users"); }
export async function create() { return fetch("/api/users", { method: "POST" }); }
export async function put() { return axios.put("/api/items", body); }
export async function dynamic(id: string) { return fetch(` + "`/api/users/${id}`" + `); }
app.get("/api/products", products);`)
	res := New().Extract(src, TypeScript)
	if !res.OK {
		t.Fatalf("parse failed: %s", res.Err)
	}
	want := []Anchor{
		{QName: "load", Protocol: "http", Role: "client", Method: "GET", Path: "/api/users", Line: 1, Confidence: 0.90},
		{QName: "create", Protocol: "http", Role: "client", Method: "POST", Path: "/api/users", Line: 2, Confidence: 0.90},
		{QName: "put", Protocol: "http", Role: "client", Method: "PUT", Path: "/api/items", Line: 3, Confidence: 0.90},
		{QName: "products", Protocol: "http", Role: "server", Method: "GET", Path: "/api/products", Line: 5, Confidence: 0.90},
	}
	if len(res.Anchors) != len(want) {
		t.Fatalf("anchors = %+v, want %d", res.Anchors, len(want))
	}
	for i := range want {
		if res.Anchors[i] != want[i] {
			t.Fatalf("anchor %d = %+v, want %+v", i, res.Anchors[i], want[i])
		}
	}
}
