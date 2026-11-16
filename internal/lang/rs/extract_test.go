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

// TestExtractGenericBounds pins the bound facts a receiver needs: the type
// parameter is marked generic and carries its declared trait paths, from both
// the inline bound list and a where clause.
func TestExtractGenericBounds(t *testing.T) {
	src := `pub trait Describe { fn describe(&self) -> u32; }
fn bounded<T: Describe>(value: T) -> u32 { value.describe() }
fn where_clause<T>(value: T) -> u32 where T: Describe { value.describe() }
struct Pair<T> { inner: T }
impl<T: Describe> Pair<T> { fn label(&self) -> u32 { self.inner.describe() } }
`
	result := New().Extract([]byte(src), Rust)
	if !result.OK {
		t.Fatalf("parse failed: %s", result.Err)
	}
	type want struct {
		enclosing, name, recvType, bound string
	}
	cases := []want{
		{"bounded", "describe", "T", "Describe"},
		{"where_clause", "describe", "T", "Describe"},
		{"Pair.label", "describe", "T", "Describe"},
	}
	for _, c := range cases {
		found := false
		for _, ref := range result.Refs {
			if ref.Enclosing != c.enclosing || ref.Name != c.name {
				continue
			}
			found = true
			if ref.ReceiverType != c.recvType || ref.ReceiverBound != c.bound || !ref.ReceiverParam {
				t.Errorf("%s.%s = type %q bound %q param %v, want %q/%q/true",
					c.enclosing, c.name, ref.ReceiverType, ref.ReceiverBound, ref.ReceiverParam, c.recvType, c.bound)
			}
		}
		if !found {
			t.Errorf("missing ref %s.%s", c.enclosing, c.name)
		}
	}
}

// TestExtractModuleLayout pins the reserved import bindings that carry module
// layout: an inline module (empty source), a #[path] value, and a supertrait.
func TestExtractModuleLayout(t *testing.T) {
	src := `#[path = "other/file.rs"]
mod attr_mod;
mod inline_mod {
    pub fn helper() -> u32 { 1 }
}
pub trait Root { fn ping(&self); }
pub trait Child: Root { fn pong(&self); }
`
	result := New().Extract([]byte(src), Rust)
	if !result.OK {
		t.Fatalf("parse failed: %s", result.Err)
	}
	got := make(map[string]Import, len(result.Imports))
	for _, imp := range result.Imports {
		got[imp.Local] = imp
	}
	if imp, ok := got["@mod:attr_mod"]; !ok || imp.Source != "other/file.rs" {
		t.Errorf("@mod:attr_mod = %+v, want source other/file.rs", imp)
	}
	if imp, ok := got["@mod:inline_mod"]; !ok || imp.Source != "" {
		t.Errorf("@mod:inline_mod = %+v, want an inline module with no source", imp)
	}
	if imp, ok := got["@trait:Child#0"]; !ok || imp.Imported != "Root" {
		t.Errorf("@trait:Child#0 = %+v, want supertrait Root", imp)
	}
}

// TestExtractReturnAndAssocTypes pins the same-file return inference: a call
// result is typed only by a local return definition, including an associated
// type bound by the same file's impl.
func TestExtractReturnAndAssocTypes(t *testing.T) {
	src := `pub struct S { n: u32 }
impl S {
    pub fn make() -> Self { S { n: 0 } }
    pub fn bump(&self) -> u32 { self.n }
}
pub fn make_s() -> S { S::make() }
pub fn use_it() -> u32 { let s = make_s(); s.bump() }
fn chained() -> u32 { make_s().bump() }
pub trait Build { type Made; fn build(&self) -> Self::Made; }
impl Build for S { type Made = S; fn build(&self) -> Self::Made { S { n: 1 } } }
fn via(s: &S) -> u32 { let made = s.build(); made.bump() }
`
	result := New().Extract([]byte(src), Rust)
	if !result.OK {
		t.Fatalf("parse failed: %s", result.Err)
	}
	got := make(map[string]Ref, len(result.Refs))
	for _, ref := range result.Refs {
		got[ref.Enclosing+":"+ref.Name] = ref
	}
	for _, key := range []string{"use_it:bump", "chained:bump", "via:bump"} {
		ref, ok := got[key]
		if !ok {
			t.Fatalf("missing ref %q", key)
		}
		if ref.ReceiverType != "S" || ref.ReceiverFrom != "return" {
			t.Errorf("%s = type %q from %q, want S/return", key, ref.ReceiverType, ref.ReceiverFrom)
		}
	}
}
