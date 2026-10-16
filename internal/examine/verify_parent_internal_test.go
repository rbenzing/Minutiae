package examine

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
)

// verifyParentFile compares all three manifest facts; each alone is enough for
// ErrIntegrity, and an empty recorded MD5 is a difference, never a skipped check.
func TestVerifyParentFileComparesEveryFact(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seg")
	content := []byte("some evidence bytes")
	if err := os.WriteFile(path, content, 0o600); err != nil {
		t.Fatal(err)
	}
	h := evidence.NewMultiHasher()
	_, _ = h.Write(content)
	good := h.Sum()
	rec := evidence.ManifestRecord{ID: "seg", Path: "seg", Size: good.Size, SHA256: good.SHA256, MD5: good.MD5}

	open := func(t *testing.T) *os.File {
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = f.Close() })
		return f
	}
	if err := verifyParentFile(Options{}, open(t), rec, 0, rec.Size); err != nil {
		t.Fatalf("matching record: %v", err)
	}
	for name, mod := range map[string]func(*evidence.ManifestRecord){
		"size":         func(r *evidence.ManifestRecord) { r.Size++ },
		"sha256":       func(r *evidence.ManifestRecord) { r.SHA256 = strings.Repeat("0", 64) },
		"md5":          func(r *evidence.ManifestRecord) { r.MD5 = strings.Repeat("0", 32) },
		"md5 not kept": func(r *evidence.ManifestRecord) { r.MD5 = "" },
	} {
		t.Run(name, func(t *testing.T) {
			bad := rec
			mod(&bad)
			if err := verifyParentFile(Options{}, open(t), bad, 0, bad.Size); !errors.Is(err, evidence.ErrIntegrity) {
				t.Fatalf("a differing %s = %v, want ErrIntegrity", name, err)
			}
		})
	}
}

// An I/O error says nothing about the evidence: it must not become an integrity finding.
func TestVerifyParentFileIOErrorIsNotIntegrity(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seg")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = f.Close() // every read now fails
	rec := evidence.ManifestRecord{ID: "seg", Path: "seg", Size: 1}
	err = verifyParentFile(Options{}, f, rec, 0, rec.Size)
	if err == nil || errors.Is(err, evidence.ErrIntegrity) {
		t.Fatalf("read failure = %v, want a plain I/O error", err)
	}
}

// Progress counts bytes across all segments: the second segment starts after the first.
func TestVerifyParentFileProgressSpansSegments(t *testing.T) {
	path := filepath.Join(t.TempDir(), "seg")
	if err := os.WriteFile(path, []byte("abcd"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	h := evidence.NewMultiHasher()
	_, _ = h.Write([]byte("abcd"))
	s := h.Sum()
	first := evidence.ManifestRecord{ID: "a", Size: 10}
	second := evidence.ManifestRecord{ID: "b", Size: 4, SHA256: s.SHA256, MD5: s.MD5}
	var done, total int64
	opts := Options{VerifyProgress: func(d, t int64) { done, total = d, t }}
	if err := verifyParentFile(opts, f, second, first.Size, first.Size+second.Size); err != nil {
		t.Fatal(err)
	}
	if done != 14 || total != 14 {
		t.Fatalf("progress = %d/%d, want 14/14", done, total)
	}
}
