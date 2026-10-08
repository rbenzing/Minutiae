package sqlitefile_test

import (
	"bytes"
	"runtime"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// varint4 writes v as a four byte varint (v must be in 2^21 .. 2^28-1).
func varint4(v uint64) []byte {
	return []byte{0x80 | byte(v>>21)&0x7f, 0x80 | byte(v>>14)&0x7f, 0x80 | byte(v>>7)&0x7f, byte(v) & 0x7f}
}

// TestHostileValueAllocationIsBoundedByRealBytes: a cell whose record declares
// a 60 MiB blob but whose chain holds three overflow pages must cost memory in
// proportion to the bytes that exist, however often it is read. Before the fix
// the value buffer was sized from the declared length first (60 MiB per read).
func TestHostileValueAllocationIsBoundedByRealBytes(t *testing.T) {
	const ps = 512
	const u = ps - 4 // content bytes of an overflow page
	b := sqlitetest.New(sqlitetest.Options{PageSize: ps})
	tb := b.CreateTable("t", "create table t(a)")
	blob := bytes.Repeat([]byte{0xab, 0xcd, 0xef}, (2<<20)/3+100) // about 2 MiB, so the length varints take four bytes
	tb.Insert(1, blob)
	data := b.Bytes()
	cell, page, off := tb.CellBytes(1)
	chain := tb.Overflow(1)
	if len(chain) < 10 {
		t.Fatalf("chain of %d pages", len(chain))
	}
	// cell: payload length (4 bytes), rowid (1), record header length (1), serial (4), value...
	p0 := uint64(len(blob)) + 5
	if cell[3]&0x80 != 0 || cell[0]&0x80 == 0 || cell[4] != 1 || cell[5] != 5 {
		t.Fatalf("unexpected cell layout % x", cell[:12])
	}
	// A claimed payload of about 60 MiB, congruent to the stored one modulo the
	// overflow page content size, so the local part of the cell does not change.
	k := (uint64(60<<20) - p0) / u
	claimed := p0 + k*u
	pg := pageAt(data, ps, page)
	copy(pg[off:], varint4(claimed))
	copy(pg[off+6:], varint4(12+2*(claimed-5)))
	// The chain really ends after three overflow pages.
	copy(pageAt(data, ps, chain[2])[:4], []byte{0, 0, 0, 0})

	rb := newRecBudget(1 << 30)
	_, v := openLive(t, data, sqlitefile.Options{Budget: rb})
	var ms0, ms1 runtime.MemStats
	runtime.ReadMemStats(&ms0)
	const reads = 20
	for range reads {
		rows := scanRows(t, v, tb.Root(), sqlitefile.TableTree)
		if len(rows) != 1 {
			t.Fatalf("%d rows", len(rows))
		}
		val := rows[0].Values[0]
		if val.Kind != sqlitefile.KindBlob || !val.Omitted || val.Len != int64(claimed-5) {
			t.Fatalf("value: kind %d omitted %v len %d (want omitted blob of %d)", val.Kind, val.Omitted, val.Len, claimed-5)
		}
	}
	runtime.ReadMemStats(&ms1)
	if got := ms1.TotalAlloc - ms0.TotalAlloc; got > 8<<20 {
		t.Errorf("%d scans of a hostile cell allocated %d MiB, want under 8 MiB", reads, got>>20)
	}
	if rb.peak > 8<<20 {
		t.Errorf("peak budget charge %d MiB", rb.peak>>20)
	}
	if !viewWarns(v, sqlitefile.WarnCellOverflowChain, 0) {
		t.Errorf("warnings %v", v.Warnings())
	}
	v.Release()
	rb.check(t)
}

// TestLargeRealValuesReadWholeAcrossGrowthSteps: values of sizes around every
// internal step (probe, growth, page) read back byte for byte, and what is
// allocated stays within a small multiple of what was read.
func TestLargeRealValuesReadWholeAcrossGrowthSteps(t *testing.T) {
	sizes := []int{0, 1, 63, 64, 65, 1000, 4095, 4096, 4097, 65535, 65536, 65537, 200000, 3 << 20}
	b := sqlitetest.New(sqlitetest.Options{PageSize: 4096})
	tb := b.CreateTable("t", "create table t(a, b)")
	want := map[int64][]byte{}
	for i, n := range sizes {
		blob := make([]byte, n)
		for j := range blob {
			blob[j] = byte(j*7 + i)
		}
		want[int64(i+1)] = blob
		tb.Insert(int64(i+1), blob, strings.Repeat("s", n%50))
	}
	_, v := openLive(t, b.Bytes(), sqlitefile.Options{})
	var ms0, ms1 runtime.MemStats
	runtime.ReadMemStats(&ms0)
	rows := scanRows(t, v, tb.Root(), sqlitefile.TableTree)
	runtime.ReadMemStats(&ms1)
	if len(rows) != len(sizes) {
		t.Fatalf("%d rows", len(rows))
	}
	total := 0
	for _, r := range rows {
		got := r.Values[0]
		if got.Kind != sqlitefile.KindBlob || got.Omitted || !bytes.Equal(got.Bytes, want[r.Rowid]) {
			t.Fatalf("rowid %d (%d bytes): kind %d omitted %v, %d bytes read", r.Rowid, len(want[r.Rowid]), got.Kind, got.Omitted, len(got.Bytes))
		}
		total += len(got.Bytes)
	}
	// Each row is cloned once by scanRows and grown at most to twice the data.
	if alloc := int(ms1.TotalAlloc - ms0.TotalAlloc); alloc > 6*total+(2<<20) {
		t.Errorf("allocated %d bytes to read %d", alloc, total)
	}
}

// TestTruncationOmitsEveryLaterValue: once a value cannot be read in full,
// every later value of the record is omitted, whether the record is decoded
// from bytes or lazily over a damaged chain (zero-width values included).
func TestTruncationOmitsEveryLaterValue(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
	tb := b.CreateTable("t", "create table t(a, b, c, d)")
	tb.Insert(1, bytes.Repeat([]byte{9}, 3000), nil, int64(0), int64(1))
	data := b.Bytes()
	chain := tb.Overflow(1)
	copy(pageAt(data, 512, chain[1])[:4], []byte{0, 0, 0, 0})
	_, v := openLive(t, data, sqlitefile.Options{})
	rows := scanRows(t, v, tb.Root(), sqlitefile.TableTree)
	if len(rows) != 1 {
		t.Fatalf("%d rows", len(rows))
	}
	for i, val := range rows[0].Values {
		if !val.Omitted {
			t.Errorf("column %d after a value cut off by the chain is not omitted: %+v", i, val)
		}
	}
	// The same record decoded from bytes that end inside the first value.
	rec, err := sqlitefile.DecodeRecord([]byte{5, 0x8f, 0x00, 0, 8, 9, 1, 2, 3}, sqlitefile.EncUTF8, sqlitefile.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if !rec.Truncated {
		t.Fatalf("%+v", rec)
	}
	for i, val := range rec.Values {
		if !val.Omitted {
			t.Errorf("DecodeRecord column %d: %+v", i, val)
		}
	}
}

// TestFullVisitedSetIsNotCalledACycle: a chain that fills the visited set says
// so, instead of claiming a cycle.
func TestFullVisitedSetIsNotCalledACycle(t *testing.T) {
	env := sqlitefile.NewTestEnv(sqlitefile.Options{})
	const local = 30
	data := payloadData(local + 3*(ovPage-4))
	src := sqlitefile.NewFakeSource()
	chainOf(src, data[local:], 5, 6, 7)
	vis, err := env.NewMapVisited(env.Ledger(), 2) // room for two pages, the chain has three
	if err != nil {
		t.Fatal(err)
	}
	p, _ := newPayloadFor(t, env, src, vis, cellFor(data, local, 5))
	buf := make([]byte, len(data))
	if n, _ := p.ReadAt(buf, 0); n >= len(data) {
		t.Fatalf("read %d bytes through a full visited set", n)
	}
	why, pg, ok := p.Damaged()
	if !ok || pg != 7 || !strings.Contains(why, "capacity") || strings.Contains(why, "cycle") {
		t.Errorf("damage (%q, page %d, %v): want the capacity, not a cycle", why, pg, ok)
	}
}

// TestScanOfHostileCellsKeepsOtherRows keeps the scan going across a damaged
// chain and checks the rows next to it are exact.
func TestScanOfHostileCellsKeepsOtherRows(t *testing.T) {
	b, tb := stdTable(t, sqlitetest.Options{PageSize: 512}, 300)
	data := b.Bytes()
	chain := tb.Overflow(97)
	copy(pageAt(data, 512, chain[0])[:4], []byte{0, 0, 0, 0})
	_, v := openLive(t, data, sqlitefile.Options{})
	rows := scanRows(t, v, tb.Root(), sqlitefile.TableTree)
	if len(rows) != 300 {
		t.Fatalf("%d rows", len(rows))
	}
	for _, r := range rows {
		if r.Rowid == 97 {
			if !r.Values[4].Omitted {
				t.Errorf("row 97's blob should be omitted: %+v", r.Values[4])
			}
			continue
		}
		if err := rowIs(r, genRow(r.Rowid)); err != nil {
			t.Fatalf("rowid %d: %v", r.Rowid, err)
		}
	}
}
