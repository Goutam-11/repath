package index

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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
	// The xlang anchors are a v6 addition. A database migrated in place (for
	// example a non-git directory) has files but no anchors for them, so one
	// full re-read is forced until the marker says extraction has happened.
	// Git worktrees already discard a stale-schema database before this point.
	if !opts.Full {
		if meta, err := st.Meta(ctx); err == nil && meta["xlang_indexed"] != "1" {
			opts.Full = true
		}
	}
	indexed, err := st.IndexedPaths(ctx)
	if err != nil {
		return Result{}, err
	}
	initial := len(indexed) == 0
	head := gitHead(ctx, root)
	sources, err := runSources(ctx, st, root, indexed, head, opts)
	if err != nil {
		return Result{}, err
	}

	var existing map[string]store.FileState
	if len(indexed) > 0 {
		changedPaths := make([]string, 0, len(sources))
		for _, src := range sources {
			if !src.Unchanged {
				changedPaths = append(changedPaths, src.Path)
			}
		}
		existing, err = st.FileHashesByPaths(ctx, changedPaths)
		if err != nil {
			return Result{}, err
		}
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
	symbolChange := store.NodeChange{}

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
			delta, err := tx.ReplaceNodes(ctx, fileID, o.lang, o.symbols)
			if err != nil {
				_ = tx.Rollback()
				return err
			}
			symbolChange.Added += delta.Added
			symbolChange.Removed += delta.Removed
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
			if err := tx.ReplaceAnchors(ctx, fileID, toAnchorRows(o.anchors, ids, o.lang)); err != nil {
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
		for _, o := range batch {
			// Resolution only reads the packed refs, the file path, and fileID; symbols,
			// imports, entrypoints, and anchors are already persisted.
			o.symbols = nil
			o.imports = nil
			o.entries = nil
			o.anchors = nil
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
	for path := range indexed {
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
	// dangling; pruning is cheaper and safer than tracking every id. When no
	// file or symbol disappeared there is nothing to dangle, and two full-table
	// scans are skipped on the common incremental path.
	if res.Deleted > 0 || symbolChange.Removed > 0 {
		if _, err := st.PruneEdges(ctx); err != nil {
			return res, err
		}
		if _, err := st.PrunePending(ctx); err != nil {
			return res, err
		}
	}

	if len(written) > 0 {
		// A cold run and a full re-index re-resolve every file from scratch, so
		// their pending rows are fresh and the retry must include them. An
		// incremental run only retries unchanged files: a changed file was just
		// resolved against the complete symbol set. Treating --full like cold is
		// what keeps a warm full index from drifting (2,077 vs 2,048 pending on
		// rayon before #49).
		fresh := initial || opts.Full
		runRetry := fresh || symbolChange.Added > 0 || symbolChange.Removed > 0 || res.Deleted > 0
		if err := resolveChanges(ctx, st, root, written, fresh, runRetry); err != nil {
			return res, err
		}
	}

	// Cross-language edges are derived from the anchors just written (and the
	// anchors of unchanged files). Rebuilding them on every run that wrote
	// files keeps a cold, incremental, and --full graph identical without
	// touching any resolver or node identity.
	if len(written) > 0 || res.Deleted > 0 {
		if err := linkCrossLanguage(ctx, st); err != nil {
			return res, err
		}
	}

	res.Edges, _ = st.CountEdges(ctx)
	res.Unresolved, _ = st.CountPending(ctx)

	for key, value := range map[string]string{
		"root":          root,
		"indexed_at":    time.Now().UTC().Format(time.RFC3339),
		"file_count":    strconv.Itoa(len(sources)),
		"git_head":      head,
		"xlang_indexed": "1",
	} {
		if err := st.SetMeta(ctx, key, value); err != nil {
			return res, err
		}
	}

	// Fold the WAL back into the database so the next process reads the main
	// file instead of replaying the log this run appended.
	_ = st.Checkpoint(ctx)

	res.Duration = time.Since(start)
	return res, nil
}

// runSources builds the source list for one run. When the index belongs to the
// current commit, git's work-tree status tells us which paths changed; every
// other file is carried through without a stat or a hash read. A missing git,
// a missing index, a different HEAD, or --full falls back to a full discovery,
// so the result can never be fresher than the hash path would have been.
func runSources(ctx context.Context, st *store.Store, root string, indexed map[string]bool, head string, opts Options) ([]Source, error) {
	if opts.Full || len(indexed) == 0 || head == "" {
		return Discover(ctx, root)
	}
	meta, err := st.Meta(ctx)
	if err != nil {
		return nil, err
	}
	if meta["git_head"] != head {
		return Discover(ctx, root)
	}
	// The two git reads are independent; run them together so the untracked
	// scan and the tracked-status read overlap.
	var wg sync.WaitGroup
	var trackedChanges map[string]bool
	var trackedOK bool
	wg.Add(1)
	go func() {
		defer wg.Done()
		trackedChanges, trackedOK = gitTrackedChanges(ctx, root)
	}()
	paths, untracked, listErr := listPathSets(ctx, root, false)
	wg.Wait()
	if listErr != nil {
		return nil, listErr
	}
	if !trackedOK {
		return Discover(ctx, root)
	}

	changed := make(map[string]bool, len(trackedChanges)+len(untracked))
	for p := range trackedChanges {
		changed[p] = true
	}
	for p := range untracked {
		changed[p] = true
	}
	out := sourcesFromPaths(root, paths, func(rel string) bool {
		return indexed[rel] && !changed[rel]
	})
	return out, nil
}

// enclosingIn returns the smallest symbol span that contains line. slices are
// ordered by span size by NodeSpansByFiles, so the first hit wins.
func enclosingIn(spans []store.NodeSpan, line int) (int64, bool) {
	for _, span := range spans {
		if span.StartLine <= line && span.EndLine >= line {
			return span.ID, true
		}
	}
	return 0, false
}

// resolveChanges turns references into edges for the files just written, then
// retries references that were waiting for a symbol to appear. Writes are
// batched: the old path opened two transactions per file and scanned the
// unindexed pending table by file_id once per file, which is why a cold index
// spent four minutes in SQLite rather than parsing. freshRun marks a run that
// re-resolved every file (initial or --full), so its own pending rows are
// retried too.
func resolveChanges(ctx context.Context, st *store.Store, root string, written []*outcome, freshRun, runRetry bool) error {
	changed := make(map[int64]bool, len(written))
	fileIDs := make([]int64, 0, len(written))
	for _, o := range written {
		changed[o.fileID] = true
		fileIDs = append(fileIDs, o.fileID)
	}

	ix, err := resolve.Build(ctx, st, root, changed)
	if err != nil {
		return err
	}

	idsByFile := make(map[int64]map[string]int64, len(written))
	for _, o := range written {
		ids, err := st.NodeIDsByFile(ctx, o.fileID)
		if err != nil {
			return err
		}
		idsByFile[o.fileID] = ids
	}

	// Resolve superclass links before any other reference: a self/cls call in a
	// re-resolved file may resolve through a base class this run did not
	// re-resolve, so the inheritance chain has to be complete first. The second
	// pass below still writes the EXTENDS edge itself, once.
	if ix.UsesInheritance() {
		links := make([]resolve.ClassLink, 0, len(fileIDs))
		for _, o := range written {
			ids := idsByFile[o.fileID]
			refs, err := facts.UnpackRefs(o.packedRefs)
			if err != nil {
				return fmt.Errorf("decode refs for %s: %w", o.src.Path, err)
			}
			for _, ref := range refs {
				if ref.Kind != "extends" {
					continue
				}
				src, ok := ids[ref.Enclosing]
				if !ok {
					continue
				}
				r := ix.Resolve(o.fileID, o.src.Path, ref)
				if r.Found && len(r.Candidates) == 0 && r.Dst != 0 {
					links = append(links, resolve.ClassLink{Src: src, Dst: r.Dst})
				}
			}
		}
		ix.AddInheritance(fileIDs, links)
	}

	// Rust trait impls are resolved before other references for the same reason:
	// a typed call may follow the trait's default method, and the link must be
	// complete before any reference in this run uses it.
	if ix.UsesRustImpls() {
		links := make([]resolve.ImplLink, 0, len(fileIDs))
		for _, o := range written {
			ids := idsByFile[o.fileID]
			refs, err := facts.UnpackRefs(o.packedRefs)
			if err != nil {
				return fmt.Errorf("decode refs for %s: %w", o.src.Path, err)
			}
			for _, ref := range refs {
				if ref.Kind != "implements" {
					continue
				}
				src, ok := ids[ref.Enclosing]
				if !ok {
					continue
				}
				r := ix.Resolve(o.fileID, o.src.Path, ref)
				if r.Found && len(r.Candidates) == 0 && r.Dst != 0 {
					links = append(links, resolve.ImplLink{Src: src, Dst: r.Dst})
				}
			}
		}
		ix.AddRustImpls(fileIDs, links)
	}

	var edges []store.Edge
	var pending []store.PendingRef
	for _, o := range written {
		ids := idsByFile[o.fileID]
		refs, err := facts.UnpackRefs(o.packedRefs)
		if err != nil {
			return fmt.Errorf("decode refs for %s: %w", o.src.Path, err)
		}
		for _, ref := range refs {
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
	}

	// A lazy import read that failed would leave part of the graph unresolved;
	// abort before persisting a result that is not the cold answer.
	if err := ix.Err(); err != nil {
		return err
	}

	if err := st.ReplacePendingFiles(ctx, fileIDs, pending); err != nil {
		return err
	}
	if _, err := st.InsertEdges(ctx, edges); err != nil {
		return err
	}

	// A symbol added by this run can satisfy references from earlier runs, and a
	// symbol removed by this run can make a previously ambiguous name unique.
	// Both cases reduce to: pending identifier names that are now globally
	// unique. The pending_name index makes that query proportional to the name
	// set instead of a 158k-row scan.
	if !runRetry {
		return nil
	}
	rows, err := st.PendingRetryByName(ctx, ix.UniqueNames())
	if err != nil {
		return err
	}

	type retryMatch struct {
		pending store.PendingRef
		result  resolve.Resolution
	}
	var matches []retryMatch
	for _, p := range rows {
		// A cold or full run has just re-resolved every file; its pending rows
		// were not retried by any earlier run, so complete the retry now. That
		// makes it as complete as the next incremental run would have made it,
		// instead of leaving 0.60-confidence edges for a later run.
		if changed[p.FileID] && !freshRun {
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
		matches = append(matches, retryMatch{pending: p, result: r})
	}

	spanFiles := make([]int64, 0, len(matches))
	seenSpanFile := make(map[int64]bool, len(matches))
	for _, m := range matches {
		if !seenSpanFile[m.pending.FileID] {
			seenSpanFile[m.pending.FileID] = true
			spanFiles = append(spanFiles, m.pending.FileID)
		}
	}
	spans, err := st.NodeSpansByFiles(ctx, spanFiles)
	if err != nil {
		return err
	}

	var retried []store.Edge
	var matched []store.PendingRef
	for _, m := range matches {
		src, ok := enclosingIn(spans[m.pending.FileID], m.pending.Line)
		if !ok || src == m.result.Dst {
			continue
		}
		kind := edgeKind(m.pending.Kind)
		if m.result.Kind != "" {
			kind = m.result.Kind
		}
		retried = append(retried, store.Edge{
			Src: src, Dst: m.result.Dst, Kind: kind,
			Confidence: m.result.Confidence, Rule: m.result.Rule, Line: m.pending.Line,
		})
		matched = append(matched, m.pending)
	}
	if err := st.DeletePendingBatch(ctx, matched); err != nil {
		return err
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
	// packedRefs is the compact form of the file's references. A cold run
	// parses the whole tree before it can resolve anything, so retaining one
	// Ref struct per reference cost ~258 MiB on the pinned Kibana tree; the
	// packed form is decoded one file at a time during resolution.
	packedRefs []byte
	imports    []facts.Import
	entries    []facts.Entrypoint
	anchors    []facts.Anchor
	skipped    bool
	fileID     int64
}

func parseOne(src Source, ex *lang.Extractor, existing map[string]store.FileState, full bool) *outcome {
	if src.Unchanged {
		return &outcome{src: src, skipped: true}
	}
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
		src:        src,
		lang:       langName,
		hash:       hash,
		ok:         result.OK,
		errMsg:     result.Err,
		symbols:    toNodes(result.Symbols),
		packedRefs: facts.PackRefs(result.Refs),
		imports:    result.Imports,
		entries:    result.Entrypoints,
		anchors:    result.Anchors,
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
