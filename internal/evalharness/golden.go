// Package evalharness scores embedding models against a golden query set
// for retrieval quality. It is consumed only by the bsearch CLI's `eval`
// subcommand — never by the daemon or the production index/search path.
package evalharness

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Query is one golden query: free text plus the set of corpus documents
// that count as correct (Relevant) or partially correct (Acceptable) when
// scoring retrieval results.
type Query struct {
	ID    string `yaml:"id"`
	Query string `yaml:"query"`
	// Relevant holds corpus-relative document paths, e.g.
	// "corpus/letters/renewal.md".
	Relevant   []string `yaml:"relevant"`
	Acceptable []string `yaml:"acceptable"`
	Tags       []string `yaml:"tags"`
}

// HasTag reports whether tag is present in the query's Tags.
func (q Query) HasTag(tag string) bool {
	return hasTag(q.Tags, tag)
}

// goldenFile is the on-disk shape of golden.yaml. Unknown keys are ignored
// (yaml.Unmarshal's default lenient behaviour) — full schema policing is
// the Python `corpusgen golden` validator's job; this loader only rejects
// what would corrupt scoring.
type goldenFile struct {
	Queries []Query `yaml:"queries"`
}

// LoadGolden parses <corpusDir>/golden.yaml and validates it against the
// corpus on disk: every Relevant/Acceptable path must exist under
// corpusDir. All violations found are collected and returned together via
// errors.Join, each prefixed with the offending query's id, so a single run
// surfaces every problem instead of stopping at the first.
//
// Rejected: duplicate query ids; empty query text; a relevant/acceptable
// path that doesn't exist under corpusDir; a path listed in both Relevant
// and Acceptable for the same query (ScoreQuery condenses it out of the
// ranking before hit-counting, so it can never score a hit, yet it would
// still inflate the recall denominator); the "zero-answer" tag combined
// with a non-empty Relevant (the tag asserts the corpus has no correct
// answer for the query).
func LoadGolden(corpusDir string) ([]Query, error) {
	data, err := os.ReadFile(filepath.Join(corpusDir, "golden.yaml"))
	if err != nil {
		return nil, fmt.Errorf("read golden.yaml: %w", err)
	}

	var file goldenFile
	if err := yaml.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("parse golden.yaml: %w", err)
	}

	var errs []error
	seen := make(map[string]bool, len(file.Queries))
	for _, q := range file.Queries {
		if seen[q.ID] {
			errs = append(errs, fmt.Errorf("%s: duplicate query id", q.ID))
		}
		seen[q.ID] = true

		if q.Query == "" {
			errs = append(errs, fmt.Errorf("%s: query text is empty", q.ID))
		}

		if q.HasTag("zero-answer") && len(q.Relevant) > 0 {
			errs = append(errs, fmt.Errorf("%s: tagged zero-answer but has non-empty relevant", q.ID))
		}

		relevantSet := make(map[string]bool, len(q.Relevant))
		for _, rel := range q.Relevant {
			relevantSet[rel] = true
			if err := checkPathExists(corpusDir, rel); err != nil {
				errs = append(errs, fmt.Errorf("%s: relevant path %q: %w", q.ID, rel, err))
			}
		}
		for _, rel := range q.Acceptable {
			if err := checkPathExists(corpusDir, rel); err != nil {
				errs = append(errs, fmt.Errorf("%s: acceptable path %q: %w", q.ID, rel, err))
			}
			// ScoreQuery condenses a path in both sets out of the ranking
			// before hit-counting (it can never score as a hit), yet it
			// still counts in the recall denominator via len(q.Relevant) —
			// silently corrupting recall for this query. Reject the
			// overlap outright rather than let it score.
			if relevantSet[rel] {
				errs = append(errs, fmt.Errorf("%s: path in both relevant and acceptable: %s", q.ID, rel))
			}
		}
	}

	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return file.Queries, nil
}

// checkPathExists reports an error if rel does not exist under corpusDir.
func checkPathExists(corpusDir, rel string) error {
	if _, err := os.Stat(filepath.Join(corpusDir, rel)); err != nil {
		return err
	}
	return nil
}
