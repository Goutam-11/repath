package resolve

import (
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Aliases is the tsconfig `baseUrl` + `paths` mapping, which is how most
// TypeScript repos write imports ("@/lib/cart").
type Aliases struct {
	base  string
	paths map[string][]string
}

// LoadAliases reads tsconfig.json (or jsconfig.json) from root. A missing or
// unreadable config is not an error: imports then resolve relatively only.
func LoadAliases(root string) *Aliases {
	aliases := &Aliases{base: ".", paths: map[string][]string{}}

	var cfg struct {
		CompilerOptions struct {
			BaseURL string              `json:"baseUrl"`
			Paths   map[string][]string `json:"paths"`
		} `json:"compilerOptions"`
	}

	data, err := os.ReadFile(filepath.Join(root, "tsconfig.json"))
	if err != nil {
		data, err = os.ReadFile(filepath.Join(root, "jsconfig.json"))
		if err != nil {
			return aliases
		}
	}

	if err := json.Unmarshal(stripJSONC(data), &cfg); err != nil {
		return aliases
	}

	aliases.base = cfg.CompilerOptions.BaseURL
	if aliases.base == "" {
		aliases.base = "."
	}
	if cfg.CompilerOptions.Paths != nil {
		aliases.paths = cfg.CompilerOptions.Paths
	}
	return aliases
}

// Resolve maps an alias specifier to a repo-relative path, or "" if it is not
// covered by `paths` (a package import, therefore external).
func (a *Aliases) Resolve(spec string) string {
	if a == nil || len(a.paths) == 0 {
		return ""
	}

	// Longest prefix wins, which is what tsc does.
	var bestPattern string
	var bestTargets []string
	for pattern, targets := range a.paths {
		if !matchesPattern(pattern, spec) {
			continue
		}
		if len(pattern) > len(bestPattern) {
			bestPattern, bestTargets = pattern, targets
		}
	}
	if bestPattern == "" || len(bestTargets) == 0 {
		return ""
	}

	wildcard := wildcardOf(bestPattern, spec)
	target := strings.Replace(bestTargets[0], "*", wildcard, 1)
	return path.Join(a.base, target)
}

func matchesPattern(pattern, spec string) bool {
	if !strings.Contains(pattern, "*") {
		return pattern == spec
	}
	prefix, suffix, _ := strings.Cut(pattern, "*")
	return strings.HasPrefix(spec, prefix) && strings.HasSuffix(spec, suffix)
}

func wildcardOf(pattern, spec string) string {
	prefix, suffix, found := strings.Cut(pattern, "*")
	if !found {
		return ""
	}
	start := len(prefix)
	end := len(spec) - len(suffix)
	if end < start {
		return ""
	}
	return spec[start:end]
}

// stripJSONC removes comments and trailing commas so tsconfig.json parses with
// encoding/json. Strings are respected, so "http://x" survives.
func stripJSONC(data []byte) []byte {
	out := make([]byte, 0, len(data))
	inString := false
	escaped := false

	for i := 0; i < len(data); i++ {
		c := data[i]

		if inString {
			out = append(out, c)
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}

		switch {
		case c == '"':
			inString = true
			out = append(out, c)
		case c == '/' && i+1 < len(data) && data[i+1] == '/':
			for i < len(data) && data[i] != '\n' {
				i++
			}
			out = append(out, '\n')
		case c == '/' && i+1 < len(data) && data[i+1] == '*':
			i += 2
			for i+1 < len(data) && !(data[i] == '*' && data[i+1] == '/') {
				i++
			}
			i++
		default:
			out = append(out, c)
		}
	}

	return removeTrailingCommas(out)
}

func removeTrailingCommas(data []byte) []byte {
	out := make([]byte, 0, len(data))
	inString := false
	escaped := false

	for i := 0; i < len(data); i++ {
		c := data[i]
		if inString {
			out = append(out, c)
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		if c == '"' {
			inString = true
			out = append(out, c)
			continue
		}
		if c == ',' {
			j := i + 1
			for j < len(data) && (data[j] == ' ' || data[j] == '\t' || data[j] == '\n' || data[j] == '\r') {
				j++
			}
			if j < len(data) && (data[j] == '}' || data[j] == ']') {
				continue
			}
		}
		out = append(out, c)
	}
	return out
}
