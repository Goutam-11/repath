package index_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Goutam-11/repath/internal/index"
	"github.com/Goutam-11/repath/internal/store"
)

func indexFlowFixture(t *testing.T, lang string) (string, *store.Store) {
	t.Helper()
	src := filepath.Join("..", "..", "testdata", "fixtures", "flows", lang)
	dst := t.TempDir()
	err := filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		t.Fatalf("copy fixture: %v", err)
	}

	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(dst, ".repath", "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx); err != nil {
		st.Close()
		t.Fatal(err)
	}
	if _, err := index.Run(ctx, st, dst, index.Options{}); err != nil {
		st.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return dst, st
}

func TestEntrypointDetection(t *testing.T) {
	cases := []struct {
		lang string
		want map[string]string
	}{
		{
			lang: "ts",
			want: map[string]string{
				"start": "ts.route.express",
				"GET":   "ts.next.route",
			},
		},
		{
			lang: "py",
			want: map[string]string{
				"list_items": "py.route.fastapi",
				"ping":       "py.route.flask",
				"test_ping":  "py.test",
				"main":       "py.main",
			},
		},
		{
			lang: "rs",
			want: map[string]string{
				"main":  "rs.main",
				"works": "rs.test",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.lang, func(t *testing.T) {
			_, st := indexFlowFixture(t, tc.lang)
			entries, err := st.Entrypoints(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]string{}
			for _, entry := range entries {
				got[entry.Node.QName] = entry.Rule
			}
			for name, rule := range tc.want {
				if got[name] != rule {
					t.Fatalf("%s entrypoint %q rule = %q, want %q (all %+v)", tc.lang, name, got[name], rule, got)
				}
			}
			if len(got) != len(tc.want) {
				t.Fatalf("%s entrypoints = %+v, want %d", tc.lang, got, len(tc.want))
			}
		})
	}
}

func TestEntrypointsRemovedOnReindex(t *testing.T) {
	root, st := indexFlowFixture(t, "ts")
	ctx := context.Background()

	path := filepath.Join(root, "src", "server.ts")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Drop the registration; the handler symbol remains but is no longer an entry.
	updated := string(data)
	updated = updated[:len(updated)-len("app.get(\"/start\", start);\n")]
	if err := os.WriteFile(path, []byte(updated), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := index.Run(ctx, st, root, index.Options{}); err != nil {
		t.Fatal(err)
	}
	entries, err := st.Entrypoints(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Node.QName == "start" {
			t.Fatalf("stale entrypoint survived reindex: %+v", entry)
		}
	}
}
