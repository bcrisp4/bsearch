package evalharness

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Results is the per-run results file (spec: "Results file"). Field names
// and JSON keys must match the spec's schema exactly: `compare` and other
// tooling consume the file on disk, not the Go type.
type Results struct {
	Bsearch    BsearchInfo    `json:"bsearch"`
	Corpus     CorpusInfo     `json:"corpus"`
	Model      ModelInfo      `json:"model"`
	Run        RunInfo        `json:"run"`
	Aggregates Aggregates     `json:"aggregates"`
	LatencyMS  LatencySummary `json:"latency_ms"`
	Queries    []QueryResult  `json:"queries"`
}

// BsearchInfo identifies the binary and pipeline version that produced a
// run, so results from different chunker/build versions aren't compared as
// if they were the same experiment.
type BsearchInfo struct {
	Version        string `json:"version"`
	ChunkerVersion string `json:"chunker_version"`
}

// CorpusInfo identifies the corpus a run was scored against.
type CorpusInfo struct {
	Name    string `json:"name"` // basename of corpus dir
	Path    string `json:"path"`
	Version string `json:"version"` // "sha256:<hex>", see CorpusVersion
}

// ModelInfo identifies the embedding model and prefix templates used for a
// run. Fingerprint distinguishes models that report the same Name but were
// served with different weights/quantization.
type ModelInfo struct {
	Name          string `json:"name"`
	Dims          int    `json:"dims"`
	Fingerprint   string `json:"fingerprint"`
	QueryPrefix   string `json:"query_prefix"`
	PassagePrefix string `json:"passage_prefix"`
}

// RunInfo records how the run was produced (timing, corpus size, query
// count) — not what it scored, which lives in Aggregates and Queries.
//
// Deliberately has no IndexedChunks field: the indexing pipeline doesn't
// currently expose a chunk count, and adding a store method solely to
// populate one results field would be scope creep for this task.
type RunInfo struct {
	StartedAt    time.Time `json:"started_at"`
	IndexSeconds float64   `json:"index_seconds"`
	IndexedDocs  int       `json:"indexed_docs"`
	Queries      int       `json:"queries"`
	// Limit is the --limit (documents per query / scoring cutoff) the run
	// was scored at. compare refuses to pair two runs with different
	// Limit — a --limit 10 run and a --limit 20 run aren't comparable, the
	// gap between them is a cutoff artifact, not a model delta.
	Limit int `json:"limit"`
}

// PercentilePair holds the p50/p95 of a latency distribution.
type PercentilePair struct {
	P50 float64 `json:"p50"`
	P95 float64 `json:"p95"`
}

// LatencySummary aggregates per-stage query latency across a run.
type LatencySummary struct {
	Embed PercentilePair `json:"embed"`
	KNN   PercentilePair `json:"knn"`
	Total PercentilePair `json:"total"`
}

// QueryLatency is one query's per-stage timing, in milliseconds.
type QueryLatency struct {
	EmbedMS float64 `json:"embed"`
	KNNMS   float64 `json:"knn"`
}

// QueryResult is one query's full record in the results file: the golden
// query, what was retrieved, its score, and its latency.
type QueryResult struct {
	ID       string      `json:"id"`
	Query    string      `json:"query"`
	Tags     []string    `json:"tags"`
	Relevant []string    `json:"relevant"`
	Ranked   []RankedDoc `json:"ranked"`
	// QueryScore is embedded so its json tags (recall_at_10, rr,
	// success_at_1, acceptable_at_10) flatten directly into this object.
	QueryScore
	LatencyMS QueryLatency `json:"latency_ms"`
}

// CorpusVersion identifies a corpus's query set and document set together
// as "sha256:<hex>". It hashes golden.yaml (always present) and
// corpus/manifest.json (when present, length-prefixed and hashed after
// golden.yaml so the two inputs can't collide at a file boundary) under
// corpusDir. compare uses this to refuse comparing runs against different
// corpus versions.
//
// A missing manifest.json is not an error (not every corpus generates
// one); any other read error is returned.
func CorpusVersion(corpusDir string) (string, error) {
	h := sha256.New()

	golden, err := os.ReadFile(filepath.Join(corpusDir, "golden.yaml"))
	if err != nil {
		return "", fmt.Errorf("corpus version: %w", err)
	}
	fmt.Fprintf(h, "%d:", len(golden))
	h.Write(golden)

	manifest, err := os.ReadFile(filepath.Join(corpusDir, "corpus", "manifest.json"))
	switch {
	case err == nil:
		fmt.Fprintf(h, "%d:", len(manifest))
		h.Write(manifest)
	case errors.Is(err, fs.ErrNotExist):
		// No manifest.json: not every corpus generates one.
	default:
		return "", fmt.Errorf("corpus version: %w", err)
	}

	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// docsVersionNoManifest is DocsVersion's return value when corpusDir has no
// corpus/manifest.json: without per-file content hashes there is no way to
// detect a deleted document, so staleness detection for such a corpus falls
// back entirely to pipeline idempotency (which does catch added/edited
// files, just not deletions). Hand-built local corpora without a manifest
// accept this limitation.
const docsVersionNoManifest = "nomanifest"

// DocsVersion identifies a corpus's document set alone — sha256 over
// corpus/manifest.json — as "sha256:<hex>", or the literal "nomanifest"
// when corpusDir has no manifest.json (see docsVersionNoManifest).
//
// This is deliberately narrower than CorpusVersion (which also hashes
// golden.yaml): the eval work database only needs to be invalidated when
// the document set changes underneath it — discovery has no deletion pass,
// so a corpus regenerated in place would otherwise leave a deleted
// document's stale vectors in a reused db. Editing golden.yaml (relabeling
// a query, fixing a typo) changes CorpusVersion but must not force a
// pointless re-embed of an unchanged corpus, so the work-db key is derived
// from DocsVersion, not CorpusVersion.
func DocsVersion(corpusDir string) (string, error) {
	manifest, err := os.ReadFile(filepath.Join(corpusDir, "corpus", "manifest.json"))
	switch {
	case err == nil:
		h := sha256.New()
		fmt.Fprintf(h, "%d:", len(manifest))
		h.Write(manifest)
		return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
	case errors.Is(err, fs.ErrNotExist):
		return docsVersionNoManifest, nil
	default:
		return "", fmt.Errorf("docs version: %w", err)
	}
}

// Percentile returns the nearest-rank percentile (p in (0,100]) of vals,
// sorting a copy so the caller's slice is never mutated. Empty input
// returns 0 rather than panicking or dividing by zero.
func Percentile(vals []float64, p float64) float64 {
	if len(vals) == 0 {
		return 0
	}

	sorted := make([]float64, len(vals))
	copy(sorted, vals)
	sort.Float64s(sorted)

	idx := int(math.Ceil(p/100*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// WriteResults writes r as indented JSON to path, creating parent
// directories as needed. The file is written 0o600 (single-user machine,
// but results may embed corpus file paths — no reason to make it
// world-readable).
func WriteResults(path string, r Results) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("write results %s: %w", path, err)
	}

	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("write results %s: %w", path, err)
	}

	if err := os.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write results %s: %w", path, err)
	}
	return nil
}

// ReadResults reads a results file previously written by WriteResults —
// compare's input.
func ReadResults(path string) (Results, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Results{}, fmt.Errorf("read results %s: %w", path, err)
	}

	var r Results
	if err := json.Unmarshal(data, &r); err != nil {
		return Results{}, fmt.Errorf("read results %s: %w", path, err)
	}
	return r, nil
}
