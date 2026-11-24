// Package artifact packs and pulls commit-keyed snapshots of an index
// (PLAN.md §4.1/§4.3). An artifact is a consistent SQLite database plus a
// manifest; it never contains source text, and every stored path is
// repo-relative.
package artifact

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/Goutam-11/repath/internal/index"
	"github.com/Goutam-11/repath/internal/store"
)

// ManifestName is the sidecar every artifact directory carries.
const ManifestName = "manifest.json"

// maxArtifactSize bounds a download so a hostile URL cannot fill the disk.
const maxArtifactSize = 4 << 30 // 4 GiB

// ErrNotFound marks a missing artifact source; the CLI maps it to exit code 3.
var ErrNotFound = errors.New("artifact not found")

// Manifest describes one exported index. The first eight fields are the PLAN
// §4.1 contract; format and db_file make the storage form explicit.
type Manifest struct {
	SchemaVersion  int     `json:"schema_version"`
	ToolVersion    string  `json:"tool_version"`
	Commit         string  `json:"commit"`
	FileCount      int     `json:"file_count"`
	NodeCount      int     `json:"node_count"`
	EdgeCount      int     `json:"edge_count"`
	UnresolvedRate float64 `json:"unresolved_rate"`
	IndexedAt      string  `json:"indexed_at"`
	Format         string  `json:"format"`
	DBFile         string  `json:"db_file"`
	// Root is the checkout the snapshot was built from. It is provenance for
	// display (the `root` field of a read answer), never an identity key: a
	// pulled or merged index keeps its own paths.
	Root string `json:"root,omitempty"`
	// Scope is the `index --scope` set the snapshot was built with, recorded so
	// a pulled index cannot silently describe a different file set.
	Scope []string `json:"scope,omitempty"`
	// Parts names the inputs of a merged artifact (see `repath merge`). A
	// single-repo artifact leaves it empty.
	Parts []MergePart `json:"parts,omitempty"`
}

// MergePart is one input of a merged artifact: its repo name, root, commit, and
// the counts it contributed. It is provenance, never a node or an edge.
type MergePart struct {
	Name    string `json:"name"`
	Root    string `json:"root,omitempty"`
	Commit  string `json:"commit,omitempty"`
	Files   int    `json:"files"`
	Nodes   int    `json:"nodes"`
	Edges   int    `json:"edges"`
	Pending int    `json:"pending"`
}

// WriteManifest writes a manifest atomically in the artifact format. It is the
// same writer Export uses; `merge` reuses it so a merged artifact is read by
// `pull`, `export --artifact`, and `map --artifact` without a second format.
func WriteManifest(path string, m Manifest) error {
	return writeManifest(path, m)
}

// ExportResult names the files an export wrote.
type ExportResult struct {
	Manifest     Manifest `json:"manifest"`
	ManifestPath string   `json:"manifest_path"`
	DBPath       string   `json:"db_path"`
}

// ArtifactName is the uncompressed file name for a commit. The full commit
// lives in the manifest; the name is short so directories stay readable.
func ArtifactName(commit string) string {
	return artifactName(commit, false)
}

func artifactName(commit string, compress bool) string {
	name := "repath-dev.sqlite"
	if commit != "" {
		short := commit
		if len(short) > 12 {
			short = short[:12]
		}
		name = "repath-" + short + ".sqlite"
	}
	if compress {
		return name + ".zst"
	}
	return name
}

// Options controls the artifact container. Compression is opt-in and recorded
// in manifest.format; pull always reads both forms.
type Options struct {
	Compress bool
}

// Export writes an uncompressed SQLite snapshot; use ExportWithOptions for the
// opt-in zstd form.
func Export(ctx context.Context, st *store.Store, root, outDir, version string) (ExportResult, error) {
	return ExportWithOptions(ctx, st, root, outDir, version, Options{})
}

// ExportWithOptions writes a consistent database snapshot and its manifest.
// The snapshot is uncompressed by default. With Compress it is zstd-encoded
// after the VACUUM INTO snapshot, and manifest.format records sqlite+zstd.
func ExportWithOptions(ctx context.Context, st *store.Store, root, outDir, version string, opts Options) (ExportResult, error) {
	if outDir == "" {
		return ExportResult{}, errors.New("artifact directory is empty")
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return ExportResult{}, fmt.Errorf("create artifact directory: %w", err)
	}

	counts, err := st.Counts(ctx)
	if err != nil {
		return ExportResult{}, err
	}
	edgeCounts, err := st.EdgeCounts(ctx)
	if err != nil {
		return ExportResult{}, err
	}
	unresolved, err := st.CountPending(ctx)
	if err != nil {
		return ExportResult{}, err
	}
	meta, err := st.Meta(ctx)
	if err != nil {
		return ExportResult{}, err
	}

	totalEdges := 0
	for _, n := range edgeCounts {
		totalEdges += n
	}
	unresolvedRate := 0.0
	if totalEdges+unresolved > 0 {
		unresolvedRate = float64(unresolved) / float64(totalEdges+unresolved)
	}

	commit := meta["git_head"]
	if commit == "" {
		commit = gitHead(ctx, root)
	}
	indexedAt := meta["indexed_at"]
	if indexedAt == "" {
		indexedAt = time.Now().UTC().Format(time.RFC3339)
	}

	name := artifactName(commit, opts.Compress)
	dbPath := filepath.Join(outDir, name)
	if err := os.Remove(dbPath); err != nil && !os.IsNotExist(err) {
		return ExportResult{}, fmt.Errorf("replace artifact: %w", err)
	}
	tmp := dbPath + ".tmp-" + strconv.Itoa(os.Getpid())
	_ = os.Remove(tmp)
	if opts.Compress {
		sqliteTmp := tmp + ".sqlite"
		_ = os.Remove(sqliteTmp)
		if err := st.VacuumInto(ctx, sqliteTmp); err != nil {
			return ExportResult{}, err
		}
		if err := compressFile(sqliteTmp, tmp); err != nil {
			_ = os.Remove(sqliteTmp)
			_ = os.Remove(tmp)
			return ExportResult{}, err
		}
		_ = os.Remove(sqliteTmp)
	} else if err := st.VacuumInto(ctx, tmp); err != nil {
		return ExportResult{}, err
	}
	if err := os.Rename(tmp, dbPath); err != nil {
		_ = os.Remove(tmp)
		return ExportResult{}, fmt.Errorf("write artifact: %w", err)
	}
	// A CI artifact is read-only; pull copies before opening it.
	if err := os.Chmod(dbPath, 0o444); err != nil {
		return ExportResult{}, fmt.Errorf("chmod artifact: %w", err)
	}

	format := "sqlite"
	if opts.Compress {
		format = "sqlite+zstd"
	}
	manifest := Manifest{
		SchemaVersion:  store.SchemaVersion,
		ToolVersion:    version,
		Commit:         commit,
		FileCount:      counts.Files,
		NodeCount:      counts.Nodes,
		EdgeCount:      totalEdges,
		UnresolvedRate: unresolvedRate,
		IndexedAt:      indexedAt,
		Format:         format,
		DBFile:         name,
		Root:           root,
		Scope:          index.ScopeFromMeta(meta),
	}
	manifestPath := filepath.Join(outDir, ManifestName)
	if err := writeManifest(manifestPath, manifest); err != nil {
		return ExportResult{}, err
	}

	return ExportResult{Manifest: manifest, ManifestPath: manifestPath, DBPath: dbPath}, nil
}

// Pull seeds root/.repath/index.db from an artifact. It refuses a schema
// version this binary does not know, then overlays the local checkout
// incrementally when there are indexable files (an empty directory keeps the
// snapshot intact so it can still be inspected).
func Pull(ctx context.Context, source, root, version string) (Manifest, error) {
	manifest, dbPath, cleanup, err := resolveSource(ctx, source)
	if err != nil {
		return Manifest{}, err
	}
	defer func() { cleanup() }()

	switch manifest.Format {
	case "", "sqlite", "sqlite+zstd":
	default:
		return manifest, fmt.Errorf("artifact format %q is not supported", manifest.Format)
	}
	if isCompressed(manifest, dbPath) {
		decompressed, err := decompressToTemp(dbPath)
		if err != nil {
			return manifest, err
		}
		previous := cleanup
		cleanup = func() {
			_ = os.Remove(decompressed)
			previous()
		}
		dbPath = decompressed
	}

	if manifest.SchemaVersion != store.SchemaVersion {
		return manifest, fmt.Errorf(
			"artifact schema v%d is not supported by this binary (expects v%d); upgrade repath or rebuild the artifact",
			manifest.SchemaVersion, store.SchemaVersion)
	}

	dest := filepath.Join(root, ".repath", "index.db")
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return manifest, fmt.Errorf("create index directory: %w", err)
	}
	if err := copyAtomic(dbPath, dest); err != nil {
		return manifest, fmt.Errorf("seed index: %w", err)
	}
	// A stale WAL beside a replaced database would be read as if it belonged to
	// the new file.
	_ = os.Remove(dest + "-wal")
	_ = os.Remove(dest + "-shm")

	st, err := store.Open(ctx, dest)
	if err != nil {
		return manifest, fmt.Errorf("open seeded index: %w", err)
	}
	defer st.Close()

	meta, err := st.Meta(ctx)
	if err != nil {
		return manifest, fmt.Errorf("read seeded index: %w", err)
	}
	if got := meta["schema_version"]; got != strconv.Itoa(store.SchemaVersion) {
		return manifest, fmt.Errorf(
			"artifact database schema_version %q does not match manifest v%d; rebuild the artifact",
			got, manifest.SchemaVersion)
	}
	if err := st.SetMeta(ctx, "root", root); err != nil {
		return manifest, err
	}
	// The manifest and the database must agree on the scope; a mismatch means
	// the artifact (or its manifest) was edited and the index would mix two
	// file sets.
	artifactScope, err := index.CleanScope(manifest.Scope)
	if err != nil {
		return manifest, fmt.Errorf("artifact scope: %w", err)
	}
	if !index.ScopeEqual(artifactScope, index.ScopeFromMeta(meta)) {
		return manifest, fmt.Errorf(
			"artifact manifest scope %v does not match the seeded index scope %v; rebuild the artifact",
			manifest.Scope, index.ScopeFromMeta(meta))
	}

	files, err := index.Discover(ctx, root)
	if err != nil {
		return manifest, fmt.Errorf("discover checkout: %w", err)
	}
	if len(files) > 0 {
		if _, err := index.Run(ctx, st, root, index.Options{}); err != nil {
			return manifest, fmt.Errorf("overlay checkout: %w", err)
		}
		// If the checkout is clean, the overlaid database is now the cache for
		// the local commit. StoreCommit is a no-op for a dirty tree.
		_ = index.StoreCommit(ctx, root, st)
	}

	return manifest, nil
}

// ErrSchema marks an artifact whose manifest or database schema is not the
// schema this binary understands. The CLI maps it to exit code 1 (an internal
// mismatch, like a corrupted index), never to "not found".
var ErrSchema = errors.New("artifact schema mismatch")

// OpenReadOnly opens an artifact's database in place, without pulling it into
// .repath/. It accepts the same local inputs Pull does: a directory, a
// manifest path, or a direct database path. A compressed artifact is decoded
// to a temporary file that the returned cleanup removes. The manifest, and the
// database's own meta row, must both match store.SchemaVersion; a stale
// artifact is refused rather than read with the wrong assumptions.
func OpenReadOnly(ctx context.Context, source string) (*store.Store, func(), Manifest, error) {
	manifest, dbPath, cleanup, err := resolveLocal(source)
	if err != nil {
		return nil, func() {}, Manifest{}, err
	}
	switch manifest.Format {
	case "", "sqlite", "sqlite+zstd":
	default:
		cleanup()
		return nil, func() {}, manifest, fmt.Errorf("artifact format %q is not supported", manifest.Format)
	}
	if isCompressed(manifest, dbPath) {
		decompressed, err := decompressToTemp(dbPath)
		if err != nil {
			cleanup()
			return nil, func() {}, manifest, err
		}
		previous := cleanup
		cleanup = func() {
			_ = os.Remove(decompressed)
			previous()
		}
		dbPath = decompressed
	}
	if manifest.SchemaVersion != store.SchemaVersion {
		cleanup()
		return nil, func() {}, manifest, fmt.Errorf(
			"%w: v%d is not supported by this binary (expects v%d); rebuild the artifact",
			ErrSchema, manifest.SchemaVersion, store.SchemaVersion)
	}
	st, err := store.OpenReadOnly(ctx, dbPath)
	if err != nil {
		cleanup()
		return nil, func() {}, manifest, err
	}
	done := func() {
		_ = st.Close()
		cleanup()
	}
	meta, err := st.Meta(ctx)
	if err != nil {
		done()
		return nil, func() {}, manifest, fmt.Errorf("read artifact meta: %w", err)
	}
	if got := meta["schema_version"]; got != strconv.Itoa(store.SchemaVersion) {
		done()
		return nil, func() {}, manifest, fmt.Errorf(
			"%w: database is v%s, manifest says v%d; rebuild the artifact",
			ErrSchema, got, manifest.SchemaVersion)
	}
	return st, done, manifest, nil
}

func isCompressed(manifest Manifest, dbPath string) bool {
	return manifest.Format == "sqlite+zstd" || strings.HasSuffix(strings.ToLower(dbPath), ".zst")
}

func compressFile(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open snapshot: %w", err)
	}
	defer in.Close()
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return fmt.Errorf("create compressed artifact: %w", err)
	}
	encoder, err := zstd.NewWriter(out, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		out.Close()
		_ = os.Remove(dest)
		return fmt.Errorf("zstd encoder: %w", err)
	}
	_, copyErr := io.Copy(encoder, in)
	closeErr := encoder.Close()
	outErr := out.Close()
	if copyErr != nil || closeErr != nil || outErr != nil {
		_ = os.Remove(dest)
		if copyErr != nil {
			return fmt.Errorf("compress artifact: %w", copyErr)
		}
		if closeErr != nil {
			return fmt.Errorf("compress artifact: %w", closeErr)
		}
		return fmt.Errorf("compress artifact: %w", outErr)
	}
	return nil
}

func decompressToTemp(src string) (string, error) {
	in, err := os.Open(src)
	if err != nil {
		return "", fmt.Errorf("open compressed artifact: %w", err)
	}
	defer in.Close()
	decoder, err := zstd.NewReader(in)
	if err != nil {
		return "", fmt.Errorf("zstd decoder: %w", err)
	}
	defer decoder.Close()

	out, err := os.CreateTemp("", "repath-zstd-*.sqlite")
	if err != nil {
		return "", err
	}
	written, copyErr := io.Copy(out, io.LimitReader(decoder, maxArtifactSize+1))
	closeErr := out.Close()
	if copyErr != nil || closeErr != nil || written > maxArtifactSize {
		_ = os.Remove(out.Name())
		if copyErr != nil {
			return "", fmt.Errorf("decompress artifact: %w", copyErr)
		}
		if closeErr != nil {
			return "", closeErr
		}
		return "", fmt.Errorf("decompressed artifact exceeds %d bytes", maxArtifactSize)
	}
	return out.Name(), nil
}

func writeManifest(path string, manifest Manifest) error {
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')

	tmp := path + ".tmp-" + strconv.Itoa(os.Getpid())
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("write manifest: %w", err)
	}
	return nil
}

// ReadManifest reads and parses one manifest file.
func ReadManifest(path string) (Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, err
	}
	var manifest Manifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("parse manifest %s: %w", path, err)
	}
	return manifest, nil
}

// resolveSource turns a path, directory, or URL into a local manifest and
// database file. cleanup removes any temporary download directory.
func resolveSource(ctx context.Context, source string) (Manifest, string, func(), error) {
	if isHTTP(source) {
		return resolveRemote(ctx, source)
	}
	return resolveLocal(source)
}

func resolveLocal(source string) (Manifest, string, func(), error) {
	noop := func() {}
	info, err := os.Stat(source)
	if err != nil {
		if os.IsNotExist(err) {
			return Manifest{}, "", noop, fmt.Errorf("%w: %s", ErrNotFound, source)
		}
		return Manifest{}, "", noop, err
	}

	var (
		manifestPath string
		dbPath       string
	)
	switch {
	case info.IsDir():
		manifestPath = filepath.Join(source, ManifestName)
	case strings.HasSuffix(strings.ToLower(source), ".json"):
		manifestPath = source
	default:
		// A direct database path: the manifest sits beside it.
		dbPath = source
		manifestPath = filepath.Join(filepath.Dir(source), ManifestName)
	}

	manifest, err := ReadManifest(manifestPath)
	if err != nil {
		if os.IsNotExist(err) {
			return Manifest{}, "", noop, fmt.Errorf("%w: %s", ErrNotFound, manifestPath)
		}
		return Manifest{}, "", noop, fmt.Errorf("read manifest: %w", err)
	}

	dir := filepath.Dir(manifestPath)
	if dbPath == "" {
		name := manifest.DBFile
		if name == "" {
			name = findSQLite(dir)
		}
		if err := validateBaseName(name); err != nil {
			return Manifest{}, "", noop, err
		}
		if name == "" {
			return Manifest{}, "", noop, fmt.Errorf("%w: no database beside %s", ErrNotFound, manifestPath)
		}
		dbPath = filepath.Join(dir, name)
	}
	if _, err := os.Stat(dbPath); err != nil {
		if os.IsNotExist(err) {
			return Manifest{}, "", noop, fmt.Errorf("%w: %s", ErrNotFound, dbPath)
		}
		return Manifest{}, "", noop, err
	}
	return manifest, dbPath, noop, nil
}

func resolveRemote(ctx context.Context, source string) (Manifest, string, func(), error) {
	tempDir, err := os.MkdirTemp("", "repath-pull-")
	if err != nil {
		return Manifest{}, "", func() {}, err
	}
	cleanup := func() { _ = os.RemoveAll(tempDir) }

	u, err := url.Parse(source)
	if err != nil {
		cleanup()
		return Manifest{}, "", func() {}, fmt.Errorf("parse url: %w", err)
	}

	manifestURL := source
	if !strings.HasSuffix(strings.ToLower(u.Path), ".json") {
		manifestURL = siblingURL(u, ManifestName)
	}
	manifestPath := filepath.Join(tempDir, ManifestName)
	if err := download(ctx, manifestURL, manifestPath); err != nil {
		cleanup()
		return Manifest{}, "", func() {}, err
	}
	manifest, err := ReadManifest(manifestPath)
	if err != nil {
		cleanup()
		return Manifest{}, "", func() {}, fmt.Errorf("read manifest: %w", err)
	}

	name := manifest.DBFile
	if name == "" && !strings.HasSuffix(strings.ToLower(u.Path), ".json") {
		name = path.Base(u.Path)
	}
	if err := validateBaseName(name); err != nil {
		cleanup()
		return Manifest{}, "", func() {}, err
	}
	if name == "" {
		cleanup()
		return Manifest{}, "", func() {}, fmt.Errorf("%w: manifest has no db_file", ErrNotFound)
	}

	dbPath := filepath.Join(tempDir, name)
	if err := download(ctx, siblingURL(u, name), dbPath); err != nil {
		cleanup()
		return Manifest{}, "", func() {}, err
	}
	return manifest, dbPath, cleanup, nil
}

func download(ctx context.Context, source, dest string) error {
	client := &http.Client{Timeout: 10 * time.Minute}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return fmt.Errorf("fetch %s: %w", source, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("fetch %s: %w", source, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%w: %s", ErrNotFound, source)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch %s: HTTP %s", source, resp.Status)
	}

	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	written, copyErr := io.Copy(out, io.LimitReader(resp.Body, maxArtifactSize+1))
	closeErr := out.Close()
	if copyErr != nil {
		_ = os.Remove(dest)
		return fmt.Errorf("download %s: %w", source, copyErr)
	}
	if closeErr != nil {
		_ = os.Remove(dest)
		return closeErr
	}
	if written > maxArtifactSize {
		_ = os.Remove(dest)
		return fmt.Errorf("download %s: artifact exceeds %d bytes", source, maxArtifactSize)
	}
	return nil
}

func siblingURL(u *url.URL, name string) string {
	v := *u
	v.Path = path.Join(path.Dir(u.Path), name)
	v.RawQuery = ""
	v.Fragment = ""
	return v.String()
}

func isHTTP(source string) bool {
	return strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://")
}

func validateBaseName(name string) error {
	if name == "" {
		return nil
	}
	if name == "." || name == ".." || filepath.Base(name) != name || strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("manifest db_file %q is not a plain file name", name)
	}
	return nil
}

// findSQLite is the fallback for a manifest that predates db_file.
func findSQLite(dir string) string {
	for _, pattern := range []string{"repath-*.sqlite", "repath-*.sqlite.zst", "*.sqlite", "*.db"} {
		matches, _ := filepath.Glob(filepath.Join(dir, pattern))
		if len(matches) == 1 {
			return filepath.Base(matches[0])
		}
	}
	return ""
}

func copyAtomic(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	tmp := dest + ".tmp-" + strconv.Itoa(os.Getpid())
	_ = os.Remove(tmp)
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, dest); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func gitHead(ctx context.Context, root string) string {
	out, err := exec.CommandContext(ctx, "git", "-C", root, "rev-parse", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
