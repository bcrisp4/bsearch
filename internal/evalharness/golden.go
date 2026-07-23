// Package evalharness scores embedding models against a golden query set
// for retrieval quality. It is consumed only by the bsearch CLI's `eval`
// subcommand — never by the daemon or the production index/search path.
package evalharness

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

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
// path that doesn't exist under corpusDir; a relevant/acceptable path that
// isn't corpus-relative under corpus/ (absolute, or escaping via "../" —
// such a path could still stat successfully yet never match the
// "corpus/..." paths eval run produces, silently corrupting scoring, and
// stats arbitrary filesystem locations besides); a duplicate path within
// Relevant or within Acceptable (ScoreQuery's hit map dedupes ranked hits,
// but len(q.Relevant) is the recall denominator — a duplicate would deflate
// recall without ever being able to inflate the hit count to compensate); a
// path listed in both Relevant and Acceptable for the same query (ScoreQuery
// condenses it out of the ranking before hit-counting, so it can never score
// a hit, yet it would still inflate the recall denominator); the
// "zero-answer" tag combined with a non-empty Relevant (the tag asserts the
// corpus has no correct answer for the query).
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
			if relevantSet[rel] {
				errs = append(errs, fmt.Errorf("%s: duplicate path in relevant: %s", q.ID, rel))
			}
			relevantSet[rel] = true

			// A path outside corpus/ can still stat successfully (it just
			// points somewhere on the real filesystem) yet can never equal
			// one of eval run's produced "corpus/..." paths — silently
			// corrupting scoring rather than failing loudly. Reject before
			// stat'ing rather than after: statting an unconfined path is
			// itself the second half of the problem.
			if err := checkPathConfined(rel); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", q.ID, err))
				continue
			}
			if err := checkPathExists(corpusDir, rel); err != nil {
				errs = append(errs, fmt.Errorf("%s: relevant path %q: %w", q.ID, rel, err))
			}
		}
		acceptableSet := make(map[string]bool, len(q.Acceptable))
		for _, rel := range q.Acceptable {
			if acceptableSet[rel] {
				errs = append(errs, fmt.Errorf("%s: duplicate path in acceptable: %s", q.ID, rel))
			}
			acceptableSet[rel] = true

			if err := checkPathConfined(rel); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", q.ID, err))
				continue
			}
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

// checkPathConfined reports an error unless rel is a corpus-relative path
// under corpus/ — the shape eval run's ranked-hit paths always take (see
// runEvalRun in cmd/bsearch/eval.go: "corpus/" + relativized path). An
// absolute path, or one that climbs out via "../" (even by first descending
// into corpus/ and climbing back out, e.g. "corpus/../../etc/hosts"), would
// still stat successfully — filepath.Join+os.Stat don't care where a path
// points — but can never come out equal to a produced "corpus/..." path, so
// it would silently never match and never score, while also having stat'd
// an arbitrary filesystem location on the caller's behalf.
//
// path.Clean (not filepath.Clean) is deliberate: golden.yaml paths are
// always "/"-separated, matching path.Join("corpus", ...) in eval.go, not
// the OS path separator.
func checkPathConfined(rel string) error {
	if filepath.IsAbs(rel) {
		return fmt.Errorf("path must be corpus-relative under corpus/: %s", rel)
	}
	if !strings.HasPrefix(path.Clean(rel), "corpus/") {
		return fmt.Errorf("path must be corpus-relative under corpus/: %s", rel)
	}
	return nil
}
