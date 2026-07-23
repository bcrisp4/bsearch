package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/bcrisp4/bsearch/internal/adapters/openai"
	"github.com/bcrisp4/bsearch/internal/adapters/sqlite"
	"github.com/bcrisp4/bsearch/internal/buildinfo"
	"github.com/bcrisp4/bsearch/internal/chunker"
	"github.com/bcrisp4/bsearch/internal/config"
	"github.com/bcrisp4/bsearch/internal/discovery"
	"github.com/bcrisp4/bsearch/internal/domain"
	"github.com/bcrisp4/bsearch/internal/evalharness"
	"github.com/bcrisp4/bsearch/internal/pipeline"
)

// eval reuses search.go's overFetchFactor/maxKNNK/maxLimit constants and its
// knnK helper directly — both live in this same package (main) — so eval's
// over-fetch policy can never drift from production search's.
const (
	// evalProgressEvery prints a liveness line every N scored queries
	// instead of per query — a large golden set (hundreds of queries)
	// would otherwise print a line per query, and every such line is a
	// place a query-text leak could creep in (DESIGN.md: Privacy).
	evalProgressEvery = 50
)

// runEval dispatches eval subcommands (run|compare|summarize).
func runEval(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: bsearch eval <run|compare|summarize>")
	}
	switch args[0] {
	case "run":
		return runEvalRun(args[1:], out)
	case "compare":
		return runEvalCompare(args[1:], out)
	case "summarize":
		return runEvalSummarize(args[1:], out)
	default:
		return fmt.Errorf("unknown eval subcommand %q (usage: bsearch eval <run|compare|summarize>)", args[0])
	}
}

// runEvalRun scores an embedding model against a golden query set
// (DESIGN.md: eval harness): index the corpus into a scratch database, then
// embed and KNN-search every golden query, scoring and timing each one.
func runEvalRun(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("eval run", flag.ContinueOnError)
	fs.SetOutput(out)
	corpusDir := fs.String("corpus", "", "golden corpus directory (must contain golden.yaml)")
	configPath := fs.String("config", config.DefaultPath(), "config file")
	workDirFlag := fs.String("work-dir", "", "scratch index directory (default ~/bsearch-eval/work)")
	outPath := fs.String("out", "", "results file path (default ~/bsearch-eval/results/<corpus>-<model>-<timestamp>.json)")
	limit := fs.Int("limit", 10, "documents to retrieve per query")
	fs.Usage = func() {
		fmt.Fprintln(out, "usage: bsearch eval run --corpus <dir> [--config <path>] [--work-dir <dir>] [--out <path>] [--limit <n>]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("eval run takes no positional arguments (got %q)", fs.Arg(0))
	}
	if *corpusDir == "" {
		return errors.New("eval run requires --corpus <dir>")
	}
	if *limit < 1 || *limit > maxLimit {
		return fmt.Errorf("--limit %d out of range [1, %d]", *limit, maxLimit)
	}

	home, homeErr := os.UserHomeDir()

	workDir := *workDirFlag
	if workDir == "" {
		if homeErr != nil {
			return fmt.Errorf("cannot resolve the default work directory (no home directory?) — pass --work-dir: %w", homeErr)
		}
		workDir = filepath.Join(home, "bsearch-eval", "work")
	}

	queries, err := evalharness.LoadGolden(*corpusDir)
	if err != nil {
		return err
	}
	if len(queries) == 0 {
		return errors.New("golden set has no queries")
	}
	corpusVersion, err := evalharness.CorpusVersion(*corpusDir)
	if err != nil {
		return err
	}
	docsVersion, err := evalharness.DocsVersion(*corpusDir)
	if err != nil {
		return err
	}

	_, embedder, err := loadInference(*configPath)
	if err != nil {
		return err
	}
	spec := embedder.Spec()

	absCorpusDir, err := filepath.Abs(*corpusDir)
	if err != nil {
		return err
	}
	corpusName := filepath.Base(absCorpusDir)
	// Discovery canonicalizes the full include root — <corpusDir>/corpus,
	// not just corpusDir — before recording Document.Path (symlinks in the
	// root or its ancestors, e.g. macOS /var -> /private/var, or the
	// corpus/ directory itself being a symlink to where the documents
	// actually live). Relativizing hits against anything else would
	// produce a path full of "../" instead of a clean corpus-relative one.
	// Resolve the same joined path discovery resolves, not just
	// absCorpusDir: EvalSymlinks(absCorpusDir) alone would still be wrong
	// when corpus/ itself is the symlink.
	resolvedDocsDir, err := filepath.EvalSymlinks(filepath.Join(absCorpusDir, "corpus"))
	if err != nil {
		return err
	}

	// The scratch db is keyed by corpus name, DOCUMENT set (DocsVersion,
	// not the combined CorpusVersion), and embedding fingerprint — a rerun
	// against the same corpus and model reuses the index rather than
	// re-embedding every document (fingerprint changes invalidate it
	// automatically, same as production's vec-spec check). The document
	// set is folded in because discovery has no deletion pass: regenerating
	// a corpus in place (same name, changed/removed docs) would otherwise
	// leave a deleted document's stale vectors in the reused db, polluting
	// rankings, even though the run records the new corpus version.
	// Keying on DocsVersion rather than CorpusVersion means editing
	// golden.yaml alone (relabeling a query, fixing a typo) no longer mints
	// a new work-db key and forces a pointless re-embed of an unchanged
	// corpus — golden.yaml carries no document content. A corpus with no
	// manifest.json falls back to the constant "nomanifest": without
	// per-file content hashes, a deleted doc isn't detectable this way, but
	// pipeline idempotency still catches added/edited files, and hand-built
	// local corpora accept the gap.
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		return err
	}
	docsHash8 := docsVersion
	if hex, ok := strings.CutPrefix(docsVersion, "sha256:"); ok {
		docsHash8 = hex[:8]
	}
	fp8 := fmt.Sprintf("%x", sha256.Sum256([]byte(spec.Fingerprint())))[:8]
	dbPath := filepath.Join(workDir, fmt.Sprintf("%s-%s-%s.db", corpusName, docsHash8, fp8))

	db, err := sqlite.Open(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()
	store := sqlite.NewStore(db)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	indexStart := time.Now()
	scanner := discovery.New(store, discovery.Options{
		Include: []string{filepath.Join(absCorpusDir, "corpus")},
		// The golden corpus is curated by corpusgen; production's deny
		// rules exist to keep noise (caches, VCS internals) out of a
		// live filesystem scan and have no business filtering a
		// hand-built eval fixture.
		Excluded: func(string) bool { return false },
	})
	scanRes, err := scanner.Scan(ctx)
	if err != nil {
		return err
	}
	for _, pe := range scanRes.PathErrors {
		fmt.Fprintf(out, "warning: %s: %v\n", stripControl(pe.Path), pe.Err)
	}
	fmt.Fprintf(out, "scanned: %d new/changed, %d unchanged, %d renamed, %d skipped (iCloud placeholder)\n",
		scanRes.Discovered, scanRes.Unchanged, scanRes.Renamed, scanRes.Dataless)
	// Unlike index.go's equivalent guard (which only fires alongside
	// PathErrors, since a live filesystem scan legitimately turning up zero
	// files elsewhere is not by itself suspicious), a golden corpus must
	// contain files regardless of whether any PathErrors were reported —
	// an empty corpus/ scores every query against nothing and reports
	// misleadingly clean zeros instead of failing loudly.
	if scanRes.Discovered+scanRes.Unchanged+scanRes.Renamed+scanRes.Dataless == 0 {
		return errors.New("scan reached no files — check --corpus points at a corpus directory with a corpus/ subtree (see warnings above, if any)")
	}

	ix, err := pipeline.New(pipeline.Options{
		Store:     store,
		Vectors:   store,
		Embedder:  embedder,
		Transient: openai.Transient,
		Progress:  out,
	})
	if err != nil {
		return err
	}
	docs, err := store.ListIndexable(ctx)
	if err != nil {
		return err
	}
	sum, runErr := ix.Run(ctx, docs)

	line := fmt.Sprintf("done: %d indexed, %d up to date, %d failed", sum.Indexed, sum.UpToDate, sum.Failed)
	if sum.Skipped > 0 {
		line += fmt.Sprintf(", %d skipped", sum.Skipped)
	}
	if sum.Warnings > 0 {
		line += fmt.Sprintf(", %d warnings", sum.Warnings)
	}
	// Only a run that actually did indexing work gets a wall-clock time —
	// a warm rerun against an up-to-date scratch db recorded 0 here would
	// otherwise be indistinguishable from "indexing was instantaneous".
	var indexSeconds float64
	if sum.Indexed > 0 {
		indexSeconds = time.Since(indexStart).Seconds()
	}
	fmt.Fprintf(out, "%s (%.1fs)\n", line, indexSeconds)
	if runErr != nil {
		return runErr
	}
	// A failed document invalidates the run: scoring against a corpus
	// that silently dropped a document would misattribute a retrieval
	// miss to the model instead of to the indexing failure.
	if sum.Failed > 0 {
		return fmt.Errorf("%d document(s) failed to index — see output above", sum.Failed)
	}
	// Unlike production indexing (where a skip just retries next run),
	// an eval corpus that indexes incompletely corrupts every score
	// computed against it: a document scan found but couldn't read right
	// now (vanished, permission denied) is silently missing from the
	// index, making a real retrieval miss indistinguishable from an
	// indexing gap. A golden corpus must index completely or not at all.
	if sum.Skipped > 0 {
		return fmt.Errorf("%d document(s) skipped (unreadable) — an eval corpus must index completely — see output above", sum.Skipped)
	}

	scored := make([]evalharness.ScoredQuery, 0, len(queries))
	results := make([]evalharness.QueryResult, 0, len(queries))
	embedMS := make([]float64, 0, len(queries))
	knnMS := make([]float64, 0, len(queries))
	totalMS := make([]float64, 0, len(queries))
	var dims int

	for i, q := range queries {
		embedStart := time.Now()
		vec, err := embedder.EmbedQuery(ctx, q.Query)
		embedElapsed := time.Since(embedStart)
		if err != nil {
			return fmt.Errorf("query %s: embed: %w", q.ID, err)
		}
		if i == 0 {
			dims = len(vec)
		}

		knnStart := time.Now()
		k := knnK(*limit)
		hits, err := store.SearchVectors(ctx, vec, k)
		knnElapsed := time.Since(knnStart)
		if err != nil {
			return fmt.Errorf("query %s: search: %w", q.ID, err)
		}

		// Collapse with the over-fetch cap k, not *limit: the spec's
		// condensed-list rule (§Scoring step 1) says acceptable docs occupy
		// no rank slot, so a relevant doc sitting just past the uncondensed
		// top-*limit must still survive into the condensed top-*limit.
		// Truncating to *limit here — before ScoreQuery removes the
		// acceptable docs — would let those acceptable docs eat rank slots
		// they aren't supposed to occupy, undercounting recall.
		collapsed := domain.CollapseBestPerDoc(hits, k)
		ranked := make([]evalharness.RankedDoc, len(collapsed))
		for n, h := range collapsed {
			// h.Doc.Path is under resolvedDocsDir (discovery's canonicalized
			// include root), not literally under "<corpusDir>/corpus" — so
			// re-prepend "corpus/" after relativizing, keeping the result a
			// clean corpus-relative path that matches golden.yaml regardless
			// of whether corpus/ itself was a symlink.
			rel, err := filepath.Rel(resolvedDocsDir, h.Doc.Path)
			if err != nil {
				return fmt.Errorf("query %s: relativize %q: %w", q.ID, h.Doc.Path, err)
			}
			ranked[n] = evalharness.RankedDoc{Path: path.Join("corpus", filepath.ToSlash(rel)), Distance: h.Distance}
		}
		evalharness.SortRanked(ranked)

		// Zero-answer queries are still embedded, searched, and recorded
		// (a model's behaviour on "no correct answer exists" is worth
		// keeping), but scoring them would divide by zero — ScoreQuery
		// itself guards that, but skipping the call keeps the intent
		// explicit here too.
		var score evalharness.QueryScore
		if !q.HasTag("zero-answer") {
			score = evalharness.ScoreQuery(q, ranked, *limit)
		}

		// The results file records only the uncondensed top *limit of the
		// full sorted ranking — ScoreQuery needed the full over-fetched
		// list to condense correctly, but persisting all k entries per
		// query would balloon the results file for no benefit.
		recordedRanked := ranked
		if len(recordedRanked) > *limit {
			recordedRanked = recordedRanked[:*limit]
		}

		scored = append(scored, evalharness.ScoredQuery{Query: q, Ranked: recordedRanked, Score: score})
		results = append(results, evalharness.QueryResult{
			ID:         q.ID,
			Query:      q.Query,
			Tags:       q.Tags,
			Relevant:   q.Relevant,
			Ranked:     recordedRanked,
			QueryScore: score,
			LatencyMS:  evalharness.QueryLatency{EmbedMS: msF(embedElapsed), KNNMS: msF(knnElapsed)},
		})

		embedMS = append(embedMS, msF(embedElapsed))
		knnMS = append(knnMS, msF(knnElapsed))
		totalMS = append(totalMS, msF(embedElapsed+knnElapsed))

		if (i+1)%evalProgressEvery == 0 || i == len(queries)-1 {
			fmt.Fprintf(out, "scored %d/%d queries\n", i+1, len(queries))
		}
	}

	res := evalharness.Results{
		Bsearch: evalharness.BsearchInfo{
			Version:        buildinfo.Version,
			ChunkerVersion: chunker.Version,
		},
		Corpus: evalharness.CorpusInfo{
			Name:    corpusName,
			Path:    absCorpusDir,
			Version: corpusVersion,
		},
		Model: evalharness.ModelInfo{
			Name:          spec.Model,
			Dims:          dims,
			Fingerprint:   spec.Fingerprint(),
			QueryPrefix:   spec.QueryTemplate,
			PassagePrefix: spec.PassageTemplate,
		},
		Run: evalharness.RunInfo{
			StartedAt:    time.Now().UTC(),
			IndexSeconds: indexSeconds,
			IndexedDocs:  sum.Indexed,
			Queries:      len(queries),
			Limit:        *limit,
		},
		Aggregates: evalharness.Aggregate(scored),
		LatencyMS: evalharness.LatencySummary{
			Embed: evalharness.PercentilePair{P50: evalharness.Percentile(embedMS, 50), P95: evalharness.Percentile(embedMS, 95)},
			KNN:   evalharness.PercentilePair{P50: evalharness.Percentile(knnMS, 50), P95: evalharness.Percentile(knnMS, 95)},
			Total: evalharness.PercentilePair{P50: evalharness.Percentile(totalMS, 50), P95: evalharness.Percentile(totalMS, 95)},
		},
		Queries: results,
	}

	resolvedOut := *outPath
	if resolvedOut == "" {
		if homeErr != nil {
			return fmt.Errorf("cannot resolve the default results path (no home directory?) — pass --out: %w", homeErr)
		}
		timestamp := time.Now().UTC().Format("20060102T150405Z")
		resolvedOut = filepath.Join(home, "bsearch-eval", "results",
			fmt.Sprintf("%s-%s-%s.json", corpusName, sanitizeModelName(spec.Model), timestamp))
	}
	if err := evalharness.WriteResults(resolvedOut, res); err != nil {
		return err
	}

	printEvalSummary(out, res, *limit, resolvedOut)
	return nil
}

// msF converts a duration to milliseconds as a float64, evalharness's unit
// for all recorded latencies.
func msF(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}

// sanitizeModelName makes a model name safe as a path component: "/" and
// ":" are common in model identifiers (e.g. "nomic-ai/nomic-embed-text-v1.5"
// or a registry tag) but not valid or wanted in a single filename segment.
func sanitizeModelName(name string) string {
	r := strings.NewReplacer("/", "-", ":", "-")
	return r.Replace(name)
}

// printEvalSummary prints the run's headline: corpus and model identity,
// the no-exact-match overall score, latency, and where the results file
// landed. Never prints query text or document content (DESIGN.md: Privacy).
func printEvalSummary(out io.Writer, r evalharness.Results, limit int, outPath string) {
	fmt.Fprintf(out, "corpus %s (%s)  model %s  %d queries\n",
		r.Corpus.Name, r.Corpus.Version, r.Model.Name, r.Run.Queries)
	agg := r.Aggregates.OverallNoExact
	fmt.Fprintf(out, "overall (excl. exact): recall@%d %.3f  MRR@%d %.3f  success@1 %.2f  (n=%d)\n",
		limit, agg.RecallAtK, limit, agg.MRRAtK, agg.SuccessAt1, agg.N)
	fmt.Fprintf(out, "latency: embed p50 %.0fms p95 %.0fms  knn p50 %.0fms p95 %.0fms\n",
		r.LatencyMS.Embed.P50, r.LatencyMS.Embed.P95, r.LatencyMS.KNN.P50, r.LatencyMS.KNN.P95)
	fmt.Fprintf(out, "results written to %s\n", outPath)
}

// runEvalCompare diffs two eval runs' scores against each other
// (evalharness.CompareResults), reporting per-query win/loss/tie and paired
// t-tests overall and per slice (DESIGN.md: eval harness, spec §Comparing
// two runs). Never prints query text, only aggregate model names, tags, and
// numbers (DESIGN.md: Privacy).
func runEvalCompare(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("eval compare", flag.ContinueOnError)
	fs.SetOutput(out)
	asJSON := fs.Bool("json", false, "emit JSON instead of human-readable output")
	fs.Usage = func() {
		fmt.Fprintln(out, "usage: bsearch eval compare [--json] <results-a.json> <results-b.json>")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 2 {
		// Also hit by flags placed after the paths: stdlib flag stops
		// parsing at the first positional, so the hint must cover both
		// "wrong number of paths" and "flags after positionals" mistakes
		// (same idiom as search.go's overloaded NArg check).
		return fmt.Errorf("eval compare takes two positional arguments: <results-a.json> <results-b.json> (got %d) — flags go before the paths", fs.NArg())
	}

	a, err := evalharness.ReadResults(fs.Arg(0))
	if err != nil {
		return err
	}
	b, err := evalharness.ReadResults(fs.Arg(1))
	if err != nil {
		return err
	}

	cmp, err := evalharness.CompareResults(a, b)
	if err != nil {
		return err
	}

	if *asJSON {
		return json.NewEncoder(out).Encode(cmp)
	}
	// a.Run.Limit and b.Run.Limit are guaranteed equal by CompareResults'
	// gate above (it errors out otherwise), but that shared limit need not
	// be 10 — the printed column headers must reflect the run's actual
	// --limit, not a hardcoded figure (spec: "*_at_10 JSON keys are fixed
	// schema names" — the human-readable labels are not).
	printComparison(out, cmp, a.Run.Limit)
	return nil
}

// printComparison renders a Comparison as a human-readable table: headline
// rows (with and without exact-match queries), then one row per slice tag
// in alphabetical order for determinism, then the significance caveat.
// Aggregate-only — never prints query text (DESIGN.md: Privacy). limit is
// the shared --limit the compared runs were scored at, used only to label
// the recall/MRR columns — the JSON field names stay recall_at_10/mrr_at_10
// regardless (fixed schema, see spec's Results file section).
func printComparison(out io.Writer, c evalharness.Comparison, limit int) {
	fmt.Fprintf(out, "corpus %s (%s)  A: %s  B: %s\n", c.Corpus.Name, c.Corpus.Version, c.ModelA.Name, c.ModelB.Name)
	fmt.Fprintf(out, "%-20s %-18s %-18s %-14s %-13s %s\n",
		"", fmt.Sprintf("recall@%d", limit), fmt.Sprintf("MRR@%d", limit), "success@1", "win/loss/tie", "p(MRR)")
	printCompareRow(out, "overall (no exact)", c.OverallNoExact)
	printCompareRow(out, "overall", c.Overall)

	tags := make([]string, 0, len(c.Slices))
	for tag := range c.Slices {
		tags = append(tags, tag)
	}
	sort.Strings(tags)
	if len(tags) > 0 {
		fmt.Fprintln(out, "slices:")
		for _, tag := range tags {
			printCompareRow(out, "  "+tag, c.Slices[tag])
		}
	}

	// Overall.N is the paired-query count actually scored (excludes
	// zero-answer queries, which have no RR/recall to compare) — the
	// caveat must report the true denominator, not a hardcoded figure
	// (spec §Comparing two runs).
	fmt.Fprintf(out, "caveat: %d scored queries gives modest power; consistent per-slice deltas beat single aggregate gaps.\n", c.Overall.N)
}

// printCompareRow prints one metric row (A -> B means, win/loss/tie, MRR
// p-value) followed by the delta-MRR line with its 95%% CI.
func printCompareRow(out io.Writer, label string, sc evalharness.SliceComparison) {
	fmt.Fprintf(out, "%-20s %.3f -> %-11.3f %.3f -> %-11.3f %.2f -> %-8.2f %3d/%d/%-6d %s\n",
		label,
		sc.A.RecallAtK, sc.B.RecallAtK,
		sc.A.MRRAtK, sc.B.MRRAtK,
		sc.A.SuccessAt1, sc.B.SuccessAt1,
		sc.Wins, sc.Losses, sc.Ties,
		formatP(sc.MRRTTest.P))
	fmt.Fprintf(out, "  Δ MRR %s  95%% CI [%s, %s]\n",
		formatSigned(sc.MRRTTest.MeanDelta), formatSigned(sc.MRRTTest.CI95Low), formatSigned(sc.MRRTTest.CI95High))
}

// formatP prints a p-value to 3 decimals, floored at "<0.001" so a
// near-zero p (common with small paired samples) doesn't misleadingly
// round to "0.000".
func formatP(p float64) string {
	if p < 0.001 {
		return "<0.001"
	}
	return fmt.Sprintf("%.3f", p)
}

// formatSigned prints a delta to 3 decimals with an explicit sign, so a
// negative (B worse than A) reads unambiguously next to a positive one.
func formatSigned(v float64) string {
	return fmt.Sprintf("%+.3f", v)
}

// evalSummarizeTimeout bounds a single doc's summarize call — long enough
// for a slow local model on a long document, short enough that a hung
// server doesn't stall the whole bench indefinitely.
const evalSummarizeTimeout = 5 * time.Minute

// evalSummarizeDocMetrics is one doc's line in metrics.json: ChatMetrics
// plus the corpus-relative path identifying which doc it timed. A wrapper
// rather than an embedded field so ChatMetrics itself stays untouched by
// this command's on-disk shape.
type evalSummarizeDocMetrics struct {
	Path             string  `json:"path"`
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	WallSeconds      float64 `json:"wall_seconds"`
	TokensPerSec     float64 `json:"tokens_per_sec"`
	UsageReported    bool    `json:"usage_reported"`
}

// evalSummarizeAggregate summarizes the run across all sampled docs.
type evalSummarizeAggregate struct {
	TotalCompletionTokens int     `json:"total_completion_tokens"`
	MeanTokensPerSec      float64 `json:"mean_tokens_per_sec"`
}

// evalSummarizeMetrics is the on-disk shape of <out-dir>/metrics.json.
type evalSummarizeMetrics struct {
	Model     string                    `json:"model"`
	Docs      []evalSummarizeDocMetrics `json:"docs"`
	Aggregate evalSummarizeAggregate    `json:"aggregate"`
}

// summaryOutputName derives eval summarize's output filename for a sampled
// document: "<category>-<stem>.md". Summaries are always prose/markdown
// regardless of the source document's format (spec: Summarizer bench "writes
// each summary to <out-dir>/<category>-<doc>.md"), so the source extension
// is stripped rather than preserved — a .txt or .docx source must not leave
// its extension on the summary file.
func summaryOutputName(relPath string) string {
	category := filepath.Base(filepath.Dir(relPath))
	basename := filepath.Base(relPath)
	stem := strings.TrimSuffix(basename, filepath.Ext(basename))
	return category + "-" + stem + ".md"
}

// runEvalSummarize benches a summarizer LLM's throughput over a sample of
// the golden corpus (DESIGN.md: eval harness): for each sampled document,
// stream a chat completion (evalharness.ChatClient.Summarize), write the
// summary text to disk, and record per-doc token/second metrics. Never
// prints document content or summary text (DESIGN.md: Privacy) — only
// paths and throughput numbers.
func runEvalSummarize(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("eval summarize", flag.ContinueOnError)
	fs.SetOutput(out)
	corpusDir := fs.String("corpus", "", "golden corpus directory (must contain a corpus/ subdirectory)")
	configPath := fs.String("config", config.DefaultPath(), "config file (inference.endpoint is used)")
	model := fs.String("model", "", "summarizer model name")
	outDirFlag := fs.String("out-dir", "", "output directory (default ~/bsearch-eval/summaries/<model>-<timestamp>/)")
	docs := fs.Int("docs", 10, "number of documents to sample from the corpus")
	fs.Usage = func() {
		fmt.Fprintln(out, "usage: bsearch eval summarize --corpus <dir> --model <name> [--config <path>] [--out-dir <dir>] [--docs <n>]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("eval summarize takes no positional arguments (got %q)", fs.Arg(0))
	}
	if *corpusDir == "" {
		return errors.New("eval summarize requires --corpus <dir>")
	}
	if *model == "" {
		return errors.New("eval summarize requires --model <name>")
	}
	if *docs < 1 {
		return fmt.Errorf("--docs %d out of range, must be >= 1", *docs)
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	sampled, err := evalharness.SampleDocs(*corpusDir, *docs)
	if err != nil {
		return err
	}
	if len(sampled) == 0 {
		return errors.New("eval summarize: corpus has no documents to sample")
	}

	home, homeErr := os.UserHomeDir()
	resolvedOutDir := *outDirFlag
	if resolvedOutDir == "" {
		if homeErr != nil {
			return fmt.Errorf("cannot resolve the default output directory (no home directory?) — pass --out-dir: %w", homeErr)
		}
		timestamp := time.Now().UTC().Format("20060102T150405Z")
		resolvedOutDir = filepath.Join(home, "bsearch-eval", "summaries",
			fmt.Sprintf("%s-%s", sanitizeModelName(*model), timestamp))
	}
	if err := os.MkdirAll(resolvedOutDir, 0o700); err != nil {
		return err
	}

	client := &evalharness.ChatClient{Endpoint: cfg.Inference.Endpoint, Model: *model}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// seenNames maps an output filename back to the first sampled doc that
	// produced it — used only to build the collision error message below.
	// Two sampled docs colliding (same category+stem, different source
	// extension) is a known theoretical gap tracked in issue #49; not
	// solved here, just refused rather than silently overwritten.
	seenNames := make(map[string]string, len(sampled))

	docMetrics := make([]evalSummarizeDocMetrics, 0, len(sampled))
	for _, relPath := range sampled {
		outName := summaryOutputName(relPath)
		if prior, dup := seenNames[outName]; dup {
			return fmt.Errorf("eval summarize: %s and %s both map to output file %s (see issue #49)", prior, relPath, outName)
		}
		seenNames[outName] = relPath

		content, err := os.ReadFile(filepath.Join(*corpusDir, relPath))
		if err != nil {
			return err
		}

		docCtx, cancel := context.WithTimeout(ctx, evalSummarizeTimeout)
		summary, metrics, err := client.Summarize(docCtx, string(content))
		cancel()
		// Abort on the first failure rather than limping on: a partial
		// bench (some docs summarized, one silently missing) would
		// misreport throughput as if the run were complete, and
		// metrics.json is only meaningful once every sampled doc succeeded.
		if err != nil {
			return fmt.Errorf("eval summarize: %s: %w", relPath, err)
		}

		summaryPath := filepath.Join(resolvedOutDir, outName)
		if err := os.WriteFile(summaryPath, []byte(summary), 0o600); err != nil { // #nosec G703 -- outName is derived from evalharness.SampleDocs's corpus-relative path, filtered through filepath.Base twice; not attacker input
			return err
		}

		fmt.Fprintf(out, "summarized %s (%.1f tok/s)\n", stripControl(relPath), metrics.TokensPerSec)

		docMetrics = append(docMetrics, evalSummarizeDocMetrics{
			Path:             relPath,
			PromptTokens:     metrics.PromptTokens,
			CompletionTokens: metrics.CompletionTokens,
			WallSeconds:      metrics.WallSeconds,
			TokensPerSec:     metrics.TokensPerSec,
			UsageReported:    metrics.UsageReported,
		})
	}

	var totalCompletionTokens int
	var sumTokensPerSec float64
	var docsWithoutUsage int
	for _, d := range docMetrics {
		totalCompletionTokens += d.CompletionTokens
		sumTokensPerSec += d.TokensPerSec
		if !d.UsageReported {
			docsWithoutUsage++
		}
	}
	// A silent fallback would let a bench's tok/s numbers look server-
	// confirmed when they're actually estimated from SSE delta count —
	// surfaced once per run, not per doc, matching evalProgressEvery's
	// "liveness, not noise" convention elsewhere in this command.
	if docsWithoutUsage > 0 {
		fmt.Fprintf(out, "warning: server did not report token usage for %d doc(s) — tokens/sec is estimated from SSE delta count\n", docsWithoutUsage)
	}

	result := evalSummarizeMetrics{
		Model: *model,
		Docs:  docMetrics,
		Aggregate: evalSummarizeAggregate{
			TotalCompletionTokens: totalCompletionTokens,
			MeanTokensPerSec:      sumTokensPerSec / float64(len(docMetrics)),
		},
	}
	payload, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(resolvedOutDir, "metrics.json"), payload, 0o600); err != nil {
		return err
	}

	fmt.Fprintf(out, "wrote %d summaries + metrics.json to %s\n", len(docMetrics), resolvedOutDir)
	return nil
}
