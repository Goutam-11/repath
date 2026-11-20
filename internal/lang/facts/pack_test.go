package facts

import (
	"reflect"
	"strings"
	"testing"
)

func TestPackRefsRoundTrip(t *testing.T) {
	refs := []Ref{
		{},
		{Kind: "call", Name: "parse", Form: "ident", Arity: 2, Line: 17, Enclosing: "run"},
		{
			Kind: "call", Name: "total", Form: "member", Receiver: "cart",
			ReceiverType: "Cart", ReceiverBound: "Describe|Copy", ReceiverFrom: "return",
			ReceiverParam: true, Arity: 0, Line: 1, Enclosing: "Cart.total",
		},
		{Kind: "extends", Name: "Base", Form: "ident", Line: 9, Enclosing: "Child"},
		{Kind: "implements", Name: "Shape", Form: "ident", Line: 1, Enclosing: "Circle"},
		{Kind: "decorator", Name: "logged", Form: "ident", Line: 3, Enclosing: "Cached.value"},
		{Kind: "call", Name: "считай", Form: "ident", Arity: 1, Line: 42, Enclosing: "функция"},
	}
	refs[2].ReceiverParam = true

	packed := PackRefs(refs)
	if len(packed) == 0 {
		t.Fatal("packed refs are empty")
	}
	got, err := UnpackRefs(packed)
	if err != nil {
		t.Fatalf("unpack: %v", err)
	}
	if !reflect.DeepEqual(got, refs) {
		t.Fatalf("round trip changed refs:\n got: %+v\nwant: %+v", got, refs)
	}
}

func TestPackRefsEmptyAndNil(t *testing.T) {
	for _, refs := range [][]Ref{nil, {}} {
		packed := PackRefs(refs)
		if packed != nil {
			t.Fatalf("packed empty refs = %v, want nil", packed)
		}
		got, err := UnpackRefs(packed)
		if err != nil || got != nil {
			t.Fatalf("unpack empty = %+v, %v", got, err)
		}
	}
}

func TestUnpackRefsRejectsMalformedInput(t *testing.T) {
	good := PackRefs([]Ref{{Kind: "call", Name: "f", Form: "ident", Arity: 1, Line: 2, Enclosing: "g"}})
	cases := []struct {
		name string
		data []byte
		want string
	}{
		{"bad count", []byte{0xff}, "bad count"},
		{"impossible count", []byte{0x40}, "exceeds"},
		{"truncated", good[:len(good)-1], ""},
		{"trailing", append(append([]byte{}, good...), 0x00), "trailing"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := UnpackRefs(tc.data)
			if err == nil || (tc.want != "" && !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestPackRefsIsSmallerThanStructs(t *testing.T) {
	refs := make([]Ref, 0, 500)
	for i := 0; i < 500; i++ {
		refs = append(refs, Ref{
			Kind: "call", Name: "get", Form: "member", Receiver: "obj",
			Line: i + 1, Enclosing: "Service.run",
		})
	}
	packed := PackRefs(refs)
	if perRef := len(packed) / len(refs); perRef > 60 {
		t.Fatalf("packed ref = %d bytes, want a compact encoding", perRef)
	}
}
