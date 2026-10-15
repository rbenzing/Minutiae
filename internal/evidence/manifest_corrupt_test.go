package evidence

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// J100: a manifest line that does not parse is corruption, never a plain error.
func TestReadManifestCorruptLineIsIntegrity(t *testing.T) {
	p := filepath.Join(t.TempDir(), "manifest.jsonl")
	if err := appendManifest(p, ManifestRecord{ID: "a"}); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{not json\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	rs, err := readManifest(p)
	if !errors.Is(err, ErrIntegrity) {
		t.Fatalf("err %v, want ErrIntegrity", err)
	}
	if rs != nil {
		t.Errorf("a corrupt manifest returned %d records", len(rs))
	}
}

// J100: every path that reads the manifest of a case with a corrupt line says integrity.
func TestCorruptManifestLineIsIntegrityThroughTheCase(t *testing.T) {
	c := newTestCase(t)
	f, err := os.OpenFile(filepath.Join(c.Dir, manifestFile), os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("garbage\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Manifest(); !errors.Is(err, ErrIntegrity) {
		t.Errorf("Manifest: %v", err)
	}
	if _, _, err := c.OpenArtifact("x"); !errors.Is(err, ErrIntegrity) {
		t.Errorf("OpenArtifact: %v", err)
	}
	if _, err := c.FindArtifact("x"); !errors.Is(err, ErrIntegrity) {
		t.Errorf("FindArtifact: %v", err)
	}
}
