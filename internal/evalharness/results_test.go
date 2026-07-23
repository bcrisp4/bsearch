package evalharness

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestCorpusVersion_Deterministic(t *testing.T) {
	v1, err := CorpusVersion(fixtureDir)
	if err != nil {
		t.Fatalf("CorpusVersion() error = %v, want nil", err)
	}
	v2, err := CorpusVersion(fixtureDir)
	if err != nil {
		t.Fatalf("CorpusVersion() error = %v, want nil", err)
	}
	if v1 != v2 {
		t.Errorf("CorpusVersion() not deterministic: %q != %q", v1, v2)
	}
	if !strings.HasPrefix(v1, "sha256:") {
		t.Errorf("CorpusVersion() = %q, want sha256: prefix", v1)
	}
}

func TestCorpusVersion_ChangesWithGolden(t *testing.T) {
	before, err := CorpusVersion(fixtureDir)
	if err != nil {
		t.Fatalf("CorpusVersion() error = %v, want nil", err)
	}

	dir := copyFixture(t)
	path := filepath.Join(dir, "golden.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden.yaml: %v", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Fatalf("write golden.yaml: %v", err)
	}

	after, err := CorpusVersion(dir)
	if err != nil {
		t.Fatalf("CorpusVersion() error = %v, want nil", err)
	}
	if before == after {
		t.Errorf("CorpusVersion() unchanged after appending a byte to golden.yaml: %q", after)
	}
}

func TestCorpusVersion_ManifestOptional(t *testing.T) {
	withManifest, err := CorpusVersion(fixtureDir)
	if err != nil {
		t.Fatalf("CorpusVersion() error = %v, want nil", err)
	}

	dir := copyFixture(t)
	if err := os.Remove(filepath.Join(dir, "corpus", "manifest.json")); err != nil {
		t.Fatalf("remove manifest.json: %v", err)
	}

	withoutManifest, err := CorpusVersion(dir)
	if err != nil {
		t.Fatalf("CorpusVersion() with no manifest.json: error = %v, want nil", err)
	}
	if withoutManifest == withManifest {
		t.Errorf("CorpusVersion() same with and without manifest.json: %q", withoutManifest)
	}
}

func TestDocsVersion_Deterministic(t *testing.T) {
	v1, err := DocsVersion(fixtureDir)
	if err != nil {
		t.Fatalf("DocsVersion() error = %v, want nil", err)
	}
	v2, err := DocsVersion(fixtureDir)
	if err != nil {
		t.Fatalf("DocsVersion() error = %v, want nil", err)
	}
	if v1 != v2 {
		t.Errorf("DocsVersion() not deterministic: %q != %q", v1, v2)
	}
	if !strings.HasPrefix(v1, "sha256:") {
		t.Errorf("DocsVersion() = %q, want sha256: prefix", v1)
	}
}

func TestDocsVersion_ChangesWithManifest(t *testing.T) {
	before, err := DocsVersion(fixtureDir)
	if err != nil {
		t.Fatalf("DocsVersion() error = %v, want nil", err)
	}

	dir := copyFixture(t)
	path := filepath.Join(dir, "corpus", "manifest.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read manifest.json: %v", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Fatalf("write manifest.json: %v", err)
	}

	after, err := DocsVersion(dir)
	if err != nil {
		t.Fatalf("DocsVersion() error = %v, want nil", err)
	}
	if before == after {
		t.Errorf("DocsVersion() unchanged after appending a byte to manifest.json: %q", after)
	}
}

func TestDocsVersion_NoManifestFallback(t *testing.T) {
	dir := copyFixture(t)
	if err := os.Remove(filepath.Join(dir, "corpus", "manifest.json")); err != nil {
		t.Fatalf("remove manifest.json: %v", err)
	}

	v, err := DocsVersion(dir)
	if err != nil {
		t.Fatalf("DocsVersion() with no manifest.json: error = %v, want nil", err)
	}
	if v != "nomanifest" {
		t.Errorf("DocsVersion() with no manifest.json = %q, want %q", v, "nomanifest")
	}
}

// TestDocsVersion_UnaffectedByGolden asserts Finding 4's fix: unlike
// CorpusVersion (which folds in golden.yaml so compare can gate on the
// query set too), DocsVersion hashes only the document set — editing
// query labels in golden.yaml must not mint a new work-db key and force a
// pointless re-embed of an unchanged corpus.
func TestDocsVersion_UnaffectedByGolden(t *testing.T) {
	before, err := DocsVersion(fixtureDir)
	if err != nil {
		t.Fatalf("DocsVersion() error = %v, want nil", err)
	}

	dir := copyFixture(t)
	path := filepath.Join(dir, "golden.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden.yaml: %v", err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Fatalf("write golden.yaml: %v", err)
	}

	after, err := DocsVersion(dir)
	if err != nil {
		t.Fatalf("DocsVersion() error = %v, want nil", err)
	}
	if before != after {
		t.Errorf("DocsVersion() changed after editing golden.yaml: before=%q after=%q", before, after)
	}
}

func TestPercentile_NearestRank(t *testing.T) {
	vals := []float64{10, 20, 30, 40, 50, 60, 70, 80, 90, 100}
	orig := append([]float64(nil), vals...)

	if got := Percentile(vals, 50); got != 50 {
		t.Errorf("Percentile(vals, 50) = %v, want 50", got)
	}
	if got := Percentile(vals, 95); got != 100 {
		t.Errorf("Percentile(vals, 95) = %v, want 100", got)
	}
	if !reflect.DeepEqual(vals, orig) {
		t.Errorf("Percentile mutated input: %v, want %v", vals, orig)
	}

	if got := Percentile([]float64{42}, 50); got != 42 {
		t.Errorf("Percentile({42}, 50) = %v, want 42", got)
	}
	if got := Percentile([]float64{42}, 95); got != 42 {
		t.Errorf("Percentile({42}, 95) = %v, want 42", got)
	}

	if got := Percentile(nil, 50); got != 0 {
		t.Errorf("Percentile(nil, 50) = %v, want 0", got)
	}
}

func TestWriteReadResults_RoundTrip(t *testing.T) {
	started := time.Date(2026, 7, 20, 12, 30, 0, 0, time.UTC)
	want := Results{
		Bsearch: BsearchInfo{
			Version:        "0.1.0",
			ChunkerVersion: "v1",
		},
		Corpus: CorpusInfo{
			Name:    "minicorpus",
			Path:    fixtureDir,
			Version: "sha256:deadbeef",
		},
		Model: ModelInfo{
			Name:          "test-model",
			Dims:          768,
			Fingerprint:   "fp1",
			QueryPrefix:   "query: ",
			PassagePrefix: "passage: ",
		},
		Run: RunInfo{
			StartedAt:    started,
			IndexSeconds: 1.5,
			IndexedDocs:  2,
			Queries:      4,
		},
		Aggregates: Aggregates{
			OverallNoExact: SliceStats{N: 3, RecallAtK: 0.5, MRRAtK: 0.5, SuccessAt1: 0.5, AcceptableAtK: 1},
			Overall:        SliceStats{N: 3, RecallAtK: 0.5, MRRAtK: 0.5, SuccessAt1: 0.5, AcceptableAtK: 1},
			Slices:         map[string]SliceStats{},
		},
		LatencyMS: LatencySummary{
			Embed: PercentilePair{P50: 10, P95: 20},
			KNN:   PercentilePair{P50: 5, P95: 8},
			Total: PercentilePair{P50: 15, P95: 28},
		},
		Queries: []QueryResult{
			{
				ID:       "q001",
				Query:    "that letter about the rent going up",
				Tags:     []string{"recall", "letters", "converted"},
				Relevant: []string{"corpus/letters/renewal.md"},
				Ranked: []RankedDoc{
					{Path: "corpus/letters/renewal.md", Distance: 0.1},
				},
				QueryScore: QueryScore{
					RecallAtK:     1,
					RR:            1,
					SuccessAt1:    1,
					AcceptableAtK: 0,
				},
				LatencyMS: QueryLatency{EmbedMS: 3.2, KNNMS: 1.1},
			},
		},
	}

	path := filepath.Join(t.TempDir(), "nested", "results.json")
	if err := WriteResults(path, want); err != nil {
		t.Fatalf("WriteResults() error = %v, want nil", err)
	}

	got, err := ReadResults(path)
	if err != nil {
		t.Fatalf("ReadResults() error = %v, want nil", err)
	}

	if !got.Run.StartedAt.Equal(want.Run.StartedAt) {
		t.Errorf("Run.StartedAt = %v, want %v", got.Run.StartedAt, want.Run.StartedAt)
	}
	got.Run.StartedAt = time.Time{}
	want.Run.StartedAt = time.Time{}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ReadResults() round trip mismatch:\ngot:  %+v\nwant: %+v", got, want)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat results file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("results file mode = %o, want 0600", perm)
	}
}

func TestReadResults_MissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.json")
	_, err := ReadResults(path)
	if err == nil {
		t.Fatal("ReadResults() error = nil, want error")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not mention path %q", err.Error(), path)
	}
}
