package resolve

import (
	"reflect"
	"testing"

	"github.com/Goutam-11/repath/internal/store"
)

func newMROIndex(bases map[int64][]int64) *Index {
	return &Index{bases: bases}
}

// TestMROLinearization pins the documented order: C3 when the hierarchy permits
// it, the class itself first, then bases left to right.
func TestMROLinearization(t *testing.T) {
	// 1 = Diamond(Left=2, Right=3), both deriving from Root=4.
	ix := newMROIndex(map[int64][]int64{
		1: {2, 3},
		2: {4},
		3: {4},
	})
	want := []int64{1, 2, 3, 4}
	if got := ix.mro(1); !reflect.DeepEqual(got, want) {
		t.Fatalf("mro(1) = %v, want %v", got, want)
	}

	// C3 interleaves the two chains: 1 = Child(2, 3), 2(Root=4), 3(Other=5).
	ix = newMROIndex(map[int64][]int64{
		1: {2, 3},
		2: {4},
		3: {5},
	})
	want = []int64{1, 2, 4, 3, 5}
	if got := ix.mro(1); !reflect.DeepEqual(got, want) {
		t.Fatalf("C3 order = %v, want %v", got, want)
	}
}

func TestMROCycleSafe(t *testing.T) {
	ix := newMROIndex(map[int64][]int64{
		1: {2},
		2: {1},
	})
	got := ix.mro(1)
	if len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("cyclic mro(1) = %v, want [1 2]", got)
	}

	// A self-referencing base must not recurse either.
	ix = newMROIndex(map[int64][]int64{7: {7}})
	if got := ix.mro(7); !reflect.DeepEqual(got, []int64{7}) {
		t.Fatalf("self-base mro(7) = %v, want [7]", got)
	}
}

func TestMRODepthCap(t *testing.T) {
	bases := make(map[int64][]int64, 12)
	for i := int64(1); i < 12; i++ {
		bases[i] = []int64{i + 1}
	}
	ix := newMROIndex(bases)
	got := ix.mro(1)
	if len(got) != maxMRODepth {
		t.Fatalf("chain length = %d, want the cap %d", len(got), maxMRODepth)
	}
	for i, id := range got {
		if id != int64(i+1) {
			t.Fatalf("chain[%d] = %d, want %d", i, id, i+1)
		}
	}
}

func TestMROInconsistentHierarchyIsDeterministic(t *testing.T) {
	// Python itself rejects this order (2 before 3, but 3's MRO is [3 2]).
	ix := newMROIndex(map[int64][]int64{
		1: {2, 3},
		2: {3},
		3: {2},
	})
	first := ix.mro(1)
	ix.mroCache = nil
	second := ix.mro(1)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("inconsistent hierarchy is not deterministic: %v vs %v", first, second)
	}
	if !reflect.DeepEqual(first, []int64{1, 2, 3}) {
		t.Fatalf("inconsistent mro = %v, want [1 2 3] with no duplicate", first)
	}
}

// TestAddInheritanceReplacesPersistedLinks pins the #49 property: a re-resolved
// class must not keep its previous base links, or a warm graph would follow a
// stale chain.
func TestAddInheritanceReplacesPersistedLinks(t *testing.T) {
	ix := &Index{
		bases: map[int64][]int64{1: {9}},
		classByID: map[int64]store.NodeRow{
			1: {ID: 1, Kind: "class", FileID: 10},
			2: {ID: 2, Kind: "class", FileID: 11},
			3: {ID: 3, Kind: "class", FileID: 12},
		},
	}
	ix.AddInheritance([]int64{10}, []ClassLink{{Src: 1, Dst: 2}, {Src: 1, Dst: 3}})
	want := []int64{2, 3}
	if got := ix.bases[1]; !reflect.DeepEqual(got, want) {
		t.Fatalf("fresh base links = %v, want %v (persisted link 9 must be replaced)", got, want)
	}
	// A class outside the written set keeps its persisted link.
	if got := ix.bases[2]; len(got) != 0 {
		t.Fatalf("untouched class links = %v, want none", got)
	}
}
