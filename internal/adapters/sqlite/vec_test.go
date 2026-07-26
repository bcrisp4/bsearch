package sqlite

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bcrisp4/bsearch/internal/domain"
)

// vecSpec is a raw-template spec for tests that only care about model+dims.
func vecSpec(model string) domain.EmbeddingSpec {
	return domain.EmbeddingSpec{Model: model}
}

func TestEnsureVecTableCreatesGenerationAndMeta(t *testing.T) {
	store, db := newTestStore(t)
	ctx := context.Background()

	if err := store.EnsureVecTable(ctx, vecSpec("test-model"), 4); err != nil {
		t.Fatalf("EnsureVecTable: %v", err)
	}

	var name string
	err := db.Reader().QueryRow(
		"SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'vec_chunks_1'").Scan(&name)
	if err != nil {
		t.Fatalf("vec_chunks_1 not created: %v", err)
	}

	var current string
	if err := db.Reader().QueryRow(
		"SELECT value FROM meta WHERE key = 'vec_current'").Scan(&current); err != nil {
		t.Fatalf("meta vec_current: %v", err)
	}
	if current != "vec_chunks_1" {
		t.Errorf("vec_current = %q, want vec_chunks_1", current)
	}

	var descriptor string
	if err := db.Reader().QueryRow(
		"SELECT value FROM meta WHERE key = 'vec_table:vec_chunks_1'").Scan(&descriptor); err != nil {
		t.Fatalf("meta descriptor: %v", err)
	}
	for _, want := range []string{"test-model", "4", "float32"} {
		if !strings.Contains(descriptor, want) {
			t.Errorf("descriptor %q missing %q", descriptor, want)
		}
	}

	// Same model+dims again: no new generation.
	if err := store.EnsureVecTable(ctx, vecSpec("test-model"), 4); err != nil {
		t.Fatalf("EnsureVecTable (repeat): %v", err)
	}
	// NOT GLOB excludes vec0's shadow tables (vec_chunks_1_rowids, ...).
	var count int
	if err := db.Reader().QueryRow(
		`SELECT count(*) FROM sqlite_master WHERE type = 'table'
		 AND name GLOB 'vec_chunks_*' AND name NOT GLOB 'vec_chunks_*_*'`).Scan(&count); err != nil {
		t.Fatalf("count vec tables: %v", err)
	}
	if count != 1 {
		t.Errorf("vec table count = %d after repeat ensure, want 1", count)
	}

	// New model: new generation becomes current.
	if err := store.EnsureVecTable(ctx, vecSpec("other-model"), 8); err != nil {
		t.Fatalf("EnsureVecTable (new model): %v", err)
	}
	if err := db.Reader().QueryRow(
		"SELECT value FROM meta WHERE key = 'vec_current'").Scan(&current); err != nil {
		t.Fatalf("meta vec_current: %v", err)
	}
	if current != "vec_chunks_2" {
		t.Errorf("vec_current = %q after model change, want vec_chunks_2", current)
	}
}

func TestUpsertVectorsAndSearch(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	if err := store.EnsureVecTable(ctx, vecSpec("test-model"), 3); err != nil {
		t.Fatalf("EnsureVecTable: %v", err)
	}
	ids := seedChunked(t, store, testDoc("hash-1", "/notes/a.md"),
		testChunks("north", "east", "up"))

	vectors := [][]float32{
		{1, 0, 0}, // north
		{0, 1, 0}, // east
		{0, 0, 1}, // up
	}
	if err := store.UpsertVectors(ctx, ids, vectors); err != nil {
		t.Fatalf("UpsertVectors: %v", err)
	}

	hits, err := store.SearchVectors(ctx, []float32{0.9, 0.1, 0}, 2)
	if err != nil {
		t.Fatalf("SearchVectors: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("hits = %d, want 2", len(hits))
	}
	if hits[0].Chunk.Text != "north" {
		t.Errorf("nearest = %q, want north", hits[0].Chunk.Text)
	}
	if hits[1].Chunk.Text != "east" {
		t.Errorf("second = %q, want east", hits[1].Chunk.Text)
	}
	if hits[0].Distance >= hits[1].Distance {
		t.Errorf("distances not ascending: %v then %v", hits[0].Distance, hits[1].Distance)
	}
	if hits[0].Doc.Path != "/notes/a.md" {
		t.Errorf("hit doc path = %q", hits[0].Doc.Path)
	}
	if hits[0].Doc.ContentHash != "hash-1" {
		t.Errorf("hit doc hash = %q, want the join key hash-1", hits[0].Doc.ContentHash)
	}
	if hits[0].Chunk.HeadingPath == "" {
		t.Error("hit chunk missing heading path")
	}
}

// One row per (chunk, referencing path): a chunk whose content lives at N
// paths yields N consecutive rows, newest mtime first, tie broken path
// ascending — the ordering contract CollapseBestPerContent's primary-path
// pick depends on.
func TestSearchVectorsFansOutPerReferencingPath(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	if err := store.EnsureVecTable(ctx, vecSpec("test-model"), 3); err != nil {
		t.Fatalf("EnsureVecTable: %v", err)
	}

	// One content at three paths. /w/b-new.md is newest; the other two tie
	// on mtime and must order by path — and /w/a-tie.md sorting before
	// /w/b-new.md lexically proves mtime takes precedence over path.
	newest := testDoc("hash-1", "/w/b-new.md")
	newest.MTime = time.Unix(200, 0)
	tieA := testDoc("hash-1", "/w/a-tie.md")
	tieA.MTime = time.Unix(100, 0)
	tieZ := testDoc("hash-1", "/w/z-tie.md")
	tieZ.MTime = time.Unix(100, 0)

	ids := seedChunked(t, store, newest, testChunks("alpha", "beta"))
	if err := store.UpsertDocuments(ctx, []domain.Document{tieA, tieZ}); err != nil {
		t.Fatalf("upsert extra paths: %v", err)
	}
	if err := store.UpsertVectors(ctx, ids, [][]float32{{1, 0, 0}, {0, 1, 0}}); err != nil {
		t.Fatalf("UpsertVectors: %v", err)
	}

	hits, err := store.SearchVectors(ctx, []float32{1, 0, 0}, 2)
	if err != nil {
		t.Fatalf("SearchVectors: %v", err)
	}
	// limit counts KNN chunks, not fanned-out rows: 2 chunks × 3 paths.
	if len(hits) != 6 {
		t.Fatalf("hits = %d, want 6 (2 chunks fanned out over 3 paths)", len(hits))
	}
	wantPaths := []string{"/w/b-new.md", "/w/a-tie.md", "/w/z-tie.md"}
	for chunk := 0; chunk < 2; chunk++ {
		rows := hits[chunk*3 : chunk*3+3]
		for i, h := range rows {
			if h.Doc.Path != wantPaths[i] {
				t.Errorf("chunk %d row %d path = %q, want %q (mtime DESC, then path ASC)",
					chunk, i, h.Doc.Path, wantPaths[i])
			}
			// Consecutive rows for one chunk are the same chunk.
			if h.Chunk.Ordinal != rows[0].Chunk.Ordinal || h.Distance != rows[0].Distance {
				t.Errorf("chunk %d row %d = ordinal %d dist %v, want the fan-out rows consecutive per chunk",
					chunk, i, h.Chunk.Ordinal, h.Distance)
			}
		}
	}
	if hits[0].Chunk.Text != "alpha" || hits[3].Chunk.Text != "beta" {
		t.Errorf("chunk order = %q then %q, want alpha (nearest) then beta",
			hits[0].Chunk.Text, hits[3].Chunk.Text)
	}
}

// The fan-out's mtime tie-break is a sort in Go, not an ORDER BY — this is
// the mutation trap for it. The two paths tie on mtime and are inserted in
// reverse-lexical order (separate upserts, so their rowids — and any index
// scan over them — come back z-then-a): only the explicit path tie-break
// puts them back ascending, so dropping it fails here where the old SQL
// ORDER BY version could not.
func TestSearchVectorsEqualMTimeTieBreaksByPathAscending(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	if err := store.EnsureVecTable(ctx, vecSpec("test-model"), 3); err != nil {
		t.Fatalf("EnsureVecTable: %v", err)
	}
	tie := time.Unix(100, 0)
	last := testDoc("hash-1", "/w/z-last.md")
	last.MTime = tie
	first := testDoc("hash-1", "/w/a-first.md")
	first.MTime = tie

	ids := seedChunked(t, store, last, testChunks("alpha"))
	if err := store.UpsertDocuments(ctx, []domain.Document{first}); err != nil {
		t.Fatalf("upsert second path: %v", err)
	}
	if err := store.UpsertVectors(ctx, ids, [][]float32{{1, 0, 0}}); err != nil {
		t.Fatalf("UpsertVectors: %v", err)
	}

	hits, err := store.SearchVectors(ctx, []float32{1, 0, 0}, 1)
	if err != nil {
		t.Fatalf("SearchVectors: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("hits = %d, want 2 (one chunk fanned over two paths)", len(hits))
	}
	if hits[0].Doc.Path != "/w/a-first.md" || hits[1].Doc.Path != "/w/z-last.md" {
		t.Errorf("fan-out order = [%q, %q], want path ascending on an mtime tie",
			hits[0].Doc.Path, hits[1].Doc.Path)
	}
}

// Deletion follows the source the moment the documents row goes: search's
// inner join stops serving the path immediately, without waiting for the
// orphan sweep to collect the content.
func TestDeleteByPathPrefixRemovesPathFromSearchImmediately(t *testing.T) {
	store, db := newTestStore(t)
	ctx := context.Background()

	if err := store.EnsureVecTable(ctx, vecSpec("test-model"), 3); err != nil {
		t.Fatalf("EnsureVecTable: %v", err)
	}
	ids := seedChunked(t, store, testDoc("hash-1", "/notes/keep.md"), testChunks("alpha"))
	if err := store.UpsertDocuments(ctx, []domain.Document{testDoc("hash-1", "/gone/x.md")}); err != nil {
		t.Fatalf("upsert second path: %v", err)
	}
	if err := store.UpsertVectors(ctx, ids, [][]float32{{1, 0, 0}}); err != nil {
		t.Fatalf("UpsertVectors: %v", err)
	}

	hits, err := store.SearchVectors(ctx, []float32{1, 0, 0}, 1)
	if err != nil {
		t.Fatalf("SearchVectors: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("hits before delete = %d, want 2 (both paths)", len(hits))
	}

	if _, err := store.DeleteByPathPrefix(ctx, "/gone"); err != nil {
		t.Fatalf("DeleteByPathPrefix: %v", err)
	}

	hits, err = store.SearchVectors(ctx, []float32{1, 0, 0}, 1)
	if err != nil {
		t.Fatalf("SearchVectors after delete: %v", err)
	}
	if len(hits) != 1 || hits[0].Doc.Path != "/notes/keep.md" {
		t.Errorf("hits after delete = %+v, want only /notes/keep.md", hits)
	}
	// The content row is still there — the delete is documents-only and the
	// sweep is what collects it later.
	if n := countRows(t, db, "SELECT count(*) FROM content WHERE content_hash = 'hash-1'"); n != 1 {
		t.Error("content row gone after a documents-only delete")
	}
}

func TestVecTableUsesCosineMetric(t *testing.T) {
	store, db := newTestStore(t)
	ctx := context.Background()

	if err := store.EnsureVecTable(ctx, vecSpec("test-model"), 4); err != nil {
		t.Fatalf("EnsureVecTable: %v", err)
	}

	var ddl string
	if err := db.Reader().QueryRow(
		"SELECT sql FROM sqlite_master WHERE name = 'vec_chunks_1'").Scan(&ddl); err != nil {
		t.Fatalf("read vec table DDL: %v", err)
	}
	if !strings.Contains(ddl, "distance_metric=cosine") {
		t.Errorf("vec table DDL %q missing distance_metric=cosine", ddl)
	}

	var descriptor string
	if err := db.Reader().QueryRow(
		"SELECT value FROM meta WHERE key = 'vec_table:vec_chunks_1'").Scan(&descriptor); err != nil {
		t.Fatalf("meta descriptor: %v", err)
	}
	if !strings.Contains(descriptor, "cosine") {
		t.Errorf("descriptor %q missing cosine metric", descriptor)
	}
}

func TestSearchRankingIsMagnitudeInvariant(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	if err := store.EnsureVecTable(ctx, vecSpec("test-model"), 3); err != nil {
		t.Fatalf("EnsureVecTable: %v", err)
	}
	ids := seedChunked(t, store, testDoc("hash-1", "/notes/a.md"),
		testChunks("north-big", "east"))

	// A non-normalized model: same direction as the query but 10× the
	// magnitude. Cosine must rank it nearest; L2 would rank "east" first
	// (|q−north-big| = 9 vs |q−east| = √2), which is exactly the silent
	// skew issue #40 guards against.
	vectors := [][]float32{
		{10, 0, 0}, // north-big
		{0, 1, 0},  // east
	}
	if err := store.UpsertVectors(ctx, ids, vectors); err != nil {
		t.Fatalf("UpsertVectors: %v", err)
	}

	hits, err := store.SearchVectors(ctx, []float32{1, 0, 0}, 2)
	if err != nil {
		t.Fatalf("SearchVectors: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("hits = %d, want 2", len(hits))
	}
	if hits[0].Chunk.Text != "north-big" {
		t.Errorf("nearest = %q, want north-big (magnitude skewed the ranking)", hits[0].Chunk.Text)
	}
}

func TestDescriptorMetricBackfillMintsNewGeneration(t *testing.T) {
	store, db := newTestStore(t)
	ctx := context.Background()

	if err := store.EnsureVecTable(ctx, vecSpec("test-model"), 3); err != nil {
		t.Fatal(err)
	}
	// Simulate a descriptor stored before the Metric field existed: that
	// table is physically L2 (created without distance_metric), so unlike
	// the layout/template backfills it must NOT match a cosine ensure —
	// treating it as cosine-compatible would silently mix metrics.
	if _, err := db.Writer().Exec(
		`UPDATE meta SET value = '{"model":"test-model","dims":3,"layout":"float32"}' WHERE key = 'vec_table:vec_chunks_1'`); err != nil {
		t.Fatal(err)
	}

	if err := store.EnsureVecTable(ctx, vecSpec("test-model"), 3); err != nil {
		t.Fatal(err)
	}
	var current string
	if err := db.Reader().QueryRow("SELECT value FROM meta WHERE key = 'vec_current'").Scan(&current); err != nil {
		t.Fatal(err)
	}
	if current != "vec_chunks_2" {
		t.Errorf("vec_current = %q — legacy L2 generation matched a cosine ensure", current)
	}
}

func TestUpsertVectorsDimsMismatch(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	if err := store.EnsureVecTable(ctx, vecSpec("test-model"), 3); err != nil {
		t.Fatalf("EnsureVecTable: %v", err)
	}
	ids := seedChunked(t, store, testDoc("hash-1", "/notes/a.md"), testChunks("alpha"))

	err := store.UpsertVectors(ctx, ids, [][]float32{{1, 0, 0, 0}}) // 4 dims into a 3-dim table
	if err == nil {
		t.Fatal("UpsertVectors accepted wrong dimensions, want error")
	}
	if !strings.Contains(err.Error(), "dim") {
		t.Errorf("error %q does not mention dimensions", err)
	}
}

func TestSearchWithoutVecTableFailsLoud(t *testing.T) {
	store, _ := newTestStore(t)

	_, err := store.SearchVectors(context.Background(), []float32{1, 0, 0}, 5)
	if err == nil {
		t.Fatal("SearchVectors with no vec table succeeded, want loud error")
	}
}

// Re-chunking replaces vectors: StoreChunks must drop the old chunks'
// vectors while the old chunk IDs still exist to find them by.
func TestStoreChunksReplacesVectors(t *testing.T) {
	store, db := newTestStore(t)
	ctx := context.Background()

	if err := store.EnsureVecTable(ctx, vecSpec("test-model"), 3); err != nil {
		t.Fatalf("EnsureVecTable: %v", err)
	}

	ids := seedChunked(t, store, testDoc("hash-1", "/notes/a.md"), testChunks("alpha", "beta"))
	if err := store.UpsertVectors(ctx, ids, [][]float32{{1, 0, 0}, {0, 1, 0}}); err != nil {
		t.Fatalf("vectors: %v", err)
	}

	if _, err := store.StoreChunks(ctx,
		domain.Content{Hash: "hash-1", State: domain.ContentStateChunked},
		testChunks("gamma")); err != nil {
		t.Fatalf("re-chunk: %v", err)
	}
	if n := countRows(t, db, "SELECT count(*) FROM vec_chunks_1"); n != 0 {
		t.Errorf("vectors after chunk replacement = %d, want 0 (stale vectors)", n)
	}
}

func TestUpsertVectorsRejectsStaleChunkIDs(t *testing.T) {
	store, db := newTestStore(t)
	ctx := context.Background()

	if err := store.EnsureVecTable(ctx, vecSpec("test-model"), 3); err != nil {
		t.Fatalf("EnsureVecTable: %v", err)
	}
	ids := seedChunked(t, store, testDoc("hash-1", "/notes/a.md"), testChunks("alpha"))
	// Content re-chunked mid-embed: old chunk IDs are dead.
	if _, err := store.StoreChunks(ctx,
		domain.Content{Hash: "hash-1", State: domain.ContentStateChunked},
		testChunks("beta")); err != nil {
		t.Fatalf("re-chunk: %v", err)
	}

	err := store.UpsertVectors(ctx, ids, [][]float32{{1, 0, 0}})
	if !errors.Is(err, domain.ErrContentSuperseded) {
		t.Fatalf("UpsertVectors(stale ids) = %v, want domain.ErrContentSuperseded", err)
	}

	var orphans int
	if err := db.Reader().QueryRow("SELECT count(*) FROM vec_chunks_1").Scan(&orphans); err != nil {
		t.Fatal(err)
	}
	if orphans != 0 {
		t.Errorf("%d orphan vectors committed", orphans)
	}
}

// The vector delete inside StoreChunks must reach EVERY generation, not just
// the current one — content re-chunked while model B is current must not
// resurface as orphan rowids when the user switches back to model A's table.
func TestStoreChunksCleansAllVecGenerations(t *testing.T) {
	store, db := newTestStore(t)
	ctx := context.Background()

	// Embed under model A (generation 1).
	if err := store.EnsureVecTable(ctx, vecSpec("model-a"), 3); err != nil {
		t.Fatal(err)
	}
	ids := seedChunked(t, store, testDoc("hash-1", "/notes/a.md"), testChunks("alpha"))
	if err := store.UpsertVectors(ctx, ids, [][]float32{{1, 0, 0}}); err != nil {
		t.Fatal(err)
	}

	// Switch current to model B (generation 2), then re-chunk the content.
	if err := store.EnsureVecTable(ctx, vecSpec("model-b"), 3); err != nil {
		t.Fatal(err)
	}
	if _, err := store.StoreChunks(ctx,
		domain.Content{Hash: "hash-1", State: domain.ContentStateChunked},
		testChunks("alpha v2")); err != nil {
		t.Fatal(err)
	}

	var stale int
	if err := db.Reader().QueryRow("SELECT count(*) FROM vec_chunks_1").Scan(&stale); err != nil {
		t.Fatal(err)
	}
	if stale != 0 {
		t.Errorf("%d orphan vectors left in non-current generation", stale)
	}
}

func TestDescriptorLayoutBackfill(t *testing.T) {
	store, db := newTestStore(t)
	ctx := context.Background()

	if err := store.EnsureVecTable(ctx, vecSpec("test-model"), 3); err != nil {
		t.Fatal(err)
	}
	// Simulate a descriptor stored before the Layout field existed (metric
	// kept current: its backfill deliberately mints a new generation and is
	// covered by TestDescriptorMetricBackfillMintsNewGeneration).
	if _, err := db.Writer().Exec(
		`UPDATE meta SET value = '{"model":"test-model","dims":3,"metric":"cosine"}' WHERE key = 'vec_table:vec_chunks_1'`); err != nil {
		t.Fatal(err)
	}

	// Re-ensure must match the old descriptor (normalized), not mint a
	// fresh empty generation.
	if err := store.EnsureVecTable(ctx, vecSpec("test-model"), 3); err != nil {
		t.Fatal(err)
	}
	var current string
	if err := db.Reader().QueryRow("SELECT value FROM meta WHERE key = 'vec_current'").Scan(&current); err != nil {
		t.Fatal(err)
	}
	if current != "vec_chunks_1" {
		t.Errorf("vec_current = %q — legacy descriptor mismatched, new empty generation minted", current)
	}
}

func TestEnsureVecTableTemplateIdentity(t *testing.T) {
	store, db := newTestStore(t)
	ctx := context.Background()

	spec := domain.EmbeddingSpec{
		Model:           "test-model",
		QueryTemplate:   "query: {q}",
		PassageTemplate: "title: {t} | text: {d}",
		CeilingTokens:   2048,
	}
	if err := store.EnsureVecTable(ctx, spec, 3); err != nil {
		t.Fatalf("EnsureVecTable: %v", err)
	}

	// Descriptor round-trips the full spec.
	var descriptor string
	if err := db.Reader().QueryRow(
		"SELECT value FROM meta WHERE key = 'vec_table:vec_chunks_1'").Scan(&descriptor); err != nil {
		t.Fatalf("meta descriptor: %v", err)
	}
	for _, want := range []string{"query: {q}", "title: {t} | text: {d}", "2048"} {
		if !strings.Contains(descriptor, want) {
			t.Errorf("descriptor %q missing %q", descriptor, want)
		}
	}

	// Same spec again: same generation.
	if err := store.EnsureVecTable(ctx, spec, 3); err != nil {
		t.Fatal(err)
	}
	var current string
	if err := db.Reader().QueryRow("SELECT value FROM meta WHERE key = 'vec_current'").Scan(&current); err != nil {
		t.Fatal(err)
	}
	if current != "vec_chunks_1" {
		t.Errorf("vec_current = %q after identical spec, want vec_chunks_1", current)
	}

	// Template change: new generation — differently-prefixed vectors are
	// incompatible even under the same model.
	changed := spec
	changed.QueryTemplate = "search_query: {q}"
	if err := store.EnsureVecTable(ctx, changed, 3); err != nil {
		t.Fatal(err)
	}
	if err := db.Reader().QueryRow("SELECT value FROM meta WHERE key = 'vec_current'").Scan(&current); err != nil {
		t.Fatal(err)
	}
	if current != "vec_chunks_2" {
		t.Errorf("vec_current = %q after template change, want vec_chunks_2", current)
	}
}

func TestEnsureVecTableCeilingChangeKeepsGeneration(t *testing.T) {
	store, db := newTestStore(t)
	ctx := context.Background()

	spec := domain.EmbeddingSpec{Model: "test-model", QueryTemplate: "q: {q}", CeilingTokens: 2048}
	if err := store.EnsureVecTable(ctx, spec, 3); err != nil {
		t.Fatal(err)
	}

	// Ceiling shapes chunk boundaries, not vectors: changing it must not
	// mint a new generation (which would empty search until re-embed).
	spec.CeilingTokens = 4096
	if err := store.EnsureVecTable(ctx, spec, 3); err != nil {
		t.Fatal(err)
	}
	var current string
	if err := db.Reader().QueryRow("SELECT value FROM meta WHERE key = 'vec_current'").Scan(&current); err != nil {
		t.Fatal(err)
	}
	if current != "vec_chunks_1" {
		t.Errorf("vec_current = %q after ceiling-only change, want vec_chunks_1", current)
	}

	// The recorded descriptor tracks the new ceiling.
	var descriptor string
	if err := db.Reader().QueryRow(
		"SELECT value FROM meta WHERE key = 'vec_table:vec_chunks_1'").Scan(&descriptor); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(descriptor, "4096") {
		t.Errorf("descriptor %q not refreshed with new ceiling", descriptor)
	}
}

func TestDescriptorTemplateBackfill(t *testing.T) {
	store, db := newTestStore(t)
	ctx := context.Background()

	if err := store.EnsureVecTable(ctx, vecSpec("test-model"), 3); err != nil {
		t.Fatal(err)
	}
	// Simulate a descriptor stored before the template fields existed
	// (metric kept current — see TestDescriptorLayoutBackfill).
	if _, err := db.Writer().Exec(
		`UPDATE meta SET value = '{"model":"test-model","dims":3,"layout":"float32","metric":"cosine"}'
		 WHERE key = 'vec_table:vec_chunks_1'`); err != nil {
		t.Fatal(err)
	}

	// A raw spec (no templates, no ceiling) must still match — not mint a
	// fresh empty generation.
	if err := store.EnsureVecTable(ctx, vecSpec("test-model"), 3); err != nil {
		t.Fatal(err)
	}
	var current string
	if err := db.Reader().QueryRow("SELECT value FROM meta WHERE key = 'vec_current'").Scan(&current); err != nil {
		t.Fatal(err)
	}
	if current != "vec_chunks_1" {
		t.Errorf("vec_current = %q — legacy descriptor mismatched raw spec, new generation minted", current)
	}
}

func TestCurrentVecSpec(t *testing.T) {
	store, _ := newTestStore(t)
	ctx := context.Background()

	// Fresh DB: nothing embedded.
	if _, _, err := store.CurrentVecSpec(ctx); !errors.Is(err, ErrNoVecTable) {
		t.Fatalf("CurrentVecSpec on fresh DB = %v, want ErrNoVecTable", err)
	}

	spec := domain.EmbeddingSpec{
		Model:           "test-model",
		QueryTemplate:   "query: {q}",
		PassageTemplate: "passage: {d}",
		CeilingTokens:   2048,
	}
	if err := store.EnsureVecTable(ctx, spec, 3); err != nil {
		t.Fatalf("EnsureVecTable: %v", err)
	}

	got, dims, err := store.CurrentVecSpec(ctx)
	if err != nil {
		t.Fatalf("CurrentVecSpec: %v", err)
	}
	// Ceiling is excluded from vector-space identity.
	want := domain.EmbeddingSpec{
		Model:           "test-model",
		QueryTemplate:   "query: {q}",
		PassageTemplate: "passage: {d}",
	}
	if got != want {
		t.Errorf("CurrentVecSpec = %+v, want %+v", got, want)
	}
	if dims != 3 {
		t.Errorf("dims = %d, want 3", dims)
	}
}

func TestEnsureVecTableRejectsInvalidInputs(t *testing.T) {
	store, db := newTestStore(t)
	ctx := context.Background()

	if err := store.EnsureVecTable(ctx, vecSpec(""), 4); err == nil {
		t.Error("empty model accepted, want error")
	}
	for _, dims := range []int{0, -1} {
		if err := store.EnsureVecTable(ctx, vecSpec("test-model"), dims); err == nil {
			t.Errorf("dims=%d accepted, want error", dims)
		}
	}
	// Nothing half-created.
	var n int
	if err := db.Reader().QueryRow("SELECT count(*) FROM meta").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("meta has %d rows after rejected calls, want 0", n)
	}
}

func TestSearchVectorsDoesNotMistakeDamageForACutover(t *testing.T) {
	// A retired vector generation is transient — "try again" is right advice.
	// A missing chunks table is permanent damage, and the KNN statement joins
	// it, so a broad "no such table" match would tell the user to retry
	// forever.
	store, db := newTestStore(t)
	ctx := context.Background()

	if err := store.EnsureVecTable(ctx, domain.EmbeddingSpec{Model: "m"}, 3); err != nil {
		t.Fatalf("EnsureVecTable: %v", err)
	}
	if _, err := db.Writer().ExecContext(ctx, "DROP TABLE chunks"); err != nil {
		t.Fatalf("drop chunks: %v", err)
	}

	_, err := store.SearchVectors(ctx, []float32{1, 0, 0}, 10)
	if err == nil {
		t.Fatal("SearchVectors against a damaged database = nil error")
	}
	if errors.Is(err, ErrNoVecTable) {
		t.Errorf("a missing chunks table was reported as a retired generation: %v", err)
	}
}
