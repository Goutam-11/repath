package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Goutam-11/repath/internal/graph"
)

func copyFlowFixture(t *testing.T, lang string) string {
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
	return dst
}

func TestFlowsCommand(t *testing.T) {
	dir := copyFlowFixture(t, "ts")

	code, out, errOut := run(t, "status", "--root", dir, "--json")
	if code != 0 {
		t.Fatalf("status exit = %d: %s", code, errOut)
	}
	var status struct {
		Entrypoints     int            `json:"entrypoints"`
		EntrypointRules map[string]int `json:"entrypoint_rules"`
	}
	if err := json.Unmarshal([]byte(out), &status); err != nil {
		t.Fatal(err)
	}
	if status.Entrypoints != 2 || status.EntrypointRules["ts.route.express"] != 1 || status.EntrypointRules["ts.next.route"] != 1 {
		t.Fatalf("status entrypoints = %+v", status)
	}

	code, out, errOut = run(t, "flows", "start", "--root", dir, "--json")
	if code != 0 {
		t.Fatalf("flows exit = %d: %s", code, errOut)
	}
	var result graph.FlowResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("flows output is not JSON: %v\n%s", err, out)
	}
	if result.Entrypoint.QName != "start" || result.EntrypointRule != "ts.route.express" {
		t.Fatalf("entrypoint = %+v rule %q", result.Entrypoint, result.EntrypointRule)
	}
	if len(result.Paths) != 1 || len(result.Paths[0]) != 2 {
		t.Fatalf("paths = %+v", result.Paths)
	}
	if result.Paths[0][0].QName != "step" || result.Paths[0][1].QName != "finish" {
		t.Fatalf("path = %+v", result.Paths[0])
	}
	if len(result.Frontier) != 1 || result.Frontier[0].QName != "finish" {
		t.Fatalf("frontier = %+v", result.Frontier)
	}

	code, out, _ = run(t, "flows", "start", "--root", dir)
	if code != 0 {
		t.Fatalf("text flows exit = %d", code)
	}
	if !strings.Contains(out, "path 1") || !strings.Contains(out, "finish") {
		t.Fatalf("text flows missing path:\n%s", out)
	}

	code, _, _ = run(t, "flows", "not_an_entrypoint", "--root", dir)
	if code != 3 {
		t.Fatalf("missing entrypoint exit = %d, want 3", code)
	}
}

func TestMapReportsEntrypoints(t *testing.T) {
	dir := copyFlowFixture(t, "py")
	code, out, errOut := run(t, "map", "--root", dir, "--json")
	if code != 0 {
		t.Fatalf("map exit = %d: %s", code, errOut)
	}
	var result struct {
		Entrypoints     int            `json:"entrypoints"`
		EntrypointRules map[string]int `json:"entrypoint_rules"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatal(err)
	}
	if result.Entrypoints != 4 || result.EntrypointRules["py.route.fastapi"] != 1 {
		t.Fatalf("map entrypoints = %+v", result)
	}
}
