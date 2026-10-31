package carve

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/carve/carvetest"
)

func gzipWindow(buf []byte, b *budget) *Window {
	return newWindow(context.Background(), bytes.NewReader(buf), 0, int64(len(buf)), b)
}

// TestGzipIsBoundedHeaderOnly: a gzip header is checked and the object is
// bounded to the window end; nothing is inflated, so no end can be proved.
func TestGzipIsBoundedHeaderOnly(t *testing.T) {
	for _, c := range []struct {
		name  string
		flags byte
		fname string
		want  []string
	}{
		{"no flags", 0, "", nil},
		{"text", 0x01, "", nil},
		{"fhcrc", 0x02, "", nil},
		{"fextra", 0x04, "", nil},
		{"fname", 0x08, "notes.txt", []string{"name=notes.txt"}},
		{"fcomment", 0x10, "", nil},
		{"fname and fcomment", 0x18, "a.bin", []string{"name=a.bin"}},
		{"every flag", 0x1F, "all.txt", []string{"name=all.txt"}},
		{"escaped name", 0x08, "a\x01b\nc", []string{"name=a\\x01b\\nc"}},
		{"long name cut at 64 bytes", 0x08, strings.Repeat("n", 100), []string{"name=" + strings.Repeat("n", 64)}},
	} {
		t.Run(c.name, func(t *testing.T) {
			buf := slices.Concat(carvetest.GzipHeader(c.flags, c.fname, "a comment"), bytes.Repeat(fillPattern, 20))
			v := validateGzip(gzipWindow(buf, newBudget(1<<30)))
			if v.Outcome != Bounded || v.Length != int64(len(buf)) {
				t.Fatalf("verdict = %+v, want bounded length %d", v, len(buf))
			}
			if !slices.Equal(v.Basis, c.want) {
				t.Errorf("basis = %q, want %q", v.Basis, c.want)
			}
		})
	}
}

func TestGzipRejects(t *testing.T) {
	good := carvetest.GzipHeader(0x08, "x", "")
	withByte := func(i int, b byte) []byte { c := slices.Clone(good); c[i] = b; return c }
	for _, c := range []struct {
		name   string
		obj    []byte
		reason string
	}{
		{"reserved bit 5", carvetest.GzipHeader(0x20, "", ""), "bad-header"},
		{"reserved bit 7", carvetest.GzipHeader(0x88, "x", ""), "bad-header"},
		{"method 9", withByte(2, 9), "bad-header"},
		{"bad magic", withByte(0, 0x1E), "bad-header"},
		{"unterminated FNAME over 64 KiB", slices.Concat([]byte{0x1F, 0x8B, 8, 0x08, 0, 0, 0, 0, 0, 3}, bytes.Repeat([]byte{'a'}, 70000)), "bad-header"},
		{"unterminated FCOMMENT over 64 KiB", slices.Concat([]byte{0x1F, 0x8B, 8, 0x10, 0, 0, 0, 0, 0, 3}, bytes.Repeat([]byte{'a'}, 70000)), "bad-header"},
		{"FNAME runs off the window", []byte{0x1F, 0x8B, 8, 0x08, 0, 0, 0, 0, 0, 3, 'a', 'b', 'c', 'd', 'e', 'f', 'g', 'h'}, "truncated"},
		{"FEXTRA past the window", []byte{0x1F, 0x8B, 8, 0x04, 0, 0, 0, 0, 0, 3, 0xFF, 0xFF, 1, 2, 3, 4, 5, 6}, "truncated"},
		{"FHCRC past the window", []byte{0x1F, 0x8B, 8, 0x02, 0, 0, 0, 0, 0, 3, 1}, "truncated"},
	} {
		t.Run(c.name, func(t *testing.T) {
			v := validateGzip(gzipWindow(c.obj, newBudget(1<<30)))
			if v.Outcome != Rejected || v.Reason != c.reason {
				t.Fatalf("verdict = %+v, want rejected %q", v, c.reason)
			}
		})
	}
}

// TestGzipReadsAreBounded: the header scan reads at most the header and 64 KiB
// (a name search), however large the window.
func TestGzipReadsAreBounded(t *testing.T) {
	huge := slices.Concat([]byte{0x1F, 0x8B, 8, 0x08, 0, 0, 0, 0, 0, 3}, bytes.Repeat([]byte{'a'}, 1<<20))
	b := newBudget(1 << 30)
	if v := validateGzip(gzipWindow(huge, b)); v.Outcome != Rejected || v.Reason != "bad-header" {
		t.Fatalf("verdict = %+v", v)
	}
	if b.spent > 10+64<<10 {
		t.Fatalf("read %d bytes, want at most %d", b.spent, 10+64<<10)
	}
	small := newBudget(8)
	if v := validateGzip(gzipWindow(huge, small)); v.Outcome != Rejected || v.Reason != "unreadable" {
		t.Fatalf("budget verdict = %+v, want rejected unreadable", v)
	}
}

// TestGzipReadErrorIsUnreadable (ruling 3E-C1).
func TestGzipReadErrorIsUnreadable(t *testing.T) {
	obj := slices.Concat(carvetest.GzipHeader(0x08, "name.txt", ""), bytes.Repeat(fillPattern, 20))
	for _, at := range []int64{0, 12} {
		w := newWindow(context.Background(), failAt{obj, at}, 0, int64(len(obj)), newBudget(1<<30))
		v := validateGzip(w)
		if v.Outcome != Rejected || v.Reason != "unreadable" || !errors.Is(w.readErr, errMedia) {
			t.Fatalf("at %d: verdict = %+v read error = %v", at, v, w.readErr)
		}
	}
}

// TestGzipHiddenAtDefaultFloor is the 3E-R2 rule: no end of a gzip member can
// be found without inflating it, so every gzip hit is bounded (confidence 25),
// below the default minimum confidence 40 that the layer above applies. The
// scan accepts and counts it, and the assumption names why the length stops
// where it does; a filter at 40 shows nothing.
func TestGzipHiddenAtDefaultFloor(t *testing.T) {
	img := carvetest.Image(8*kib, fillPattern)
	carvetest.Place(img, 1024, carvetest.GzipHeader(0x08, "a.txt", ""))
	var hits []Hit
	st, err := Scan(context.Background(), bytes.NewReader(img), whole(img), Builtin(), Options{Align: 1}, func(h Hit) error {
		hits = append(hits, h)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	const floor = 40
	var shown []Hit
	for _, h := range hits {
		if h.Confidence >= floor {
			shown = append(shown, h)
		}
	}
	if len(hits) != 1 || len(shown) != 0 || st.Accepted[Bounded] != 1 {
		t.Fatalf("hits = %+v shown = %d accepted = %v, want one bounded hit hidden by the floor", hits, len(shown), st.Accepted)
	}
	h := hits[0]
	if h.Type != "gzip" || h.Outcome != Bounded || h.Method != "carve-bounded" || h.Confidence != 25 ||
		h.Offset != 1024 || h.Length != int64(len(img))-1024 ||
		!slices.Equal(h.Assumptions, []string{"cut-at-run-end"}) || !slices.Equal(h.Basis, []string{"name=a.txt"}) {
		t.Fatalf("hit = %+v", h)
	}
}

func TestGzipRealEncoderObject(t *testing.T) {
	obj, err := os.ReadFile(filepath.Join("testdata", "objects", "gzip.gz"))
	if err != nil {
		t.Fatal(err)
	}
	buf := slices.Concat(obj, bytes.Repeat(fillPattern, 10))
	v := validateGzip(gzipWindow(buf, newBudget(1<<30)))
	if v.Outcome != Bounded || v.Length != int64(len(buf)) {
		t.Fatalf("verdict = %+v, want bounded to the window end %d", v, len(buf))
	}
}
