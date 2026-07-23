package evalharness

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// SampleDocs picks up to n documents from <corpusDir>/corpus: category
// directories sorted, one doc per category round-robin (alphabetical within),
// so the sample spans categories and is identical across runs and models.
// Only .md/.markdown/.txt files count (same filter as discovery).
// Returned paths are corpus-relative ("corpus/<cat>/<file>").
func SampleDocs(corpusDir string, n int) ([]string, error) {
	root := filepath.Join(corpusDir, "corpus")

	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("sample docs: %w", err)
	}

	var categories []string
	for _, e := range entries {
		if e.IsDir() {
			categories = append(categories, e.Name())
		}
	}
	sort.Strings(categories)

	// Per-category sorted file lists, built up front so round-robin below
	// is just index bookkeeping.
	filesByCategory := make([][]string, len(categories))
	for i, cat := range categories {
		catEntries, err := os.ReadDir(filepath.Join(root, cat))
		if err != nil {
			return nil, fmt.Errorf("sample docs: %w", err)
		}
		var files []string
		for _, e := range catEntries {
			if e.IsDir() {
				continue
			}
			if isTextFile(e.Name()) {
				files = append(files, e.Name())
			}
		}
		sort.Strings(files)
		filesByCategory[i] = files
	}

	var out []string
	for pass := 0; len(out) < n; pass++ {
		added := false
		for i, cat := range categories {
			if pass >= len(filesByCategory[i]) {
				continue
			}
			out = append(out, "corpus/"+cat+"/"+filesByCategory[i][pass])
			added = true
			if len(out) == n {
				break
			}
		}
		if !added {
			// Every category exhausted: no point looping further.
			break
		}
	}
	return out, nil
}

// isTextFile reports whether the file is in the summarizer bench's
// text/markdown sample set. Duplicated (not imported) from
// internal/discovery/discovery.go's isTextFile: evalharness must not import
// discovery, and the two filters are allowed to diverge in future.
func isTextFile(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".md", ".markdown", ".txt":
		return true
	}
	return false
}
