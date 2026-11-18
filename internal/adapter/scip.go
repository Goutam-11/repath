// Package adapter ingests compiler-produced facts and merges them into the
// symbol graph. It is the seam PLAN §16.6 reserved: tree-sitter stays the
// fallback, adapter edges carry their own rule ids, and an existing edge is
// never silently overwritten.
package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Goutam-11/repath/internal/store"
)

// ErrNotFound marks a missing input; the CLI maps it to exit code 3.
var ErrNotFound = errors.New("adapter input not found")

// adapterConfidence is deliberately just below an exact tree-sitter import
// edge: the SCIP definition is exact, but the pending-ref match is by
// file+line+name because the index does not store reference columns.
const adapterConfidence = 0.99

// Stats is one adapter run's coverage. It is persisted in meta under
// "adapter.<name>" and surfaced by `status --json`.
type Stats struct {
	Adapter     string `json:"adapter"`
	Definitions int    `json:"definitions"`
	References  int    `json:"references"`
	Matched     int    `json:"matched"`
	Replaced    int    `json:"replaced"`
	// ReplacedEdges is the number of tree-sitter ambiguous candidate rows
	// removed by those replacements; the exact adapter edges are counted in
	// Edges.
	ReplacedEdges int    `json:"replaced_edges"`
	Edges         int    `json:"edges"`
	Conflicts     int    `json:"conflicts"`
	Skipped       int    `json:"skipped"`
	Unmatched     int    `json:"unmatched"`
	LastRun       string `json:"last_run"`
}

// SCIPIndex is the subset of a SCIP index repath consumes: per-document
// occurrences with a zero-based start line, symbol string, and role bitset.
type SCIPIndex struct {
	Documents []SCIPDocument
}

// SCIPDocument is one source file in a SCIP index.
type SCIPDocument struct {
	Path        string
	Language    string
	Occurrences []SCIPOccurrence
}

// SCIPOccurrence is one symbol occurrence. Roles bit 1 is a definition; the
// rest are references, imports, writes, and generated markers.
type SCIPOccurrence struct {
	Line   int
	Symbol string
	Roles  uint64
}

// LoadSCIP reads a SCIP protobuf file. SCIP is produced by `scip-typescript`;
// repath parses only the fields the graph needs and never shells out.
func LoadSCIP(path string) (SCIPIndex, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return SCIPIndex{}, fmt.Errorf("%w: %s", ErrNotFound, path)
		}
		return SCIPIndex{}, fmt.Errorf("read scip: %w", err)
	}
	index, err := ParseSCIP(data)
	if err != nil {
		return SCIPIndex{}, fmt.Errorf("parse scip %s: %w", path, err)
	}
	return index, nil
}

// ParseSCIP decodes the subset of the SCIP protobuf schema in this package. It
// is hand-rolled to keep the dependency rule intact.
func ParseSCIP(data []byte) (SCIPIndex, error) {
	var index SCIPIndex
	err := walkProto(data, func(number, wire int, _ uint64, raw []byte) error {
		if number != 2 || wire != 2 {
			return nil
		}
		doc, err := parseSCIPDocument(raw)
		if err != nil {
			return err
		}
		index.Documents = append(index.Documents, doc)
		return nil
	})
	if err != nil {
		return SCIPIndex{}, err
	}
	return index, nil
}

func parseSCIPDocument(data []byte) (SCIPDocument, error) {
	var doc SCIPDocument
	err := walkProto(data, func(number, wire int, _ uint64, raw []byte) error {
		switch number {
		case 1:
			if wire == 2 {
				doc.Path = string(raw)
			}
		case 2:
			if wire == 2 {
				occ, err := parseSCIPOccurrence(raw)
				if err != nil {
					return err
				}
				doc.Occurrences = append(doc.Occurrences, occ)
			}
		case 4:
			if wire == 2 {
				doc.Language = string(raw)
			}
		}
		return nil
	})
	return doc, err
}

func parseSCIPOccurrence(data []byte) (SCIPOccurrence, error) {
	var occ SCIPOccurrence
	err := walkProto(data, func(number, wire int, value uint64, raw []byte) error {
		switch number {
		case 1:
			switch wire {
			case 0:
				occ.Line = int(value)
			case 2:
				values, err := packedVarints(raw)
				if err != nil {
					return err
				}
				if len(values) > 0 {
					occ.Line = int(values[0])
				}
			}
		case 2:
			if wire == 2 {
				occ.Symbol = string(raw)
			}
		case 3:
			if wire == 0 {
				occ.Roles = value
			}
		}
		return nil
	})
	return occ, err
}

// walkProto is a minimal protobuf wire reader. Unknown fields are skipped, so
// a newer SCIP producer can add fields without breaking ingestion.
func walkProto(data []byte, fn func(number, wire int, value uint64, raw []byte) error) error {
	for offset := 0; offset < len(data); {
		key, n := readVarint(data[offset:])
		if n <= 0 {
			return errors.New("truncated protobuf key")
		}
		offset += n
		number := int(key >> 3)
		wire := int(key & 7)
		switch wire {
		case 0:
			value, n := readVarint(data[offset:])
			if n <= 0 {
				return errors.New("truncated protobuf varint")
			}
			offset += n
			if err := fn(number, wire, value, nil); err != nil {
				return err
			}
		case 1:
			if offset+8 > len(data) {
				return errors.New("truncated protobuf fixed64")
			}
			raw := data[offset : offset+8]
			offset += 8
			if err := fn(number, wire, 0, raw); err != nil {
				return err
			}
		case 2:
			length, n := readVarint(data[offset:])
			if n <= 0 || offset+n > len(data) || length > uint64(len(data)-offset-n) {
				return errors.New("truncated protobuf bytes")
			}
			offset += n
			raw := data[offset : offset+int(length)]
			offset += int(length)
			if err := fn(number, wire, 0, raw); err != nil {
				return err
			}
		case 5:
			if offset+4 > len(data) {
				return errors.New("truncated protobuf fixed32")
			}
			raw := data[offset : offset+4]
			offset += 4
			if err := fn(number, wire, 0, raw); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported protobuf wire type %d", wire)
		}
	}
	return nil
}

func readVarint(data []byte) (uint64, int) {
	var value uint64
	for i, b := range data {
		if i >= 10 {
			return 0, -1
		}
		value |= uint64(b&0x7f) << (7 * i)
		if b&0x80 == 0 {
			return value, i + 1
		}
	}
	return 0, -1
}

func packedVarints(data []byte) ([]uint64, error) {
	var out []uint64
	for offset := 0; offset < len(data); {
		value, n := readVarint(data[offset:])
		if n <= 0 {
			return nil, errors.New("truncated packed varint")
		}
		out = append(out, value)
		offset += n
	}
	return out, nil
}

// symbolName extracts the final display name from a SCIP symbol string. It
// strips the package prefix and the type/method/term descriptors; an empty
// result means the occurrence is a module, a local, or otherwise not useful as
// a graph target.
func symbolName(symbol string) string {
	if strings.HasPrefix(symbol, "local ") {
		return ""
	}
	if slash := strings.LastIndex(symbol, "/"); slash >= 0 {
		symbol = symbol[slash+1:]
	}
	if symbol == "" {
		return ""
	}

	switch {
	case strings.HasSuffix(symbol, "()."):
		symbol = strings.TrimSuffix(symbol, "().")
	case strings.HasSuffix(symbol, "."):
		symbol = strings.TrimSuffix(symbol, ".")
	case strings.HasSuffix(symbol, "#"):
		symbol = strings.TrimSuffix(symbol, "#")
	case strings.HasSuffix(symbol, "/"), strings.HasSuffix(symbol, ":"):
		// SCIP namespace/module descriptors end in "/" or ":" and are not
		// graph targets; this also covers scip-python's app/__init__: form.
		return ""
	}

	if hash := strings.LastIndex(symbol, "#"); hash >= 0 {
		symbol = symbol[hash+1:]
	}
	if bracket := strings.Index(symbol, "["); bracket >= 0 {
		symbol = symbol[:bracket]
	}
	if strings.HasPrefix(symbol, "(") || symbol == "" {
		return ""
	}
	return symbol
}

// rustSymbolName parses rust-analyzer's SCIP symbol grammar:
//
//	rust-analyzer cargo <crate> <version> <descriptor>
//
// Descriptors are crate/module paths ("crate/", "models/"), types ("Cart#"),
// terms ("run().", "Cart#total()."), enum variants ("Kind#A#"), and impl
// members ("impl#[Cart][Total]total()."). A leading "local" symbol is a local
// binding and is not a graph target.
func rustSymbolName(symbol string) string {
	if strings.HasPrefix(symbol, "local ") {
		return ""
	}
	parts := strings.SplitN(symbol, " ", 5)
	if len(parts) < 5 {
		return ""
	}
	return rustDescriptorName(parts[4])
}

func rustDescriptorName(desc string) string {
	if desc == "" || strings.HasSuffix(desc, "/") {
		// Crate and module descriptors are not symbols.
		return ""
	}
	if strings.HasPrefix(desc, "impl#") {
		close := strings.LastIndex(desc, "]")
		if close < 0 {
			return ""
		}
		return rustTermName(desc[close+1:])
	}
	if strings.HasSuffix(desc, "#") {
		trimmed := strings.TrimSuffix(desc, "#")
		if hash := strings.LastIndex(trimmed, "#"); hash >= 0 {
			// Nested type or enum variant: "Outer#Inner#", "Kind#A#".
			return rustTypeName(trimmed[hash+1:])
		}
		return rustTypeName(trimmed)
	}
	if hash := strings.LastIndex(desc, "#"); hash >= 0 {
		return rustTermName(desc[hash+1:])
	}
	return rustTermName(desc)
}

func rustTypeName(s string) string {
	if slash := strings.LastIndex(s, "/"); slash >= 0 {
		s = s[slash+1:]
	}
	if bracket := strings.Index(s, "["); bracket >= 0 {
		s = s[:bracket]
	}
	return s
}

func rustTermName(s string) string {
	s = strings.TrimSuffix(s, ".")
	s = strings.TrimSuffix(s, "()")
	if bracket := strings.Index(s, "["); bracket >= 0 {
		s = s[:bracket]
	}
	s = strings.TrimSuffix(s, "#")
	if slash := strings.LastIndex(s, "/"); slash >= 0 {
		s = s[slash+1:]
	}
	if strings.HasPrefix(s, "(") {
		return ""
	}
	return s
}

type pendingKey struct {
	fileID int64
	line   int
	name   string
}

// scipFamily is one language adapter over the shared SCIP parser. The parser
// stays language-neutral; only the document filter, the rule ids, and the
// tree-sitter rule prefix differ.
type scipFamily struct {
	adapter string
	prefix  string
	matches func(SCIPDocument) bool
	rule    func(store.PendingRef) string
	name    func(string) string
}

var (
	tsSCIPFamily = scipFamily{adapter: "scip", prefix: "ts.", matches: isTypeScriptDocument, rule: tsAdapterRule, name: symbolName}
	pySCIPFamily = scipFamily{adapter: "scip-python", prefix: "py.", matches: isPythonDocument, rule: pyAdapterRule, name: symbolName}
	rsSCIPFamily = scipFamily{adapter: "scip-rust", prefix: "rs.", matches: isRustDocument, rule: rsAdapterRule, name: rustSymbolName}
)

// ApplySCIP merges one parsed SCIP index and returns the first language
// family's stats. It is the single-language convenience used by tests; the CLI
// calls ApplySCIPAll so a Python run is stored under its own adapter name.
func ApplySCIP(ctx context.Context, st *store.Store, index SCIPIndex) (Stats, error) {
	all, err := ApplySCIPAll(ctx, st, index)
	if err != nil {
		return Stats{}, err
	}
	if len(all) == 0 {
		return Stats{}, nil
	}
	return all[0], nil
}

// ApplySCIPAll applies every language family present in the index and persists
// one stats entry per family under meta.adapter.<name>.
func ApplySCIPAll(ctx context.Context, st *store.Store, index SCIPIndex) ([]Stats, error) {
	families := familiesFor(index)
	out := make([]Stats, 0, len(families))
	for _, family := range families {
		stats, err := applySCIPFamily(ctx, st, index, family)
		if err != nil {
			return out, err
		}
		out = append(out, stats)
	}
	return out, nil
}

// familiesFor detects the producers in one SCIP file. scip-typescript and
// scip-python are normally run separately, but a combined index is handled by
// applying each family over its own documents and reporting both stats.
func familiesFor(index SCIPIndex) []scipFamily {
	hasTS, hasPy, hasRust := false, false, false
	for _, doc := range index.Documents {
		if isTypeScriptDocument(doc) {
			hasTS = true
		}
		if isPythonDocument(doc) {
			hasPy = true
		}
		if isRustDocument(doc) {
			hasRust = true
		}
	}
	var families []scipFamily
	if hasTS {
		families = append(families, tsSCIPFamily)
	}
	if hasPy {
		families = append(families, pySCIPFamily)
	}
	if hasRust {
		families = append(families, rsSCIPFamily)
	}
	if len(families) == 0 {
		// Preserve the pre-Python behavior: an empty or unrecognized index
		// still records a zero TypeScript run instead of silently disappearing.
		families = append(families, tsSCIPFamily)
	}
	return families
}

// applySCIPFamily merges one parsed SCIP index into the graph for one language.
// Two narrow paths exist:
//
//   - pending references: matched by file + line + name, deleted from pending,
//     and inserted as adapter edges;
//   - tree-sitter ambiguous references: a 2-5 candidate group at confidence
//     0.40 and rule `%.ambiguous` is replaced by the exact SCIP target only
//     when the compiler's occurrence maps to the same source node and line and
//     its target is one of that group's candidates. Exactly one occurrence must
//     map to the group; two distinct same-line references leave it alone.
//
// A same-key edge is a conflict and is never overwritten. Every replacement is
// counted in stats (`replaced` groups, `replaced_edges` candidate rows) and the
// run's stats are persisted.
func applySCIPFamily(ctx context.Context, st *store.Store, index SCIPIndex, family scipFamily) (Stats, error) {
	stats := Stats{Adapter: family.adapter, LastRun: time.Now().UTC().Format(time.RFC3339)}

	fileIDFor, err := filePathLookup(ctx, st)
	if err != nil {
		return stats, err
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
	ambiguousByLine, err := loadAmbiguousGroups(ctx, st, family)
	if err != nil {
		return stats, err
	}

	definitions := make(map[string]int64)
	for _, doc := range index.Documents {
		if !family.matches(doc) {
			continue
		}
		fileID, ok := fileIDFor(doc.Path)
		if !ok {
			continue
		}
		for _, occ := range doc.Occurrences {
			if occ.Roles&1 == 0 {
				continue
			}
			stats.Definitions++
			name := family.name(occ.Symbol)
			if name == "" {
				stats.Skipped++
				continue
			}
			nodeID, found, err := st.EnclosingNode(ctx, fileID, occ.Line+1)
			if err != nil {
				return stats, err
			}
			if !found {
				stats.Skipped++
				continue
			}
			if _, exists := definitions[occ.Symbol]; !exists {
				definitions[occ.Symbol] = nodeID
			}
		}
	}

	var edges []store.Edge
	var unresolved []scipUnmatchedRef
	matched := make(map[pendingKey]bool)
	seenEdges := make(map[string]bool)
	for _, doc := range index.Documents {
		if !family.matches(doc) {
			continue
		}
		fileID, ok := fileIDFor(doc.Path)
		if !ok {
			continue
		}
		for _, occ := range doc.Occurrences {
			if occ.Roles&1 != 0 {
				continue
			}
			stats.References++
			name := family.name(occ.Symbol)
			if name == "" {
				stats.Skipped++
				continue
			}
			target, ok := definitions[occ.Symbol]
			if !ok {
				stats.Skipped++
				continue
			}
			src, found, err := st.EnclosingNode(ctx, fileID, occ.Line+1)
			if err != nil {
				return stats, err
			}
			if !found || src == target {
				stats.Skipped++
				continue
			}

			key := pendingKey{fileID: fileID, line: occ.Line + 1, name: name}
			refs := pendingByKey[key]
			if len(refs) == 0 || matched[key] {
				unresolved = append(unresolved, scipUnmatchedRef{
					src: src, line: occ.Line + 1, target: target, name: name,
				})
				continue
			}
			matched[key] = true
			stats.Matched += len(refs)
			for _, ref := range refs {
				kind := edgeKind(ref.Kind)
				edgeKey := fmt.Sprintf("%d/%d/%s", src, target, kind)
				if !seenEdges[edgeKey] {
					seenEdges[edgeKey] = true
					edges = append(edges, store.Edge{
						Src: src, Dst: target, Kind: kind,
						Confidence: adapterConfidence, Rule: family.rule(ref), Line: ref.Line,
					})
				}
				if err := st.DeletePending(ctx, ref); err != nil {
					return stats, err
				}
			}
		}
	}

	// Replace ambiguous groups before the pending merge so an exact adapter
	// edge owns its key first; a later pending edge at the same key is still
	// reported as a conflict, never overwritten.
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

// scipUnmatchedRef is a compiler reference that no pending row matched. It is
// retained to attempt an ambiguous-group replacement.
type scipUnmatchedRef struct {
	src    int64
	line   int
	target int64
	name   string
}

type ambiguousLineKey struct {
	src  int64
	line int
}

// ambiguousGroup is one tree-sitter ambiguous reference: every candidate edge
// shares source node, line, kind, rule, and the `candidates` id list.
type ambiguousGroup struct {
	src  int64
	line int
	kind string
	rule string
	ids  []int64
	set  map[int64]bool
	dsts []int64
}

// loadAmbiguousGroups reads the tree-sitter ambiguous candidate rows for this
// language family, read-only, and indexes them by source node + line. A row
// that fails to parse is skipped rather than guessed: without the candidate id
// list the group cannot be checked safely.
func loadAmbiguousGroups(ctx context.Context, st *store.Store, family scipFamily) (map[ambiguousLineKey][]*ambiguousGroup, error) {
	rows, err := st.AmbiguousCandidateEdges(ctx)
	if err != nil {
		return nil, err
	}
	byKey := make(map[string]*ambiguousGroup)
	byLine := make(map[ambiguousLineKey][]*ambiguousGroup)
	for _, row := range rows {
		if !strings.HasPrefix(row.Rule, family.prefix) || !strings.HasSuffix(row.Rule, ".ambiguous") {
			continue
		}
		key := fmt.Sprintf("%d/%d/%s/%s/%s", row.Src, row.Line, row.Kind, row.Rule, row.Candidates)
		group := byKey[key]
		if group == nil {
			ids, ok := parseCandidateIDs(row.Candidates)
			if !ok {
				continue
			}
			group = &ambiguousGroup{
				src: row.Src, line: row.Line, kind: row.Kind, rule: row.Rule,
				ids: ids, set: make(map[int64]bool, len(ids)),
			}
			for _, id := range ids {
				group.set[id] = true
			}
			byKey[key] = group
			lineKey := ambiguousLineKey{src: row.Src, line: row.Line}
			byLine[lineKey] = append(byLine[lineKey], group)
		}
		if row.Dst != row.Src {
			group.dsts = append(group.dsts, row.Dst)
		}
	}
	return byLine, nil
}

func parseCandidateIDs(raw string) ([]int64, bool) {
	if raw == "" {
		return nil, false
	}
	parts := strings.Split(raw, ",")
	ids := make([]int64, 0, len(parts))
	for _, part := range parts {
		id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
		if err != nil {
			return nil, false
		}
		ids = append(ids, id)
	}
	return ids, len(ids) > 0
}

// replaceAmbiguousGroups replaces only groups that exactly one compiler
// reference occurrence disambiguates. It returns the number of compiler
// references left unmatched (no candidate group, a non-candidate target, or a
// group two references could share).
func replaceAmbiguousGroups(ctx context.Context, st *store.Store, stats *Stats, family scipFamily, byLine map[ambiguousLineKey][]*ambiguousGroup, refs []scipUnmatchedRef) (int, error) {
	mapped := make(map[*ambiguousGroup][]scipUnmatchedRef)
	unmatched := 0
	for _, ref := range refs {
		var hit *ambiguousGroup
		hits := 0
		for _, group := range byLine[ambiguousLineKey{src: ref.src, line: ref.line}] {
			if group.set[ref.target] {
				hit = group
				hits++
			}
		}
		if hits != 1 {
			unmatched++
			continue
		}
		mapped[hit] = append(mapped[hit], ref)
	}
	for group, refs := range mapped {
		// Two same-named calls on one line collapse into a single candidate
		// row set, and the index stores no reference column, so one compiler
		// occurrence cannot be attributed to one of them. Leave the group and
		// count the references instead of silently merging them.
		if len(refs) != 1 {
			unmatched += len(refs)
			continue
		}
		ref := refs[0]
		exact := store.Edge{
			Src: group.src, Dst: ref.target, Kind: group.kind,
			Confidence: adapterConfidence,
			Rule:       family.rule(store.PendingRef{Kind: refKind(group.kind), Form: ambiguousForm(group.rule)}),
			Line:       group.line,
		}
		deleted, inserted, conflict, err := st.ReplaceAmbiguousGroup(ctx, group.src, group.line, group.kind, group.dsts, exact)
		if err != nil {
			return unmatched, err
		}
		switch {
		case conflict:
			stats.Conflicts++
			unmatched++
		case !inserted:
			unmatched++
		default:
			stats.Replaced++
			stats.ReplacedEdges += deleted
			stats.Edges++
		}
	}
	return unmatched, nil
}

// ambiguousForm mirrors the pending-ref form for a replaced ambiguous edge:
// an ambiguous method rule means the reference was a member call.
func ambiguousForm(ambiguousRule string) string {
	if strings.Contains(ambiguousRule, ".method") {
		return "member"
	}
	return ""
}

// refKind maps an edge kind back to the reference kind the rule selectors use.
func refKind(kind string) string {
	switch kind {
	case "CALLS":
		return "call"
	case "EXTENDS":
		return "extends"
	case "IMPLEMENTS":
		return "implements"
	default:
		return "reference"
	}
}

func isTypeScriptDocument(doc SCIPDocument) bool {
	if strings.Contains(doc.Language, "TypeScript") || strings.Contains(doc.Language, "JavaScript") {
		return true
	}
	switch strings.ToLower(filepath.Ext(doc.Path)) {
	case ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs":
		return true
	}
	return false
}

// isPythonDocument accepts scip-python's documents. 0.6.8 leaves the language
// field empty, so the path extension is the reliable signal.
func isPythonDocument(doc SCIPDocument) bool {
	if strings.Contains(strings.ToLower(doc.Language), "python") {
		return true
	}
	return strings.EqualFold(filepath.Ext(doc.Path), ".py")
}

// isRustDocument accepts rust-analyzer documents, which carry language
// "rust". The extension is the fallback for a producer that omits it.
func isRustDocument(doc SCIPDocument) bool {
	if strings.Contains(strings.ToLower(doc.Language), "rust") {
		return true
	}
	return strings.EqualFold(filepath.Ext(doc.Path), ".rs")
}

// adapterRule is the shared selector for compiler-adapter rule ids: a member
// reference gets .method, a call .call, a constructor-like reference .new, and
// everything else .reference. The caller supplies the namespace prefix, so
// `scip.ts`, `scip.py`, `lsp.ts`, and `lsp.py` stay one rule table.
func adapterRule(prefix string, ref store.PendingRef) string {
	if ref.Form == "member" || ref.Form == "this" {
		return prefix + ".method"
	}
	switch ref.Kind {
	case "call":
		return prefix + ".call"
	case "new":
		return prefix + ".new"
	default:
		return prefix + ".reference"
	}
}

func tsAdapterRule(ref store.PendingRef) string { return adapterRule("scip.ts", ref) }

func pyAdapterRule(ref store.PendingRef) string { return adapterRule("scip.py", ref) }

func rsAdapterRule(ref store.PendingRef) string { return adapterRule("scip.rs", ref) }

func edgeKind(refKind string) string {
	switch refKind {
	case "call":
		return "CALLS"
	case "extends":
		return "EXTENDS"
	case "implements":
		return "IMPLEMENTS"
	default:
		return "REFERENCES"
	}
}

const statsPrefix = "adapter."

func saveStats(ctx context.Context, st *store.Store, stats Stats) error {
	data, err := json.Marshal(stats)
	if err != nil {
		return err
	}
	return st.SetMeta(ctx, statsPrefix+stats.Adapter, string(data))
}

// SavedStats reads every persisted adapter run from meta, sorted by name.
func SavedStats(ctx context.Context, st *store.Store) ([]Stats, error) {
	meta, err := st.Meta(ctx)
	if err != nil {
		return nil, err
	}
	var out []Stats
	for key, value := range meta {
		if !strings.HasPrefix(key, statsPrefix) {
			continue
		}
		var stats Stats
		if err := json.Unmarshal([]byte(value), &stats); err != nil {
			continue
		}
		if stats.Adapter == "" {
			stats.Adapter = strings.TrimPrefix(key, statsPrefix)
		}
		out = append(out, stats)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Adapter < out[j].Adapter })
	return out, nil
}
