package chunker

import (
	"strings"
	"testing"

	"github.com/bcrisp4/bsearch/internal/domain"
)

// para returns a single-line paragraph of roughly n bytes.
func para(n int) string {
	var b strings.Builder
	for b.Len() < n {
		b.WriteString("word ")
	}
	return strings.TrimSpace(b.String()[:n-1]) + "\n"
}

// checkInvariants asserts the core chunk properties: Text equals its byte
// span and ordinals are dense from 0.
func checkInvariants(t *testing.T, text string, res Result) {
	t.Helper()
	for i, c := range res.Chunks {
		if c.Ordinal != i {
			t.Fatalf("ordinal %d at index %d", c.Ordinal, i)
		}
		if c.ByteStart < 0 || c.ByteEnd > len(text) || c.ByteStart >= c.ByteEnd {
			t.Fatalf("chunk %d bad span [%d,%d) len=%d", i, c.ByteStart, c.ByteEnd, len(text))
		}
		if c.Text != text[c.ByteStart:c.ByteEnd] {
			t.Fatalf("chunk %d Text != span:\ntext: %q\nspan: %q", i, c.Text, text[c.ByteStart:c.ByteEnd])
		}
	}
}

func chunkAll(t *testing.T, text string) Result {
	t.Helper()
	res := Chunk("d1", text, 0)
	checkInvariants(t, text, res)
	for _, c := range res.Chunks {
		if c.DocID != "d1" {
			t.Fatalf("DocID = %q", c.DocID)
		}
	}
	return res
}

func TestChunkEmptyDoc(t *testing.T) {
	for _, src := range []string{"", "\n\n", "---\ntitle: x\n---\n"} {
		res := chunkAll(t, src)
		if len(res.Chunks) != 0 || len(res.Warnings) != 0 {
			t.Fatalf("src %q: want nothing, got %+v", src, res)
		}
	}
}

func TestChunkNoHeadings(t *testing.T) {
	src := "just a paragraph\n\nand another\n"
	res := chunkAll(t, src)
	if len(res.Chunks) != 1 {
		t.Fatalf("chunks: %d", len(res.Chunks))
	}
	c := res.Chunks[0]
	if c.HeadingPath != "" {
		t.Fatalf("HeadingPath = %q", c.HeadingPath)
	}
	if c.ByteStart != 0 || c.ByteEnd != len(src) {
		t.Fatalf("span [%d,%d)", c.ByteStart, c.ByteEnd)
	}
}

func TestChunkHeadingExcludedFromText(t *testing.T) {
	src := "# Title\n\nbody content here\n"
	res := chunkAll(t, src)
	if len(res.Chunks) != 1 {
		t.Fatalf("chunks: %d", len(res.Chunks))
	}
	c := res.Chunks[0]
	if c.HeadingPath != "Title" {
		t.Fatalf("HeadingPath = %q", c.HeadingPath)
	}
	if strings.Contains(c.Text, "# Title") {
		t.Fatalf("heading line must not be in Text: %q", c.Text)
	}
	if c.Text != "body content here\n" {
		t.Fatalf("Text = %q", c.Text)
	}
}

func TestChunkBreadcrumbNesting(t *testing.T) {
	src := "# A\n\nintro a\n\n## B\n\nbody b\n\n### C\n\nbody c\n\n## D\n\nbody d\n\n# E\n\nbody e\n"
	res := chunkAll(t, src)
	want := map[string]string{
		"A":         "intro a\n",
		"A > B":     "body b\n",
		"A > B > C": "body c\n",
		"A > D":     "body d\n", // sibling H2 pops C
		"E":         "body e\n", // new H1 pops everything
	}
	if len(res.Chunks) != len(want) {
		t.Fatalf("chunks: %d, want %d: %+v", len(res.Chunks), len(want), res.Chunks)
	}
	for _, c := range res.Chunks {
		if want[c.HeadingPath] != c.Text {
			t.Fatalf("path %q: Text %q, want %q", c.HeadingPath, c.Text, want[c.HeadingPath])
		}
	}
}

func TestChunkSkippedHeadingLevels(t *testing.T) {
	// H1 straight to H3: path still A > C.
	src := "# A\n\n### C\n\nbody\n"
	res := chunkAll(t, src)
	if len(res.Chunks) != 1 || res.Chunks[0].HeadingPath != "A > C" {
		t.Fatalf("chunks: %+v", res.Chunks)
	}
}

func TestChunkHeadingWithoutContent(t *testing.T) {
	src := "# A\n\n## B\n\nbody\n"
	res := chunkAll(t, src)
	if len(res.Chunks) != 1 {
		t.Fatalf("empty sections must not chunk: %+v", res.Chunks)
	}
	if res.Chunks[0].HeadingPath != "A > B" {
		t.Fatalf("path: %q", res.Chunks[0].HeadingPath)
	}
}

func TestChunkPreambleNeverMerged(t *testing.T) {
	// Tiny preamble next to a large mergeable section stays separate.
	src := "tiny intro\n\n# A\n\n" + para(500)
	res := chunkAll(t, src)
	if len(res.Chunks) != 2 {
		t.Fatalf("chunks: %+v", res.Chunks)
	}
	if res.Chunks[0].HeadingPath != "" || res.Chunks[0].Text != "tiny intro\n" {
		t.Fatalf("preamble chunk: %+v", res.Chunks[0])
	}
}

func TestChunkTinyMergesIntoNeighbour(t *testing.T) {
	src := "# Top\n\n## A\n\n" + para(100) + "\n## B\n\n" + para(500)
	res := chunkAll(t, src)
	if len(res.Chunks) != 1 {
		t.Fatalf("want merge into one chunk: %+v", res.Chunks)
	}
	c := res.Chunks[0]
	if c.HeadingPath != "Top" {
		t.Fatalf("merged path should be common prefix: %q", c.HeadingPath)
	}
	// Merged span is contiguous and includes the interior "## B" heading line.
	if !strings.Contains(c.Text, "## B") {
		t.Fatalf("merged span should include interior heading: %q", c.Text)
	}
}

func TestChunkTwoTiniesDoNotMerge(t *testing.T) {
	src := "## A\n\n" + para(100) + "\n## B\n\n" + para(100)
	res := chunkAll(t, src)
	if len(res.Chunks) != 2 {
		t.Fatalf("two tiny chunks must not fold together: %+v", res.Chunks)
	}
}

func TestChunkMergeRespectsMax(t *testing.T) {
	src := "## A\n\n" + para(100) + "\n## B\n\n" + para(4050)
	res := chunkAll(t, src)
	if len(res.Chunks) != 2 {
		t.Fatalf("merge exceeding maxBytes must not happen: got %d chunks", len(res.Chunks))
	}
}

func TestChunkMergePrefersSharedPath(t *testing.T) {
	// Tiny B has prev sibling A (same parent Top) and next section under a
	// different H1 — must merge backward into A.
	src := "# Top\n\n## A\n\n" + para(500) + "\n## B\n\n" + para(100) + "\n# Other\n\n" + para(500)
	res := chunkAll(t, src)
	if len(res.Chunks) != 2 {
		t.Fatalf("chunks: %+v", res.Chunks)
	}
	merged := res.Chunks[0]
	if merged.HeadingPath != "Top" {
		t.Fatalf("merged path: %q", merged.HeadingPath)
	}
	if !strings.Contains(merged.Text, "## B") {
		t.Fatalf("B should merge backward into A: %q", merged.Text)
	}
	if res.Chunks[1].HeadingPath != "Other" {
		t.Fatalf("Other must stay separate: %+v", res.Chunks[1])
	}
}

func TestChunkOversizedSectionSplits(t *testing.T) {
	// 30 paragraphs ≈ 200 B each ≈ 6 KB total — over maxBytes, must split.
	var b strings.Builder
	b.WriteString("# Big\n\n")
	for range 30 {
		b.WriteString(para(200))
		b.WriteString("\n")
	}
	src := b.String()
	res := chunkAll(t, src)
	if len(res.Chunks) < 2 {
		t.Fatalf("want split, got %d chunks", len(res.Chunks))
	}
	blockStarts := map[int]bool{}
	for _, blk := range scanBlocks(src) {
		blockStarts[blk.start] = true
	}
	for i, c := range res.Chunks {
		if len(c.Text) > maxBytes {
			t.Fatalf("chunk %d over max: %d", i, len(c.Text))
		}
		if c.HeadingPath != "Big" {
			t.Fatalf("chunk %d path: %q", i, c.HeadingPath)
		}
		if !blockStarts[c.ByteStart] {
			t.Fatalf("chunk %d start %d not block-aligned", i, c.ByteStart)
		}
	}
	// Successive chunks overlap (block-aligned trailing blocks ≤ overlapBytes).
	for i := 1; i < len(res.Chunks); i++ {
		if res.Chunks[i].ByteStart >= res.Chunks[i-1].ByteEnd {
			t.Fatalf("chunks %d/%d do not overlap: [%d,%d) then [%d,%d)",
				i-1, i, res.Chunks[i-1].ByteStart, res.Chunks[i-1].ByteEnd,
				res.Chunks[i].ByteStart, res.Chunks[i].ByteEnd)
		}
	}
}

func TestChunkOverlapSkippedWhenTrailingBlockTooBig(t *testing.T) {
	// Paragraphs of ~900 B: trailing block alone exceeds overlapBytes, so
	// chunks must not overlap.
	var b strings.Builder
	b.WriteString("# Big\n\n")
	for range 6 {
		b.WriteString(para(900))
		b.WriteString("\n")
	}
	src := b.String()
	res := chunkAll(t, src)
	if len(res.Chunks) < 2 {
		t.Fatalf("want split, got %d chunks", len(res.Chunks))
	}
	for i := 1; i < len(res.Chunks); i++ {
		if res.Chunks[i].ByteStart < res.Chunks[i-1].ByteEnd {
			t.Fatalf("overlap should be skipped for oversized trailing block")
		}
	}
}

func TestChunkLargeFenceStaysAtomic(t *testing.T) {
	// Fence bigger than maxBytes but under the (unlimited) ceiling: one
	// atomic chunk, no warning.
	var b strings.Builder
	b.WriteString("# Code\n\nintro paragraph here\n\n```\n")
	for range 300 {
		b.WriteString("line of code\n")
	}
	b.WriteString("```\n")
	src := b.String()
	res := chunkAll(t, src)
	if len(res.Warnings) != 0 {
		t.Fatalf("warnings: %+v", res.Warnings)
	}
	var fenceChunk *domain.Chunk
	for i := range res.Chunks {
		if strings.Contains(res.Chunks[i].Text, "```") {
			fenceChunk = &res.Chunks[i]
			break
		}
	}
	if fenceChunk == nil {
		t.Fatal("no chunk contains the fence")
	}
	if strings.Count(fenceChunk.Text, "line of code") != 300 {
		t.Fatalf("fence split despite being atomic: %d lines", strings.Count(fenceChunk.Text, "line of code"))
	}
}

func TestChunkTableStaysAtomic(t *testing.T) {
	var b strings.Builder
	b.WriteString("# T\n\n| a | b |\n|---|---|\n")
	for range 100 {
		b.WriteString("| cell one | cell two |\n")
	}
	b.WriteString("\n")
	b.WriteString(para(300))
	src := b.String()
	res := chunkAll(t, src)
	found := false
	for _, c := range res.Chunks {
		if n := strings.Count(c.Text, "| cell one |"); n == 100 {
			found = true
		} else if n > 0 {
			t.Fatalf("table split across chunks: %d rows in one chunk", n)
		}
	}
	if !found {
		t.Fatal("no chunk holds the whole table")
	}
}

func TestChunkCeilingSplitsAtomicBlockWithWarning(t *testing.T) {
	// Ceiling 300 tokens → effective 300*4-256 = 944 bytes. A ~3 KB fence
	// must be split with one warning per piece, never truncated.
	var b strings.Builder
	b.WriteString("# Code\n\n```\n")
	for range 200 {
		b.WriteString("code line here\n")
	}
	b.WriteString("```\n")
	src := b.String()
	res := Chunk("d1", src, 300)
	checkInvariants(t, src, res)
	if len(res.Chunks) < 2 {
		t.Fatalf("want ceiling split, got %d chunks", len(res.Chunks))
	}
	if len(res.Warnings) != len(res.Chunks) {
		t.Fatalf("want one warning per piece: %d warnings, %d chunks", len(res.Warnings), len(res.Chunks))
	}
	eff := 300*4 - prefixReserve
	total := 0
	for i, c := range res.Chunks {
		if len(c.Text) > eff {
			t.Fatalf("chunk %d exceeds effective ceiling: %d > %d", i, len(c.Text), eff)
		}
		total += strings.Count(c.Text, "code line here")
	}
	if total != 200 {
		t.Fatalf("content lost in ceiling split: %d/200 lines", total)
	}
	for _, w := range res.Warnings {
		c := res.Chunks[w.Ordinal]
		if c.ByteStart != w.ByteStart || c.ByteEnd != w.ByteEnd {
			t.Fatalf("warning span mismatch: warning %+v chunk %+v", w, c)
		}
		if w.Reason == "" {
			t.Fatal("warning needs a reason")
		}
	}
}

func TestChunkCeilingSplitNeverMidRune(t *testing.T) {
	// Multibyte content with a tiny ceiling: splits must land on rune
	// boundaries (invariant check would catch invalid spans via Text
	// comparison, so assert valid UTF-8 explicitly).
	src := "# É\n\n" + strings.Repeat("héllo wörld émoji 😀 ", 200) + "\n"
	res := Chunk("d1", src, 100)
	checkInvariants(t, src, res)
	for i, c := range res.Chunks {
		for _, r := range c.Text {
			if r == '�' {
				t.Fatalf("chunk %d contains replacement rune — split mid-rune", i)
			}
		}
	}
}

func TestChunkHeadingTextEndingInHash(t *testing.T) {
	// CommonMark: a trailing #-run is a closing sequence only when preceded
	// by a space. "## C#" must keep its hash.
	src := "## C#\n\nsharp language notes\n\n## Sub sec ##\n\nclosed heading body\n"
	res := chunkAll(t, src)
	paths := map[string]bool{}
	for _, c := range res.Chunks {
		paths[c.HeadingPath] = true
	}
	if !paths["C#"] {
		t.Fatalf("heading 'C#' truncated: %v", paths)
	}
	if !paths["Sub sec"] {
		t.Fatalf("closing-sequence stripping broken: %v", paths)
	}
}

func TestChunkNoSubsetChunks(t *testing.T) {
	// Overlap seeding must never emit a chunk whose span is contained in
	// another chunk's span: small first block, large second, oversized
	// section. Preamble (unmergeable) so the tiny-merge pass cannot mask a
	// subset chunk produced by packing.
	src := para(200) + "\n" + para(1900) + "\n" + para(1900) + "\n" + para(1900)
	res := chunkAll(t, src)
	for i, a := range res.Chunks {
		for j, b := range res.Chunks {
			if i != j && a.ByteStart >= b.ByteStart && a.ByteEnd <= b.ByteEnd {
				t.Fatalf("chunk %d [%d,%d) is a subset of chunk %d [%d,%d)",
					i, a.ByteStart, a.ByteEnd, j, b.ByteStart, b.ByteEnd)
			}
		}
	}
}
