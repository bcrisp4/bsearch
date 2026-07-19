package sqlite

import (
	"context"
	"strings"
	"testing"
)

func TestEnsureVecTableCreatesGenerationAndMeta(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)
	ctx := context.Background()

	if err := store.EnsureVecTable(ctx, "test-model", 4); err != nil {
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
	if err := store.EnsureVecTable(ctx, "test-model", 4); err != nil {
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
	if err := store.EnsureVecTable(ctx, "other-model", 8); err != nil {
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
	db := openTestDB(t)
	store := NewStore(db)
	ctx := context.Background()

	if err := store.EnsureVecTable(ctx, "test-model", 3); err != nil {
		t.Fatalf("EnsureVecTable: %v", err)
	}
	ids, err := store.UpsertDocument(ctx, testDoc("d_1", "/notes/a.md"),
		testChunks("d_1", "north", "east", "up"))
	if err != nil {
		t.Fatalf("UpsertDocument: %v", err)
	}

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
	if hits[0].Chunk.HeadingPath == "" {
		t.Error("hit chunk missing heading path")
	}
}

func TestUpsertVectorsDimsMismatch(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)
	ctx := context.Background()

	if err := store.EnsureVecTable(ctx, "test-model", 3); err != nil {
		t.Fatalf("EnsureVecTable: %v", err)
	}
	ids, err := store.UpsertDocument(ctx, testDoc("d_1", "/notes/a.md"), testChunks("d_1", "alpha"))
	if err != nil {
		t.Fatalf("UpsertDocument: %v", err)
	}

	err = store.UpsertVectors(ctx, ids, [][]float32{{1, 0, 0, 0}}) // 4 dims into a 3-dim table
	if err == nil {
		t.Fatal("UpsertVectors accepted wrong dimensions, want error")
	}
	if !strings.Contains(err.Error(), "dim") {
		t.Errorf("error %q does not mention dimensions", err)
	}
}

func TestSearchWithoutVecTableFailsLoud(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)

	_, err := store.SearchVectors(context.Background(), []float32{1, 0, 0}, 5)
	if err == nil {
		t.Fatal("SearchVectors with no vec table succeeded, want loud error")
	}
}

func TestDeleteAndReplaceRemoveVectors(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)
	ctx := context.Background()

	if err := store.EnsureVecTable(ctx, "test-model", 3); err != nil {
		t.Fatalf("EnsureVecTable: %v", err)
	}

	vecCount := func() int {
		t.Helper()
		var n int
		if err := db.Reader().QueryRow("SELECT count(*) FROM vec_chunks_1").Scan(&n); err != nil {
			t.Fatalf("count vectors: %v", err)
		}
		return n
	}

	// Replacement path: re-upserting a document must drop old chunk vectors.
	ids, err := store.UpsertDocument(ctx, testDoc("d_1", "/notes/a.md"), testChunks("d_1", "alpha", "beta"))
	if err != nil {
		t.Fatalf("upsert d_1: %v", err)
	}
	if err := store.UpsertVectors(ctx, ids, [][]float32{{1, 0, 0}, {0, 1, 0}}); err != nil {
		t.Fatalf("vectors d_1: %v", err)
	}
	if _, err := store.UpsertDocument(ctx, testDoc("d_1", "/notes/a.md"), testChunks("d_1", "gamma")); err != nil {
		t.Fatalf("re-upsert d_1: %v", err)
	}
	if n := vecCount(); n != 0 {
		t.Errorf("vectors after chunk replacement = %d, want 0 (stale vectors)", n)
	}

	// Delete path.
	ids, err = store.UpsertDocument(ctx, testDoc("d_2", "/notes/b.md"), testChunks("d_2", "delta"))
	if err != nil {
		t.Fatalf("upsert d_2: %v", err)
	}
	if err := store.UpsertVectors(ctx, ids, [][]float32{{0, 0, 1}}); err != nil {
		t.Fatalf("vectors d_2: %v", err)
	}
	if err := store.DeleteDocument(ctx, "d_2"); err != nil {
		t.Fatalf("DeleteDocument: %v", err)
	}
	if n := vecCount(); n != 0 {
		t.Errorf("vectors after document delete = %d, want 0", n)
	}
}

func TestUpsertVectorsRejectsStaleChunkIDs(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)
	ctx := context.Background()

	if err := store.EnsureVecTable(ctx, "test-model", 3); err != nil {
		t.Fatalf("EnsureVecTable: %v", err)
	}
	ids, err := store.UpsertDocument(ctx, testDoc("d_1", "/notes/a.md"), testChunks("d_1", "alpha"))
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// Document re-indexed mid-embed: old chunk IDs are dead.
	if _, err := store.UpsertDocument(ctx, testDoc("d_1", "/notes/a.md"), testChunks("d_1", "beta")); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}

	err = store.UpsertVectors(ctx, ids, [][]float32{{1, 0, 0}})
	if err == nil {
		t.Fatal("UpsertVectors accepted stale chunk IDs — permanent orphan vectors")
	}

	var orphans int
	if err := db.Reader().QueryRow("SELECT count(*) FROM vec_chunks_1").Scan(&orphans); err != nil {
		t.Fatal(err)
	}
	if orphans != 0 {
		t.Errorf("%d orphan vectors committed", orphans)
	}
}

func TestDeleteCleansAllVecGenerations(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)
	ctx := context.Background()

	// Embed under model A (generation 1).
	if err := store.EnsureVecTable(ctx, "model-a", 3); err != nil {
		t.Fatal(err)
	}
	ids, err := store.UpsertDocument(ctx, testDoc("d_1", "/notes/a.md"), testChunks("d_1", "alpha"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertVectors(ctx, ids, [][]float32{{1, 0, 0}}); err != nil {
		t.Fatal(err)
	}

	// Switch current to model B (generation 2), then delete the doc.
	if err := store.EnsureVecTable(ctx, "model-b", 3); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteDocument(ctx, "d_1"); err != nil {
		t.Fatal(err)
	}

	// The non-current generation must be clean too: switching back to
	// model A must not resurface orphan rowids.
	var stale int
	if err := db.Reader().QueryRow("SELECT count(*) FROM vec_chunks_1").Scan(&stale); err != nil {
		t.Fatal(err)
	}
	if stale != 0 {
		t.Errorf("%d orphan vectors left in non-current generation", stale)
	}
}

func TestDescriptorLayoutBackfill(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)
	ctx := context.Background()

	if err := store.EnsureVecTable(ctx, "test-model", 3); err != nil {
		t.Fatal(err)
	}
	// Simulate a descriptor stored before the Layout field existed.
	if _, err := db.Writer().Exec(
		`UPDATE meta SET value = '{"model":"test-model","dims":3}' WHERE key = 'vec_table:vec_chunks_1'`); err != nil {
		t.Fatal(err)
	}

	// Re-ensure must match the old descriptor (normalized), not mint a
	// fresh empty generation.
	if err := store.EnsureVecTable(ctx, "test-model", 3); err != nil {
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

func TestEnsureVecTableRejectsInvalidInputs(t *testing.T) {
	db := openTestDB(t)
	store := NewStore(db)
	ctx := context.Background()

	if err := store.EnsureVecTable(ctx, "", 4); err == nil {
		t.Error("empty model accepted, want error")
	}
	for _, dims := range []int{0, -1} {
		if err := store.EnsureVecTable(ctx, "test-model", dims); err == nil {
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
