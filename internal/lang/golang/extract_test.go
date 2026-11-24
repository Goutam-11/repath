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

var (
	BlockA, BlockB = 1, 2
	BlockC         string
)

const (
	KindB = iota
	KindC
)

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
		{"BlockA", "var", 0, true},
		{"BlockB", "var", 0, true},
		{"BlockC", "var", 0, true},
		{"KindA", "const", 0, true},
		{"KindB", "const", 0, true},
		{"KindC", "const", 0, true},
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

func TestExtractValueAndChainedRefs(t *testing.T) {
	src := `package demo

type Engine struct{ protocol string }

func (e *Engine) start() {}

type Holder struct{ engine *Engine }

func helper() {}

func register(f func()) {}

func init() { helper() }

func run(h *Holder) {
	register(helper)
	defer func() { helper() }()
	h.engine.start()
	local := 1
	register2(local)
}

func register2(f func()) {}
`
	res := New().Extract([]byte(src), Go)
	if !res.OK {
		t.Fatalf("parse failed: %s", res.Err)
	}

	var values, calls, chained []Ref
	for _, ref := range res.Refs {
		switch {
		case ref.Name == "helper" && ref.Kind == "ref":
			values = append(values, ref)
		case ref.Name == "helper" && ref.Kind == "call" && ref.Enclosing == "run":
			calls = append(calls, ref)
		case ref.Name == "start":
			chained = append(chained, ref)
		}
		if ref.Name == "local" || ref.Name == "f" {
			t.Fatalf("bound parameter or variable %q emitted a reference: %+v", ref.Name, ref)
		}
	}

	// A function value passed as an argument is a REFERENCES reference attached
	// to the enclosing function.
	if len(values) != 1 || values[0].Enclosing != "run" || values[0].Form != "ident" {
		t.Fatalf("function value refs = %+v, want one ident ref enclosed in run", values)
	}
	// A call inside a deferred function literal keeps the enclosing function as
	// its source (no "(closure)" pseudo-symbol).
	if len(calls) != 1 || calls[0].Enclosing != "run" {
		t.Fatalf("closure call refs = %+v, want one call enclosed in run", calls)
	}
	// A one-hop chained receiver is typed from the declared field.
	if len(chained) != 1 || chained[0].Form != "member" || chained[0].Receiver != "h.engine" ||
		chained[0].ReceiverType != "Engine" || chained[0].Enclosing != "run" {
		t.Fatalf("chained refs = %+v, want h.engine typed Engine", chained)
	}

	// The init body is walked like any other function.
	found := false
	for _, ref := range res.Refs {
		if ref.Name == "helper" && ref.Kind == "call" && ref.Enclosing == "init" {
			found = true
		}
	}
	if !found {
		t.Fatal("no call ref enclosed in init; init body was not walked")
	}
}

func TestExtractCallCalleeAndKeyedValue(t *testing.T) {
	src := `package demo

type Engine struct{}

func (e *Engine) start() {}

func makeEngine() *Engine { return &Engine{} }

type Registry struct{ handler func() }

func handler() {}

func build() Registry { return Registry{handler: handler} }

func run() { makeEngine().start() }
`
	res := New().Extract([]byte(src), Go)
	if !res.OK {
		t.Fatalf("parse failed: %s", res.Err)
	}
	var keyed, chained bool
	for _, ref := range res.Refs {
		if ref.Name == "handler" && ref.Kind == "ref" && ref.Enclosing == "build" {
			keyed = true
		}
		if ref.Name == "makeEngine" && ref.Kind == "call" && ref.Enclosing == "run" {
			chained = true
		}
	}
	if !keyed {
		t.Error("no value ref for a function value in a keyed composite-literal field")
	}
	if !chained {
		t.Error("no call ref for the callee of a chained call (makeEngine().start())")
	}
}

func TestExtractUnresolvedChainedReceiverStaysUntyped(t *testing.T) {
	src := `package demo

type Holder struct{ engine *Engine }

func run(h *Holder) {
	h.engine.start()
	h.missing.stop()
}
`
	res := New().Extract([]byte(src), Go)
	if !res.OK {
		t.Fatalf("parse failed: %s", res.Err)
	}
	types := map[string]string{}
	for _, ref := range res.Refs {
		if ref.Kind == "call" && ref.Form == "member" {
			types[ref.Receiver] = ref.ReceiverType
		}
	}
	// The field exists: Engine is not declared here, so the extractor still
	// records the declared field type and the resolver decides it is unknown.
	if types["h.engine"] != "Engine" {
		t.Fatalf("h.engine type = %q, want Engine", types["h.engine"])
	}
	// The field does not exist: no type, and no name-only guess.
	if types["h.missing"] != "" {
		t.Fatalf("h.missing type = %q, want empty", types["h.missing"])
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
