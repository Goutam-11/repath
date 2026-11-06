// Package store owns the SQLite index: schema, migrations, and every read and
// write. The rest of repath reaches the database only through this package.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// SchemaVersion is bumped whenever the schema below changes. A binary refuses
// an index written by a newer version.
const SchemaVersion = 1

const schema = `
CREATE TABLE IF NOT EXISTS meta    (key TEXT PRIMARY KEY, value TEXT);
CREATE TABLE IF NOT EXISTS modules (id INTEGER PRIMARY KEY, name TEXT, kind TEXT, root TEXT);
CREATE TABLE IF NOT EXISTS files (
    id        INTEGER PRIMARY KEY,
    path      TEXT UNIQUE NOT NULL,
    module_id INTEGER NOT NULL DEFAULT 0,
    lang      TEXT NOT NULL,
    size      INTEGER NOT NULL,
    mtime     INTEGER NOT NULL,
    hash      TEXT NOT NULL,
    parse_ok  INTEGER NOT NULL,
    err       TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS nodes (
    id         INTEGER PRIMARY KEY,
    kind       TEXT NOT NULL,
    name       TEXT NOT NULL,
    qname      TEXT NOT NULL,
    file_id    INTEGER NOT NULL,
    module_id  INTEGER NOT NULL DEFAULT 0,
    lang       TEXT NOT NULL,
    arity      INTEGER NOT NULL DEFAULT 0,
    start_line INTEGER NOT NULL,
    end_line   INTEGER NOT NULL,
    exported   INTEGER NOT NULL DEFAULT 0,
    UNIQUE (lang, qname, file_id, arity)
);
CREATE TABLE IF NOT EXISTS edges (
    src        INTEGER NOT NULL,
    dst        INTEGER NOT NULL,
    kind       TEXT NOT NULL,
    confidence REAL NOT NULL,
    rule       TEXT NOT NULL,
    line       INTEGER NOT NULL DEFAULT 0,
    candidates TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (src, dst, kind)
);
CREATE TABLE IF NOT EXISTS pending (
    name    TEXT NOT NULL,
    file_id INTEGER NOT NULL,
    kind    TEXT NOT NULL,
    line    INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS edges_rev    ON edges (dst, kind, confidence);
CREATE INDEX IF NOT EXISTS nodes_qn     ON nodes (qname);
CREATE INDEX IF NOT EXISTS nodes_loc    ON nodes (file_id, start_line);
CREATE INDEX IF NOT EXISTS pending_name ON pending (name);
`

// Store is one index database.
type Store struct {
	db *sql.DB
}

// File is one row of the files table.
type File struct {
	Path    string
	Lang    string
	Size    int64
	MTime   int64
	Hash    string
	ParseOK bool
	Err     string
}

// Node is one symbol definition.
type Node struct {
	Kind      string
	Name      string
	QName     string
	Arity     int
	StartLine int
	EndLine   int
	Exported  bool
}

// FileState is the part of a file row the indexer compares against.
type FileState struct {
	ID      int64
	Hash    string
	MTime   int64
	ParseOK bool
}

// Counts summarises an index for `repath status`.
type Counts struct {
	Files         int
	Nodes         int
	ParseFailures int
	ByKind        map[string]int
}

// Open opens (creating if needed) the database at path. The parent directory is
// created. SQLite is configured for one writer and concurrent readers.
func Open(ctx context.Context, path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create index directory: %w", err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	// One connection keeps the schema pragmas and serialises writes. Readers in
	// another process still get WAL snapshots.
	db.SetMaxOpenConns(1)

	for _, pragma := range []string{
		"PRAGMA journal_mode = WAL",
		"PRAGMA synchronous = NORMAL",
		"PRAGMA busy_timeout = 5000",
		"PRAGMA foreign_keys = ON",
	} {
		if _, err := db.ExecContext(ctx, pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("%s: %w", pragma, err)
		}
	}

	return &Store{db: db}, nil
}

// Migrate applies the schema idempotently and records the schema version.
func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	if err := s.SetMeta(ctx, "schema_version", fmt.Sprint(SchemaVersion)); err != nil {
		return err
	}
	return nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// SetMeta upserts one metadata key.
func (s *Store) SetMeta(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO meta (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// Meta reads every metadata key.
func (s *Store) Meta(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM meta`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// FileMap returns the current file rows keyed by repo-relative path.
func (s *Store) FileMap(ctx context.Context) (map[string]FileState, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, path, hash, mtime, parse_ok FROM files`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]FileState)
	for rows.Next() {
		var path string
		var st FileState
		if err := rows.Scan(&st.ID, &path, &st.Hash, &st.MTime, &st.ParseOK); err != nil {
			return nil, err
		}
		out[path] = st
	}
	return out, rows.Err()
}

// Counts summarises the index.
func (s *Store) Counts(ctx context.Context) (Counts, error) {
	c := Counts{ByKind: map[string]int{}}

	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM files`).Scan(&c.Files); err != nil {
		return c, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM files WHERE parse_ok = 0`).Scan(&c.ParseFailures); err != nil {
		return c, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM nodes`).Scan(&c.Nodes); err != nil {
		return c, err
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT kind, COUNT(*) FROM nodes GROUP BY kind ORDER BY COUNT(*) DESC, kind`)
	if err != nil {
		return c, err
	}
	defer rows.Close()
	for rows.Next() {
		var kind string
		var n int
		if err := rows.Scan(&kind, &n); err != nil {
			return c, err
		}
		c.ByKind[kind] = n
	}
	return c, rows.Err()
}

// Begin starts a write transaction.
func (s *Store) Begin(ctx context.Context) (*Tx, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	return &Tx{tx: tx}, nil
}

// Tx is a write transaction. All writes for a batch of files share one.
type Tx struct {
	tx *sql.Tx
}

// Commit commits the transaction.
func (t *Tx) Commit() error { return t.tx.Commit() }

// Rollback aborts the transaction; it is safe after commit.
func (t *Tx) Rollback() error { return t.tx.Rollback() }

// UpsertFile inserts or updates one file row and returns its id.
func (t *Tx) UpsertFile(ctx context.Context, f File) (int64, error) {
	row := t.tx.QueryRowContext(ctx, `
		INSERT INTO files (path, lang, size, mtime, hash, parse_ok, err)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET
			lang = excluded.lang, size = excluded.size, mtime = excluded.mtime,
			hash = excluded.hash, parse_ok = excluded.parse_ok, err = excluded.err
		RETURNING id`,
		f.Path, f.Lang, f.Size, f.MTime, f.Hash, boolInt(f.ParseOK), f.Err)

	var id int64
	if err := row.Scan(&id); err != nil {
		return 0, fmt.Errorf("upsert file %s: %w", f.Path, err)
	}
	return id, nil
}

// ReplaceNodes deletes the file's current nodes and inserts the new ones.
//
// Edges that pointed at deleted nodes are left for the resolver to prune; M0
// stores no edges yet.
func (t *Tx) ReplaceNodes(ctx context.Context, fileID int64, lang string, nodes []Node) (int, error) {
	if _, err := t.tx.ExecContext(ctx, `DELETE FROM nodes WHERE file_id = ?`, fileID); err != nil {
		return 0, err
	}

	stmt, err := t.tx.PrepareContext(ctx, `
		INSERT INTO nodes (kind, name, qname, file_id, lang, arity, start_line, end_line, exported)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (lang, qname, file_id, arity) DO NOTHING`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	inserted := 0
	for _, n := range nodes {
		res, err := stmt.ExecContext(ctx,
			n.Kind, n.Name, n.QName, fileID, lang, n.Arity, n.StartLine, n.EndLine, boolInt(n.Exported))
		if err != nil {
			return inserted, err
		}
		if rows, err := res.RowsAffected(); err == nil && rows > 0 {
			inserted++
		}
	}
	return inserted, nil
}

// DeleteFile removes a file row and its nodes.
func (t *Tx) DeleteFile(ctx context.Context, path string) error {
	if _, err := t.tx.ExecContext(ctx,
		`DELETE FROM nodes WHERE file_id = (SELECT id FROM files WHERE path = ?)`, path); err != nil {
		return err
	}
	_, err := t.tx.ExecContext(ctx, `DELETE FROM files WHERE path = ?`, path)
	return err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
