package evidence

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestMultiHasherKnownVectors(t *testing.T) {
	cases := []struct {
		in, sha, md5 string
	}{
		{"", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", "d41d8cd98f00b204e9800998ecf8427e"},
		{"abc", "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad", "900150983cd24fb0d6963f7d28e17f72"},
	}
	for _, c := range cases {
		h := NewMultiHasher()
		if _, err := io.WriteString(h, c.in); err != nil {
			t.Fatal(err)
		}
		d := h.Sum()
		if d.SHA256 != c.sha || d.MD5 != c.md5 || d.Size != int64(len(c.in)) {
			t.Errorf("%q: got %+v", c.in, d)
		}
	}
}

func TestHashFileMatchesStream(t *testing.T) {
	data := bytes.Repeat([]byte("minutiae-"), 200_000) // ~1.8 MB, several read chunks
	p := filepath.Join(t.TempDir(), "f.bin")
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	h := NewMultiHasher()
	_, _ = h.Write(data)
	got, err := HashFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if got != h.Sum() {
		t.Fatalf("HashFile %+v != stream %+v", got, h.Sum())
	}
}
