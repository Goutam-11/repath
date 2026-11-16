package py

import (
	"strings"
	"testing"
)

const sample = `from .util import clamp, parse as to_int
from ..pkg import helper
from . import util
import os.path as osp
import pkg.sub

@logged
class Service(Base, metaclass=ABCMeta):
    def __init__(self, amount: int):
        self.amount = amount

    @route("/total")
    def total(self):
        return clamp(self.amount) + to_int("1") + util.add(1)

    @property
    def label(self):
        return self.total()

def build(user: User) -> Service:
    service = Service(1)
    return service.total()
`

func byQName(symbols []Symbol) map[string]Symbol {
	out := make(map[string]Symbol, len(symbols))
	for _, s := range symbols {
		out[s.QName] = s
	}
	return out
}

func TestExtractDefinitions(t *testing.T) {
	result := New().Extract([]byte(sample), Python)
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
		{"Service", "class", 0, true},
		{"Service.__init__", "method", 1, true},
		{"Service.total", "method", 0, true},
		{"Service.label", "method", 0, true},
		{"build", "function", 1, true},
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
		if sym.Exported != c.exported {
			t.Errorf("%s exported = %v, want %v", c.qname, sym.Exported, c.exported)
		}
		if sym.StartLine == 0 || sym.EndLine < sym.StartLine {
			t.Errorf("%s has bad lines %d..%d", c.qname, sym.StartLine, sym.EndLine)
		}
	}
}

func TestExtractImports(t *testing.T) {
	result := New().Extract([]byte(sample), Python)
	if !result.OK {
		t.Fatalf("parse failed: %s", result.Err)
	}

	got := make(map[string]Import, len(result.Imports))
	for _, imp := range result.Imports {
		got[imp.Local] = imp
	}

	cases := []Import{
		{Local: "clamp", Imported: "clamp", Source: ".util"},
		{Local: "to_int", Imported: "parse", Source: ".util"},
		{Local: "helper", Imported: "helper", Source: "..pkg"},
		{Local: "util", Imported: "util", Source: "."},
		{Local: "osp", Imported: "*", Source: "os.path"},
		{Local: "pkg", Imported: "*", Source: "pkg.sub"},
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

func TestExtractRefs(t *testing.T) {
	result := New().Extract([]byte(sample), Python)
	if !result.OK {
		t.Fatalf("parse failed: %s", result.Err)
	}

	found := map[string]Ref{}
	for _, ref := range result.Refs {
		found[ref.Enclosing+":"+ref.Name] = ref
	}

	cases := []struct {
		key      string
		kind     string
		form     string
		receiver string
		recvType string
	}{
		{"Service:Base", "extends", "ident", "", ""},
		{"Service:logged", "decorator", "ident", "", ""},
		{"Service.total:route", "decorator", "ident", "", ""},
		{"Service.label:property", "decorator", "ident", "", ""},
		{"Service.total:clamp", "call", "ident", "", ""},
		{"Service.total:to_int", "call", "ident", "", ""},
		{"Service.total:add", "call", "member", "util", ""},
		{"Service.label:total", "call", "this", "", ""},
		{"build:Service", "call", "ident", "", ""},
		{"build:total", "call", "member", "service", "Service"},
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
}

func TestExtractNestedAndSelfAttributeType(t *testing.T) {
	src := `class Repo:
    def __init__(self):
        self.store = Store()

    def load(self):
        return self.store.get()

    def outer(self):
        def inner():
            return 1
        return inner()
`
	result := New().Extract([]byte(src), Python)
	if !result.OK {
		t.Fatalf("parse failed: %s", result.Err)
	}

	got := byQName(result.Symbols)
	for _, qname := range []string{"Repo", "Repo.__init__", "Repo.load", "Repo.outer", "Repo.outer.inner"} {
		if _, ok := got[qname]; !ok {
			t.Errorf("missing symbol %q (have %v)", qname, keys(got))
		}
	}
	if got["Repo.outer.inner"].Kind != "function" {
		t.Errorf("nested def kind = %q, want function", got["Repo.outer.inner"].Kind)
	}
	if got["Repo.outer.inner"].Exported {
		t.Error("nested def is marked exported")
	}

	var load Ref
	for _, ref := range result.Refs {
		if ref.Enclosing == "Repo.load" && ref.Name == "get" {
			load = ref
		}
	}
	if load.Receiver != "self.store" || load.ReceiverType != "Store" {
		t.Fatalf("self.store.get ref = %+v, want ReceiverType Store", load)
	}
}

func TestExtractReportsSyntaxError(t *testing.T) {
	result := New().Extract([]byte("def broken(:\n    return 1\n"), Python)
	if result.OK {
		t.Fatal("OK = true for a broken file")
	}
	if !strings.Contains(result.Err, "syntax error") {
		t.Fatalf("Err = %q, want a syntax error", result.Err)
	}
}

func TestExtractEmptyFile(t *testing.T) {
	result := New().Extract(nil, Python)
	if !result.OK {
		t.Fatal("empty file reported as broken")
	}
	if len(result.Symbols) != 0 || len(result.Refs) != 0 || len(result.Imports) != 0 {
		t.Fatalf("empty file produced %d symbols, %d refs, %d imports",
			len(result.Symbols), len(result.Refs), len(result.Imports))
	}
}

func keys(m map[string]Symbol) []string {
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

// TestExtractInheritanceAndReceivers covers the #54 extraction facts: super()
// calls, local factory return types, class attributes (including a same-file
// base), staticmethod arity, and the property alias decorator.
func TestExtractInheritanceAndReceivers(t *testing.T) {
	src := `class Service:
    pass

class Cache:
    def read(self):
        return 1

class Base:
    cache: Cache

    def step(self):
        return 1

class Child(Base):
    def go(self):
        self.cache.read()
        self.step()
        return super().step()

def make() -> Service:
    return Service()

def use() -> int:
    service = make()
    return service.total()

class Tools:
    @staticmethod
    def helper(x: int) -> int:
        return x

    @classmethod
    def build(cls) -> "Tools":
        return cls()

class Cached:
    @property
    def value(self) -> int:
        return 1

    @value.setter
    def value(self, new: int) -> None:
        self.raw = new
`
	result := New().Extract([]byte(src), Python)
	if !result.OK {
		t.Fatalf("parse failed: %s", result.Err)
	}

	byQNameSym := byQName(result.Symbols)
	if got := byQNameSym["Tools.helper"].Arity; got != 1 {
		t.Errorf("staticmethod arity = %d, want 1 (no bound first parameter)", got)
	}
	if got := byQNameSym["Tools.build"].Arity; got != 0 {
		t.Errorf("classmethod arity = %d, want 0 (cls is bound)", got)
	}

	found := make(map[string]Ref, len(result.Refs))
	for _, ref := range result.Refs {
		found[ref.Enclosing+":"+ref.Name+"@"+ref.Form] = ref
	}

	if ref, ok := found["Child.go:read@member"]; !ok {
		t.Errorf("missing self.cache.read ref (have %v)", refKeys(found))
	} else if ref.Receiver != "self.cache" || ref.ReceiverType != "Cache" {
		t.Errorf("self.cache.read = receiver %q type %q, want self.cache/Cache", ref.Receiver, ref.ReceiverType)
	}
	if ref, ok := found["Child.go:step@this"]; !ok {
		t.Errorf("missing self.step ref (have %v)", refKeys(found))
	} else if ref.ReceiverType != "" {
		t.Errorf("self.step receiver type = %q, want empty", ref.ReceiverType)
	}
	if ref, ok := found["Child.go:step@super"]; !ok {
		t.Errorf("missing super().step ref (have %v)", refKeys(found))
	} else if ref.Receiver != "" {
		t.Errorf("super().step receiver = %q, want empty", ref.Receiver)
	}
	if ref, ok := found["use:total@member"]; !ok {
		t.Errorf("missing factory-typed service.total ref (have %v)", refKeys(found))
	} else if ref.ReceiverType != "Service" {
		t.Errorf("factory-typed receiver = %q, want Service", ref.ReceiverType)
	}
	// The property setter decorator stays one member reference named after the
	// decorator attribute; the resolver maps it back to the property.
	if ref, ok := found["Cached.value:setter@member"]; !ok {
		t.Errorf("missing @value.setter ref (have %v)", refKeys(found))
	} else if ref.Kind != "decorator" || ref.Receiver != "value" {
		t.Errorf("@value.setter = kind %q receiver %q, want decorator/value", ref.Kind, ref.Receiver)
	}
}

// TestTypeNameUnwrapsUnions pins the annotation shapes that identify exactly one
// receiver type: a forward reference, Optional[X], PEP 604 `X | None`, and a
// quoted union. Two distinct members stay untyped instead of guessing.
func TestTypeNameUnwrapsUnions(t *testing.T) {
	src := `from typing import Optional

def use(a: "Service | None", b: Optional[Service], c: "Service", d: Service | None, e: Service | Other, f: "pkg.Service"):
    a.go()
    b.go()
    c.go()
    d.go()
    e.go()
    f.go()
`
	result := New().Extract([]byte(src), Python)
	if !result.OK {
		t.Fatalf("parse failed: %s", result.Err)
	}
	got := make(map[int]string)
	for _, ref := range result.Refs {
		if ref.Enclosing == "use" && ref.Name == "go" {
			got[ref.Line] = ref.ReceiverType
		}
	}
	// Lines 4-9 are the six a..f calls in order.
	want := []string{"Service", "Service", "Service", "Service", "", "Service"}
	for i, typ := range want {
		line := i + 4
		if got[line] != typ {
			t.Errorf("line %d receiver type = %q, want %q", line, got[line], typ)
		}
	}
}

// TestClassAttrOrderAndFactoryShadowing pins two ordering guards: a class
// attribute annotated after its first use is still typed, and a parameter that
// shadows a same-file factory name never borrows the factory's return type.
func TestClassAttrOrderAndFactoryShadowing(t *testing.T) {
	src := `class Cache:
    def read(self):
        return 1


class Repo:
    def load(self):
        return self.cache.read()

    cache: Cache


def make() -> Cache:
    return Cache()


def shadow(make):
    local = make()
    return local.read()


def use():
    made = make()
    return made.read()
`
	result := New().Extract([]byte(src), Python)
	if !result.OK {
		t.Fatalf("parse failed: %s", result.Err)
	}
	types := make(map[string]string)
	for _, ref := range result.Refs {
		if ref.Name == "read" {
			types[ref.Enclosing] = ref.ReceiverType
		}
	}
	if got := types["Repo.load"]; got != "Cache" {
		t.Errorf("self.cache.read type = %q, want Cache (annotation written after the method)", got)
	}
	if got := types["shadow"]; got != "" {
		t.Errorf("shadowed factory call type = %q, want empty", got)
	}
	if got := types["use"]; got != "Cache" {
		t.Errorf("factory-typed call type = %q, want Cache", got)
	}
}
