package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
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

// EdgePair is one edge's endpoints plus its rule, without the endpoint symbols
// joined in. It is the shape a resolution input needs.
type EdgePair struct {
	Src, Dst int64
	Rule     string
}

// UnambiguousEdgePairs returns the endpoints of every edge of one kind whose
// resolution was not ambiguous (its candidates column is empty). Resolution
// uses this as a persisted input: the Python inheritance walk needs the
// superclass links of the files the current run did not re-resolve. It never
// changes the graph, so it is not a query answer.
func (s *Store) UnambiguousEdgePairs(ctx context.Context, kind string) ([]EdgePair, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT src, dst, rule FROM edges WHERE kind = ? AND candidates = ''`, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []EdgePair
	for rows.Next() {
		var p EdgePair
		if err := rows.Scan(&p.Src, &p.Dst, &p.Rule); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

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
	// Candidates is the comma-separated node id list of an ambiguous group; it
	// is empty for a single-target edge.
	Candidates string
}

// Edges returns every edge at or above minConfidence with its endpoints named.
func (s *Store) Edges(ctx context.Context, minConfidence float64) ([]EdgeDetail, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT sf.path, s.qname, df.path, d.qname, e.kind, e.confidence, e.rule, e.line, e.candidates
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
			&e.Kind, &e.Confidence, &e.Rule, &e.Line, &e.Candidates); err != nil {
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

// PendingFileDim is the file dimension the pending classifier needs. Files and
// modules are dimensions, not nodes, so this is read separately from pending.
type PendingFileDim struct {
	Lang     string
	ParseOK  bool
	ModuleID int64
}

// SummarizePending classifies pending references into exactly one reason bucket
// and derives the per-language and top-N name/module lists. It is pure so
// status, check, and any later caller share one classifier instead of
// reimplementing it.
//
// Precedence is parse-failure > member-scope > missing-definition > scope-gap,
// so the four reason counts always sum to Total. Nothing is retained after the
// call: the pending slice is owned by the caller. Its row storage is ~7 MB and
// the scanned strings add ~3 MB for 105k pending rows; peak RSS also carries
// Go allocator headroom (measured +25 MB peak RSS on the Django corpus), all released
// when the call returns.
func SummarizePending(pending []PendingRef, fileDim map[int64]PendingFileDim, moduleNames map[int64]string, defined map[string]bool, topN int) UnresolvedSummary {
	if topN <= 0 {
		topN = 10
	}
	out := UnresolvedSummary{
		Total:    len(pending),
		ByReason: make(map[string]int, len(PendingReasons)),
		ByLang:   map[string]int{},
	}
	for _, reason := range PendingReasons {
		out.ByReason[reason] = 0
	}

	nameCounts := make(map[string]int)
	moduleCounts := make(map[string]int)
	for _, ref := range pending {
		dim, hasDim := fileDim[ref.FileID]
		switch {
		case hasDim && !dim.ParseOK:
			out.ByReason[PendingReasonParseFailure]++
		case defined[ref.Name] && ref.Form == "member":
			out.ByReason[PendingReasonMemberScope]++
		case !defined[ref.Name]:
			out.ByReason[PendingReasonMissingDefinition]++
		default:
			out.ByReason[PendingReasonScopeGap]++
		}
		if hasDim {
			out.ByLang[dim.Lang]++
			moduleCounts[pendingModuleName(moduleNames, dim.ModuleID)]++
		}
		nameCounts[ref.Name]++
	}

	out.TopNames = topPendingBuckets(nameCounts, topN)
	out.TopModules = topPendingBuckets(moduleCounts, topN)
	return out
}

// pendingModuleName resolves a module id to its display name; files without a
// module (or with an unnamed one) fall back to "(root)" like module detection.
func pendingModuleName(moduleNames map[int64]string, moduleID int64) string {
	if name := moduleNames[moduleID]; name != "" {
		return name
	}
	return "(root)"
}

// topPendingBuckets sorts a name→count map by count descending, then name
// ascending, and truncates to topN. An empty map yields a nil list.
func topPendingBuckets(counts map[string]int, topN int) []PendingBucket {
	if len(counts) == 0 {
		return nil
	}
	out := make([]PendingBucket, 0, len(counts))
	for name, count := range counts {
		out = append(out, PendingBucket{Name: name, Count: count})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Name < out[j].Name
	})
	if len(out) > topN {
		out = out[:topN]
	}
	return out
}

// pendingClassificationRefs reads only the pending columns SummarizePending
// uses (name, file id, form). The status path does not need kind or line, and
// skipping them keeps the transient allocation proportional to the table
// without a third string per row.
func (s *Store) pendingClassificationRefs(ctx context.Context) ([]PendingRef, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, file_id, form FROM pending`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []PendingRef
	for rows.Next() {
		var ref PendingRef
		if err := rows.Scan(&ref.Name, &ref.FileID, &ref.Form); err != nil {
			return nil, err
		}
		out = append(out, ref)
	}
	return out, rows.Err()
}

// PendingFileDims reads the file dimension for unresolved-reference
// classification: language, parse success, and module id.
func (s *Store) PendingFileDims(ctx context.Context) (map[int64]PendingFileDim, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, lang, parse_ok, module_id FROM files`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[int64]PendingFileDim)
	for rows.Next() {
		var id int64
		var dim PendingFileDim
		if err := rows.Scan(&id, &dim.Lang, &dim.ParseOK, &dim.ModuleID); err != nil {
			return nil, err
		}
		out[id] = dim
	}
	return out, rows.Err()
}

// pendingDefinedNames returns the node names that pending references ask for.
// Restricting the query to pending names keeps it a bounded read on large trees.
func (s *Store) pendingDefinedNames(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT DISTINCT n.name
FROM nodes n
JOIN (SELECT DISTINCT name FROM pending) pn ON pn.name = n.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]bool)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}

// UnresolvedSummary classifies every pending reference into exactly one reason
// bucket and reports the largest name and module buckets. topN bounds each
// list; the reason totals are never truncated.
//
// The classification is SummarizePending; this method only performs the four
// read-only lookups it needs (pending rows, the file dimension, module names,
// and the node names pending references use) so status and check cannot
// diverge. The pending slice is transient: UnresolvedSummary does not retain it.
func (s *Store) UnresolvedSummary(ctx context.Context, topN int) (UnresolvedSummary, error) {
	pending, err := s.pendingClassificationRefs(ctx)
	if err != nil {
		return UnresolvedSummary{}, err
	}
	fileDim, err := s.PendingFileDims(ctx)
	if err != nil {
		return UnresolvedSummary{}, err
	}
	modules, err := s.Modules(ctx)
	if err != nil {
		return UnresolvedSummary{}, err
	}
	moduleNames := make(map[int64]string, len(modules))
	for _, module := range modules {
		moduleNames[module.ID] = module.Name
	}
	defined := map[string]bool{}
	if len(pending) > 0 {
		if defined, err = s.pendingDefinedNames(ctx); err != nil {
			return UnresolvedSummary{}, err
		}
	}
	return SummarizePending(pending, fileDim, moduleNames, defined, topN), nil
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
