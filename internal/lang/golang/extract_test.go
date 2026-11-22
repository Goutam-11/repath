package golang

import (
	"strings"
	"testing"
)

func byQName(symbols []Symbol) map[string]Symbol {
	out := make(map[string]Symbol, len(symbols))
	for _, s := range symbols {
		out[s.QName] = s
	}
	return out
}

func TestExtractDeclarations(t *testing.T) {
	src := `package demo

type Reader interface {
	Read(p []byte) (int, error)
	Close() error
}

type Base struct{ ID int }

type User struct {
	Base
	Name string
}

type ID string

var Global = 1

const KindA = iota

func (u *User) Greet(prefix string) string { return prefix }
func (u User) Label() string { return u.Name }
func NewUser(name string) *User { return &User{Name: name} }
`
	res := New().Extract([]byte(src), Go)
	if !res.OK {
		t.Fatalf("parse failed: %s", res.Err)
	}
	got := byQName(res.Symbols)
	cases := []struct {
		qname  string
		kind   string
		arity  int
		export bool
	}{
		{"Reader", "interface", 0, true},
		{"Reader.Read", "method", 1, true},
		{"Reader.Close", "method", 0, true},
		{"Base", "struct", 0, true},
		{"User", "struct", 0, true},
		{"ID", "type", 0, true},
		{"Global", "var", 0, true},
		{"KindA", "const", 0, true},
		{"User.Greet", "method", 1, true},
		{"User.Label", "method", 0, true},
		{"NewUser", "function", 1, true},
	}
	for _, c := range cases {
		s, ok := got[c.qname]
		if !ok {
			t.Fatalf("missing symbol %q (have %v)", c.qname, keys(got))
		}
		if s.Kind != c.kind || s.Arity != c.arity || s.Exported != c.export {
			t.Fatalf("%s = %+v, want kind=%s arity=%d exported=%t", c.qname, s, c.kind, c.arity, c.export)
		}
	}
}

func TestExtractImportsAndEntrypoints(t *testing.T) {
	src := `package main

import (
	"fmt"
	str "strings"

	"example.com/app/store"
)

func main() {
	_ = fmt.Sprint(store.Name)
	_ = str.ToUpper("x")
}

func TestThing(t *testing.T) {}

func BenchmarkThing(b *testing.B) {}
`
	res := New().Extract([]byte(src), Go)
	if !res.OK {
		t.Fatalf("parse failed: %s", res.Err)
	}
	imports := map[string]string{}
	for _, imp := range res.Imports {
		imports[imp.Local] = imp.Source
	}
	if imports["fmt"] != "fmt" || imports["str"] != "strings" || imports["store"] != "example.com/app/store" {
		t.Fatalf("imports = %+v", res.Imports)
	}
	rules := map[string]string{}
	for _, ep := range res.Entrypoints {
		rules[ep.QName] = ep.Rule
	}
	if rules["main"] != "go.main" {
		t.Fatalf("main entrypoint = %+v", res.Entrypoints)
	}
	for _, name := range []string{"TestThing", "BenchmarkThing"} {
		if rules[name] != "go.test" {
			t.Fatalf("%s entrypoint = %q, want go.test", name, rules[name])
		}
	}
}

func TestExtractReceiverTyping(t *testing.T) {
	src := `package demo

import "example.com/app/store"

type User struct{ Name string }

func (u *User) Greet() string { return u.Name }
func (s *store.Store) unused() {}

func run() {
	u := &User{}
	u.Greet()
	var v User
	v.Greet()
	p := store.New()
	_ = p
}
`
	res := New().Extract([]byte(src), Go)
	if !res.OK {
		t.Fatalf("parse failed: %s", res.Err)
	}
	typed := map[string]string{}
	for _, ref := range res.Refs {
		if ref.Kind == "call" && ref.Form == "member" {
			typed[ref.Receiver] = ref.ReceiverType
		}
	}
	if typed["u"] != "User" || typed["v"] != "User" {
		t.Fatalf("receiver types = %+v, want u and v typed User", typed)
	}
}

func TestExtractSyntaxError(t *testing.T) {
	res := New().Extract([]byte("package demo\n\nfunc broken( {\n"), Go)
	if res.OK {
		t.Fatal("OK = true for a broken file")
	}
	if !strings.Contains(res.Err, "syntax error") {
		t.Fatalf("Err = %q", res.Err)
	}
}

func keys(m map[string]Symbol) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
