package evalharness

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fixtureDir = "testdata/minicorpus"

// copyFixture copies the minicorpus fixture into a fresh t.TempDir so a test
// can mutate golden.yaml (via editGolden) without disturbing the fixture on
// disk shared by every other test. Returns the copy's root directory.
func copyFixture(t *testing.T) string {
	t.Helper()
	dst := t.TempDir()
	err := filepath.WalkDir(fixtureDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(fixtureDir, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		t.Fatalf("copyFixture: %v", err)
	}
	return dst
}

// editGolden rewrites golden.yaml under dir, replacing the first occurrence
// of old with new. Used to mutate a copied fixture into an invalid one.
func editGolden(t *testing.T, dir, old, replacement string) {
	t.Helper()
	path := filepath.Join(dir, "golden.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("editGolden: read: %v", err)
	}
	content := string(data)
	if !strings.Contains(content, old) {
		t.Fatalf("editGolden: %q not found in golden.yaml", old)
	}
	content = strings.Replace(content, old, replacement, 1)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("editGolden: write: %v", err)
	}
}

func TestLoadGolden_ValidFixture(t *testing.T) {
	queries, err := LoadGolden(fixtureDir)
	if err != nil {
		t.Fatalf("LoadGolden() error = %v, want nil", err)
	}
	if len(queries) != 4 {
		t.Fatalf("len(queries) = %d, want 4", len(queries))
	}

	q001 := queries[0]
	if len(q001.Relevant) != 1 || q001.Relevant[0] != "corpus/letters/renewal.md" {
		t.Errorf("q001.Relevant = %v, want [corpus/letters/renewal.md]", q001.Relevant)
	}

	q002 := queries[1]
	if len(q002.Acceptable) != 1 {
		t.Errorf("len(q002.Acceptable) = %d, want 1", len(q002.Acceptable))
	}

	q004 := queries[3]
	if !q004.HasTag("zero-answer") {
		t.Errorf("q004.HasTag(%q) = false, want true", "zero-answer")
	}
}

func TestLoadGolden_MissingPath(t *testing.T) {
	dir := copyFixture(t)
	editGolden(t, dir, "corpus/letters/renewal.md", "corpus/letters/nope.md")

	_, err := LoadGolden(dir)
	if err == nil {
		t.Fatal("LoadGolden() error = nil, want error naming the missing path")
	}
	if !strings.Contains(err.Error(), "q001") {
		t.Errorf("error %q does not contain %q", err.Error(), "q001")
	}
	if !strings.Contains(err.Error(), "nope.md") {
		t.Errorf("error %q does not contain %q", err.Error(), "nope.md")
	}
}

func TestLoadGolden_DuplicateID(t *testing.T) {
	dir := copyFixture(t)
	editGolden(t, dir, "id: q002", "id: q001")

	_, err := LoadGolden(dir)
	if err == nil {
		t.Fatal("LoadGolden() error = nil, want error naming the duplicate id")
	}
	if !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("error %q does not contain %q", err.Error(), "duplicate")
	}
}

func TestLoadGolden_ZeroAnswerWithRelevant(t *testing.T) {
	dir := copyFixture(t)
	editGolden(t, dir, "relevant: []", "relevant:\n      - corpus/letters/renewal.md")

	_, err := LoadGolden(dir)
	if err == nil {
		t.Fatal("LoadGolden() error = nil, want error naming the zero-answer conflict")
	}
	if !strings.Contains(err.Error(), "zero-answer") {
		t.Errorf("error %q does not contain %q", err.Error(), "zero-answer")
	}
}

func TestLoadGolden_RelevantAcceptableOverlap(t *testing.T) {
	dir := copyFixture(t)
	editGolden(t, dir, "tags: [recall, letters, converted]",
		"acceptable:\n      - corpus/letters/renewal.md\n    tags: [recall, letters, converted]")

	_, err := LoadGolden(dir)
	if err == nil {
		t.Fatal("LoadGolden() error = nil, want error naming the relevant/acceptable overlap")
	}
	if !strings.Contains(err.Error(), "q001") {
		t.Errorf("error %q does not contain %q", err.Error(), "q001")
	}
	if !strings.Contains(err.Error(), "both relevant and acceptable") {
		t.Errorf("error %q does not contain %q", err.Error(), "both relevant and acceptable")
	}
	if !strings.Contains(err.Error(), "corpus/letters/renewal.md") {
		t.Errorf("error %q does not contain the offending path", err.Error())
	}
}

func TestLoadGolden_EmptyQueryText(t *testing.T) {
	dir := copyFixture(t)
	editGolden(t, dir, "query: that letter about the rent going up", "query: ''")

	_, err := LoadGolden(dir)
	if err == nil {
		t.Fatal("LoadGolden() error = nil, want error naming the empty query")
	}
	if !strings.Contains(err.Error(), "q001") {
		t.Errorf("error %q does not contain %q", err.Error(), "q001")
	}
}
