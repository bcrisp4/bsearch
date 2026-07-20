package chunker

import "strings"

// blockKind classifies one atomic unit of markdown. Fences and tables are
// atomic — never split mid-block (DESIGN.md: Chunking).
type blockKind int

const (
	blockParagraph blockKind = iota
	blockHeading
	blockFence
	blockTable
)

// block is one contiguous byte span [start, end) of the normalized source.
// Spans include the trailing newline of their last line (except at EOF).
type block struct {
	kind       blockKind
	start, end int
	level      int    // blockHeading only: 1–6
	text       string // blockHeading only: trimmed heading text
}

// line is one source line with its byte span; end includes the newline.
type line struct {
	start, end int
}

func splitLines(text string) []line {
	out := make([]line, 0, strings.Count(text, "\n")+1)
	start := 0
	for i := 0; i < len(text); i++ {
		if text[i] == '\n' {
			out = append(out, line{start, i + 1})
			start = i + 1
		}
	}
	if start < len(text) {
		out = append(out, line{start, len(text)})
	}
	return out
}

// trimLine returns the line's content without the trailing newline (and \r).
func trimLine(text string, l line) string {
	s := text[l.start:l.end]
	s = strings.TrimSuffix(s, "\n")
	return strings.TrimSuffix(s, "\r")
}

// scanBlocks tokenizes normalized markdown into atomic blocks. YAML
// frontmatter at byte 0 is skipped entirely (never a block); ATX headings,
// fenced code (``` / ~~~ matched as pairs), and pipe tables are recognized;
// everything else groups into paragraph blocks at blank-line boundaries.
// Setext headings and indented code blocks are out of scope (package doc).
func scanBlocks(text string) []block {
	lines := splitLines(text)
	i := skipFrontmatter(text, lines)

	var blocks []block
	paraStart := -1 // start line index of an open paragraph run, -1 if none
	flushPara := func(endLine int) {
		if paraStart >= 0 {
			blocks = append(blocks, block{
				kind:  blockParagraph,
				start: lines[paraStart].start,
				end:   lines[endLine-1].end,
			})
			paraStart = -1
		}
	}

	for i < len(lines) {
		s := trimLine(text, lines[i])

		if strings.TrimSpace(s) == "" {
			flushPara(i)
			i++
			continue
		}

		if level, htext, ok := parseATXHeading(s); ok {
			flushPara(i)
			blocks = append(blocks, block{
				kind:  blockHeading,
				start: lines[i].start,
				end:   lines[i].end,
				level: level,
				text:  htext,
			})
			i++
			continue
		}

		if marker, ok := fenceOpen(s); ok {
			flushPara(i)
			j := i + 1
			for j < len(lines) && !fenceCloses(trimLine(text, lines[j]), marker) {
				j++
			}
			end := len(text)
			if j < len(lines) {
				end = lines[j].end // include the closing fence line
				j++
			}
			blocks = append(blocks, block{kind: blockFence, start: lines[i].start, end: end})
			i = j
			continue
		}

		if isPipeLine(s) && i+1 < len(lines) && isDelimiterRow(trimLine(text, lines[i+1])) {
			flushPara(i)
			j := i + 2
			for j < len(lines) && isPipeLine(trimLine(text, lines[j])) {
				j++
			}
			blocks = append(blocks, block{kind: blockTable, start: lines[i].start, end: lines[j-1].end})
			i = j
			continue
		}

		if paraStart < 0 {
			paraStart = i
		}
		i++
	}
	flushPara(len(lines))
	return blocks
}

// skipFrontmatter returns the index of the first content line, skipping a
// YAML frontmatter section delimited by an opening "---" on the very first
// line and a closing "---" or "..." line. Delimiter lines may carry
// trailing spaces/tabs (common copy-paste artifact). Unterminated
// frontmatter is not frontmatter — the whole file is content.
func skipFrontmatter(text string, lines []line) int {
	delim := func(l line) string {
		return strings.TrimRight(trimLine(text, l), " \t")
	}
	if len(lines) == 0 || delim(lines[0]) != "---" {
		return 0
	}
	for i := 1; i < len(lines); i++ {
		if s := delim(lines[i]); s == "---" || s == "..." {
			return i + 1
		}
	}
	return 0
}

// parseATXHeading recognizes "#"–"######" followed by a space, returning
// the level and the heading text. A trailing #-run is stripped only when
// it is a CommonMark closing sequence — preceded by a space (or the whole
// text) — so headings like "## C#" keep their hash.
func parseATXHeading(s string) (level int, text string, ok bool) {
	n := 0
	for n < len(s) && s[n] == '#' {
		n++
	}
	if n < 1 || n > 6 || n >= len(s) || s[n] != ' ' {
		return 0, "", false
	}
	t := strings.TrimSpace(s[n+1:])
	if stripped := strings.TrimRight(t, "#"); stripped == "" || strings.HasSuffix(stripped, " ") {
		t = strings.TrimSpace(stripped)
	}
	return n, t, true
}

// fenceOpen reports whether the line opens a code fence, returning the
// marker (its run of ` or ~) used to match the close.
func fenceOpen(s string) (marker string, ok bool) {
	trimmed := strings.TrimLeft(s, " ")
	if len(s)-len(trimmed) > 3 {
		return "", false
	}
	for _, c := range []byte{'`', '~'} {
		n := 0
		for n < len(trimmed) && trimmed[n] == c {
			n++
		}
		if n >= 3 {
			return trimmed[:n], true
		}
	}
	return "", false
}

// fenceCloses reports whether the line closes a fence opened with marker:
// same character, at least as long, nothing but the fence (no info string).
func fenceCloses(s, marker string) bool {
	trimmed := strings.TrimLeft(s, " ")
	if len(s)-len(trimmed) > 3 {
		return false
	}
	trimmed = strings.TrimRight(trimmed, " ")
	if len(trimmed) < len(marker) {
		return false
	}
	c := marker[0]
	for i := 0; i < len(trimmed); i++ {
		if trimmed[i] != c {
			return false
		}
	}
	return true
}

func isPipeLine(s string) bool {
	return strings.Contains(s, "|")
}

// isDelimiterRow recognizes a table delimiter row: cells of dashes with
// optional leading/trailing colons, separated by pipes, e.g. "|---|:--:|".
func isDelimiterRow(s string) bool {
	s = strings.TrimSpace(s)
	if !strings.Contains(s, "-") || !strings.Contains(s, "|") {
		return false
	}
	s = strings.TrimPrefix(s, "|")
	s = strings.TrimSuffix(s, "|")
	for _, cell := range strings.Split(s, "|") {
		cell = strings.TrimSpace(cell)
		cell = strings.TrimPrefix(cell, ":")
		cell = strings.TrimSuffix(cell, ":")
		if cell == "" || strings.Trim(cell, "-") != "" {
			return false
		}
	}
	return true
}
