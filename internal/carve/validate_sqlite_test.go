package carve

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/rbenzing/minutiae/internal/carve/carvetest"
)

func sqliteWindow(buf []byte, b *budget) *Window {
	return newWindow(context.Background(), bytes.NewReader(buf), 0, int64(len(buf)), b)
}

// sqliteVerdict validates obj followed by trail bytes of fill, the window
// being the whole buffer (what the scanner hands a validator at the run end).
func sqliteVerdict(obj []byte, trail int) Verdict {
	buf := append(slices.Clone(obj), bytes.Repeat(fillPattern, trail/len(fillPattern)+1)[:trail]...)
	return validateSQLite(sqliteWindow(buf, newBudget(1<<30)))
}

func TestSQLiteValidatesBuilderVariants(t *testing.T) {
	for name, o := range map[string]carvetest.SQLiteOpts{
		"512":            {PageSize: 512, Pages: 3},
		"4096":           {PageSize: 4096, Pages: 2},
		"65536 as 1":     {PageSize: 65536, Pages: 2},
		"table leaf":     {Page1Type: 0x0D, Pages: 2},
		"table interior": {Page1Type: 0x05, Pages: 2},
	} {
		t.Run(name, func(t *testing.T) {
			o.ValidFor = true
			obj := carvetest.SQLiteHeader(o)
			// Fill after the object is excluded from the length.
			v := sqliteVerdict(obj, 5000)
			ps := o.PageSize
			if ps == 0 {
				ps = 4096
			}
			if v.Outcome != Validated || v.Length != int64(len(obj)) {
				t.Fatalf("verdict = %+v, want validated length %d", v, len(obj))
			}
			for _, want := range []string{"page_size=" + strconv.Itoa(int(ps)), "pages=" + strconv.Itoa(len(obj)/int(ps))} {
				if !slices.Contains(v.Basis, want) {
					t.Errorf("basis = %v, want %s", v.Basis, want)
				}
			}
		})
	}
}

func TestSQLiteRejectsAndBounds(t *testing.T) {
	base := func() []byte {
		return carvetest.SQLiteHeader(carvetest.SQLiteOpts{PageSize: 512, Pages: 4, ValidFor: true})
	}
	patch := func(f func(b []byte)) []byte { b := base(); f(b); return b }
	put32 := func(off int, v uint32) func(b []byte) {
		return func(b []byte) { binary.BigEndian.PutUint32(b[off:], v) }
	}
	for _, c := range []struct {
		name   string
		obj    []byte
		reason string
	}{
		{"page size 1000", patch(func(b []byte) { binary.BigEndian.PutUint16(b[16:], 1000) }), "bad-page-size"},
		{"page size 256", patch(func(b []byte) { binary.BigEndian.PutUint16(b[16:], 256) }), "bad-page-size"},
		{"page size 0", patch(func(b []byte) { binary.BigEndian.PutUint16(b[16:], 0) }), "bad-page-size"},
		{"fraction 64 broken", patch(func(b []byte) { b[21] = 63 }), "bad-header"},
		{"fraction 32 broken", patch(func(b []byte) { b[22] = 33 }), "bad-header"},
		{"fraction 31", carvetest.SQLiteHeader(carvetest.SQLiteOpts{PageSize: 512, Pages: 4, ValidFor: true, BadFractions: true}), "bad-header"},
		{"write version 3", patch(func(b []byte) { b[18] = 3 }), "bad-header"},
		{"read version 0", patch(func(b []byte) { b[19] = 0 }), "bad-header"},
		{"text encoding 4", patch(put32(56, 4)), "bad-header"},
		{"text encoding 0", patch(put32(56, 0)), "bad-header"},
		{"schema format 5", patch(put32(44, 5)), "bad-header"},
		{"schema format 0", patch(put32(44, 0)), "bad-header"},
		{"freelist trunk past the pages", patch(put32(32, 5)), "bad-header"},
		{"freelist count past the pages", patch(put32(36, 5)), "bad-header"},
		{"page 1 type 0x0A", carvetest.SQLiteHeader(carvetest.SQLiteOpts{PageSize: 512, Pages: 4, ValidFor: true, Page1Type: 0x0A}), "bad-btree"},
		{"pages beyond the window", base()[:512*3], "truncated"},
		{"huge page count", patch(put32(28, 0xFFFFFFFF)), "truncated"},
		{"bad signature", patch(func(b []byte) { b[0] = 'X' }), "bad-signature"},
		{"cut inside the header", base()[:60], "truncated"},
	} {
		t.Run(c.name, func(t *testing.T) {
			v := validateSQLite(sqliteWindow(c.obj, newBudget(1<<30)))
			if v.Outcome != Rejected || v.Reason != c.reason {
				t.Fatalf("verdict = %+v, want rejected %q", v, c.reason)
			}
		})
	}
	// A legacy file without a valid in-header size is bounded to the window
	// end: the page count is not trusted when the change counter differs from
	// the version-valid-for field or the count is zero.
	for name, obj := range map[string][]byte{
		"valid-for differs": carvetest.SQLiteHeader(carvetest.SQLiteOpts{PageSize: 512, Pages: 2}),
		"zero page count":   patch(put32(28, 0)),
	} {
		t.Run(name, func(t *testing.T) {
			buf := slices.Concat(obj, bytes.Repeat([]byte{1}, 300))
			v := validateSQLite(sqliteWindow(buf, newBudget(1<<30)))
			if v.Outcome != Bounded || v.Length != int64(len(buf)) || !slices.Contains(v.Assumptions, "size-from-header-invalid") ||
				!slices.Contains(v.Basis, "page_size=512") {
				t.Fatalf("verdict = %+v, want bounded to the window end %d", v, len(buf))
			}
		})
	}
	if v := sqliteVerdict(base(), 0); v.Outcome != Validated {
		t.Fatalf("control verdict = %+v", v)
	}
}

// TestSQLiteWindowBoundsAndBudget: only the first 101 bytes are read whatever
// the declared size; a spent budget and a cancelled context are rejected
// "unreadable" (ruling 3E-C1).
func TestSQLiteWindowBoundsAndBudget(t *testing.T) {
	obj := carvetest.SQLiteHeader(carvetest.SQLiteOpts{PageSize: 65536, Pages: 8, ValidFor: true})
	b := newBudget(1 << 30)
	if v := validateSQLite(sqliteWindow(obj, b)); v.Outcome != Validated || b.spent != 101 {
		t.Fatalf("verdict = %+v spent = %d, want validated after 101 bytes", v, b.spent)
	}
	b = newBudget(100)
	if v := validateSQLite(sqliteWindow(obj, b)); v.Outcome != Rejected || v.Reason != "unreadable" || !b.exhausted {
		t.Fatalf("verdict = %+v exhausted = %v", v, b.exhausted)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := newWindow(ctx, bytes.NewReader(obj), 0, int64(len(obj)), newBudget(1<<30))
	if v := validateSQLite(w); v.Outcome != Rejected || v.Reason != "unreadable" {
		t.Fatalf("cancelled verdict = %+v", v)
	}
}

// TestSQLiteReadErrorIsUnreadable (ruling 3E-C1): a media read error in the
// header is rejected "unreadable" (never "truncated") and recorded.
func TestSQLiteReadErrorIsUnreadable(t *testing.T) {
	obj := carvetest.SQLiteHeader(carvetest.SQLiteOpts{PageSize: 512, Pages: 4, ValidFor: true})
	w := newWindow(context.Background(), failAt{obj, 0}, 0, int64(len(obj)), newBudget(1<<30))
	v := validateSQLite(w)
	if v.Outcome != Rejected || v.Reason != "unreadable" || !errors.Is(w.readErr, errMedia) {
		t.Fatalf("verdict = %+v readErr = %v, want rejected unreadable", v, w.readErr)
	}
}

func TestSQLiteRealEncoderObjects(t *testing.T) {
	dir := filepath.Join("testdata", "objects")
	raw, err := os.ReadFile(filepath.Join(dir, "objects.expect.json"))
	if err != nil {
		t.Fatal(err)
	}
	var exp struct {
		Objects []struct {
			File, Family string
			Size         int64
			Facts        struct {
				PageSize  int `json:"page_size"`
				PageCount int `json:"page_count"`
			}
		}
	}
	if err := json.Unmarshal(raw, &exp); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, o := range exp.Objects {
		if o.Family != "sqlite" {
			continue
		}
		n++
		t.Run(o.File, func(t *testing.T) {
			obj, err := os.ReadFile(filepath.Join(dir, o.File))
			if err != nil {
				t.Fatal(err)
			}
			v := sqliteVerdict(obj, 4096)
			if v.Outcome != Validated || v.Length != o.Size || int64(len(obj)) != o.Size {
				t.Fatalf("verdict = %+v, want validated length %d", v, o.Size)
			}
			if o.Facts.PageSize == 0 || o.Facts.PageCount == 0 {
				t.Fatalf("expectation lacks the PRAGMA facts: %+v", o.Facts)
			}
			for _, want := range []string{"page_size=" + strconv.Itoa(o.Facts.PageSize), "pages=" + strconv.Itoa(o.Facts.PageCount)} {
				if !slices.Contains(v.Basis, want) {
					t.Errorf("basis = %v, want %s", v.Basis, want)
				}
			}
		})
	}
	if n < 1 {
		t.Fatal("no sqlite object in the expectations")
	}
}
