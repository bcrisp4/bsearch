package chunker

import (
	"strings"
	"testing"
)

// kinds extracts the kind sequence for compact assertions.
func kinds(bs []block) []blockKind {
	out := make([]blockKind, len(bs))
	for i, b := range bs {
		out[i] = b.kind
	}
	return out
}

func kindsEqual(a, b []blockKind) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestScanBlocksEmpty(t *testing.T) {
	if bs := scanBlocks(""); len(bs) != 0 {
		t.Fatalf("want no blocks, got %v", bs)
	}
	if bs := scanBlocks("\n\n  \n"); len(bs) != 0 {
		t.Fatalf("whitespace-only: want no blocks, got %v", bs)
	}
}

func TestScanBlocksParagraphs(t *testing.T) {
	src := "one two\nthree\n\nsecond para\n"
	bs := scanBlocks(src)
	if !kindsEqual(kinds(bs), []blockKind{blockParagraph, blockParagraph}) {
		t.Fatalf("kinds: %v", kinds(bs))
	}
	if src[bs[0].start:bs[0].end] != "one two\nthree\n" {
		t.Fatalf("para 0 span: %q", src[bs[0].start:bs[0].end])
	}
	if src[bs[1].start:bs[1].end] != "second para\n" {
		t.Fatalf("para 1 span: %q", src[bs[1].start:bs[1].end])
	}
}

func TestScanBlocksATXHeading(t *testing.T) {
	src := "# Title\n\n## Sub sec ##\n\nbody\n"
	bs := scanBlocks(src)
	if !kindsEqual(kinds(bs), []blockKind{blockHeading, blockHeading, blockParagraph}) {
		t.Fatalf("kinds: %v", kinds(bs))
	}
	if bs[0].level != 1 || bs[0].text != "Title" {
		t.Fatalf("h1: level=%d text=%q", bs[0].level, bs[0].text)
	}
	// Trailing closing #s stripped from text.
	if bs[1].level != 2 || bs[1].text != "Sub sec" {
		t.Fatalf("h2: level=%d text=%q", bs[1].level, bs[1].text)
	}
}

func TestScanBlocksHeadingNeedsSpace(t *testing.T) {
	// "#tag" is not a heading; 7 #s is not a heading.
	src := "#tag\n\n####### seven\n"
	bs := scanBlocks(src)
	if !kindsEqual(kinds(bs), []blockKind{blockParagraph, blockParagraph}) {
		t.Fatalf("kinds: %v", kinds(bs))
	}
}

func TestScanBlocksFrontmatterSkipped(t *testing.T) {
	src := "---\ntitle: x\ntags: [a]\n---\n# H\nbody\n"
	bs := scanBlocks(src)
	if !kindsEqual(kinds(bs), []blockKind{blockHeading, blockParagraph}) {
		t.Fatalf("kinds: %v", kinds(bs))
	}
	if src[bs[0].start:bs[0].end] != "# H\n" {
		t.Fatalf("first block should start after frontmatter: %q", src[bs[0].start:bs[0].end])
	}
}

func TestScanBlocksFrontmatterDotsClose(t *testing.T) {
	src := "---\ntitle: x\n...\nbody\n"
	bs := scanBlocks(src)
	if !kindsEqual(kinds(bs), []blockKind{blockParagraph}) {
		t.Fatalf("kinds: %v", kinds(bs))
	}
	if src[bs[0].start:bs[0].end] != "body\n" {
		t.Fatalf("span: %q", src[bs[0].start:bs[0].end])
	}
}

func TestScanBlocksUnterminatedFrontmatterIsContent(t *testing.T) {
	src := "---\ntitle: x\nno closing\n"
	bs := scanBlocks(src)
	if len(bs) == 0 || bs[0].start != 0 {
		t.Fatalf("unterminated frontmatter must be content from byte 0: %v", bs)
	}
}

func TestScanBlocksFrontmatterOnly(t *testing.T) {
	if bs := scanBlocks("---\ntitle: x\n---\n"); len(bs) != 0 {
		t.Fatalf("want no blocks, got %v", bs)
	}
}

func TestScanBlocksFence(t *testing.T) {
	src := "```go\n# not a heading\n| not | a table |\n```\nafter\n"
	bs := scanBlocks(src)
	if !kindsEqual(kinds(bs), []blockKind{blockFence, blockParagraph}) {
		t.Fatalf("kinds: %v", kinds(bs))
	}
	if !strings.Contains(src[bs[0].start:bs[0].end], "# not a heading") {
		t.Fatalf("fence span: %q", src[bs[0].start:bs[0].end])
	}
}

func TestScanBlocksFencePairing(t *testing.T) {
	// ``` fence is not closed by ~~~, and vice versa; longer close allowed.
	src := "```\n~~~\nstill code\n````\nafter\n"
	bs := scanBlocks(src)
	if !kindsEqual(kinds(bs), []blockKind{blockFence, blockParagraph}) {
		t.Fatalf("kinds: %v", kinds(bs))
	}
	if !strings.Contains(src[bs[0].start:bs[0].end], "still code") {
		t.Fatalf("fence span: %q", src[bs[0].start:bs[0].end])
	}
}

func TestScanBlocksFenceCloseNeedsLength(t *testing.T) {
	// Opened with 4 backticks; 3 backticks inside stay literal.
	src := "````\n```\ncode\n````\nafter\n"
	bs := scanBlocks(src)
	if !kindsEqual(kinds(bs), []blockKind{blockFence, blockParagraph}) {
		t.Fatalf("kinds: %v", kinds(bs))
	}
}

func TestScanBlocksUnclosedFenceRunsToEOF(t *testing.T) {
	src := "before\n\n```\ncode forever\n# still code\n"
	bs := scanBlocks(src)
	if !kindsEqual(kinds(bs), []blockKind{blockParagraph, blockFence}) {
		t.Fatalf("kinds: %v", kinds(bs))
	}
	if bs[1].end != len(src) {
		t.Fatalf("fence should run to EOF: end=%d len=%d", bs[1].end, len(src))
	}
}

func TestScanBlocksTable(t *testing.T) {
	src := "| a | b |\n|---|---|\n| 1 | 2 |\n\nafter\n"
	bs := scanBlocks(src)
	if !kindsEqual(kinds(bs), []blockKind{blockTable, blockParagraph}) {
		t.Fatalf("kinds: %v", kinds(bs))
	}
	if src[bs[0].start:bs[0].end] != "| a | b |\n|---|---|\n| 1 | 2 |\n" {
		t.Fatalf("table span: %q", src[bs[0].start:bs[0].end])
	}
}

func TestScanBlocksPipeLineWithoutDelimiterIsParagraph(t *testing.T) {
	src := "| just | pipes |\nno delimiter row\n"
	bs := scanBlocks(src)
	if !kindsEqual(kinds(bs), []blockKind{blockParagraph}) {
		t.Fatalf("kinds: %v", kinds(bs))
	}
}

func TestScanBlocksSetextIsNotHeading(t *testing.T) {
	// Paragraph followed by --- underline: paragraph + HR-ish paragraph,
	// never a heading (documented v1 exclusion).
	src := "Would-be title\n---\nbody\n"
	bs := scanBlocks(src)
	for _, b := range bs {
		if b.kind == blockHeading {
			t.Fatalf("setext underline parsed as heading: %v", bs)
		}
	}
}

func TestScanBlocksCRLF(t *testing.T) {
	src := "---\r\ntitle: x\r\n---\r\n# H\r\n\r\nbody\r\n"
	bs := scanBlocks(src)
	if !kindsEqual(kinds(bs), []blockKind{blockHeading, blockParagraph}) {
		t.Fatalf("kinds: %v", kinds(bs))
	}
	if bs[0].level != 1 || bs[0].text != "H" {
		t.Fatalf("heading: level=%d text=%q", bs[0].level, bs[0].text)
	}
	if src[bs[1].start:bs[1].end] != "body\r\n" {
		t.Fatalf("para span: %q", src[bs[1].start:bs[1].end])
	}
}

func TestScanBlocksNoTrailingNewline(t *testing.T) {
	src := "# H\nbody without newline"
	bs := scanBlocks(src)
	if !kindsEqual(kinds(bs), []blockKind{blockHeading, blockParagraph}) {
		t.Fatalf("kinds: %v", kinds(bs))
	}
	if bs[1].end != len(src) {
		t.Fatalf("last block must end at EOF: %d != %d", bs[1].end, len(src))
	}
}

func TestScanBlocksFrontmatterTrailingWhitespace(t *testing.T) {
	// Delimiter lines with trailing spaces/tabs are still frontmatter.
	src := "---  \ntitle: x\n--- \t\nbody\n"
	bs := scanBlocks(src)
	if !kindsEqual(kinds(bs), []blockKind{blockParagraph}) {
		t.Fatalf("kinds: %v", kinds(bs))
	}
	if src[bs[0].start:bs[0].end] != "body\n" {
		t.Fatalf("frontmatter leaked: %q", src[bs[0].start:bs[0].end])
	}
}
