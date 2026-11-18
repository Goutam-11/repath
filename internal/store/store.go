// Package store owns the SQLite index: schema, migrations, and every read and
// write. The rest of repath reaches the database only through this package.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// SchemaVersion is bumped whenever the schema below changes. A binary refuses
// an index written by a newer version.
const SchemaVersion = 6

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
    form    TEXT NOT NULL DEFAULT '',
    line    INTEGER NOT NULL DEFAULT 0
);
-- imports are resolution inputs, not edges: they let a later run follow
-- __init__.py re-exports without keeping every file's AST in memory.
CREATE TABLE IF NOT EXISTS imports (
    file_id  INTEGER NOT NULL,
    local    TEXT NOT NULL,
    imported TEXT NOT NULL,
    source   TEXT NOT NULL,
    line     INTEGER NOT NULL DEFAULT 0,
    public   INTEGER NOT NULL DEFAULT 0
);
-- Entrypoints are a dimension over symbols, not a node kind. One symbol can be
-- an entrypoint for several rules (for example a main that is also a test).
CREATE TABLE IF NOT EXISTS entrypoints (
    id         INTEGER PRIMARY KEY,
    node_id    INTEGER NOT NULL,
    file_id    INTEGER NOT NULL,
    rule       TEXT NOT NULL,
    name       TEXT NOT NULL DEFAULT '',
    confidence REAL NOT NULL,
    line       INTEGER NOT NULL DEFAULT 0,
    UNIQUE (node_id, rule)
);
CREATE INDEX IF NOT EXISTS edges_rev    ON edges (dst, kind, confidence);
CREATE INDEX IF NOT EXISTS nodes_qn     ON nodes (qname);
CREATE INDEX IF NOT EXISTS nodes_loc    ON nodes (file_id, start_line);
CREATE INDEX IF NOT EXISTS pending_name ON pending (name);
CREATE INDEX IF NOT EXISTS imports_file ON imports (file_id);
CREATE INDEX IF NOT EXISTS entrypoints_rule ON entrypoints (rule);
CREATE INDEX IF NOT EXISTS entrypoints_file ON entrypoints (file_id);
CREATE UNIQUE INDEX IF NOT EXISTS modules_root ON modules (root);
-- Cross-language anchors are inputs for the bounded xlang pass, not nodes or
-- edges: an HTTP route on the server, a call site on the client. Replaced per
-- file like entrypoints; matched into rule-tagged REFERENCES edges at 0.45.
CREATE TABLE IF NOT EXISTS xlang_anchors (
    id         INTEGER PRIMARY KEY,
    node_id    INTEGER NOT NULL,
    file_id    INTEGER NOT NULL,
    lang       TEXT NOT NULL,
    protocol   TEXT NOT NULL,
    role       TEXT NOT NULL,
    method     TEXT NOT NULL,
    path       TEXT NOT NULL,
    line       INTEGER NOT NULL DEFAULT 0,
    confidence REAL NOT NULL,
    UNIQUE (node_id, protocol, role, method, path, line)
);
CREATE INDEX IF NOT EXISTS xlang_anchors_match ON xlang_anchors (protocol, role, method, path);
CREATE INDEX IF NOT EXISTS xlang_anchors_file  ON xlang_anchors (file_id);
`

// Store is one index database.
type Store struct {
	db *sql.DB
}

// File is one row of the files table.
type File struct {
	Path     string
	Lang     string
	ModuleID int64
	Size     int64
	MTime    int64
	Hash     string
	ParseOK  bool
	Err      string
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

// OpenReadOnly opens an existing index without creating, migrating, or writing
// it. The serve command uses this so a read-only HTTP server cannot become a
// second SQLite writer; query_only is enforced by SQLite as well.
func OpenReadOnly(ctx context.Context, path string) (*Store, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("open read-only %s: %w", path, err)
	}
	if _, err := os.Stat(abs); err != nil {
		return nil, fmt.Errorf("open read-only %s: %w", abs, err)
	}

	dsn := (&url.URL{Scheme: "file", Path: abs}).String() +
		"?mode=ro&_pragma=busy_timeout(5000)&_pragma=query_only(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open read-only %s: %w", abs, err)
	}
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("open read-only %s: %w", abs, err)
	}
	return &Store{db: db}, nil
}

// Migrate applies the schema idempotently and records the schema version.
func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	// Additive columns are migrated in place so an M2 index keeps its symbols
	// and edges instead of requiring a full rebuild.
	if err := s.ensureColumn(ctx, "pending", "form", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureColumn(ctx, "imports", "public", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := s.SetMeta(ctx, "schema_version", fmt.Sprint(SchemaVersion)); err != nil {
		return err
	}
	return nil
}

func (s *Store) ensureColumn(ctx context.Context, table, column, ddl string) error {
	rows, err := s.db.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return fmt.Errorf("inspect %s: %w", table, err)
	}
	defer rows.Close()

	for rows.Next() {
		var cid int
		var name, typ string
		var notNull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &pk); err != nil {
			return err
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, "ALTER TABLE "+table+" ADD COLUMN "+column+" "+ddl); err != nil {
		return fmt.Errorf("add %s.%s: %w", table, column, err)
	}
	return nil
}

// Checkpoint folds the write-ahead log into the database. A later process then
// reads the main file instead of replaying a large WAL; PASSIVE never blocks a
// concurrent reader.
func (s *Store) Checkpoint(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(PASSIVE)`)
	return err
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// VacuumInto writes a consistent snapshot of the database to path. The target
// must not exist. Reading through the connection means committed WAL content is
// included, which a plain file copy would miss.
func (s *Store) VacuumInto(ctx context.Context, path string) error {
	if _, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, path); err != nil {
		return fmt.Errorf("vacuum into %s: %w", path, err)
	}
	return nil
}

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

// IndexedPaths is the path set of the current index. The incremental pipeline
// needs membership, not hashes, for every clean path; reading one column for 38k
// rows is much cheaper than reading three.
func (s *Store) IndexedPaths(ctx context.Context) (map[string]bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT path FROM files`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]bool)
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, err
		}
		out[path] = true
	}
	return out, rows.Err()
}

// FileHashesByPaths returns the stored hashes of selected paths, for deciding
// whether a git-reported change actually changed the file content.
func (s *Store) FileHashesByPaths(ctx context.Context, paths []string) (map[string]FileState, error) {
	out := make(map[string]FileState, len(paths))
	for start := 0; start < len(paths); start += maxParams {
		end := start + maxParams
		if end > len(paths) {
			end = len(paths)
		}
		batch := paths[start:end]
		args := make([]any, len(batch))
		for i, path := range batch {
			args[i] = path
		}
		rows, err := s.db.QueryContext(ctx,
			`SELECT id, path, hash FROM files WHERE path IN (`+placeholders(len(batch))+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var path string
			var state FileState
			if err := rows.Scan(&state.ID, &path, &state.Hash); err != nil {
				rows.Close()
				return nil, err
			}
			out[path] = state
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

// NodeSpan is a symbol id and its line span, used to find the enclosing symbol
// for several pending references in one query.
type NodeSpan struct {
	ID        int64
	StartLine int
	EndLine   int
}

// NodeSpansByFiles loads the spans of every symbol in the given files, ordered
// smallest span first. The pending retry used to issue one point query per
// matched reference.
func (s *Store) NodeSpansByFiles(ctx context.Context, fileIDs []int64) (map[int64][]NodeSpan, error) {
	out := make(map[int64][]NodeSpan, len(fileIDs))
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
			`SELECT file_id, id, start_line, end_line FROM nodes
			 WHERE file_id IN (`+placeholders(len(batch))+`)
			 ORDER BY file_id, (end_line - start_line) ASC`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var fileID int64
			var span NodeSpan
			if err := rows.Scan(&fileID, &span.ID, &span.StartLine, &span.EndLine); err != nil {
				rows.Close()
				return nil, err
			}
			out[fileID] = append(out[fileID], span)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
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
		INSERT INTO files (path, lang, module_id, size, mtime, hash, parse_ok, err)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET
			lang = excluded.lang, module_id = excluded.module_id,
			size = excluded.size, mtime = excluded.mtime,
			hash = excluded.hash, parse_ok = excluded.parse_ok, err = excluded.err
		RETURNING id`,
		f.Path, f.Lang, f.ModuleID, f.Size, f.MTime, f.Hash, boolInt(f.ParseOK), f.Err)

	var id int64
	if err := row.Scan(&id); err != nil {
		return 0, fmt.Errorf("upsert file %s: %w", f.Path, err)
	}
	return id, nil
}

// NodeChange reports what replacing a file's symbols did to the graph. The
// incremental pipeline uses Added/Removed to decide whether the global name set
// changed enough to retry pending identifiers.
type NodeChange struct {
	Written int
	Added   int
	Removed int
}

// ReplaceNodes upserts the file's symbols and removes the ones that are gone.
// Node ids are kept when (qname, arity) still exists, so an unrelated edit does
// not orphan the incoming edges of every other symbol in the file.
func (t *Tx) ReplaceNodes(ctx context.Context, fileID int64, lang string, nodes []Node) (NodeChange, error) {
	type nodeKey struct {
		qname string
		arity int
	}
	keyOf := func(qname string, arity int) nodeKey { return nodeKey{qname: qname, arity: arity} }

	rows, err := t.tx.QueryContext(ctx, `SELECT id, qname, arity FROM nodes WHERE file_id = ?`, fileID)
	if err != nil {
		return NodeChange{}, err
	}
	existing := make(map[nodeKey]int64)
	for rows.Next() {
		var id int64
		var qname string
		var arity int
		if err := rows.Scan(&id, &qname, &arity); err != nil {
			rows.Close()
			return NodeChange{}, err
		}
		existing[keyOf(qname, arity)] = id
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return NodeChange{}, err
	}
	rows.Close()

	stmt, err := t.tx.PrepareContext(ctx, `
		INSERT INTO nodes (kind, name, qname, file_id, lang, arity, start_line, end_line, exported)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (lang, qname, file_id, arity) DO UPDATE SET
			kind = excluded.kind,
			name = excluded.name,
			start_line = excluded.start_line,
			end_line = excluded.end_line,
			exported = excluded.exported
		RETURNING id`)
	if err != nil {
		return NodeChange{}, err
	}
	defer stmt.Close()

	seen := make(map[nodeKey]bool, len(nodes))
	written := 0
	added := 0
	removed := 0
	for _, n := range nodes {
		key := keyOf(n.QName, n.Arity)
		if seen[key] {
			continue
		}
		seen[key] = true
		if _, existed := existing[key]; !existed {
			added++
		}

		var id int64
		if err := stmt.QueryRowContext(ctx,
			n.Kind, n.Name, n.QName, fileID, lang, n.Arity, n.StartLine, n.EndLine, boolInt(n.Exported)).
			Scan(&id); err != nil {
			return NodeChange{Written: written, Added: added, Removed: removed}, err
		}
		written++
	}

	for key, id := range existing {
		if seen[key] {
			continue
		}
		if _, err := t.tx.ExecContext(ctx, `DELETE FROM nodes WHERE id = ?`, id); err != nil {
			return NodeChange{Written: written, Added: added, Removed: removed}, err
		}
		removed++
	}
	return NodeChange{Written: written, Added: added, Removed: removed}, nil
}

// NodeIDsByFile maps qname to node id for one file inside a write transaction.
func (t *Tx) NodeIDsByFile(ctx context.Context, fileID int64) (map[string]int64, error) {
	rows, err := t.tx.QueryContext(ctx, `SELECT qname, id FROM nodes WHERE file_id = ?`, fileID)
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

// EntrypointRow is one symbol anchored by an entrypoint rule.
type EntrypointRow struct {
	NodeID     int64
	FileID     int64
	Rule       string
	Name       string
	Confidence float64
	Line       int
}

// ReplaceEntrypoints swaps the entrypoint dimension for one file.
func (t *Tx) ReplaceEntrypoints(ctx context.Context, fileID int64, entries []EntrypointRow) error {
	if _, err := t.tx.ExecContext(ctx, `DELETE FROM entrypoints WHERE file_id = ?`, fileID); err != nil {
		return err
	}
	if len(entries) == 0 {
		return nil
	}
	stmt, err := t.tx.PrepareContext(ctx, `
		INSERT INTO entrypoints (node_id, file_id, rule, name, confidence, line)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (node_id, rule) DO UPDATE SET
			name = excluded.name,
			confidence = excluded.confidence,
			line = excluded.line`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, entry := range entries {
		if _, err := stmt.ExecContext(ctx, entry.NodeID, fileID, entry.Rule, entry.Name, entry.Confidence, entry.Line); err != nil {
			return err
		}
	}
	return nil
}

// AnchorRow is one persisted cross-language anchor. It is an input for the
// xlang pass, never a node or an edge.
type AnchorRow struct {
	NodeID     int64
	Lang       string
	Protocol   string
	Role       string
	Method     string
	Path       string
	Line       int
	Confidence float64
}

// ReplaceAnchors swaps the cross-language anchors for one file. Anchors are
// inputs like imports: they are replaced with the file's fresh facts and never
// touched by the resolver.
func (t *Tx) ReplaceAnchors(ctx context.Context, fileID int64, anchors []AnchorRow) error {
	if _, err := t.tx.ExecContext(ctx, `DELETE FROM xlang_anchors WHERE file_id = ?`, fileID); err != nil {
		return err
	}
	if len(anchors) == 0 {
		return nil
	}
	stmt, err := t.tx.PrepareContext(ctx, `
		INSERT INTO xlang_anchors (node_id, file_id, lang, protocol, role, method, path, line, confidence)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, anchor := range anchors {
		if _, err := stmt.ExecContext(ctx, anchor.NodeID, fileID, anchor.Lang,
			anchor.Protocol, anchor.Role, anchor.Method, anchor.Path, anchor.Line, anchor.Confidence); err != nil {
			return err
		}
	}
	return nil
}

// Anchors lists every cross-language anchor, stable by protocol, role, method,
// path, and node.
func (s *Store) Anchors(ctx context.Context) ([]AnchorRow, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT node_id, lang, protocol, role, method, path, line, confidence
		FROM xlang_anchors
		ORDER BY protocol, role, method, path, node_id, line`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AnchorRow
	for rows.Next() {
		var a AnchorRow
		if err := rows.Scan(&a.NodeID, &a.Lang, &a.Protocol, &a.Role, &a.Method, &a.Path, &a.Line, &a.Confidence); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// DeleteEdgesByRulePrefix removes every edge whose rule starts with prefix. The
// xlang pass uses it to rebuild its own edges from anchors; no other rule shares
// the prefix.
func (s *Store) DeleteEdgesByRulePrefix(ctx context.Context, prefix string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM edges WHERE rule LIKE ?`, prefix+"%")
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DeleteFile removes a file row and its nodes, entrypoints, and import bindings.
func (t *Tx) DeleteFile(ctx context.Context, path string) error {
	if _, err := t.tx.ExecContext(ctx,
		`DELETE FROM nodes WHERE file_id = (SELECT id FROM files WHERE path = ?)`, path); err != nil {
		return err
	}
	if _, err := t.tx.ExecContext(ctx,
		`DELETE FROM entrypoints WHERE file_id = (SELECT id FROM files WHERE path = ?)`, path); err != nil {
		return err
	}
	if _, err := t.tx.ExecContext(ctx,
		`DELETE FROM xlang_anchors WHERE file_id = (SELECT id FROM files WHERE path = ?)`, path); err != nil {
		return err
	}
	if _, err := t.tx.ExecContext(ctx,
		`DELETE FROM imports WHERE file_id = (SELECT id FROM files WHERE path = ?)`, path); err != nil {
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
