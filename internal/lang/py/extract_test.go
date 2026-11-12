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
