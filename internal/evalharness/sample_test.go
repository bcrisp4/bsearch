package evalharness

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// writeFile creates path (and its parent dirs) under dir with arbitrary
// content, failing the test on any error.
func writeFile(t *testing.T, dir, rel string) {
	t.Helper()
	path := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("writeFile: mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte("content of "+rel), 0o644); err != nil {
		t.Fatalf("writeFile: write: %v", err)
	}
}

func TestSampleDocs_RoundRobinAcrossCategories(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "corpus/a/1.md")
	writeFile(t, dir, "corpus/a/2.md")
	writeFile(t, dir, "corpus/b/1.md")
	writeFile(t, dir, "corpus/c/1.md")
	writeFile(t, dir, "corpus/c/2.md")
	writeFile(t, dir, "corpus/c/3.md")
	writeFile(t, dir, "corpus/manifest.json")

	got, err := SampleDocs(dir, 5)
	if err != nil {
		t.Fatalf("SampleDocs() error = %v, want nil", err)
	}

	want := []string{
		"corpus/a/1.md",
		"corpus/b/1.md",
		"corpus/c/1.md",
		"corpus/a/2.md",
		"corpus/c/2.md",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SampleDocs() = %v, want %v", got, want)
	}
}

func TestSampleDocs_SkipsNonText(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "corpus/a/1.md")
	writeFile(t, dir, "corpus/a/2.pdf")
	writeFile(t, dir, "corpus/manifest.json")

	got, err := SampleDocs(dir, 10)
	if err != nil {
		t.Fatalf("SampleDocs() error = %v, want nil", err)
	}

	want := []string{"corpus/a/1.md"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SampleDocs() = %v, want %v", got, want)
	}
}

func TestSampleDocs_NLargerThanCorpus(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "corpus/a/1.md")
	writeFile(t, dir, "corpus/a/2.md")
	writeFile(t, dir, "corpus/b/1.md")

	got, err := SampleDocs(dir, 50)
	if err != nil {
		t.Fatalf("SampleDocs() error = %v, want nil", err)
	}

	want := []string{"corpus/a/1.md", "corpus/b/1.md", "corpus/a/2.md"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("SampleDocs() = %v, want %v", got, want)
	}
}
