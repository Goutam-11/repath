package testpath

import "testing"

func TestIsTest(t *testing.T) {
	cases := map[string]bool{
		"src/a.ts":             false,
		"src/a.test.ts":        true,
		"src/a.spec.ts":        true,
		"tests/helper.ts":      true,
		"test/helper.py":       true,
		"pkg/test_util.py":     true,
		"pkg/conftest.py":      true,
		"src/__tests__/a.ts":   true,
		"tests/integration.rs": true,
		"testdata/fixture.ts":  true,
		"pkg/__tests__/a.js":   true,
		"src/a.test.jsx":       true,
		"src/a.spec.jsx":       true,
		"pkg/test_client.py":   true,
		"tests/conftest.py":    true,
		"contest/winner.ts":    false,
		"testament/play.ts":    false,
	}
	for path, want := range cases {
		if got := IsTest(path); got != want {
			t.Errorf("IsTest(%q) = %v, want %v", path, got, want)
		}
	}
}
