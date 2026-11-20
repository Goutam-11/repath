package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestGraphMLMatchesXSDSchema is the opt-in #108 check: both export modes must
// validate against the vendored GraphML 1.0 schema set. It never runs in
// `make check` (REPATH_XSD=1 opts in) and skips when xmllint is absent, so CI
// needs no XML toolchain.
func TestGraphMLMatchesXSDSchema(t *testing.T) {
	if os.Getenv("REPATH_XSD") != "1" {
		t.Skip("set REPATH_XSD=1 to validate GraphML against the vendored schema")
	}
	xmllint, err := exec.LookPath("xmllint")
	if err != nil {
		t.Skip("xmllint not installed")
	}
	schema := filepath.Join("..", "..", "testdata", "export", "graphml", "graphml.xsd")
	root := exportFixture(t)
	cases := []struct {
		name string
		args []string
	}{
		{"symbol", []string{"export", "--graphml", "--root", root}},
		{"module", []string{"export", "--graphml", "--modules", "--root", root}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, doc, stderr := run(t, tc.args...)
			if code != 0 {
				t.Fatalf("export exit = %d, stderr = %s", code, stderr)
			}
			path := filepath.Join(t.TempDir(), tc.name+".graphml")
			if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
				t.Fatal(err)
			}
			out, err := exec.Command(xmllint, "--noout", "--schema", schema, path).CombinedOutput()
			if err != nil {
				t.Fatalf("xmllint rejected %s: %v\n%s", tc.name, err, out)
			}
		})
	}
}
