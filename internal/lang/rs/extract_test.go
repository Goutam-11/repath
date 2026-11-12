package rs

import (
	"strings"
	"testing"
)

const sample = `use crate::models::{User, Role as R};
use crate::store::{self, Store};
use super::util::*;
use std::collections::HashMap;

pub struct Service {
    store: Store,
    user: User,
}

pub trait Greeter {
    fn greet(&self) -> String;
}

impl Greeter for Service {
    fn greet(&self) -> String {
        self.store.load()
    }
}

impl Service {
    pub fn new(store: Store) -> Self {
        Self { store, user: User::new() }
    }
    pub fn total(&self) -> u32 {
        let s = &self.store;
        s.count() + helper(1)
    }
    fn hidden(&self) {}
}

pub fn helper(x: u32) -> u32 { x }

fn run() {
    let svc = Service::new(Store::open());
    let n: u32 = svc.total();
    let u = User { name: String::new() };
    println!("{}", n);
}

macro_rules! blind {
    () => { hidden_macro_call() };
}
`

const usesSample = `use crate::foo::{self, Bar, baz as qux, nested::{A, B}};
use crate::foo as f;
pub use crate::models::User;
use crate::{alpha, beta};
use std::collections::HashMap;

pub enum Color { Red, Green(u8), Blue { level: u8 } }
pub const LIMIT: u32 = 10;
pub static NAME: &str = "x";
pub type Alias<T> = Vec<T>;

mod inline {
    pub fn inner() -> u32 { helper() }
}
mod external;

pub trait Shape {
    fn area(&self) -> u32;
    fn name(&self) -> String { "shape".to_string() }
}

impl Shape for Wrapper {
    fn area(&self) -> u32 { 1 }
}

pub struct Wrapper<T>(pub T);
pub struct Unit;
pub struct Tuple(u32, String);

fn generic<T: Copy>(v: T) -> T { v }

fn calls() {
    let a = foo::bar::<u32>(1);
    let b = Vec::<u8>::new();
    let c = Wrapper::<u32>(1);
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
	result := New().Extract([]byte(sample), Rust)
	if !result.OK {
		t.Fatalf("parse failed: %s", result.Err)
	}

	got := byQName(result.Symbols)
	cases := []struct {
		qname    string
		kind     string
		arity    int
		exported bool
	}{
		{"Service", "struct", 0, true},
		{"Greeter", "trait", 0, true},
		{"Greeter.greet", "method", 0, true},
		{"Service.greet", "method", 0, true},
		{"Service.new", "method", 1, true},
		{"Service.total", "method", 0, true},
		{"Service.hidden", "method", 0, false},
		{"helper", "function", 1, true},
		{"run", "function", 0, false},
	}

	for _, c := range cases {
		sym, found := got[c.qname]
		if !found {
			t.Errorf("missing symbol %q (have %v)", c.qname, symbolKeys(got))
			continue
		}
		if sym.Kind != c.kind {
			t.Errorf("%s kind = %q, want %q", c.qname, sym.Kind, c.kind)
		}
		if sym.Arity != c.arity {
			t.Errorf("%s arity = %d, want %d", c.qname, sym.Arity, c.arity)
		}
		if sym.Exported != c.exported {
			t.Errorf("%s exported = %v, want %v", c.qname, sym.Exported, c.exported)
		}
		if sym.StartLine == 0 || sym.EndLine < sym.StartLine {
			t.Errorf("%s has bad lines %d..%d", c.qname, sym.StartLine, sym.EndLine)
		}
	}
}

func TestExtractMoreItems(t *testing.T) {
	result := New().Extract([]byte(usesSample), Rust)
	if !result.OK {
		t.Fatalf("parse failed: %s", result.Err)
	}

	got := byQName(result.Symbols)
	cases := map[string]string{
		"Color":        "enum",
		"LIMIT":        "const",
		"NAME":         "const",
		"Alias":        "type",
		"inline.inner": "function",
		"Shape":        "trait",
		"Shape.area":   "method",
		"Shape.name":   "method",
		"Wrapper":      "struct",
		"Wrapper.area": "method",
		"Unit":         "struct",
		"Tuple":        "struct",
		"generic":      "function",
		"calls":        "function",
	}
	for qname, kind := range cases {
		sym, found := got[qname]
		if !found {
			t.Errorf("missing symbol %q (have %v)", qname, symbolKeys(got))
			continue
		}
		if sym.Kind != kind {
			t.Errorf("%s kind = %q, want %q", qname, sym.Kind, kind)
		}
	}
	if _, found := got["external"]; found {
		t.Error("external mod declaration produced a node; modules are dimensions, not symbols")
	}
}

func TestExtractImports(t *testing.T) {
	result := New().Extract([]byte(sample), Rust)
	if !result.OK {
		t.Fatalf("parse failed: %s", result.Err)
	}

	got := make(map[string]Import, len(result.Imports))
	for _, imp := range result.Imports {
		got[imp.Local] = imp
	}

	cases := []Import{
		{Local: "User", Imported: "User", Source: "crate::models"},
		{Local: "R", Imported: "Role", Source: "crate::models"},
		{Local: "store", Imported: "*", Source: "crate::store"},
		{Local: "Store", Imported: "Store", Source: "crate::store"},
		{Local: "*", Imported: "*", Source: "super::util"},
		{Local: "HashMap", Imported: "HashMap", Source: "std::collections"},
	}
	for _, want := range cases {
		imp, found := got[want.Local]
		if !found {
			t.Errorf("missing import %q (have %v)", want.Local, importKeys(got))
			continue
		}
		if imp.Imported != want.Imported || imp.Source != want.Source {
			t.Errorf("import %q = %+v, want imported=%q source=%q",
				want.Local, imp, want.Imported, want.Source)
		}
	}
}

func TestExtractUseTreesAndPublic(t *testing.T) {
	result := New().Extract([]byte(usesSample), Rust)
	if !result.OK {
		t.Fatalf("parse failed: %s", result.Err)
	}

	got := make(map[string]Import, len(result.Imports))
	for _, imp := range result.Imports {
		got[imp.Local] = imp
	}

	cases := []Import{
		{Local: "foo", Imported: "*", Source: "crate::foo"},
		{Local: "Bar", Imported: "Bar", Source: "crate::foo"},
		{Local: "qux", Imported: "baz", Source: "crate::foo"},
		{Local: "A", Imported: "A", Source: "crate::foo::nested"},
		{Local: "f", Imported: "foo", Source: "crate"},
		{Local: "User", Imported: "User", Source: "crate::models", Public: true},
		{Local: "alpha", Imported: "alpha", Source: "crate"},
	}
	for _, want := range cases {
		imp, found := got[want.Local]
		if !found {
			t.Errorf("missing import %q (have %v)", want.Local, importKeys(got))
			continue
		}
		if imp.Imported != want.Imported || imp.Source != want.Source || imp.Public != want.Public {
			t.Errorf("import %q = %+v, want imported=%q source=%q public=%v",
				want.Local, imp, want.Imported, want.Source, want.Public)
		}
	}
}

func TestExtractRefs(t *testing.T) {
	result := New().Extract([]byte(sample), Rust)
	if !result.OK {
		t.Fatalf("parse failed: %s", result.Err)
	}

	found := map[string]Ref{}
	for _, ref := range result.Refs {
		found[ref.Enclosing+":"+ref.Receiver+":"+ref.Name] = ref
	}

	cases := []struct {
		key      string
		kind     string
		form     string
		receiver string
		recvType string
	}{
		{"Service.greet:self.store:load", "call", "member", "self.store", "Store"},
		{"Service.new::Self", "new", "ident", "", ""},
		{"Service.new:User:new", "call", "scoped", "User", ""},
		{"run:Service:new", "call", "scoped", "Service", ""},
		{"run:Store:open", "call", "scoped", "Store", ""},
		{"run:svc:total", "call", "member", "svc", "Service"},
		{"run::User", "new", "ident", "", ""},
		{"run:String:new", "call", "scoped", "String", ""},
	}
	for _, c := range cases {
		ref, ok := found[c.key]
		if !ok {
			t.Errorf("missing ref %q (have %v)", c.key, refKeys(found))
			continue
		}
		if ref.Kind != c.kind || ref.Form != c.form {
			t.Errorf("%s = kind %q form %q, want %q/%q", c.key, ref.Kind, ref.Form, c.kind, c.form)
		}
		if ref.Receiver != c.receiver {
			t.Errorf("%s receiver = %q, want %q", c.key, ref.Receiver, c.receiver)
		}
		if ref.ReceiverType != c.recvType {
			t.Errorf("%s receiver type = %q, want %q", c.key, ref.ReceiverType, c.recvType)
		}
	}

	if ref, ok := found["run:println"]; ok {
		t.Fatalf("macro invocation produced a reference: %+v", ref)
	}
	if _, ok := found["blind:hidden_macro_call"]; ok {
		t.Fatal("macro body produced a reference; macro token trees must stay blind")
	}
}

func TestExtractTraitImpl(t *testing.T) {
	result := New().Extract([]byte(usesSample), Rust)
	if !result.OK {
		t.Fatalf("parse failed: %s", result.Err)
	}

	var impl Ref
	for _, ref := range result.Refs {
		if ref.Kind == "implements" {
			impl = ref
		}
	}
	if impl.Name != "Shape" || impl.Enclosing != "Wrapper" {
		t.Fatalf("implements ref = %+v, want Wrapper -> Shape", impl)
	}
}

func TestExtractReportsSyntaxError(t *testing.T) {
	result := New().Extract([]byte("fn broken( {\n    let x = 1;\n}\n"), Rust)
	if result.OK {
		t.Fatal("OK = true for a broken file")
	}
	if !strings.Contains(result.Err, "syntax error") {
		t.Fatalf("Err = %q, want a syntax error", result.Err)
	}
}

func TestExtractEmptyFile(t *testing.T) {
	result := New().Extract(nil, Rust)
	if !result.OK {
		t.Fatal("empty file reported as broken")
	}
	if len(result.Symbols) != 0 || len(result.Refs) != 0 || len(result.Imports) != 0 {
		t.Fatalf("empty file produced %d symbols, %d refs, %d imports",
			len(result.Symbols), len(result.Refs), len(result.Imports))
	}
}

func symbolKeys(m map[string]Symbol) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func importKeys(m map[string]Import) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func refKeys(m map[string]Ref) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
