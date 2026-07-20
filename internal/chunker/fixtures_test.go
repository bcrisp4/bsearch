package chunker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFixturesInvariants runs Normalize+Chunk over every testdata file and
// asserts the core properties: Text equals its byte span, ordinals dense,
// warning ordinals valid.
func TestFixturesInvariants(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "*.md"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no fixtures: %v", err)
	}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			raw, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			text, err := Normalize(raw)
			if err != nil {
				t.Fatalf("Normalize: %v", err)
			}
			res := Chunk("d1", text, 0)
			checkInvariants(t, text, res)
			for _, w := range res.Warnings {
				if w.Ordinal < 0 || w.Ordinal >= len(res.Chunks) {
					t.Fatalf("warning ordinal out of range: %+v", w)
				}
			}
		})
	}
}

func TestFixtureObsidianNote(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "obsidian-note.md"))
	if err != nil {
		t.Fatal(err)
	}
	text, err := Normalize(raw)
	if err != nil {
		t.Fatal(err)
	}
	res := Chunk("d1", text, 0)
	checkInvariants(t, text, res)

	all := ""
	for _, c := range res.Chunks {
		all += c.Text + "\n---\n"
	}
	if strings.Contains(all, "tags: [home, energy]") {
		t.Fatal("frontmatter leaked into chunks")
	}
	if !strings.Contains(all, "preamble") {
		t.Fatal("preamble content missing")
	}
	for _, c := range res.Chunks {
		if n := strings.Count(c.Text, "| Unit |"); n > 0 && !strings.Contains(c.Text, "| Cylinder |") {
			t.Fatal("table split across chunks")
		}
		if strings.Contains(c.Text, "survey booked") && !strings.Contains(c.Text, "#not-a-heading inside fence") {
			t.Fatal("fence split across chunks")
		}
		if strings.HasPrefix(c.HeadingPath, "not-a-heading") {
			t.Fatal("heading parsed inside fence")
		}
	}
}

func TestFixtureCRLF(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "crlf.md"))
	if err != nil {
		t.Fatal(err)
	}
	text, err := Normalize(raw)
	if err != nil {
		t.Fatal(err)
	}
	res := Chunk("d1", text, 0)
	checkInvariants(t, text, res)
	if len(res.Chunks) == 0 {
		t.Fatal("no chunks")
	}
	c := res.Chunks[0]
	if c.HeadingPath != "Windows File" {
		t.Fatalf("path: %q", c.HeadingPath)
	}
	if strings.Contains(c.Text, "title: crlf test") {
		t.Fatal("CRLF frontmatter leaked")
	}
}

func TestFixtureUTF16(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "utf16le.md"))
	if err != nil {
		t.Fatal(err)
	}
	text, err := Normalize(raw)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	res := Chunk("d1", text, 0)
	checkInvariants(t, text, res)
	if len(res.Chunks) != 1 {
		t.Fatalf("chunks: %+v", res.Chunks)
	}
	if res.Chunks[0].HeadingPath != "UTF-16 Doc" {
		t.Fatalf("path: %q", res.Chunks[0].HeadingPath)
	}
	if !strings.Contains(res.Chunks[0].Text, "naïve café — dash") {
		t.Fatalf("non-ASCII content mangled: %q", res.Chunks[0].Text)
	}
}
