package index

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Goutam-11/repath/internal/lang"
	"github.com/Goutam-11/repath/internal/lang/facts"
	"github.com/Goutam-11/repath/internal/resolve"
	"github.com/Goutam-11/repath/internal/store"
)

// Options controls one index run.
type Options struct {
	// Full re-parses every file even when its hash is unchanged.
	Full bool
}

// Result reports what one run changed.
type Result struct {
	Files         int
	Indexed       int
	Unchanged     int
	Deleted       int
	ParseFailures int
	Nodes         int
	Edges         int
	Unresolved    int
	Duration      time.Duration
}

// Run discovers files, parses the changed ones, writes them in batches, then
// resolves the new references into edges. Parsing runs on a worker pool; every
// database write happens on the calling goroutine.
func Run(ctx context.Context, st *store.Store, root string, opts Options) (Result, error) {
	start := time.Now()

	sources, err := Discover(ctx, root)
	if err != nil {
		return Result{}, err
	}
	existing, err := st.FileMap(ctx)
	if err != nil {
		return Result{}, err
	}

	modules := DetectModules(root, sources)
	moduleIDs, err := st.UpsertModules(ctx, modules)
	if err != nil {
		return Result{}, err
	}

	discovered := make(map[string]struct{}, len(sources))
	for _, src := range sources {
		discovered[src.Path] = struct{}{}
	}

	workers := runtime.NumCPU()
	if workers > 8 {
		workers = 8
	}
	if workers < 1 {
		workers = 1
	}

	jobs := make(chan Source)
	parsed := make(chan *outcome, workers*2)

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ex := lang.NewExtractor()
			for src := range jobs {
				parsed <- parseOne(src, ex, existing, opts.Full)
			}
		}()
	}

	go func() {
		defer close(jobs)
		for _, src := range sources {
			select {
			case jobs <- src:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() {
		wg.Wait()
		close(parsed)
	}()

	res := Result{Files: len(sources)}
	batch := make([]*outcome, 0, 200)
	var written []*outcome

	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		tx, err := st.Begin(ctx)
		if err != nil {
			return err
		}
		for _, o := range batch {
			fileID, err := tx.UpsertFile(ctx, store.File{
				Path:     o.src.Path,
				Lang:     o.lang,
				ModuleID: moduleIDs[ModuleRootFor(modules, o.src.Path)],
				Size:     o.src.Size,
				MTime:    o.src.MTime,
				Hash:     o.hash,
				ParseOK:  o.ok,
				Err:      o.errMsg,
			})
			if err != nil {
				_ = tx.Rollback()
				return err
			}
			if _, err := tx.ReplaceNodes(ctx, fileID, o.lang, o.symbols); err != nil {
				_ = tx.Rollback()
				return err
			}
			if err := tx.ReplaceImports(ctx, fileID, toImportRows(o.imports)); err != nil {
				_ = tx.Rollback()
				return err
			}
			ids, err := tx.NodeIDsByFile(ctx, fileID)
			if err != nil {
				_ = tx.Rollback()
				return err
			}
			if err := tx.ReplaceEntrypoints(ctx, fileID, toEntrypointRows(o.entries, ids)); err != nil {
				_ = tx.Rollback()
				return err
			}
			o.fileID = fileID
			res.Indexed++
			res.Nodes += len(o.symbols)
			if !o.ok {
				res.ParseFailures++
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		written = append(written, batch...)
		batch = batch[:0]
		return nil
	}

	for o := range parsed {
		if o.skipped {
			res.Unchanged++
			continue
		}
		batch = append(batch, o)
		if len(batch) >= 200 {
			if err := flush(); err != nil {
				return res, err
			}
		}
	}
	if err := flush(); err != nil {
		return res, err
	}

	deleted := make([]string, 0)
	for path := range existing {
		if _, ok := discovered[path]; !ok {
			deleted = append(deleted, path)
		}
	}
	if len(deleted) > 0 {
		tx, err := st.Begin(ctx)
		if err != nil {
			return res, err
		}
		for _, path := range deleted {
			if err := tx.DeleteFile(ctx, path); err != nil {
				_ = tx.Rollback()
				return res, err
			}
		}
		if err := tx.Commit(); err != nil {
			return res, err
		}
		res.Deleted = len(deleted)
	}

	// Edges and pending rows that pointed at replaced or deleted nodes are now
	// dangling; pruning is cheaper and safer than tracking every id.
	if _, err := st.PruneEdges(ctx); err != nil {
		return res, err
	}
	if _, err := st.PrunePending(ctx); err != nil {
		return res, err
	}

	if len(written) > 0 {
		if err := resolveChanges(ctx, st, root, written); err != nil {
			return res, err
		}
	}

	edgeCounts, err := st.EdgeCounts(ctx)
	if err == nil {
		for _, n := range edgeCounts {
			res.Edges += n
		}
	}
	res.Unresolved, _ = st.CountPending(ctx)

	for key, value := range map[string]string{
		"root":       root,
		"indexed_at": time.Now().UTC().Format(time.RFC3339),
		"file_count": strconv.Itoa(len(sources)),
		"git_head":   gitHead(ctx, root),
	} {
		if err := st.SetMeta(ctx, key, value); err != nil {
			return res, err
		}
	}

	res.Duration = time.Since(start)
	return res, nil
}

// resolveChanges turns references into edges for the files just written, then
// retries references that were waiting for a symbol to appear.
func resolveChanges(ctx context.Context, st *store.Store, root string, written []*outcome) error {
	ix, err := resolve.Build(ctx, st, root)
	if err != nil {
		return err
	}

	changed := make(map[int64]bool, len(written))
	for _, o := range written {
		changed[o.fileID] = true

		ids, err := st.NodeIDsByFile(ctx, o.fileID)
		if err != nil {
			return err
		}

		var edges []store.Edge
		var pending []store.PendingRef
		for _, ref := range o.refs {
			src, ok := ids[ref.Enclosing]
			if !ok {
				// Top-level references have no containing symbol to attach to.
				continue
			}

			r := ix.Resolve(o.fileID, o.src.Path, ref)
			kind := edgeKind(ref.Kind)
			if r.Kind != "" {
				kind = r.Kind
			}
			switch {
			case r.External:
			case r.Found && len(r.Candidates) > 0:
				for _, candidate := range r.Candidates {
					if candidate == src {
						continue
					}
					edges = append(edges, store.Edge{
						Src: src, Dst: candidate, Kind: kind,
						Confidence: r.Confidence, Rule: r.Rule, Line: ref.Line,
						Candidates: joinIDs(r.Candidates),
					})
				}
			case r.Found:
				if r.Dst == src {
					continue
				}
				edges = append(edges, store.Edge{
					Src: src, Dst: r.Dst, Kind: kind,
					Confidence: r.Confidence, Rule: r.Rule, Line: ref.Line,
				})
			default:
				pending = append(pending, store.PendingRef{
					Name: ref.Name, FileID: o.fileID, Kind: ref.Kind, Form: ref.Form, Line: ref.Line,
				})
			}
		}

		if _, err := st.InsertEdges(ctx, edges); err != nil {
			return err
		}
		if err := st.ReplacePending(ctx, o.fileID, pending); err != nil {
			return err
		}
	}

	// A symbol added by this run can satisfy references from earlier runs.
	rows, err := st.Pending(ctx)
	if err != nil {
		return err
	}
	var retried []store.Edge
	for _, p := range rows {
		if changed[p.FileID] {
			continue
		}
		// A member reference has no import context here; retrying it by bare
		// name would be the guessing policy repath removed. Only identifiers
		// can be re-linked safely when a symbol later appears.
		if p.Form != "" && p.Form != "ident" {
			continue
		}
		r, ok := ix.ResolvePending(p.FileID, p.Kind, p.Name)
		if !ok {
			continue
		}
		src, ok, err := st.EnclosingNode(ctx, p.FileID, p.Line)
		if err != nil {
			return err
		}
		if !ok || src == r.Dst {
			continue
		}
		kind := edgeKind(p.Kind)
		if r.Kind != "" {
			kind = r.Kind
		}
		retried = append(retried, store.Edge{
			Src: src, Dst: r.Dst, Kind: kind,
			Confidence: r.Confidence, Rule: r.Rule, Line: p.Line,
		})
		if err := st.DeletePending(ctx, p); err != nil {
			return err
		}
	}
	_, err = st.InsertEdges(ctx, retried)
	return err
}

// outcome is one file's parse result, ready to write.
type outcome struct {
	src     Source
	lang    string
	hash    string
	ok      bool
	errMsg  string
	symbols []store.Node
	refs    []facts.Ref
	imports []facts.Import
	entries []facts.Entrypoint
	skipped bool
	fileID  int64
}

func parseOne(src Source, ex *lang.Extractor, existing map[string]store.FileState, full bool) *outcome {
	data, err := os.ReadFile(src.Abs)
	if err != nil {
		return &outcome{src: src, ok: false, errMsg: err.Error()}
	}

	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])

	if !full {
		if state, found := existing[src.Path]; found && state.Hash == hash {
			return &outcome{src: src, skipped: true}
		}
	}

	if isBinary(data) {
		return &outcome{src: src, hash: hash, ok: false, errMsg: "binary content"}
	}

	dialect, langName, ok := lang.For(src.Path)
	if !ok {
		return &outcome{src: src, hash: hash, ok: false, errMsg: "unsupported language"}
	}

	result := ex.Extract(data, dialect)
	augmentPathEntrypoints(src.Path, &result)
	return &outcome{
		src:     src,
		lang:    langName,
		hash:    hash,
		ok:      result.OK,
		errMsg:  result.Err,
		symbols: toNodes(result.Symbols),
		refs:    result.Refs,
		imports: result.Imports,
		entries: result.Entrypoints,
	}
}

// toNodes converts extractor output into rows; the store must not depend on a
// language package.
func toNodes(symbols []facts.Symbol) []store.Node {
	nodes := make([]store.Node, 0, len(symbols))
	for _, s := range symbols {
		nodes = append(nodes, store.Node{
			Kind:      s.Kind,
			Name:      s.Name,
			QName:     s.QName,
			Arity:     s.Arity,
			StartLine: s.StartLine,
			EndLine:   s.EndLine,
			Exported:  s.Exported,
		})
	}
	return nodes
}

// toImportRows converts extractor imports into rows. Imports are resolution
// inputs and are stored separately from edges and nodes.
func toImportRows(imports []facts.Import) []store.ImportRow {
	rows := make([]store.ImportRow, 0, len(imports))
	for _, imp := range imports {
		rows = append(rows, store.ImportRow{
			Local: imp.Local, Imported: imp.Imported, Source: imp.Source,
			Line: imp.Line, Public: imp.Public,
		})
	}
	return rows
}

func edgeKind(refKind string) string {
	switch refKind {
	case "call":
		return "CALLS"
	case "new":
		return "REFERENCES"
	case "extends":
		return "EXTENDS"
	case "implements":
		return "IMPLEMENTS"
	}
	return "REFERENCES"
}

func joinIDs(ids []int64) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, strconv.FormatInt(id, 10))
	}
	return strings.Join(parts, ",")
}

func isBinary(data []byte) bool {
	limit := len(data)
	if limit > 8000 {
		limit = 8000
	}
	for _, b := range data[:limit] {
		if b == 0 {
			return true
		}
	}
	return false
}

func gitHead(ctx context.Context, root string) string {
	out, err := exec.CommandContext(ctx, "git", "-C", root, "rev-parse", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
