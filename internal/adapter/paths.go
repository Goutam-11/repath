package adapter

import (
	"context"
	"path/filepath"
	"strings"

	"github.com/Goutam-11/repath/internal/store"
)

// filePathLookup maps compiler document paths to indexed file ids. It accepts
// an exact repo-relative path first, then a unique path suffix; an ambiguous
// suffix is skipped rather than guessed. A producer may root its index at a
// subdirectory of the repo (for example src/tsconfig.json), so the suffix
// fallback is what lets `charCode.ts` match `src/charCode.ts`.
func filePathLookup(ctx context.Context, st *store.Store) (func(string) (int64, bool), error) {
	paths, err := st.FilePaths(ctx)
	if err != nil {
		return nil, err
	}
	pathIDs := make(map[string]int64, len(paths))
	for id, path := range paths {
		pathIDs[path] = id
	}
	return func(compilerPath string) (int64, bool) {
		compilerPath = strings.TrimPrefix(filepath.ToSlash(compilerPath), "./")
		if id, ok := pathIDs[compilerPath]; ok {
			return id, true
		}
		var matches []int64
		for path, id := range pathIDs {
			if strings.HasSuffix(path, "/"+compilerPath) {
				matches = append(matches, id)
			}
		}
		if len(matches) == 1 {
			return matches[0], true
		}
		return 0, false
	}, nil
}
