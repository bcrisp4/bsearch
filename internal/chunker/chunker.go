package chunker

import (
	"strings"
	"unicode/utf8"

	"github.com/bcrisp4/bsearch/internal/domain"
)

// Version is the chunker stage version recorded in
// Document.StageVersions under key "chunker". Bump on any change that
// alters output for identical input (DESIGN.md: Pipeline metadata).
const Version = "1"

// Budgets in bytes, heuristic ~4 bytes/token (DESIGN.md: Chunking). The
// chars/4 heuristic over-counts English prose ~40%, so real chunks land
// well under nominal; the min/max bounds carry that slack. Token counts
// are never persisted — nothing downstream may trust them.
const (
	targetBytes       = 2048 // ~512 nominal tokens
	minBytes          = 256  // ~64 — smaller chunks merge into a neighbour
	maxBytes          = 4096 // ~1024 — packing and merge cap, not an atomic-block cap
	overlapBytes      = 256  // ~12.5% of target, always block-aligned
	breadcrumbReserve = 256  // ceiling headroom for heading path + model prefix template
)

// Warning reports an atomic block (code fence, table, single paragraph)
// that exceeded the embedder input ceiling and was split as a fallback —
// never truncated. The pipeline surfaces these in status.
type Warning struct {
	Ordinal     int // index into Result.Chunks
	HeadingPath string
	ByteStart   int
	ByteEnd     int
	Reason      string
}

// Result is the output of Chunk.
type Result struct {
	Chunks   []domain.Chunk
	Warnings []Warning
}

// Chunk splits normalized markdown (see Normalize) into embeddable chunks.
// inputCeilingTokens is the embedding model's input limit from pipeline
// metadata; <= 0 means unlimited. Pure and deterministic; Normalize owns
// the only error path.
//
// Every chunk's Text is exactly text[ByteStart:ByteEnd]; successive chunks
// of a split section may overlap.
func Chunk(docID, text string, inputCeilingTokens int) Result {
	// Effective ceiling in bytes, with headroom reserved for the breadcrumb
	// and model prefix template that join the chunk at embed time.
	ceiling := 0 // 0 = unlimited
	if inputCeilingTokens > 0 {
		ceiling = inputCeilingTokens*4 - breadcrumbReserve
		if ceiling < 64 {
			ceiling = 64
		}
	}
	capBytes := maxBytes // packing/merge cap
	target := targetBytes
	if ceiling > 0 && ceiling < capBytes {
		capBytes = ceiling
	}
	if target > capBytes {
		target = capBytes
	}

	var cands []candidate
	for _, sec := range sections(text) {
		cands = append(cands, chunkSection(text, sec, target, capBytes, ceiling)...)
	}
	cands = mergeTiny(cands, capBytes)

	res := Result{}
	for i, c := range cands {
		res.Chunks = append(res.Chunks, domain.Chunk{
			DocID:       docID,
			Ordinal:     i,
			Text:        text[c.start:c.end],
			HeadingPath: c.path,
			ByteStart:   c.start,
			ByteEnd:     c.end,
		})
		if c.warn != "" {
			res.Warnings = append(res.Warnings, Warning{
				Ordinal:     i,
				HeadingPath: c.path,
				ByteStart:   c.start,
				ByteEnd:     c.end,
				Reason:      c.warn,
			})
		}
	}
	return res
}

// candidate is a chunk before merge/ordinal assignment.
type candidate struct {
	start, end int
	path       string
	preamble   bool   // content before the first heading — never merged
	warn       string // non-empty: ceiling-split piece — never merged
}

// section is a run of content blocks sharing one heading path.
type section struct {
	path     string
	blocks   []block
	preamble bool
}

// sections walks the block list with a heading stack, grouping content
// blocks under their heading path. Headings themselves are not content:
// the breadcrumb carries them (chunk text stays pure). Headings with no
// content produce no section.
func sections(text string) []section {
	type frame struct {
		level int
		text  string
	}
	var stack []frame
	path := func() string {
		parts := make([]string, len(stack))
		for i, f := range stack {
			parts[i] = f.text
		}
		return strings.Join(parts, " > ")
	}

	var out []section
	cur := section{preamble: true}
	flush := func() {
		if len(cur.blocks) > 0 {
			out = append(out, cur)
		}
	}
	for _, b := range scanBlocks(text) {
		if b.kind == blockHeading {
			flush()
			for len(stack) > 0 && stack[len(stack)-1].level >= b.level {
				stack = stack[:len(stack)-1]
			}
			stack = append(stack, frame{b.level, b.text})
			cur = section{path: path()}
			continue
		}
		cur.blocks = append(cur.blocks, b)
	}
	flush()
	return out
}

// chunkSection turns one section into candidates: a single chunk when it
// fits, otherwise greedy atomic-block packing to target with block-aligned
// trailing overlap. Atomic blocks larger than the ceiling are hard-split
// with a warning per piece.
func chunkSection(text string, sec section, target, capBytes, ceiling int) []candidate {
	span := func(first, last block) int { return last.end - first.start }
	cand := func(start, end int) candidate {
		return candidate{start: start, end: end, path: sec.path, preamble: sec.preamble}
	}

	total := span(sec.blocks[0], sec.blocks[len(sec.blocks)-1])
	if total <= capBytes && (ceiling == 0 || total <= ceiling) {
		return []candidate{cand(sec.blocks[0].start, sec.blocks[len(sec.blocks)-1].end)}
	}

	// Greedy packing. cur holds the open group; curNew counts blocks added
	// since the overlap seed — a group is only emitted once it contains new
	// content, so overlap can never produce a duplicate chunk.
	var groups [][]block
	var cur []block
	curNew := 0
	for _, b := range sec.blocks {
		if len(cur) > 0 && curNew > 0 && span(cur[0], b) > target {
			groups = append(groups, cur)
			cur = overlapSuffix(cur)
			curNew = 0
			if len(cur) > 0 && span(cur[0], b) > capBytes {
				cur = nil // oversized incoming block: drop overlap rather than exceed cap
			}
		}
		cur = append(cur, b)
		curNew++
	}
	if curNew > 0 {
		groups = append(groups, cur)
	}

	var out []candidate
	for _, g := range groups {
		start, end := g[0].start, g[len(g)-1].end
		if ceiling > 0 && end-start > ceiling {
			// Only single atomic blocks can exceed the cap (multi-block
			// groups are bounded by packing); split as a fallback.
			for _, p := range hardSplit(text, start, end, ceiling) {
				c := cand(p[0], p[1])
				c.warn = "atomic block exceeds embedder input ceiling; split"
				out = append(out, c)
			}
			continue
		}
		out = append(out, cand(start, end))
	}
	return out
}

// overlapSuffix returns the trailing blocks of a group whose combined span
// fits overlapBytes — the block-aligned overlap seeding the next group.
// Empty when even the last block alone is too big.
func overlapSuffix(g []block) []block {
	i := len(g)
	for i > 0 && g[len(g)-1].end-g[i-1].start <= overlapBytes {
		i--
	}
	return append([]block(nil), g[i:]...)
}

// hardSplit cuts text[start:end] into pieces of at most limit bytes,
// preferring blank-line boundaries, then line breaks, then spaces, falling
// back to a rune boundary. Every byte is kept — never truncation.
func hardSplit(text string, start, end, limit int) [][2]int {
	var out [][2]int
	for start < end {
		if end-start <= limit {
			out = append(out, [2]int{start, end})
			break
		}
		w := text[start : start+limit]
		cut := 0
		if i := strings.LastIndex(w, "\n\n"); i > 0 {
			cut = i + 2
		} else if i := strings.LastIndexByte(w, '\n'); i > 0 {
			cut = i + 1
		} else if i := strings.LastIndexByte(w, ' '); i > 0 {
			cut = i + 1
		} else {
			cut = limit
			for cut > 0 && !utf8.RuneStart(text[start+cut]) {
				cut--
			}
			if cut == 0 {
				_, cut = utf8.DecodeRuneInString(text[start:])
			}
		}
		out = append(out, [2]int{start, start + cut})
		start += cut
	}
	return out
}

// mergeTiny folds chunks under minBytes into an adjacent chunk. Guards
// (lessons from lore): the neighbour must itself be >= minBytes (two tiny
// chunks never fold together), the merged span must fit the cap, and
// preamble chunks and ceiling-split pieces never merge. The neighbour
// sharing the longer heading-path prefix wins; ties go to the previous.
// The merged span is contiguous (it absorbs any interior heading line) and
// its path is the common prefix of the two paths.
func mergeTiny(cands []candidate, capBytes int) []candidate {
	mergeable := func(c candidate) bool { return !c.preamble && c.warn == "" }
	i := 0
	for i < len(cands) {
		c := cands[i]
		if c.end-c.start >= minBytes || !mergeable(c) {
			i++
			continue
		}
		fits := func(n candidate) bool {
			return mergeable(n) && n.end-n.start >= minBytes &&
				max(n.end, c.end)-min(n.start, c.start) <= capBytes
		}
		prevOK := i > 0 && fits(cands[i-1])
		nextOK := i+1 < len(cands) && fits(cands[i+1])
		var into int
		switch {
		case prevOK && nextOK:
			if sharedComponents(c.path, cands[i+1].path) > sharedComponents(c.path, cands[i-1].path) {
				into = i + 1
			} else {
				into = i - 1
			}
		case prevOK:
			into = i - 1
		case nextOK:
			into = i + 1
		default:
			i++
			continue
		}
		n := cands[into]
		merged := candidate{
			start: min(n.start, c.start),
			end:   max(n.end, c.end),
			path:  commonPathPrefix(n.path, c.path),
		}
		cands[into] = merged
		cands = append(cands[:i], cands[i+1:]...)
		if into > i {
			// merged element shifted left into position i; re-examine it
			continue
		}
		// merged into previous: stay at i (now the next element)
	}
	return cands
}

func splitPath(p string) []string {
	if p == "" {
		return nil
	}
	return strings.Split(p, " > ")
}

func sharedComponents(a, b string) int {
	as, bs := splitPath(a), splitPath(b)
	n := 0
	for n < len(as) && n < len(bs) && as[n] == bs[n] {
		n++
	}
	return n
}

func commonPathPrefix(a, b string) string {
	n := sharedComponents(a, b)
	return strings.Join(splitPath(a)[:n], " > ")
}
