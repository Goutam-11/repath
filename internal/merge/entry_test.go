package merge_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/Goutam-11/repath/internal/artifact"
	"github.com/Goutam-11/repath/internal/merge"
)

// TestMergeKeepsEntrypoints pins that the entrypoint dimension survives a merge:
// it is re-anchored to the new node ids, not dropped with the old ones.
func TestMergeKeepsEntrypoints(t *testing.T) {
	ctx := context.Background()
	src, err := filepath.Abs(filepath.Join("..", "..", "testdata", "fixtures", "go"))
	if err != nil {
		t.Fatal(err)
	}
	art := export(t, src)
	st, done, _, err := artifact.OpenReadOnly(ctx, art)
	if err != nil {
		t.Fatal(err)
	}
	eps, _ := st.Entrypoints(ctx)
	done()
	if len(eps) == 0 {
		t.Fatal("fixture artifact has no entrypoints")
	}
	out := filepath.Join(t.TempDir(), "out")
	if _, err := merge.Run(ctx, []string{art, art}, out, "test"); err != nil {
		t.Fatal(err)
	}
	mst, mdone, _, err := artifact.OpenReadOnly(ctx, out)
	if err != nil {
		t.Fatal(err)
	}
	defer mdone()
	meps, _ := mst.Entrypoints(ctx)

	if len(meps) != 2*len(eps) {
		t.Fatalf("merged entrypoints = %d, want %d (two parts)", len(meps), 2*len(eps))
	}
}
