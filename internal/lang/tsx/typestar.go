package tsx

import "bytes"

// typeStarSource returns a same-length copy of src in which the `type` keyword
// of every top-level `export type *` / `export type * as ns` statement is
// replaced by spaces.
//
// TypeScript 5.0 allows a type-only wildcard re-export, but the pinned
// tree-sitter-typescript grammar (v1.9.4, also the newest published version)
// rejects it, which stores an otherwise valid module as parse_ok=0 and drops
// every re-export it carries (#122). Blanking the keyword makes the statement
// parse as a plain `export *` — type-only distinctions are not modelled anyway
// — while preserving byte length exactly, so every node position, line, and
// column stays correct.
//
// The rewrite is deliberately textual and line-anchored: a statement-level
// export starts a line in every formatting style worth supporting, and only a
// file whose first parse already failed is re-parsed with the result.
func typeStarSource(src []byte) ([]byte, bool) {
	var out []byte
	start := 0
	for start <= len(src) {
		end := bytes.IndexByte(src[start:], '\n')
		if end < 0 {
			end = len(src)
		} else {
			end += start
		}
		if at, ok := typeStarKeyword(src[start:end]); ok {
			if out == nil {
				out = append([]byte(nil), src...)
			}
			copy(out[start+at:start+at+len("type")], "    ")
		}
		if end >= len(src) {
			break
		}
		start = end + 1
	}
	return out, out != nil
}

// typeStarKeyword reports the offset of the `type` keyword when line begins
// (after horizontal whitespace) with `export type *`. A token boundary is
// required on both sides of each keyword.
func typeStarKeyword(line []byte) (int, bool) {
	i := 0
	for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
		i++
	}
	if !keywordAt(line, i, "export") {
		return 0, false
	}
	i += len("export")
	if i >= len(line) || (line[i] != ' ' && line[i] != '\t') {
		return 0, false
	}
	for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
		i++
	}
	if !keywordAt(line, i, "type") {
		return 0, false
	}
	typeAt := i
	i += len("type")
	if i >= len(line) || (line[i] != ' ' && line[i] != '\t') {
		return 0, false
	}
	for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
		i++
	}
	if i >= len(line) || line[i] != '*' {
		return 0, false
	}
	if i+1 < len(line) {
		switch line[i+1] {
		case ' ', '\t', '\r', ';':
		default:
			return 0, false
		}
	}
	return typeAt, true
}

func keywordAt(b []byte, i int, keyword string) bool {
	if i+len(keyword) > len(b) || string(b[i:i+len(keyword)]) != keyword {
		return false
	}
	if i+len(keyword) == len(b) {
		return true
	}
	c := b[i+len(keyword)]
	return !(c == '_' || c == '$' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9'))
}
