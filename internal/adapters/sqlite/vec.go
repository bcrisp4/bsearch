package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	sqlitevec "github.com/asg017/sqlite-vec-go-bindings/cgo"

	"github.com/bcrisp4/bsearch/internal/domain"
)

var _ domain.VectorStore = (*Store)(nil)

// ErrNoVecTable means nothing has been embedded yet (or the configured model
// has no vector table). Callers surface it, never treat it as empty results.
// The sentinel itself belongs to the port (domain) so the query path can
// recognise it without importing this adapter; this alias keeps the storage
// package's own call sites reading naturally.
var ErrNoVecTable = domain.ErrNoVecTable

// vecDescriptor is the identity of one vector-table generation, stored as
// JSON in meta under vec_table:<name>. The table name itself is just a
// generation handle (vec_chunks_<N>) — identity lives here so future
// attributes (quantization layout) extend without a renaming scheme.
//
// Adding a field? Give it a backfill default in normalize() — old stored
// JSON must keep matching, or every upgrade silently mints a fresh empty
// generation and search goes empty until re-embed.
type vecDescriptor struct {
	Model  string `json:"model"`
	Dims   int    `json:"dims"`
	Layout string `json:"layout"` // "float32" until quantization lands
	// Metric is the vec0 distance metric baked into the table's DDL —
	// "cosine" for every generation created since issue #40; "l2" is the
	// backfill for tables created before the field existed (vec0's
	// default). Part of identity: distances from different metrics are
	// incomparable, so a metric change must mint a new generation.
	Metric string `json:"metric"`
	// Prefix templates are part of the identity: vectors embedded with
	// different prefixes are as incompatible as a different model's
	// (DESIGN.md: Embeddings/LLM). The input ceiling is recorded for
	// auditability but excluded from identity (see identity()): it shapes
	// chunk boundaries, not the vector a given text maps to, so a ceiling
	// change is a chunker-level partial rebuild, never a generation swap.
	// Empty/zero = raw/unlimited — also the backfill default for
	// descriptors stored before these fields existed.
	QueryTemplate   string `json:"query_template,omitempty"`
	PassageTemplate string `json:"passage_template,omitempty"`
	CeilingTokens   int    `json:"ceiling_tokens,omitempty"`
}

// identity strips fields that don't affect vector-space compatibility;
// two generations with equal identities hold interchangeable vectors.
func (d vecDescriptor) identity() vecDescriptor {
	d.CeilingTokens = 0
	return d
}

// normalize fills defaults for fields added after a descriptor was stored.
func (d vecDescriptor) normalize() vecDescriptor {
	if d.Layout == "" {
		d.Layout = "float32"
	}
	if d.Metric == "" {
		// Pre-#40 tables were created without distance_metric, i.e. L2.
		// Deliberately NOT "cosine": that would match new ensures and
		// silently keep serving L2 rankings from the old table.
		d.Metric = "l2"
	}
	return d
}

const (
	metaVecCurrent = "vec_current"
	metaVecPrefix  = "vec_table:"
)

// vecTableName guards meta-sourced table names before SQL interpolation
// (identifiers can't be bound as parameters).
var vecTableName = regexp.MustCompile(`^vec_chunks_([0-9]+)$`)

// listVecTables returns every generation's name→descriptor.
func listVecTables(ctx context.Context, q queryer) (map[string]vecDescriptor, error) {
	rows, err := q.QueryContext(ctx,
		"SELECT key, value FROM meta WHERE key LIKE ?", metaVecPrefix+"%")
	if err != nil {
		return nil, fmt.Errorf("scan vec descriptors: %w", err)
	}
	defer rows.Close()

	tables := make(map[string]vecDescriptor)
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, err
		}
		name := key[len(metaVecPrefix):]
		if !vecTableName.MatchString(name) {
			return nil, fmt.Errorf("corrupt vec table key %q", key)
		}
		var desc vecDescriptor
		if err := json.Unmarshal([]byte(value), &desc); err != nil {
			return nil, fmt.Errorf("corrupt descriptor %s: %w", key, err)
		}
		tables[name] = desc.normalize()
	}
	return tables, rows.Err()
}

// EnsureVecTable makes a vector table for spec+dims the current one,
// creating a new generation if none matches. Model, dims, and templates
// participate in identity — a template change mints a new generation,
// same as a model change. The ceiling does not (recorded only): its
// vectors stay valid, and re-chunking under a new ceiling is stage
// versioning's partial rebuild, not a cutover that empties search.
//
// M1 semantics: the switch is immediate — a model change points search at
// the (initially empty) new generation until re-embedding fills it. Staged
// blue/green cutover (old table serves while the new one fills) is issue
// #24; DESIGN.md (Pipeline metadata and model migration) records both.
func (s *Store) EnsureVecTable(ctx context.Context, spec domain.EmbeddingSpec, dims int) error {
	// Validate upfront: an empty model would poison descriptor identity in
	// meta; bad dims would only surface as an opaque vec0 SQL error.
	// 8192 is vec0's dimension ceiling.
	if spec.Model == "" {
		return errors.New("ensure vec table: model must not be empty")
	}
	if dims < 1 || dims > 8192 {
		return fmt.Errorf("ensure vec table: dims %d out of range [1, 8192]", dims)
	}
	want := vecDescriptor{
		Model:           spec.Model,
		Dims:            dims,
		Layout:          "float32",
		Metric:          domain.VectorMetric,
		QueryTemplate:   spec.QueryTemplate,
		PassageTemplate: spec.PassageTemplate,
		CeilingTokens:   spec.CeilingTokens,
	}

	return s.withTx(ctx, func(tx *sql.Tx) error {
		tables, err := listVecTables(ctx, tx)
		if err != nil {
			return err
		}

		// An existing generation with this identity? Point current at it.
		// Otherwise mint the next generation number.
		name, maxGen := "", 0
		for existing, desc := range tables {
			gen, _ := strconv.Atoi(vecTableName.FindStringSubmatch(existing)[1])
			maxGen = max(maxGen, gen)
			if desc.identity() == want.identity() {
				name = existing
				// Refresh non-identity fields (ceiling) so the recorded
				// descriptor tracks current config.
				if desc != want {
					descJSON, err := json.Marshal(want)
					if err != nil {
						return err
					}
					if err := setMeta(ctx, tx, metaVecPrefix+name, string(descJSON)); err != nil {
						return err
					}
				}
			}
		}

		if name == "" {
			name = fmt.Sprintf("vec_chunks_%d", maxGen+1)
			// Cosine, not vec0's default L2: magnitude-invariant, so a
			// non-normalized embedding model can't silently skew rankings
			// (issue #40).
			if _, err := tx.ExecContext(ctx, fmt.Sprintf(
				"CREATE VIRTUAL TABLE %s USING vec0(embedding float[%d] distance_metric=%s)",
				name, dims, domain.VectorMetric)); err != nil {
				return fmt.Errorf("create vec table %s: %w", name, err)
			}
			descJSON, err := json.Marshal(want)
			if err != nil {
				return err
			}
			if err := setMeta(ctx, tx, metaVecPrefix+name, string(descJSON)); err != nil {
				return err
			}
		}
		return setMeta(ctx, tx, metaVecCurrent, name)
	})
}

// currentVecTable resolves the current generation's name and descriptor.
func currentVecTable(ctx context.Context, q queryer) (string, vecDescriptor, error) {
	var name, descJSON string
	// Self-join fetches name + descriptor in one round trip.
	err := q.QueryRowContext(ctx, `
		SELECT cur.value, tbl.value FROM meta cur
		JOIN meta tbl ON tbl.key = ? || cur.value
		WHERE cur.key = ?`, metaVecPrefix, metaVecCurrent).Scan(&name, &descJSON)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// Distinguish "nothing embedded" from a dangling vec_current.
		if _, ok, metaErr := getMeta(ctx, q, metaVecCurrent); metaErr != nil {
			return "", vecDescriptor{}, metaErr
		} else if ok {
			return "", vecDescriptor{}, errors.New("vec_current points at a table with no descriptor")
		}
		return "", vecDescriptor{}, ErrNoVecTable
	case err != nil:
		return "", vecDescriptor{}, fmt.Errorf("read vec_current: %w", err)
	}
	if !vecTableName.MatchString(name) {
		return "", vecDescriptor{}, fmt.Errorf("corrupt vec_current %q", name)
	}
	var desc vecDescriptor
	if err := json.Unmarshal([]byte(descJSON), &desc); err != nil {
		return "", vecDescriptor{}, fmt.Errorf("corrupt descriptor for %s: %w", name, err)
	}
	return name, desc.normalize(), nil
}

// CurrentVecSpec reports the embedding identity (model + prefix templates)
// and dims of the current vector table, for search-time compatibility checks:
// dims alone can't catch a model swapped to another of equal dimensions, which
// would silently search the wrong vector space. The ceiling is excluded — it
// is not part of vector-space identity (see vecDescriptor). Layout is part of
// generation identity but has no EmbeddingSpec field to carry it; when
// quantization adds a second layout, this method and its callers' checks must
// widen with it. Returns ErrNoVecTable when nothing has been embedded.
func (s *Store) CurrentVecSpec(ctx context.Context) (domain.EmbeddingSpec, int, error) {
	_, desc, err := currentVecTable(ctx, s.db.Reader())
	if err != nil {
		return domain.EmbeddingSpec{}, 0, err
	}
	return domain.EmbeddingSpec{
		Model:           desc.Model,
		QueryTemplate:   desc.QueryTemplate,
		PassageTemplate: desc.PassageTemplate,
	}, desc.Dims, nil
}

// UpsertVectors stores one vector per chunk storage ID, replacing existing
// rows. Callers batch: one call per embedding batch, one short transaction.
//
// Every chunk ID must still exist in chunks: vec0 has no foreign keys, and a
// vector committed for a chunk that was replaced mid-embed would be a
// permanent orphan eating KNN k-slots (AUTOINCREMENT never reuses the ID, so
// no later write can ever clean it). Stale IDs are a loud error, wrapped as
// domain.ErrContentSuperseded so the caller can tell them from a broken store.
//
// The caller stands down rather than retrying: pipeline.movedOn abandons the
// pass and reports OutcomeSuperseded, and the scheduler puts the content back
// on the retry schedule. Nothing here is re-read and nothing is retried in
// place. Chunks key on the content hash, never on a document or a path
// (ADR 0015).
func (s *Store) UpsertVectors(ctx context.Context, chunkIDs []int64, vectors [][]float32) error {
	if len(chunkIDs) != len(vectors) {
		return fmt.Errorf("chunk ids (%d) and vectors (%d) mismatch", len(chunkIDs), len(vectors))
	}
	if len(chunkIDs) == 0 {
		return nil
	}

	return s.withTx(ctx, func(tx *sql.Tx) error {
		table, desc, err := currentVecTable(ctx, tx)
		if err != nil {
			return err
		}

		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(chunkIDs)), ",")
		args := make([]any, len(chunkIDs))
		for i, id := range chunkIDs {
			args[i] = id
		}
		var live int
		if err := tx.QueryRowContext(ctx,
			"SELECT count(*) FROM chunks WHERE id IN ("+placeholders+")", args...).Scan(&live); err != nil {
			return fmt.Errorf("check chunk ids: %w", err)
		}
		if live != len(chunkIDs) {
			// Re-chunked or swept under us. Wrapped so the caller can tell
			// this apart from a broken store and stand down instead of
			// failing the drain (domain.ErrContentSuperseded).
			return fmt.Errorf("%d of %d chunk ids no longer exist: %w",
				len(chunkIDs)-live, len(chunkIDs), domain.ErrContentSuperseded)
		}

		// vec0 has no upsert; delete-then-insert is the documented pattern.
		del, err := tx.PrepareContext(ctx, fmt.Sprintf("DELETE FROM %s WHERE rowid = ?", table))
		if err != nil {
			return err
		}
		defer del.Close()
		ins, err := tx.PrepareContext(ctx,
			fmt.Sprintf("INSERT INTO %s (rowid, embedding) VALUES (?, ?)", table))
		if err != nil {
			return err
		}
		defer ins.Close()

		for i, vec := range vectors {
			if len(vec) != desc.Dims {
				return fmt.Errorf("vector %d has %d dims, table %s wants %d (model %s changed under us?)",
					i, len(vec), table, desc.Dims, desc.Model)
			}
			blob, err := sqlitevec.SerializeFloat32(vec)
			if err != nil {
				return fmt.Errorf("serialize vector %d: %w", i, err)
			}
			if _, err := del.ExecContext(ctx, chunkIDs[i]); err != nil {
				return fmt.Errorf("clear vector rowid %d: %w", chunkIDs[i], err)
			}
			if _, err := ins.ExecContext(ctx, chunkIDs[i], blob); err != nil {
				return fmt.Errorf("insert vector rowid %d: %w", chunkIDs[i], err)
			}
		}
		return nil
	})
}

// SearchVectors returns the limit nearest chunks by ascending distance,
// fanned out per referencing path: a chunk whose content lives at N paths
// yields N consecutive Hits, newest mtime first then path ascending — the
// ordering contract CollapseBestPerContent's primary-path pick depends on.
// limit counts KNN chunks (the vec subquery), not fanned-out rows.
//
// Two queries in one read transaction, fanned out in Go, rather than one
// three-way join: joining documents into the KNN statement re-materialised
// every chunk's text once per referencing path and pushed the lot through a
// temp B-tree sorter — measured at 118 ms / 128 MB for k=80 against a
// 1000-path content, on the p95 < 500 ms hot path. Here the chunk columns
// are read once per chunk; the fan-out duplicates string headers, so a
// heavily-copied file costs pointers, not payload. The transaction is what
// keeps the two reads one snapshot — a path deleted between them can
// neither appear with no chunk nor vice versa.
//
// The fan-out keeps inner-join semantics: content no document references
// (deleted, sweep pending) contributes nothing, so a deletion takes effect
// the moment its documents row goes — and unread rows (NULL content_hash)
// never match the hash lookup.
//
// Path-prefix scoping, when it lands (#84), attaches as a predicate on
// d.path in the documents lookup — with two traps recorded here because
// this is the line an implementer will read. The predicate must be
// DeleteByPathPrefix's exact shape, parenthesised and with its binds:
// `(d.path = ? OR (d.path > ? AND d.path < ?))` bound as
// (dir, dir+"/", dir+"0") after the TrimRight/empty-prefix guards — the
// second bind is dir+"/", NOT dir, or scoping to /Documents/tax silently
// admits /Documents/tax.old; and unparenthesised, the OR detaches the
// hash condition it sits beside. Separately, k is spent in the vec0
// subquery before any scope filter runs, so a tight scope starves results
// at default k — #84 records the escalation/filtered-query options.
func (s *Store) SearchVectors(ctx context.Context, query []float32, limit int) ([]domain.Hit, error) {
	tx, err := s.db.Reader().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("search vectors: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // read-only

	table, desc, err := currentVecTable(ctx, tx)
	if err != nil {
		return nil, err
	}
	if len(query) != desc.Dims {
		return nil, fmt.Errorf("query has %d dims, table %s wants %d (model %s)",
			len(query), table, desc.Dims, desc.Model)
	}
	blob, err := sqlitevec.SerializeFloat32(query)
	if err != nil {
		return nil, fmt.Errorf("serialize query: %w", err)
	}

	chunks, err := knnChunks(ctx, tx, table, blob, limit)
	if err != nil {
		return nil, err
	}
	if len(chunks) == 0 {
		return nil, nil
	}
	docsByHash, err := documentsByHash(ctx, tx, chunkHashes(chunks))
	if err != nil {
		return nil, err
	}

	hits := make([]domain.Hit, 0, limit)
	for _, ch := range chunks {
		for _, doc := range docsByHash[ch.Doc.ContentHash] {
			h := ch
			h.Doc = doc
			hits = append(hits, h)
		}
	}
	return hits, nil
}

// knnChunks runs the KNN subquery joined to chunks only — no documents, so
// each of the k chunk rows is read exactly once, however many paths hold it.
// Rows come back (distance, chunk id) ascending; Doc carries only the
// content hash until documentsByHash fills in the referencing paths.
func knnChunks(ctx context.Context, tx *sql.Tx, table string, blob []byte, k int) ([]domain.Hit, error) {
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(`
		SELECT c.content_hash, c.ordinal, c.text, c.heading_path, c.byte_start, c.byte_end, v.distance
		FROM (SELECT rowid, distance FROM %s WHERE embedding MATCH ? AND k = ?) v
		JOIN chunks c ON c.id = v.rowid
		ORDER BY v.distance, c.id`, table), blob, k)
	if err != nil {
		// An indexer in another process can retire this generation between
		// the descriptor lookup and this query — the table is simply gone.
		// That's "nothing to search right now", not a storage fault, so it
		// reports as ErrNoVecTable. SQLite gives no distinct result code for
		// a missing table, hence the message match; it names the vector
		// table specifically, because the statement also joins chunks and a
		// missing chunks table is permanent damage, not a generation cutover
		// worth retrying.
		if strings.Contains(err.Error(), "no such table: "+table) {
			return nil, fmt.Errorf("vector table %s was retired mid-query: %w", table, ErrNoVecTable)
		}
		return nil, fmt.Errorf("knn query on %s: %w", table, err)
	}
	defer rows.Close()

	var hits []domain.Hit
	for rows.Next() {
		var h domain.Hit
		if err := rows.Scan(
			&h.Doc.ContentHash, &h.Chunk.Ordinal, &h.Chunk.Text,
			&h.Chunk.HeadingPath, &h.Chunk.ByteStart, &h.Chunk.ByteEnd,
			&h.Distance); err != nil {
			return nil, err
		}
		hits = append(hits, h)
	}
	return hits, rows.Err()
}

// chunkHashes collects the distinct content hashes of a chunk result.
func chunkHashes(chunks []domain.Hit) []string {
	seen := make(map[string]struct{}, len(chunks))
	hashes := make([]string, 0, len(chunks))
	for _, ch := range chunks {
		if _, dup := seen[ch.Doc.ContentHash]; dup {
			continue
		}
		seen[ch.Doc.ContentHash] = struct{}{}
		hashes = append(hashes, ch.Doc.ContentHash)
	}
	return hashes
}

// documentsByHash fetches every documents row referencing the hashes, each
// hash's rows sorted newest mtime first, tie broken by path ascending — the
// primary-path rule (GetWork's, and the wire contract's). The sort is here,
// in Go, because the fan-out loop emits rows in slice order: dropping it
// would misassign primaries silently.
func documentsByHash(ctx context.Context, tx *sql.Tx, hashes []string) (map[string][]domain.Document, error) {
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(hashes)), ",")
	args := make([]any, len(hashes))
	for i, h := range hashes {
		args[i] = h
	}
	//nolint:gosec // G202: the spliced text is generated "?" placeholders; the hashes are bound
	rows, err := tx.QueryContext(ctx,
		"SELECT content_hash, path, size, mtime FROM documents WHERE content_hash IN ("+placeholders+")",
		args...)
	if err != nil {
		return nil, fmt.Errorf("resolve hit paths: %w", err)
	}
	defer rows.Close()

	docs := make(map[string][]domain.Document, len(hashes))
	for rows.Next() {
		var (
			doc     domain.Document
			mtimeNS int64
		)
		if err := rows.Scan(&doc.ContentHash, &doc.Path, &doc.Size, &mtimeNS); err != nil {
			return nil, err
		}
		doc.MTime = time.Unix(0, mtimeNS)
		docs[doc.ContentHash] = append(docs[doc.ContentHash], doc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("resolve hit paths: %w", err)
	}
	for _, list := range docs {
		sort.Slice(list, func(i, j int) bool {
			if !list[i].MTime.Equal(list[j].MTime) {
				return list[i].MTime.After(list[j].MTime)
			}
			return list[i].Path < list[j].Path
		})
	}
	return docs, nil
}
