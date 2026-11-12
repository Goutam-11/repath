// Package testpath classifies repository paths as test code. It is shared by
// `check` (dead-code and boundary exclusions) and by `tests` (test-impact
// mapping), so the two commands cannot drift.
package testpath

import (
	"path/filepath"
	"strings"
)

// IsTest is a heuristic classifier. It deliberately errs toward treating a
// file as test code so test helpers do not become dead-code noise.
func IsTest(path string) bool {
	p := strings.ToLower(filepath.ToSlash(path))
	base := filepath.Base(p)
	if base == "conftest.py" {
		return true
	}
	if strings.HasPrefix(base, "test_") && strings.HasSuffix(base, ".py") {
		return true
	}
	for _, marker := range []string{"/test/", "/tests/", "/__tests__/", "/testdata/"} {
		if strings.Contains(p, marker) || strings.HasPrefix(p, strings.TrimPrefix(marker, "/")) {
			return true
		}
	}
	for _, suffix := range []string{
		".test.ts", ".test.tsx", ".test.js", ".test.jsx",
		".spec.ts", ".spec.tsx", ".spec.js", ".spec.jsx",
		".test.py", ".spec.py",
	} {
		if strings.HasSuffix(p, suffix) {
			return true
		}
	}
	return false
}
