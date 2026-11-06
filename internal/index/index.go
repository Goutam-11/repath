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
	"github.com/Goutam-11/repath/internal/lang/tsx"
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
	Duration      time.Duration
}

// Run discovers files, parses the changed ones, and writes them in batches.
// Parsing runs on a worker pool; every database write happens on the calling
// goroutine, because SQLite has exactly one writer.
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
	parsed := make(chan outcome, workers*2)

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ex := tsx.New()
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
	batch := make([]outcome, 0, 200)

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
				Path:    o.src.Path,
				Lang:    o.lang,
				Size:    o.src.Size,
				MTime:   o.src.MTime,
				Hash:    o.hash,
				ParseOK: o.ok,
				Err:     o.errMsg,
			})
			if err != nil {
				_ = tx.Rollback()
				return err
			}
			if _, err := tx.ReplaceNodes(ctx, fileID, o.lang, o.symbols); err != nil {
				_ = tx.Rollback()
				return err
			}
			res.Indexed++
			res.Nodes += len(o.symbols)
			if !o.ok {
				res.ParseFailures++
			}
		}
		return tx.Commit()
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
			batch = batch[:0]
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

// outcome is one file's parse result, ready to write.
type outcome struct {
	src     Source
	lang    string
	hash    string
	ok      bool
	errMsg  string
	symbols []store.Node
	skipped bool
}

func parseOne(src Source, ex *tsx.Extractor, existing map[string]store.FileState, full bool) outcome {
	data, err := os.ReadFile(src.Abs)
	if err != nil {
		return outcome{src: src, ok: false, errMsg: err.Error()}
	}

	sum := sha256.Sum256(data)
	hash := hex.EncodeToString(sum[:])

	if !full {
		if state, found := existing[src.Path]; found && state.Hash == hash {
			return outcome{src: src, skipped: true}
		}
	}

	if isBinary(data) {
		return outcome{src: src, hash: hash, ok: false, errMsg: "binary content"}
	}

	dialect, langName, ok := lang.For(src.Path)
	if !ok {
		return outcome{src: src, hash: hash, ok: false, errMsg: "unsupported language"}
	}

	symbols, parseOK, errMsg := ex.Extract(data, dialect)
	return outcome{src: src, lang: langName, hash: hash, ok: parseOK, errMsg: errMsg, symbols: toNodes(symbols)}
}

// toNodes converts extractor output into rows; the store must not depend on a
// language package.
func toNodes(symbols []tsx.Symbol) []store.Node {
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
