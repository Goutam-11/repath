package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// NodeRow is a symbol read back from the index.
type NodeRow struct {
	ID        int64
	Kind      string
	Name      string
	QName     string
	Lang      string
	FileID    int64
	Arity     int
	StartLine int
	EndLine   int
	Exported  bool
}

// Edge is one relation between two symbols.
type Edge struct {
	Src        int64
	Dst        int64
	Kind       string
	Confidence float64
	Rule       string
	Line       int
	Candidates string
}

// PendingRef is a reference whose target was not in the index yet.
type PendingRef struct {
	Name   string
	FileID int64
	Kind   string
	Form   string
	Line   int
}

// EdgeRow is an edge with the source symbol and file path joined in.
type EdgeRow struct {
	Edge
	SrcNode NodeRow
	SrcPath string
}

// Nodes reads every symbol. Callers that need a global name index use this.
func (s *Store) Nodes(ctx context.Context) ([]NodeRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, kind, name, qname, lang, file_id, arity, start_line, end_line, exported FROM nodes`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []NodeRow
	for rows.Next() {
		var n NodeRow
		if err := rows.Scan(&n.ID, &n.Kind, &n.Name, &n.QName, &n.Lang, &n.FileID,
			&n.Arity, &n.StartLine, &n.EndLine, &n.Exported); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ResolveNodes reads the seven symbol columns resolution uses, omitting the
// language and line-span columns that only display needs.
func (s *Store) ResolveNodes(ctx context.Context) ([]NodeRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, kind, name, qname, file_id, arity, exported FROM nodes`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []NodeRow
	for rows.Next() {
		var n NodeRow
		if err := rows.Scan(&n.ID, &n.Kind, &n.Name, &n.QName, &n.FileID, &n.Arity, &n.Exported); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// MapNode is the subset of a symbol `map` needs to rank and roll up: no name,
// language, arity, or end line. Loading ten columns for every symbol was the
// biggest single cost in `map` on the real tree.
type MapNode struct {
	ID        int64
	FileID    int64
	QName     string
	Kind      string
	StartLine int
}

// MapNodes reads every symbol with only the columns map consumes.
func (s *Store) MapNodes(ctx context.Context) ([]MapNode, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, file_id, qname, kind, start_line FROM nodes`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []MapNode
	for rows.Next() {
		var n MapNode
		if err := rows.Scan(&n.ID, &n.FileID, &n.QName, &n.Kind, &n.StartLine); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// FilePathsByIDs maps selected file ids to paths. `map` needs paths only for
// the ranked symbols it returns, not for all 38k files.
func (s *Store) FilePathsByIDs(ctx context.Context, ids []int64) (map[int64]string, error) {
	out := make(map[int64]string, len(ids))
	for start := 0; start < len(ids); start += maxParams {
		end := start + maxParams
		if end > len(ids) {
			end = len(ids)
		}
		batch := ids[start:end]
		args := make([]any, len(batch))
		for i, id := range batch {
			args[i] = id
		}
		rows, err := s.db.QueryContext(ctx,
			`SELECT id, path FROM files WHERE id IN (`+placeholders(len(batch))+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int64
			var path string
			if err := rows.Scan(&id, &path); err != nil {
				rows.Close()
				return nil, err
			}
			out[id] = path
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

// FilePaths maps file ids to repo-relative paths.
func (s *Store) FilePaths(ctx context.Context) (map[int64]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, path FROM files`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[int64]string)
	for rows.Next() {
		var id int64
		var path string
		if err := rows.Scan(&id, &path); err != nil {
			return nil, err
		}
		out[id] = path
	}
	return out, rows.Err()
}

// InsertEdges writes edges, updating an existing (src, dst, kind) row. Rows are
// grouped into multi-row statements: one prepared exec per edge was the second
// half of the cold-index SQLite cost after the per-file pending scan.
func (s *Store) InsertEdges(ctx context.Context, edges []Edge) (int, error) {
	if len(edges) == 0 {
		return 0, nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	written := 0
	for start := 0; start < len(edges); start += edgeBatchSize {
		end := start + edgeBatchSize
		if end > len(edges) {
			end = len(edges)
		}
		batch := edges[start:end]

		var query strings.Builder
		query.WriteString(`INSERT INTO edges (src, dst, kind, confidence, rule, line, candidates) VALUES `)
		args := make([]any, 0, len(batch)*7)
		for i, e := range batch {
			if i > 0 {
				query.WriteByte(',')
			}
			query.WriteString("(?, ?, ?, ?, ?, ?, ?)")
			args = append(args, e.Src, e.Dst, e.Kind, e.Confidence, e.Rule, e.Line, e.Candidates)
		}
		query.WriteString(` ON CONFLICT (src, dst, kind) DO UPDATE SET
			confidence = excluded.confidence,
			rule = excluded.rule,
			line = excluded.line,
			candidates = excluded.candidates`)
		if _, err := tx.ExecContext(ctx, query.String(), args...); err != nil {
			tx.Rollback()
			return written, err
		}
		written += len(batch)
	}
	if err := tx.Commit(); err != nil {
		return written, err
	}
	return written, nil
}

// edgeBatchSize keeps a multi-row statement under SQLite's default variable
// limit (7 columns per row).
const edgeBatchSize = 100

// PruneEdges removes edges whose endpoints no longer exist, which is what
// incremental re-indexing leaves behind when a file changes.
func (s *Store) PruneEdges(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM edges
		WHERE src NOT IN (SELECT id FROM nodes) OR dst NOT IN (SELECT id FROM nodes)`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Pending lists references still waiting for a target.
func (s *Store) Pending(ctx context.Context) ([]PendingRef, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, file_id, kind, form, line FROM pending`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []PendingRef
	for rows.Next() {
		var p PendingRef
		if err := rows.Scan(&p.Name, &p.FileID, &p.Kind, &p.Form, &p.Line); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ReplacePendingFiles swaps the pending queue for several files in one
// transaction. Pending has no file_id index, so a per-file delete is a full
// table scan; deleting every changed file before inserting keeps the scan from
// growing with each inserted row.
func (s *Store) ReplacePendingFiles(ctx context.Context, fileIDs []int64, refs []PendingRef) error {
	if len(fileIDs) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	for start := 0; start < len(fileIDs); start += maxParams {
		end := start + maxParams
		if end > len(fileIDs) {
			end = len(fileIDs)
		}
		batch := fileIDs[start:end]
		args := make([]any, len(batch))
		for i, id := range batch {
			args[i] = id
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM pending WHERE file_id IN (`+placeholders(len(batch))+`)`, args...); err != nil {
			tx.Rollback()
			return err
		}
	}
	if len(refs) > 0 {
		stmt, err := tx.PrepareContext(ctx, `INSERT INTO pending (name, file_id, kind, form, line) VALUES (?, ?, ?, ?, ?)`)
		if err != nil {
			tx.Rollback()
			return err
		}
		defer stmt.Close()
		for _, p := range refs {
			if _, err := stmt.ExecContext(ctx, p.Name, p.FileID, p.Kind, p.Form, p.Line); err != nil {
				tx.Rollback()
				return err
			}
		}
	}
	return tx.Commit()
}

// PendingRetryByName lists the pending references that a retry can resolve:
// identifiers whose name is now globally unique. The name set travels as one
// JSON array so the query is a single indexed statement rather than 70
// parameter-chunked ones; pending_name keeps the probe proportional to the set.
func (s *Store) PendingRetryByName(ctx context.Context, names []string) ([]PendingRef, error) {
	if len(names) == 0 {
		return nil, nil
	}
	encoded, err := json.Marshal(names)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT name, file_id, kind, form, line FROM pending
		WHERE name IN (SELECT value FROM json_each(?))
		  AND (form = '' OR form = 'ident')`, string(encoded))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []PendingRef
	for rows.Next() {
		var p PendingRef
		if err := rows.Scan(&p.Name, &p.FileID, &p.Kind, &p.Form, &p.Line); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// DeletePendingBatch removes several pending rows in one transaction, grouping
// the row-value deletes so the retry does not pay one exec per reference.
func (s *Store) DeletePendingBatch(ctx context.Context, refs []PendingRef) error {
	if len(refs) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	for start := 0; start < len(refs); start += pendingDeleteBatch {
		end := start + pendingDeleteBatch
		if end > len(refs) {
			end = len(refs)
		}
		batch := refs[start:end]

		var query strings.Builder
		query.WriteString(`DELETE FROM pending WHERE (name, file_id, kind, form, line) IN (VALUES `)
		args := make([]any, 0, len(batch)*5)
		for i, p := range batch {
			if i > 0 {
				query.WriteByte(',')
			}
			query.WriteString("(?, ?, ?, ?, ?)")
			args = append(args, p.Name, p.FileID, p.Kind, p.Form, p.Line)
		}
		query.WriteByte(')')
		if _, err := tx.ExecContext(ctx, query.String(), args...); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// pendingDeleteBatch keeps the five-column row values under the variable limit.
const pendingDeleteBatch = 100

// ImportRow is one local binding created by an import statement. Imports are
// resolution inputs, never graph edges.
type ImportRow struct {
	FileID   int64
	Local    string
	Imported string
	Source   string
	Line     int
	Public   bool
}

// Imports reads every stored import binding.
func (s *Store) Imports(ctx context.Context) ([]ImportRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT file_id, local, imported, source, line, public FROM imports ORDER BY rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ImportRow
	for rows.Next() {
		var imp ImportRow
		if err := rows.Scan(&imp.FileID, &imp.Local, &imp.Imported, &imp.Source, &imp.Line, &imp.Public); err != nil {
			return nil, err
		}
		out = append(out, imp)
	}
	return out, rows.Err()
}

// ImportsByFiles reads the import bindings of selected files. The imports_file
// index keeps this proportional to the changed files, not the whole tree.
func (s *Store) ImportsByFiles(ctx context.Context, fileIDs []int64) ([]ImportRow, error) {
	var out []ImportRow
	for start := 0; start < len(fileIDs); start += maxParams {
		end := start + maxParams
		if end > len(fileIDs) {
			end = len(fileIDs)
		}
		batch := fileIDs[start:end]
		args := make([]any, len(batch))
		for i, id := range batch {
			args[i] = id
		}
		rows, err := s.db.QueryContext(ctx,
			`SELECT file_id, local, imported, source, line, public FROM imports
			 WHERE file_id IN (`+placeholders(len(batch))+`) ORDER BY rowid`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var imp ImportRow
			if err := rows.Scan(&imp.FileID, &imp.Local, &imp.Imported, &imp.Source, &imp.Line, &imp.Public); err != nil {
				rows.Close()
				return nil, err
			}
			out = append(out, imp)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

// ReplaceImports swaps the import bindings for one file, in a transaction.
func (t *Tx) ReplaceImports(ctx context.Context, fileID int64, imports []ImportRow) error {
	if _, err := t.tx.ExecContext(ctx, `DELETE FROM imports WHERE file_id = ?`, fileID); err != nil {
		return err
	}
	if len(imports) == 0 {
		return nil
	}
	stmt, err := t.tx.PrepareContext(ctx,
		`INSERT INTO imports (file_id, local, imported, source, line, public) VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, imp := range imports {
		if _, err := stmt.ExecContext(ctx, fileID, imp.Local, imp.Imported, imp.Source, imp.Line, boolInt(imp.Public)); err != nil {
			return err
		}
	}
	return nil
}

// DeletePending removes one pending row.
func (s *Store) DeletePending(ctx context.Context, p PendingRef) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM pending WHERE name = ? AND file_id = ? AND kind = ? AND form = ? AND line = ?`,
		p.Name, p.FileID, p.Kind, p.Form, p.Line)
	return err
}

// PrunePending drops pending rows whose file is gone.
func (s *Store) PrunePending(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM pending WHERE file_id NOT IN (SELECT id FROM files)`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// SearchSymbols returns symbols whose name matches, exact qname first.
func (s *Store) SearchSymbols(ctx context.Context, name string, limit int) ([]NodeRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, kind, name, qname, lang, file_id, arity, start_line, end_line, exported
		FROM nodes
		WHERE qname = ? OR name = ?
		ORDER BY CASE WHEN qname = ? THEN 0 ELSE 1 END, qname
		LIMIT ?`, name, name, name, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []NodeRow
	for rows.Next() {
		var n NodeRow
		if err := rows.Scan(&n.ID, &n.Kind, &n.Name, &n.QName, &n.Lang, &n.FileID,
			&n.Arity, &n.StartLine, &n.EndLine, &n.Exported); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// NodeIDsByFile maps qname to node id for one file.
func (s *Store) NodeIDsByFile(ctx context.Context, fileID int64) (map[string]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT qname, id FROM nodes WHERE file_id = ?`, fileID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]int64)
	for rows.Next() {
		var qname string
		var id int64
		if err := rows.Scan(&qname, &id); err != nil {
			return nil, err
		}
		out[qname] = id
	}
	return out, rows.Err()
}

// EnclosingNode finds the smallest symbol whose span contains line.
func (s *Store) EnclosingNode(ctx context.Context, fileID int64, line int) (int64, bool, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `
		SELECT id FROM nodes
		WHERE file_id = ? AND start_line <= ? AND end_line >= ?
		ORDER BY (end_line - start_line) ASC
		LIMIT 1`, fileID, line, line).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return id, true, nil
}

// EdgeDetail is an edge with both file paths and qnames resolved, for reports.
type EdgeDetail struct {
	SrcPath, SrcQName string
	DstPath, DstQName string
	Kind              string
	Confidence        float64
	Rule              string
	Line              int
}

// Edges returns every edge at or above minConfidence with its endpoints named.
func (s *Store) Edges(ctx context.Context, minConfidence float64) ([]EdgeDetail, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT sf.path, s.qname, df.path, d.qname, e.kind, e.confidence, e.rule, e.line
		FROM edges e
		JOIN nodes s ON s.id = e.src
		JOIN files sf ON sf.id = s.file_id
		JOIN nodes d ON d.id = e.dst
		JOIN files df ON df.id = d.file_id
		WHERE e.confidence >= ?
		ORDER BY sf.path, s.qname, e.kind, df.path, d.qname`, minConfidence)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []EdgeDetail
	for rows.Next() {
		var e EdgeDetail
		if err := rows.Scan(&e.SrcPath, &e.SrcQName, &e.DstPath, &e.DstQName,
			&e.Kind, &e.Confidence, &e.Rule, &e.Line); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// NodeByID loads one symbol.
func (s *Store) NodeByID(ctx context.Context, id int64) (NodeRow, error) {
	var n NodeRow
	err := s.db.QueryRowContext(ctx, `
		SELECT id, kind, name, qname, lang, file_id, arity, start_line, end_line, exported
		FROM nodes WHERE id = ?`, id).
		Scan(&n.ID, &n.Kind, &n.Name, &n.QName, &n.Lang, &n.FileID,
			&n.Arity, &n.StartLine, &n.EndLine, &n.Exported)
	if err == sql.ErrNoRows {
		return n, fmt.Errorf("symbol %d not found", id)
	}
	return n, err
}

// Callers returns edges pointing at nodeID at or above minConfidence.
func (s *Store) Callers(ctx context.Context, nodeID int64, minConfidence float64) ([]EdgeRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT e.src, e.dst, e.kind, e.confidence, e.rule, e.line,
		       n.id, n.kind, n.name, n.qname, n.lang, n.file_id, n.arity, n.start_line, n.end_line, n.exported,
		       f.path
		FROM edges e
		JOIN nodes n ON n.id = e.src
		JOIN files f ON f.id = n.file_id
		WHERE e.dst = ? AND e.confidence >= ?
		ORDER BY e.confidence DESC, n.qname`, nodeID, minConfidence)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []EdgeRow
	for rows.Next() {
		var e EdgeRow
		if err := rows.Scan(&e.Src, &e.Dst, &e.Kind, &e.Confidence, &e.Rule, &e.Line,
			&e.SrcNode.ID, &e.SrcNode.Kind, &e.SrcNode.Name, &e.SrcNode.QName, &e.SrcNode.Lang,
			&e.SrcNode.FileID, &e.SrcNode.Arity, &e.SrcNode.StartLine, &e.SrcNode.EndLine, &e.SrcNode.Exported,
			&e.SrcPath); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// CountEdges is the total row count, cheaper than grouping by kind.
func (s *Store) CountEdges(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM edges`).Scan(&n)
	return n, err
}

// EdgeCounts counts edges by kind.
func (s *Store) EdgeCounts(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT kind, COUNT(*) FROM edges GROUP BY kind`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]int{}
	for rows.Next() {
		var kind string
		var n int
		if err := rows.Scan(&kind, &n); err != nil {
			return nil, err
		}
		out[kind] = n
	}
	return out, rows.Err()
}

// CountPending counts unresolved references.
func (s *Store) CountPending(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pending`).Scan(&n)
	return n, err
}

// Pending reason buckets. They are coarse, documented classifications of an
// unresolved reference and are computed at read time from the pending table,
// the file's parse_ok flag, and whether the name exists anywhere in nodes. The
// constants are also the classifier precedence, in order.
const (
	PendingReasonParseFailure      = "parse-failure"
	PendingReasonMemberScope       = "member-scope"
	PendingReasonMissingDefinition = "missing-definition"
	PendingReasonScopeGap          = "scope-gap"
)

// PendingReasons is the canonical bucket order used by JSON consumers and text
// renderers. Keep it in sync with the constants above.
var PendingReasons = []string{
	PendingReasonParseFailure,
	PendingReasonMemberScope,
	PendingReasonMissingDefinition,
	PendingReasonScopeGap,
}

// PendingBucket is one named group in an unresolved summary.
type PendingBucket struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

// UnresolvedSummary explains the pending (unresolved) references. It is
// derived at read time so it cannot drift from the counts, and its reason
// counts always sum to Total.
type UnresolvedSummary struct {
	Total      int             `json:"total"`
	ByReason   map[string]int  `json:"by_reason"`
	ByLang     map[string]int  `json:"by_lang,omitempty"`
	TopNames   []PendingBucket `json:"top_names,omitempty"`
	TopModules []PendingBucket `json:"top_modules,omitempty"`
}

// UnresolvedSummary classifies every pending reference into exactly one reason
// bucket and reports the largest name and module buckets. topN bounds each
// list; the reason totals are never truncated. The classifier is:
//
//	parse-failure       the reference's file has parse_ok = 0
//	member-scope        form = 'member' and the name exists somewhere
//	missing-definition  no node has that name anywhere
//	scope-gap           the name exists but the import/scope did not resolve
//
// Precedence is the order above. This is a heuristic diagnosis of the pending
// table, not a compiler error: a member-scope reference may be an inherited
// method or a typed receiver the resolver cannot infer, and a scope-gap may be
// a barrel re-export or a workspace package. The aggregation runs in SQL and
// never loads the pending rows into Go.
func (s *Store) UnresolvedSummary(ctx context.Context, topN int) (UnresolvedSummary, error) {
	if topN <= 0 {
		topN = 10
	}
	out := UnresolvedSummary{
		ByReason: make(map[string]int, len(PendingReasons)),
		ByLang:   map[string]int{},
	}
	for _, reason := range PendingReasons {
		out.ByReason[reason] = 0
	}

	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM pending`).Scan(&out.Total); err != nil {
		return UnresolvedSummary{}, err
	}
	if out.Total == 0 {
		return out, nil
	}

	// Roll pending up per name first: Django has 105k pending rows but only a
	// few thousand distinct names, so the nodes-name join stays small. The
	// files join is a primary-key probe and the per-name grouping is indexed
	// by pending_name, so this stays a bounded read on large trees.
	var parseFailure, memberScope, missingDefinition, scopeGap int
	err := s.db.QueryRowContext(ctx, `
WITH pending_names(name) AS (SELECT DISTINCT name FROM pending),
defined(name) AS (
    SELECT DISTINCT n.name
    FROM nodes n
    JOIN pending_names pn ON pn.name = n.name
),
pn AS (
    SELECT p.name AS name,
           COUNT(*) AS total,
           SUM(CASE WHEN p.form = 'member' AND COALESCE(f.parse_ok, 1) <> 0 THEN 1 ELSE 0 END) AS members,
           SUM(CASE WHEN COALESCE(f.parse_ok, 1) = 0 THEN 1 ELSE 0 END) AS parse_fail
    FROM pending p
    LEFT JOIN files f ON f.id = p.file_id
    GROUP BY p.name
)
SELECT
    COALESCE(SUM(pn.parse_fail), 0),
    COALESCE(SUM(CASE WHEN d.name IS NOT NULL THEN pn.members ELSE 0 END), 0),
    COALESCE(SUM(CASE WHEN d.name IS NULL THEN pn.total - pn.parse_fail ELSE 0 END), 0),
    COALESCE(SUM(CASE WHEN d.name IS NOT NULL THEN pn.total - pn.parse_fail - pn.members ELSE 0 END), 0)
FROM pn
LEFT JOIN defined d ON d.name = pn.name`).
		Scan(&parseFailure, &memberScope, &missingDefinition, &scopeGap)
	if err != nil {
		return UnresolvedSummary{}, err
	}
	out.ByReason[PendingReasonParseFailure] = parseFailure
	out.ByReason[PendingReasonMemberScope] = memberScope
	out.ByReason[PendingReasonMissingDefinition] = missingDefinition
	out.ByReason[PendingReasonScopeGap] = scopeGap

	langRows, err := s.db.QueryContext(ctx, `
SELECT f.lang, COUNT(*)
FROM pending p
JOIN files f ON f.id = p.file_id
GROUP BY f.lang
ORDER BY COUNT(*) DESC, f.lang`)
	if err != nil {
		return UnresolvedSummary{}, err
	}
	for langRows.Next() {
		var lang string
		var count int
		if err := langRows.Scan(&lang, &count); err != nil {
			langRows.Close()
			return UnresolvedSummary{}, err
		}
		out.ByLang[lang] = count
	}
	err = langRows.Err()
	langRows.Close()
	if err != nil {
		return UnresolvedSummary{}, err
	}

	if out.TopNames, err = s.pendingBuckets(ctx,
		`SELECT name, COUNT(*) c FROM pending GROUP BY name ORDER BY c DESC, name LIMIT ?`, topN); err != nil {
		return UnresolvedSummary{}, err
	}
	if out.TopModules, err = s.pendingBuckets(ctx, `
SELECT COALESCE(NULLIF(m.name, ''), '(root)') AS module, COUNT(*) c
FROM pending p
JOIN files f ON f.id = p.file_id
LEFT JOIN modules m ON m.id = f.module_id
GROUP BY module
ORDER BY c DESC, module
LIMIT ?`, topN); err != nil {
		return UnresolvedSummary{}, err
	}
	return out, nil
}

// pendingBuckets runs a grouped pending query whose two columns are a name and
// a count, with a deterministic name tie-break supplied by the caller's ORDER BY.
func (s *Store) pendingBuckets(ctx context.Context, query string, limit int) ([]PendingBucket, error) {
	rows, err := s.db.QueryContext(ctx, query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []PendingBucket
	for rows.Next() {
		var bucket PendingBucket
		if err := rows.Scan(&bucket.Name, &bucket.Count); err != nil {
			return nil, err
		}
		out = append(out, bucket)
	}
	return out, rows.Err()
}

// AmbiguousEdges counts 0.40-confidence ambiguity edges, which are excluded by
// the default query threshold but must stay visible in status.
func (s *Store) AmbiguousEdges(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM edges WHERE rule LIKE '%.ambiguous'`).Scan(&n)
	return n, err
}

// CallerCount and CalleeCount are cheap counts for the find card.
func (s *Store) CallerCount(ctx context.Context, nodeID int64, minConfidence float64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM edges WHERE dst = ? AND confidence >= ?`, nodeID, minConfidence).Scan(&n)
	return n, err
}

// CalleeCount counts outgoing edges.
func (s *Store) CalleeCount(ctx context.Context, nodeID int64, minConfidence float64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM edges WHERE src = ? AND confidence >= ?`, nodeID, minConfidence).Scan(&n)
	return n, err
}

// UnresolvedNames lists distinct pending names, for status output.
func (s *Store) UnresolvedNames(ctx context.Context, limit int) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT name, COUNT(*) c FROM pending GROUP BY name ORDER BY c DESC, name LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var name string
		var count int
		if err := rows.Scan(&name, &count); err != nil {
			return nil, err
		}
		out = append(out, fmt.Sprintf("%s (%d)", name, count))
	}
	return out, rows.Err()
}

// Entrypoint is one entrypoint row joined to its symbol and file.
type Entrypoint struct {
	EntrypointRow
	Node NodeRow
	File string
}

// Entrypoints lists every entrypoint, stable by file and line.
func (s *Store) Entrypoints(ctx context.Context) ([]Entrypoint, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT ep.node_id, ep.file_id, ep.rule, ep.name, ep.confidence, ep.line,
		       n.id, n.kind, n.name, n.qname, n.lang, n.file_id, n.arity, n.start_line, n.end_line, n.exported,
		       f.path
		FROM entrypoints ep
		JOIN nodes n ON n.id = ep.node_id
		JOIN files f ON f.id = ep.file_id
		ORDER BY f.path, ep.line, ep.rule`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Entrypoint
	for rows.Next() {
		var ep Entrypoint
		if err := rows.Scan(
			&ep.NodeID, &ep.FileID, &ep.Rule, &ep.Name, &ep.Confidence, &ep.Line,
			&ep.Node.ID, &ep.Node.Kind, &ep.Node.Name, &ep.Node.QName, &ep.Node.Lang,
			&ep.Node.FileID, &ep.Node.Arity, &ep.Node.StartLine, &ep.Node.EndLine,
			&ep.Node.Exported, &ep.File,
		); err != nil {
			return nil, err
		}
		out = append(out, ep)
	}
	return out, rows.Err()
}

// EntrypointCount counts the entrypoint dimension.
func (s *Store) EntrypointCount(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM entrypoints`).Scan(&n)
	return n, err
}

// EntrypointCounts counts entrypoints by rule.
func (s *Store) EntrypointCounts(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT rule, COUNT(*) FROM entrypoints GROUP BY rule ORDER BY rule`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]int{}
	for rows.Next() {
		var rule string
		var n int
		if err := rows.Scan(&rule, &n); err != nil {
			return nil, err
		}
		out[rule] = n
	}
	return out, rows.Err()
}
