package index

import (
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
)

// metaScope is the meta key that records the scope an index was built with. An
// absent key means the whole repository.
const metaScope = "scope"

// CleanScope normalises `--scope` arguments into sorted, de-duplicated
// repository-relative paths. It rejects absolute paths, paths that escape the
// repository, and "." (which would be the whole repo spelled differently).
func CleanScope(raw []string) ([]string, error) {
	seen := make(map[string]bool, len(raw))
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		spec := strings.TrimSpace(item)
		if spec == "" {
			return nil, fmt.Errorf("--scope needs a directory")
		}
		spec = strings.ReplaceAll(spec, "\\", "/")
		if path.IsAbs(spec) || strings.HasPrefix(spec, "/") {
			return nil, fmt.Errorf("--scope %q must be repository-relative", item)
		}
		clean := path.Clean(spec)
		if clean == "." || clean == "" {
			return nil, fmt.Errorf("--scope %q is the whole repository; omit the flag instead", item)
		}
		if clean == ".." || strings.HasPrefix(clean, "../") {
			return nil, fmt.Errorf("--scope %q escapes the repository", item)
		}
		if seen[clean] {
			continue
		}
		seen[clean] = true
		out = append(out, clean)
	}
	sort.Strings(out)
	return out, nil
}

// ScopeFromMeta decodes the recorded scope. An empty or malformed value means
// the whole repository, which is the only safe reading.
func ScopeFromMeta(meta map[string]string) []string {
	raw := strings.TrimSpace(meta[metaScope])
	if raw == "" || raw == "null" {
		return nil
	}
	var scope []string
	if err := json.Unmarshal([]byte(raw), &scope); err != nil {
		return nil
	}
	if len(scope) == 0 {
		return nil
	}
	sort.Strings(scope)
	return scope
}

// scopeMeta encodes a scope for the meta table. The whole repository is stored
// as an empty value so a plain `index` clears a previous scope.
func scopeMeta(scope []string) string {
	if len(scope) == 0 {
		return ""
	}
	data, err := json.Marshal(scope)
	if err != nil {
		return ""
	}
	return string(data)
}

// ScopeEqual reports whether two normalised scope sets are identical.
func ScopeEqual(a, b []string) bool { return sameScope(a, b) }

func sameScope(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// underScope reports whether a repository-relative path is inside any scope.
func underScope(rel string, scope []string) bool {
	if len(scope) == 0 {
		return true
	}
	for _, root := range scope {
		if rel == root || strings.HasPrefix(rel, root+"/") {
			return true
		}
	}
	return false
}

// effectiveScope resolves what a run should index. WholeRepo is the explicit
// "the user asked for the whole repository" signal (`index` without --scope);
// otherwise an explicit Scope wins, and the default (no flags) preserves what
// the index already records so a lazy refresh never flips the scope.
func effectiveScope(opts Options, recorded []string) []string {
	switch {
	case opts.WholeRepo:
		return nil
	case opts.Scope != nil:
		return opts.Scope
	default:
		return recorded
	}
}
