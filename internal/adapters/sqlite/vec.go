package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	sqlitevec "github.com/asg017/sqlite-vec-go-bindings/cgo"

	"github.com/bcrisp4/bsearch/internal/domain"
)

var _ domain.VectorStore = (*Store)(nil)

// ErrNoVecTable means nothing has been embedded yet (or the configured model
// has no vector table). Callers surface it, never treat it as empty results.
var ErrNoVecTable = errors.New("no current vector table (nothing embedded yet?)")

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
			if _, err := tx.ExecContext(ctx, fmt.Sprintf(
				"CREATE VIRTUAL TABLE %s USING vec0(embedding float[%d])", name, dims)); err != nil {
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

// UpsertVectors stores one vector per chunk storage ID, replacing existing
// rows. Callers batch: one call per embedding batch, one short transaction.
//
// Every chunk ID must still exist in chunks: vec0 has no foreign keys, and a
// vector committed for a chunk that was replaced mid-embed would be a
// permanent orphan eating KNN k-slots (AUTOINCREMENT never reuses the ID, so
// no later write can ever clean it). Stale IDs are a loud error — the caller
// re-reads the document's current chunks and retries.
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
			return fmt.Errorf("%d of %d chunk ids no longer exist (document re-indexed mid-embed?)",
				len(chunkIDs)-live, len(chunkIDs))
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

// SearchVectors returns the limit nearest chunks by ascending distance.
func (s *Store) SearchVectors(ctx context.Context, query []float32, limit int) ([]domain.Hit, error) {
	table, desc, err := currentVecTable(ctx, s.db.Reader())
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

	rows, err := s.db.Reader().QueryContext(ctx, fmt.Sprintf(`
		SELECT c.id, c.doc_id, c.ordinal, c.text, c.heading_path, c.byte_start, c.byte_end,
		       %s, v.distance
		FROM (SELECT rowid, distance FROM %s WHERE embedding MATCH ? AND k = ?) v
		JOIN chunks c ON c.id = v.rowid
		JOIN documents d ON d.id = c.doc_id
		ORDER BY v.distance`, prefixDocColumns("d"), table), blob, limit)
	if err != nil {
		return nil, fmt.Errorf("knn query on %s: %w", table, err)
	}
	defer rows.Close()

	var hits []domain.Hit
	for rows.Next() {
		var (
			h       domain.Hit
			chunkID int64
			raw     docRow
		)
		targets := []any{
			&chunkID, &h.Chunk.DocID, &h.Chunk.Ordinal, &h.Chunk.Text,
			&h.Chunk.HeadingPath, &h.Chunk.ByteStart, &h.Chunk.ByteEnd,
		}
		targets = append(targets, raw.targets(&h.Doc)...)
		targets = append(targets, &h.Distance)
		if err := rows.Scan(targets...); err != nil {
			return nil, err
		}
		if err := raw.finish(&h.Doc); err != nil {
			return nil, err
		}
		hits = append(hits, h)
	}
	return hits, rows.Err()
}
