package store

import (
	"context"
	"database/sql"
	"fmt"
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

// InsertEdges writes edges, ignoring ones that already exist.
func (s *Store) InsertEdges(ctx context.Context, edges []Edge) (int, error) {
	if len(edges) == 0 {
		return 0, nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO edges (src, dst, kind, confidence, rule, line, candidates)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (src, dst, kind) DO UPDATE SET
			confidence = excluded.confidence,
			rule = excluded.rule,
			line = excluded.line,
			candidates = excluded.candidates`)
	if err != nil {
		tx.Rollback()
		return 0, err
	}
	defer stmt.Close()

	written := 0
	for _, e := range edges {
		if _, err := stmt.ExecContext(ctx, e.Src, e.Dst, e.Kind, e.Confidence, e.Rule, e.Line, e.Candidates); err != nil {
			tx.Rollback()
			return written, err
		}
		written++
	}
	if err := tx.Commit(); err != nil {
		return written, err
	}
	return written, nil
}

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

// ReplacePending swaps the pending queue for one file, in a transaction.
func (s *Store) ReplacePending(ctx context.Context, fileID int64, refs []PendingRef) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM pending WHERE file_id = ?`, fileID); err != nil {
		tx.Rollback()
		return err
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO pending (name, file_id, kind, form, line) VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		tx.Rollback()
		return err
	}
	defer stmt.Close()
	for _, p := range refs {
		if _, err := stmt.ExecContext(ctx, p.Name, fileID, p.Kind, p.Form, p.Line); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
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
