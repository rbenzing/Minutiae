package sqlitetest_test

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// goldenHeader4096 is the 100-byte header of the default empty database
// (page size 4096, UTF-8, change counter 1), written out byte by byte.
var goldenHeader4096 = []byte{
	'S', 'Q', 'L', 'i', 't', 'e', ' ', 'f', 'o', 'r', 'm', 'a', 't', ' ', '3', 0, // magic
	0x10, 0x00, // page size 4096
	0x01, 0x01, // write / read version
	0x00,             // reserved bytes per page
	0x40, 0x20, 0x20, // payload fractions
	0x00, 0x00, 0x00, 0x01, // file change counter
	0x00, 0x00, 0x00, 0x01, // database size in pages
	0x00, 0x00, 0x00, 0x00, // first freelist trunk
	0x00, 0x00, 0x00, 0x00, // freelist page count
	0x00, 0x00, 0x00, 0x00, // schema cookie
	0x00, 0x00, 0x00, 0x04, // schema format
	0x00, 0x00, 0x00, 0x00, // default cache size
	0x00, 0x00, 0x00, 0x00, // largest root page
	0x00, 0x00, 0x00, 0x01, // text encoding UTF-8
	0x00, 0x00, 0x00, 0x00, // user_version
	0x00, 0x00, 0x00, 0x00, // incremental vacuum
	0x00, 0x00, 0x00, 0x00, // application_id
	0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, // reserved for expansion
	0x00, 0x00, 0x00, 0x01, // version-valid-for
	0x00, 0x2e, 0x76, 0x88, // SQLITE_VERSION_NUMBER (3045000)
}

func TestBuilderEmptyDatabaseLayout(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{})
	data := b.Bytes()
	if len(data) != 4096 || b.PageSize() != 4096 {
		t.Fatalf("empty database is %d bytes, page size %d; want one 4096-byte page", len(data), b.PageSize())
	}
	if len(goldenHeader4096) != 100 {
		t.Fatalf("golden header is %d bytes", len(goldenHeader4096))
	}
	if !bytes.Equal(data[:100], goldenHeader4096) {
		t.Errorf("header differs from the golden bytes:\n got % x\nwant % x", data[:100], goldenHeader4096)
	}
	// Page 1 is an empty table leaf: flag 0x0d, no freeblock, no cells, cell
	// content starts at the end of the page, no fragmented bytes.
	if want := []byte{0x0d, 0, 0, 0, 0, 0x10, 0x00, 0}; !bytes.Equal(data[100:108], want) {
		t.Errorf("page 1 b-tree header = % x, want % x", data[100:108], want)
	}
	if !bytes.Equal(data[108:], make([]byte, 4096-108)) {
		t.Error("the rest of page 1 is not zero")
	}
}

func TestBuilderOptionsReachTheHeader(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 65536, Reserved: 8, Encoding: 3, AutoVacuum: 2, UserVersion: -5, AppID: 0xdeadbeef, ChangeCounter: 9})
	d := b.Bytes()
	if len(d) != 65536 {
		t.Fatalf("len = %d", len(d))
	}
	be := binary.BigEndian
	checks := []struct {
		name      string
		got, want uint32
	}{
		{"page size field (65536 is stored as 1)", uint32(be.Uint16(d[16:])), 1},
		{"reserved", uint32(d[20]), 8},
		{"change counter", be.Uint32(d[24:]), 9},
		{"page count", be.Uint32(d[28:]), 1},
		{"largest root page (autovacuum on)", be.Uint32(d[52:]), 1},
		{"encoding", be.Uint32(d[56:]), 3},
		{"user_version", be.Uint32(d[60:]), 0xfffffffb},
		{"incremental vacuum", be.Uint32(d[64:]), 1},
		{"application_id", be.Uint32(d[68:]), 0xdeadbeef},
		{"version-valid-for", be.Uint32(d[92:]), 9},
	}
	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %#x, want %#x", c.name, c.got, c.want)
		}
	}
	// cell content starts at the usable size, stored as is below 65536
	if got := be.Uint16(d[105:]); got != uint16(65536-8) {
		t.Errorf("cell content start = %d, want %d", got, 65536-8)
	}
	// full autovacuum leaves the incremental flag clear
	f := sqlitetest.New(sqlitetest.Options{AutoVacuum: 1}).Bytes()
	if be.Uint32(f[52:]) != 1 || be.Uint32(f[64:]) != 0 {
		t.Errorf("full autovacuum: largest root %d, incremental %d", be.Uint32(f[52:]), be.Uint32(f[64:]))
	}
	// with no reserved bytes a 65536-byte page stores its content start as 0
	z := sqlitetest.New(sqlitetest.Options{PageSize: 65536}).Bytes()
	if be.Uint16(z[105:]) != 0 {
		t.Errorf("65536-byte page: content start field = %d, want 0", be.Uint16(z[105:]))
	}
}

func TestBuilderMutationHelpers(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{ChangeCounter: 4})
	// Bytes is a copy
	d := b.Bytes()
	d[0] = 'X'
	if b.Bytes()[0] != 'S' {
		t.Error("Bytes returned the live slice")
	}
	// PageBytes is live
	b.PageBytes(1)[200] = 0x7e
	if b.Bytes()[200] != 0x7e {
		t.Error("PageBytes is not the live slice")
	}
	b.Patch(300, 1, 2, 3)
	if got := b.Bytes()[300:303]; !bytes.Equal(got, []byte{1, 2, 3}) {
		t.Errorf("Patch wrote % x", got)
	}
	be := binary.BigEndian
	b.SetHeaderPages(7, true)
	if be.Uint32(b.Bytes()[28:]) != 7 || be.Uint32(b.Bytes()[92:]) != be.Uint32(b.Bytes()[24:]) {
		t.Error("SetHeaderPages(7, true): count not 7, or version-valid-for differs from the change counter")
	}
	b.SetHeaderPages(7, false)
	if be.Uint32(b.Bytes()[28:]) != 7 || be.Uint32(b.Bytes()[92:]) == be.Uint32(b.Bytes()[24:]) {
		t.Error("SetHeaderPages(7, false): version-valid-for must differ from the change counter")
	}
	// out-of-range use is a programming error in a test helper: it panics
	for name, f := range map[string]func(){
		"page 0":           func() { b.PageBytes(0) },
		"page 2":           func() { b.PageBytes(2) },
		"patch past end":   func() { b.Patch(4095, 1, 2) },
		"patch negative":   func() { b.Patch(-1, 1) },
		"bad page size":    func() { sqlitetest.New(sqlitetest.Options{PageSize: 1000}) },
		"bad encoding":     func() { sqlitetest.New(sqlitetest.Options{Encoding: 4}) },
		"bad autovacuum":   func() { sqlitetest.New(sqlitetest.Options{AutoVacuum: 3}) },
		"usable below 480": func() { sqlitetest.New(sqlitetest.Options{PageSize: 512, Reserved: 33}) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s did not panic", name)
				}
			}()
			f()
		}()
	}
}
