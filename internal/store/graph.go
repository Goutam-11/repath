package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// Module is one package or directory grouping.
type Module struct {
	Name string
	Kind string
	Root string
}

// ModuleRow is a stored module.
type ModuleRow struct {
	ID   int64
	Name string
	Kind string
	Root string
}

// EdgeOut is an edge with both endpoints joined to their symbols and files.
type EdgeOut struct {
	Src, Dst   int64
	Kind       string
	Confidence float64
	Rule       string
	Line       int
	SrcNode    NodeRow
	DstNode    NodeRow
	SrcPath    string
	DstPath    string
}

// UpsertModules records the module list and returns root → id.
func (s *Store) UpsertModules(ctx context.Context, modules []Module) (map[string]int64, error) {
	ids := make(map[string]int64, len(modules))

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO modules (name, kind, root) VALUES (?, ?, ?)
		ON CONFLICT(root) DO UPDATE SET name = excluded.name, kind = excluded.kind
		RETURNING id`)
	if err != nil {
		tx.Rollback()
		return nil, err
	}
	defer stmt.Close()

	for _, m := range modules {
		var id int64
		if err := stmt.QueryRowContext(ctx, m.Name, m.Kind, m.Root).Scan(&id); err != nil {
			tx.Rollback()
			return nil, fmt.Errorf("module %s: %w", m.Root, err)
		}
		ids[m.Root] = id
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return ids, nil
}

// Modules lists every stored module.
func (s *Store) Modules(ctx context.Context) ([]ModuleRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, kind, root FROM modules ORDER BY root`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ModuleRow
	for rows.Next() {
		var m ModuleRow
		if err := rows.Scan(&m.ID, &m.Name, &m.Kind, &m.Root); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// FileModules maps file id → module id.
func (s *Store) FileModules(ctx context.Context) (map[int64]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, module_id FROM files`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[int64]int64)
	for rows.Next() {
		var fileID, moduleID int64
		if err := rows.Scan(&fileID, &moduleID); err != nil {
			return nil, err
		}
		out[fileID] = moduleID
	}
	return out, rows.Err()
}

// Incoming returns edges whose destination is in dstIDs.
func (s *Store) Incoming(ctx context.Context, dstIDs []int64, minConfidence float64) ([]EdgeOut, error) {
	return s.edgesWhere(ctx, "e.dst", dstIDs, minConfidence)
}

// Outgoing returns edges whose source is in srcIDs.
func (s *Store) Outgoing(ctx context.Context, srcIDs []int64, minConfidence float64) ([]EdgeOut, error) {
	return s.edgesWhere(ctx, "e.src", srcIDs, minConfidence)
}

// GraphEdges returns every edge at or above minConfidence.
func (s *Store) GraphEdges(ctx context.Context, minConfidence float64) ([]EdgeOut, error) {
	return s.edgesWhere(ctx, "", nil, minConfidence)
}

// maxParams keeps each IN list under SQLite's variable limit.
const maxParams = 900

func (s *Store) edgesWhere(ctx context.Context, column string, ids []int64, minConfidence float64) ([]EdgeOut, error) {
	if column != "" && len(ids) == 0 {
		return nil, nil
	}

	var (
		out       []EdgeOut
		remaining []int64
	)
	if column == "" {
		remaining = []int64{0}
	} else {
		remaining = ids
	}

	for len(remaining) > 0 {
		batch := remaining
		if len(batch) > maxParams {
			batch = remaining[:maxParams]
		}
		remaining = remaining[len(batch):]

		query := `
			SELECT e.src, e.dst, e.kind, e.confidence, e.rule, e.line,
			       sn.id, sn.kind, sn.name, sn.qname, sn.lang, sn.file_id, sn.arity, sn.start_line, sn.end_line, sn.exported, sf.path,
			       dn.id, dn.kind, dn.name, dn.qname, dn.lang, dn.file_id, dn.arity, dn.start_line, dn.end_line, dn.exported, df.path
			FROM edges e
			JOIN nodes sn ON sn.id = e.src
			JOIN files sf ON sf.id = sn.file_id
			JOIN nodes dn ON dn.id = e.dst
			JOIN files df ON df.id = dn.file_id
			WHERE e.confidence >= ?`

		args := []any{minConfidence}
		if column != "" {
			query += " AND " + column + " IN (" + placeholders(len(batch)) + ")"
			for _, id := range batch {
				args = append(args, id)
			}
		}

		rows, err := s.db.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var e EdgeOut
			if err := rows.Scan(
				&e.Src, &e.Dst, &e.Kind, &e.Confidence, &e.Rule, &e.Line,
				&e.SrcNode.ID, &e.SrcNode.Kind, &e.SrcNode.Name, &e.SrcNode.QName, &e.SrcNode.Lang,
				&e.SrcNode.FileID, &e.SrcNode.Arity, &e.SrcNode.StartLine, &e.SrcNode.EndLine, &e.SrcNode.Exported,
				&e.SrcPath,
				&e.DstNode.ID, &e.DstNode.Kind, &e.DstNode.Name, &e.DstNode.QName, &e.DstNode.Lang,
				&e.DstNode.FileID, &e.DstNode.Arity, &e.DstNode.StartLine, &e.DstNode.EndLine, &e.DstNode.Exported,
				&e.DstPath,
			); err != nil {
				rows.Close()
				return nil, err
			}
			out = append(out, e)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}

	return out, nil
}

// NodesByFile returns every symbol defined in one file.
func (s *Store) NodesByFile(ctx context.Context, path string) ([]NodeRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, kind, name, qname, lang, file_id, arity, start_line, end_line, exported
		FROM nodes WHERE file_id = (SELECT id FROM files WHERE path = ?)
		ORDER BY start_line`, path)
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

// FileByPath returns a file id for a repo-relative path.
func (s *Store) FileByPath(ctx context.Context, path string) (int64, bool, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM files WHERE path = ?`, path).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return id, true, nil
}

func placeholders(n int) string {
	if n == 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
