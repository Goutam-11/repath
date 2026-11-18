package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Goutam-11/repath/internal/store"
)

// lspFactsSchema is the only LSP facts-file version this build understands.
// The file is an input contract like SCIP, not a database schema change.
const lspFactsSchema = 1

// LSPFacts is one externally produced LSP fact set for exactly one language.
// An external client (see docs/adapters.md) speaks the language server and
// writes this file; repath never starts a server.
type LSPFacts struct {
	Schema      int             `json:"schema"`
	Language    string          `json:"language"`
	Producer    string          `json:"producer"`
	Root        string          `json:"root"`
	Definitions []LSPDefinition `json:"definitions"`
	References  []LSPReference  `json:"references"`
}

// LSPDefinition is one symbol definition the producer queried.
type LSPDefinition struct {
	File   string `json:"file"`
	Line   int    `json:"line"`
	Column int    `json:"column"`
	Name   string `json:"name"`
}

// LSPReference is one reference occurrence and the definition it resolves to.
type LSPReference struct {
	File   string    `json:"file"`
	Line   int       `json:"line"`
	Column int       `json:"column"`
	Name   string    `json:"name"`
	Target LSPTarget `json:"target"`
}

// LSPTarget names a definition by repo-relative file and 1-based line.
type LSPTarget struct {
	File string `json:"file"`
	Line int    `json:"line"`
}

func (t LSPTarget) key() lspTargetKey { return lspTargetKey{file: t.File, line: t.Line} }

type lspTargetKey struct {
	file string
	line int
}

type lspDefinitionKey struct {
	file string
	line int
	name string
}

// lspLanguage is the per-language naming this seam shares with the SCIP
// adapter: the stats name, the visible rule prefix, and the tree-sitter rule
// prefix used to find ambiguous candidate groups.
type lspLanguage struct {
	adapter    string
	rulePrefix string
	treePrefix string
}

func lspLanguageFor(language string) (lspLanguage, bool) {
	switch language {
	case "ts":
		return lspLanguage{adapter: "lsp", rulePrefix: "lsp.ts", treePrefix: "ts."}, true
	case "python":
		return lspLanguage{adapter: "lsp-python", rulePrefix: "lsp.py", treePrefix: "py."}, true
	case "rust":
		return lspLanguage{adapter: "lsp-rust", rulePrefix: "lsp.rs", treePrefix: "rs."}, true
	}
	return lspLanguage{}, false
}

// LoadLSPFacts reads a facts file produced by an external LSP client.
func LoadLSPFacts(path string) (LSPFacts, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return LSPFacts{}, fmt.Errorf("%w: %s", ErrNotFound, path)
		}
		return LSPFacts{}, fmt.Errorf("read lsp facts: %w", err)
	}
	facts, err := ParseLSPFacts(data)
	if err != nil {
		return LSPFacts{}, fmt.Errorf("parse lsp facts %s: %w", path, err)
	}
	return facts, nil
}

// ParseLSPFacts validates and normalizes a facts file before anything is
// written, so a malformed file can never produce a half-merged index.
func ParseLSPFacts(data []byte) (LSPFacts, error) {
	var facts LSPFacts
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&facts); err != nil {
		return LSPFacts{}, fmt.Errorf("invalid JSON: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return LSPFacts{}, fmt.Errorf("unexpected data after the facts object")
	}
	if facts.Schema != lspFactsSchema {
		return LSPFacts{}, fmt.Errorf("schema %d is not supported (want %d)", facts.Schema, lspFactsSchema)
	}
	lang, ok := lspLanguageFor(facts.Language)
	if !ok {
		return LSPFacts{}, fmt.Errorf("language %q is not ts, python, or rust", facts.Language)
	}

	defs := make([]LSPDefinition, 0, len(facts.Definitions))
	defKeys := make(map[lspTargetKey]bool, len(facts.Definitions))
	seenDefs := make(map[lspDefinitionKey]bool, len(facts.Definitions))
	for i, def := range facts.Definitions {
		file, err := normalizeLSPPath(def.File)
		if err != nil {
			return LSPFacts{}, fmt.Errorf("definition %d: %w", i+1, err)
		}
		if !lspSourceFile(lang, file) {
			return LSPFacts{}, fmt.Errorf("definition %d: %s is not a %s source file", i+1, file, facts.Language)
		}
		if def.Line < 1 {
			return LSPFacts{}, fmt.Errorf("definition %d: line %d is not 1-based", i+1, def.Line)
		}
		if def.Column < 1 {
			return LSPFacts{}, fmt.Errorf("definition %d: column %d is not 1-based", i+1, def.Column)
		}
		if strings.TrimSpace(def.Name) == "" {
			return LSPFacts{}, fmt.Errorf("definition %d: name is empty", i+1)
		}
		def.File = file
		key := lspDefinitionKey{file: file, line: def.Line, name: def.Name}
		if seenDefs[key] {
			continue
		}
		seenDefs[key] = true
		defs = append(defs, def)
		defKeys[lspTargetKey{file: file, line: def.Line}] = true
	}

	refs := make([]LSPReference, 0, len(facts.References))
	seenRefs := make(map[string]bool, len(facts.References))
	for i, ref := range facts.References {
		file, err := normalizeLSPPath(ref.File)
		if err != nil {
			return LSPFacts{}, fmt.Errorf("reference %d: %w", i+1, err)
		}
		if !lspSourceFile(lang, file) {
			return LSPFacts{}, fmt.Errorf("reference %d: %s is not a %s source file", i+1, file, facts.Language)
		}
		if ref.Line < 1 {
			return LSPFacts{}, fmt.Errorf("reference %d: line %d is not 1-based", i+1, ref.Line)
		}
		if ref.Column < 1 {
			return LSPFacts{}, fmt.Errorf("reference %d: column %d is not 1-based", i+1, ref.Column)
		}
		if strings.TrimSpace(ref.Name) == "" {
			return LSPFacts{}, fmt.Errorf("reference %d: name is empty", i+1)
		}
		targetFile, err := normalizeLSPPath(ref.Target.File)
		if err != nil {
			return LSPFacts{}, fmt.Errorf("reference %d target: %w", i+1, err)
		}
		if !lspSourceFile(lang, targetFile) {
			return LSPFacts{}, fmt.Errorf("reference %d target: %s is not a %s source file", i+1, targetFile, facts.Language)
		}
		if ref.Target.Line < 1 {
			return LSPFacts{}, fmt.Errorf("reference %d target: line %d is not 1-based", i+1, ref.Target.Line)
		}
		if !defKeys[lspTargetKey{file: targetFile, line: ref.Target.Line}] {
			return LSPFacts{}, fmt.Errorf("reference %d: target %s:%d is not a reported definition", i+1, targetFile, ref.Target.Line)
		}
		ref.File = file
		ref.Target.File = targetFile
		id := fmt.Sprintf("%s|%d|%d|%s|%s|%d", ref.File, ref.Line, ref.Column, ref.Name, ref.Target.File, ref.Target.Line)
		if seenRefs[id] {
			continue
		}
		seenRefs[id] = true
		refs = append(refs, ref)
	}

	facts.Definitions = defs
	facts.References = refs
	return facts, nil
}

// normalizeLSPPath accepts only a clean repo-relative source path.
func normalizeLSPPath(raw string) (string, error) {
	path := strings.TrimSpace(raw)
	if path == "" {
		return "", fmt.Errorf("file is empty")
	}
	path = strings.TrimPrefix(filepath.ToSlash(path), "./")
	if filepath.IsAbs(path) || strings.HasPrefix(path, "/") {
		return "", fmt.Errorf("file %q must be repo-relative", raw)
	}
	if path == ".." || strings.HasPrefix(path, "../") || strings.Contains(path, "/../") {
		return "", fmt.Errorf("file %q must stay inside the repo", raw)
	}
	return path, nil
}

// lspSourceFile keeps a facts file honest about the language it claims.
func lspSourceFile(lang lspLanguage, file string) bool {
	switch lang.rulePrefix {
	case "lsp.ts":
		switch strings.ToLower(filepath.Ext(file)) {
		case ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs":
			return true
		}
	case "lsp.py":
		return strings.EqualFold(filepath.Ext(file), ".py")
	case "lsp.rs":
		return strings.EqualFold(filepath.Ext(file), ".rs")
	}
	return false
}

// ApplyLSPFacts merges one language's LSP facts into the graph. It uses the
// same pending-first, never-overwrite policy as the SCIP adapter. The one
// additional input is the reference occurrence count: a pending key is left
// alone when the compiler reports fewer occurrences than the pending rows,
// because deleting a row no fact accounts for would be a guess.
func ApplyLSPFacts(ctx context.Context, st *store.Store, facts LSPFacts) (Stats, error) {
	lang, ok := lspLanguageFor(facts.Language)
	if !ok {
		return Stats{}, fmt.Errorf("lsp facts: unsupported language %q", facts.Language)
	}
	stats := Stats{
		Adapter:     lang.adapter,
		Definitions: len(facts.Definitions),
		References:  len(facts.References),
		LastRun:     time.Now().UTC().Format(time.RFC3339),
	}
	fileIDFor, err := filePathLookup(ctx, st)
	if err != nil {
		return stats, err
	}

	// Map every reported definition to the repath symbol enclosing its line.
	targets := make(map[lspTargetKey]int64, len(facts.Definitions))
	seenTargets := make(map[lspTargetKey]bool, len(facts.Definitions))
	for _, def := range facts.Definitions {
		key := lspTargetKey{file: def.File, line: def.Line}
		if seenTargets[key] {
			continue
		}
		seenTargets[key] = true
		fileID, ok := fileIDFor(def.File)
		if !ok {
			stats.Skipped++
			continue
		}
		node, found, err := st.EnclosingNode(ctx, fileID, def.Line)
		if err != nil {
			return stats, err
		}
		if !found {
			stats.Skipped++
			continue
		}
		targets[key] = node
	}

	pending, err := st.Pending(ctx)
	if err != nil {
		return stats, err
	}
	pendingByKey := make(map[pendingKey][]store.PendingRef, len(pending))
	for _, p := range pending {
		key := pendingKey{fileID: p.FileID, line: p.Line, name: p.Name}
		pendingByKey[key] = append(pendingByKey[key], p)
	}

	family := scipFamily{
		adapter: lang.adapter,
		prefix:  lang.treePrefix,
		rule:    func(ref store.PendingRef) string { return adapterRule(lang.rulePrefix, ref) },
		name:    symbolName,
	}
	ambiguousByLine, err := loadAmbiguousGroups(ctx, st, family)
	if err != nil {
		return stats, err
	}

	type resolvedRef struct {
		fileID int64
		line   int
		name   string
		src    int64
		target int64
	}
	resolved := make([]resolvedRef, 0, len(facts.References))
	for _, ref := range facts.References {
		fileID, ok := fileIDFor(ref.File)
		if !ok {
			stats.Skipped++
			continue
		}
		src, found, err := st.EnclosingNode(ctx, fileID, ref.Line)
		if err != nil {
			return stats, err
		}
		if !found {
			stats.Skipped++
			continue
		}
		target, ok := targets[ref.Target.key()]
		if !ok {
			stats.Skipped++
			continue
		}
		if src == target {
			stats.Skipped++
			continue
		}
		resolved = append(resolved, resolvedRef{fileID: fileID, line: ref.Line, name: ref.Name, src: src, target: target})
	}

	byKey := make(map[pendingKey][]resolvedRef, len(resolved))
	for _, ref := range resolved {
		key := pendingKey{fileID: ref.fileID, line: ref.line, name: ref.name}
		byKey[key] = append(byKey[key], ref)
	}
	keys := make([]pendingKey, 0, len(byKey))
	for key := range byKey {
		if len(pendingByKey[key]) > 0 {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].fileID != keys[j].fileID {
			return keys[i].fileID < keys[j].fileID
		}
		if keys[i].line != keys[j].line {
			return keys[i].line < keys[j].line
		}
		return keys[i].name < keys[j].name
	})

	var edges []store.Edge
	seenEdges := make(map[string]bool)
	merged := make(map[pendingKey]bool, len(keys))
	for _, key := range keys {
		rows := pendingByKey[key]
		keyRefs := byKey[key]
		// The compiler must have seen at least as many occurrences at this key
		// as tree-sitter left pending. If it saw fewer, at least one pending
		// row has no fact behind it; leave the whole key pending.
		if len(keyRefs) < len(rows) {
			continue
		}
		targetSet := make(map[int64]bool, len(keyRefs))
		for _, ref := range keyRefs {
			targetSet[ref.target] = true
		}
		uniform := true
		for _, row := range rows {
			if row.Kind != rows[0].Kind || row.Form != rows[0].Form {
				uniform = false
				break
			}
		}
		single := len(targetSet) == 1
		if !single && (len(keyRefs) != len(rows) || !uniform) {
			// Several targets are only attributable when every pending row maps
			// to exactly one occurrence and the rows share one shape.
			continue
		}
		dsts := make([]int64, 0, len(targetSet))
		for dst := range targetSet {
			dsts = append(dsts, dst)
		}
		sort.Slice(dsts, func(i, j int) bool { return dsts[i] < dsts[j] })
		merged[key] = true
		stats.Matched += len(rows)
		if single {
			// Mirror the SCIP path: one edge per pending row shape.
			for _, row := range rows {
				rowKind := edgeKind(row.Kind)
				rowRule := adapterRule(lang.rulePrefix, row)
				edgeKey := fmt.Sprintf("%d/%d/%s", keyRefs[0].src, dsts[0], rowKind)
				if !seenEdges[edgeKey] {
					seenEdges[edgeKey] = true
					edges = append(edges, store.Edge{
						Src: keyRefs[0].src, Dst: dsts[0], Kind: rowKind,
						Confidence: adapterConfidence, Rule: rowRule, Line: row.Line,
					})
				}
			}
		} else {
			rowKind := edgeKind(rows[0].Kind)
			rowRule := adapterRule(lang.rulePrefix, rows[0])
			for _, dst := range dsts {
				edgeKey := fmt.Sprintf("%d/%d/%s", keyRefs[0].src, dst, rowKind)
				if seenEdges[edgeKey] {
					continue
				}
				seenEdges[edgeKey] = true
				edges = append(edges, store.Edge{
					Src: keyRefs[0].src, Dst: dst, Kind: rowKind,
					Confidence: adapterConfidence, Rule: rowRule, Line: rows[0].Line,
				})
			}
		}
		for _, row := range rows {
			if err := st.DeletePending(ctx, row); err != nil {
				return stats, err
			}
		}
	}

	unresolved := make([]scipUnmatchedRef, 0, len(resolved))
	for _, ref := range resolved {
		key := pendingKey{fileID: ref.fileID, line: ref.line, name: ref.name}
		if merged[key] {
			continue
		}
		unresolved = append(unresolved, scipUnmatchedRef{src: ref.src, line: ref.line, target: ref.target, name: ref.name})
	}
	unmatched, err := replaceAmbiguousGroups(ctx, st, &stats, family, ambiguousByLine, unresolved)
	if err != nil {
		return stats, err
	}
	stats.Unmatched += unmatched

	inserted, conflicts, err := st.MergeEdges(ctx, edges)
	if err != nil {
		return stats, err
	}
	stats.Edges += inserted
	stats.Conflicts += conflicts
	if err := saveStats(ctx, st, stats); err != nil {
		return stats, err
	}
	return stats, nil
}
