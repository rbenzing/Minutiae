package sqlitefile_test

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// TestOpenEncodingFieldFollowsEngine: a header encoding field outside 1..3 is
// read the way the engine reads it (engineEncodingForField, measured by
// TestEngineTextEncodingHeaderField): Info.Encoding is that encoding, the
// field is still reported invalid with a warning, and the engine does not
// refuse the file. Text written in that encoding decodes through Info.Encoding.
func TestOpenEncodingFieldFollowsEngine(t *testing.T) {
	const text = "héllo, wörld ☃ 𝄞"
	for field, enc := range engineEncodingForField {
		t.Run(fmt.Sprintf("%#x", field), func(t *testing.T) {
			b := sqlitetest.New(sqlitetest.Options{Encoding: enc})
			tb := b.CreateTable("t", "CREATE TABLE t(a)")
			tb.Insert(1, text)
			b.Patch(56, byte(field>>24), byte(field>>16), byte(field>>8), byte(field))
			db := openBytes(t, b.Bytes())
			i := db.Info()
			if i.Encoding != sqlitefile.Encoding(enc) || i.EncodingValid {
				t.Errorf("Encoding %v valid=%v, want %v and not valid", i.Encoding, i.EncodingValid, sqlitefile.Encoding(enc))
			}
			if !hasWarning(db, sqlitefile.WarnHdrEncodingInvalid) || len(i.EngineRefuses) != 0 {
				t.Errorf("warnings %v, EngineRefuses %q", warningCodes(db), i.EngineRefuses)
			}
			cellBytes, pg, off := tb.CellBytes(1)
			page, err := db.RawPage(pg)
			if err != nil {
				t.Fatal(err)
			}
			h, err := sqlitefile.ParsePageHeader(page, pg)
			if err != nil {
				t.Fatal(err)
			}
			cell, err := sqlitefile.ParseCell(page, i.UsableSize, h, off)
			if err != nil || cell.Length != len(cellBytes) {
				t.Fatalf("cell %+v, %v", cell, err)
			}
			rec, err := sqlitefile.DecodeRecord(cell.Local, i.Encoding, sqlitefile.Limits{})
			if err != nil {
				t.Fatal(err)
			}
			if got, ok := rec.Values[0].Text(); !ok || got != text {
				t.Errorf("text read through Info.Encoding = (%q, %v), want %q", got, ok, text)
			}
		})
	}
	// Only the low two bits count; anything above them is invalid and warned about.
	for _, c := range []struct {
		field uint32
		want  sqlitefile.Encoding
	}{{0x100, sqlitefile.EncUTF8}, {0x80000003, sqlitefile.EncUTF16BE}} {
		b := sqlitetest.New(sqlitetest.Options{})
		b.Patch(56, byte(c.field>>24), byte(c.field>>16), byte(c.field>>8), byte(c.field))
		db := openBytes(t, b.Bytes())
		if i := db.Info(); i.Encoding != c.want || i.EncodingValid || !hasWarning(db, sqlitefile.WarnHdrEncodingInvalid) {
			t.Errorf("field %#x: %v valid=%v, want %v not valid, with a warning", c.field, i.Encoding, i.EncodingValid, c.want)
		}
	}
}

// TestOpenTrailingPartialPageIsReadAsPresent: the last, partial page of a
// truncated file is served with the bytes that are there; the rest is
// unreadable, never zero-filled.
func TestOpenTrailingPartialPageIsReadAsPresent(t *testing.T) {
	data := sqlitetest.New(sqlitetest.Options{}).Bytes()
	tail := bytes.Repeat([]byte{0xaa}, 100)
	data = append(data, tail...)
	r := &countingReader{data: data}
	db, err := sqlitefile.Open(r, int64(len(data)), sqlitefile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	p2, err := db.RawPage(2)
	if err != nil || !bytes.Equal(p2, tail) {
		t.Errorf("RawPage(2) = (%d bytes, %v), want the 100 bytes that are in the file and no padding", len(p2), err)
	}
	for _, pg := range []uint32{0, 3} {
		if _, err := db.RawPage(pg); !errors.Is(err, sqlitefile.ErrPageUnavailable) {
			t.Errorf("RawPage(%d) = %v, want ErrPageUnavailable", pg, err)
		}
	}
	if r.maxAt > int64(len(data)) {
		t.Errorf("a read reached offset %d, size is %d", r.maxAt, len(data))
	}
	// A page that has no byte in the file at all stays unavailable even when
	// the header count promises it.
	b := sqlitetest.New(sqlitetest.Options{})
	b.SetHeaderPages(5, true)
	d2 := openBytes(t, append(b.Bytes(), tail...))
	if _, err := d2.RawPage(3); !errors.Is(err, sqlitefile.ErrPageUnavailable) {
		t.Errorf("RawPage(3) = %v", err)
	}
}

// TestPartialLastPageCellsAreReadWhereTheyLie: a table whose last leaf is cut
// off mid-page. Cells sit at the end of a page, so the cut loses the first rows
// of that leaf; a cell wholly inside the bytes present is read, a cell that
// reaches past them is a damaged cell.
func TestPartialLastPageCellsAreReadWhereTheyLie(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
	tb := b.CreateTable("t", "CREATE TABLE t(a)")
	for i := int64(1); i <= 40; i++ {
		tb.Insert(i, fmt.Sprintf("value %02d", i))
	}
	leaves := tb.Leaves()
	last := leaves[len(leaves)-1]
	if len(leaves) < 2 || last != b.Snapshot().Pages() {
		t.Fatalf("leaves %v of %d pages", leaves, b.Snapshot().Pages())
	}
	full := b.Bytes()
	base := int(last-1) * 512
	db := openBytes(t, full[:base+490]) // the file ends 490 bytes into the last leaf
	if db.Info().FilePages != last {
		t.Fatalf("FilePages %d, want %d", db.Info().FilePages, last)
	}
	page, err := db.RawPage(last)
	if err != nil || len(page) != 490 {
		t.Fatalf("RawPage(%d) = (%d bytes, %v)", last, len(page), err)
	}
	h, err := sqlitefile.ParsePageHeader(page, last)
	if err != nil {
		t.Fatal(err)
	}
	ptrs, err := sqlitefile.CellPointers(page, h, db.Info().UsableSize)
	if err != nil || len(ptrs.Bad) == 0 {
		t.Fatalf("cells at the end of the page cannot all be inside 490 bytes: %+v (%v)", ptrs, err)
	}
	readable := 0
	for _, ptr := range ptrs.Good {
		off := ptr.Offset
		cell, err := sqlitefile.ParseCell(page, db.Info().UsableSize, h, off)
		if err != nil {
			continue // a cell that starts inside but ends past the bytes present
		}
		readable++
		if want := full[base+off : base+off+cell.Length]; !bytes.Equal(page[off:off+cell.Length], want) {
			t.Errorf("cell at %d differs from the file", off)
		}
	}
	if readable == 0 || readable == h.CellCount {
		t.Errorf("%d of %d cells readable: the cut must keep some and lose some", readable, h.CellCount)
	}
	if _, err := sqlitefile.ParseCell(page, db.Info().UsableSize, h, 489); !errors.Is(err, sqlitefile.ErrCorrupt) {
		t.Errorf("a cell reaching past the bytes present: %v", err)
	}
}
