package sqlitefile_test

// ScanJournal (plan 3I, Task 9): the rollback journal scan on its own. What
// the engine does with a journal is recorded by TestEngineJournalRules; the
// rules these tests pin were decided by that probe.

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// jpg is a deterministic page of ps bytes.
func jpg(ps int, seed byte) []byte {
	p := make([]byte, ps)
	for i := range p {
		p[i] = byte(i*31) + seed
	}
	return p
}

func newJ(ps, sector int, nonce, initial uint32) *sqlitetest.Journal {
	return sqlitetest.New(sqlitetest.Options{PageSize: ps}).NewJournal(sector, nonce, initial)
}

func scanJ(t testing.TB, b []byte, dbPS int, o sqlitefile.Options) *sqlitefile.JournalScan {
	t.Helper()
	s, err := sqlitefile.ScanJournal(bytes.NewReader(b), int64(len(b)), dbPS, o)
	if err != nil {
		t.Fatalf("ScanJournal: %v", err)
	}
	if s == nil {
		t.Fatal("ScanJournal returned no scan")
	}
	return s
}

func jwarns(s *sqlitefile.JournalScan, code string) bool {
	for _, w := range s.Warnings {
		if w.Code == code {
			return true
		}
	}
	return false
}

func jcodes(s *sqlitefile.JournalScan) []string {
	var out []string
	for _, w := range s.Warnings {
		out = append(out, w.Code)
	}
	return out
}

func TestJournalChecksumGolden(t *testing.T) {
	// Hand computed. A 512-byte page is sampled at 312 and 112 (the next, -88,
	// is not above zero); every other byte is 0xFF to show it is not summed.
	p := bytes.Repeat([]byte{0xFF}, 512)
	p[312], p[112] = 0x41, 0x10
	if got, want := sqlitefile.JournalChecksum(0x0000FF00, p), uint32(0x0000FF00+0x41+0x10); got != want || want != 0xFF51 {
		t.Errorf("512-byte page: got %#x, want %#x (0xFF51)", got, want)
	}
	// 1024 bytes: 824, 624, 424, 224 and 24 (24 is above zero) are sampled.
	q := bytes.Repeat([]byte{0xFF}, 1024)
	for i, off := range []int{824, 624, 424, 224, 24} {
		q[off] = byte(i + 1)
	}
	if got := sqlitefile.JournalChecksum(1000, q); got != 1015 {
		t.Errorf("1024-byte page: got %d, want 1015", got)
	}
	// The index must be above zero: on a 400-byte page 200 counts, 0 does not.
	r := bytes.Repeat([]byte{0xFF}, 400)
	r[200], r[0] = 7, 99
	if got := sqlitefile.JournalChecksum(5, r); got != 12 {
		t.Errorf("400-byte page: got %d, want 12 (index 0 is not sampled)", got)
	}
	// 32-bit wrap.
	if got := sqlitefile.JournalChecksum(0xFFFFFFFF, p); got != 0x40+0x10 {
		t.Errorf("wrap: got %#x, want %#x", got, 0x40+0x10)
	}
}

func TestJournalChecksumMatchesBuilder(t *testing.T) {
	for _, ps := range []int{512, 1024, 4096, 65536} {
		for _, nonce := range []uint32{0, 1, 0xdeadbeef, 0xffffffff} {
			j := newJ(ps, 512, nonce, 9)
			pages := [][]byte{jpg(ps, 1), jpg(ps, 77), bytes.Repeat([]byte{0xFF}, ps)}
			for i, p := range pages {
				j.Record(uint32(i+2), p)
			}
			b := j.Bytes()
			s := scanJ(t, b, ps, sqlitefile.Options{})
			if len(s.Records) != 3 {
				t.Fatalf("ps %d nonce %#x: %d records", ps, nonce, len(s.Records))
			}
			for i, r := range s.Records {
				if !r.ChecksumOK {
					t.Errorf("ps %d nonce %#x record %d: the builder's checksum is not accepted", ps, nonce, i)
				}
				stored := uint32(b[r.Offset+int64(ps)])<<24 | uint32(b[r.Offset+int64(ps)+1])<<16 | uint32(b[r.Offset+int64(ps)+2])<<8 | uint32(b[r.Offset+int64(ps)+3])
				if got := sqlitefile.JournalChecksum(nonce, pages[i]); got != stored {
					t.Errorf("ps %d nonce %#x record %d: library %#x, builder %#x", ps, nonce, i, got, stored)
				}
			}
		}
	}
}

func TestJournalHeaderParse(t *testing.T) {
	for _, sector := range []int{512, 4096} {
		j := newJ(1024, sector, 0xCAFEBABE, 17)
		j.Record(5, jpg(1024, 1))
		j.Record(9, jpg(1024, 2))
		b := j.Bytes()
		s := scanJ(t, b, 1024, sqlitefile.Options{})
		in := s.Info
		if !in.Present || !in.Hot || !in.HeaderValid || !in.SectorValid || !in.PageSizeMatchesDB || in.ZeroedHeader || in.NonceDerived || in.HasSuperJournal {
			t.Errorf("sector %d: flags %+v", sector, in)
		}
		if in.Size != int64(len(b)) || in.PageSize != 1024 || in.SectorSize != uint32(sector) || in.InitialPages != 17 || in.Nonce != 0xCAFEBABE {
			t.Errorf("sector %d: fields %+v", sector, in)
		}
		if len(in.Segments) != 1 || in.Segments[0] != (sqlitefile.JournalSegment{Offset: 0, DeclaredRecords: 2, Records: 2, HeaderOK: true}) {
			t.Errorf("sector %d: segments %+v", sector, in.Segments)
		}
		if in.RecordsTotal != 2 || in.RecordsValid != 2 || in.RecordsBadChecksum != 0 || in.FirstBadRecord != -1 || in.TrailingBytes != 0 {
			t.Errorf("sector %d: counts %+v", sector, in)
		}
		if !in.Applied || in.AppliedRecords != 2 || in.NotAppliedReason != "" {
			t.Errorf("sector %d: verdict %+v", sector, in)
		}
	}
	// The page size comes from the header: without a database page size, or
	// with another one, the records are still cut by the journal's own.
	j := newJ(1024, 512, 1, 4)
	j.Record(2, jpg(1024, 3))
	b := j.Bytes()
	for _, dbps := range []int{0, 2048} {
		s := scanJ(t, b, dbps, sqlitefile.Options{})
		if !s.Info.HeaderValid || s.Info.PageSize != 1024 || s.Info.PageSizeMatchesDB || len(s.Records) != 1 || s.Info.Applied {
			t.Errorf("db page size %d: %+v", dbps, s.Info)
		}
	}
}

func TestJournalHeaderSectorBeyondFile(t *testing.T) {
	// The engine reads a header of the sector's size: a file shorter than that
	// has no header, nothing is applied.
	b := newJ(1024, 512, 1, 4).Bytes()
	b = b[:300]
	b2 := newJ(1024, 4096, 1, 4).Bytes()
	b2 = b2[:1000]
	for _, x := range [][]byte{b, b2} {
		s := scanJ(t, x, 1024, sqlitefile.Options{})
		if s.Info.HeaderValid || s.Info.Applied || len(s.Records) != 0 || !s.Info.Hot || !jwarns(s, sqlitefile.WarnJournalHeaderInvalid) {
			t.Errorf("size %d: %+v %v", len(x), s.Info, jcodes(s))
		}
	}
}

func TestJournalRecordsParsed(t *testing.T) {
	const ps = 1024
	j := newJ(ps, 512, 7, 12)
	pages := []uint32{5, 9, 2}
	for i, pg := range pages {
		j.Record(pg, jpg(ps, byte(i)))
	}
	b := j.Bytes()
	s := scanJ(t, b, ps, sqlitefile.Options{})
	if len(s.Records) != 3 {
		t.Fatalf("%d records", len(s.Records))
	}
	for i, r := range s.Records {
		wantOff := int64(512 + i*(4+ps+4) + 4)
		if r.Index != i || r.Segment != 0 || r.Page != pages[i] || r.Offset != wantOff || !r.ChecksumOK || !r.Applied {
			t.Errorf("record %d: %+v (offset want %d)", i, r, wantOff)
		}
		if !bytes.Equal(b[r.Offset:r.Offset+ps], jpg(ps, byte(i))) {
			t.Errorf("record %d: the bytes at Offset are not the page data", i)
		}
	}
}

func TestJournalNRecFFFFFFFF(t *testing.T) {
	const ps = 1024
	build := func() *sqlitetest.Journal {
		j := newJ(ps, 512, 7, 12)
		for i := range 3 {
			j.Record(uint32(i+2), jpg(ps, byte(i)))
		}
		j.SetHeader(0xffffffff, 7, 12, 512, ps)
		return j
	}
	s := scanJ(t, build().Bytes(), ps, sqlitefile.Options{})
	if len(s.Records) != 3 || s.Info.Segments[0].DeclaredRecords != 3 || s.Info.Segments[0].Records != 3 || s.Info.TrailingBytes != 0 {
		t.Errorf("count by size: %+v", s.Info)
	}
	// A trailing partial record is not a record; its bytes are trailing.
	b := build().Bytes()
	b = b[:len(b)-100]
	s = scanJ(t, b, ps, sqlitefile.Options{})
	if len(s.Records) != 2 || s.Info.Segments[0].DeclaredRecords != 2 || s.Info.TrailingBytes != int64(4+ps+4-100) {
		t.Errorf("partial last record: %d records, %+v", len(s.Records), s.Info)
	}
	// With a super-journal trailer the records end before it.
	j := build()
	j.SuperJournal("/x/super")
	s = scanJ(t, j.Bytes(), ps, sqlitefile.Options{})
	if len(s.Records) != 3 || !s.Info.HasSuperJournal || s.Info.TrailingBytes != 0 {
		t.Errorf("0xffffffff with a trailer: %d records, %+v", len(s.Records), s.Info)
	}
	// A declared count above what the file holds lists only the whole records present.
	j = newJ(ps, 512, 7, 12)
	for i := range 3 {
		j.Record(uint32(i+2), jpg(ps, byte(i)))
	}
	j.SetHeader(1000, 7, 12, 512, ps)
	s = scanJ(t, j.Bytes(), ps, sqlitefile.Options{})
	if len(s.Records) != 3 || s.Info.Segments[0].DeclaredRecords != 1000 || s.Info.Segments[0].Records != 3 {
		t.Errorf("declared 1000: %+v", s.Info.Segments)
	}
}

func TestJournalNRecZeroSegment(t *testing.T) {
	const ps = 1024
	j := newJ(ps, 512, 7, 12)
	j.Record(2, jpg(ps, 1))
	j.Record(3, jpg(ps, 2))
	j.NewSegment() // seg 2: nRec 0, no records
	j.NewSegment() // seg 3 follows at the next sector
	j.Record(4, jpg(ps, 3))
	s := scanJ(t, j.Bytes(), ps, sqlitefile.Options{})
	var counts []uint32
	for _, sg := range s.Info.Segments {
		counts = append(counts, sg.Records)
	}
	if fmt.Sprint(counts) != "[2 0 1]" || len(s.Records) != 3 || s.Records[2].Segment != 2 || s.Records[2].Page != 4 {
		t.Errorf("segments %v, records %+v", counts, s.Records)
	}
	// Records after a zero-count header belong to no segment: the next header
	// is looked for at the next sector and is not there (the engine stops).
	j = newJ(ps, 512, 7, 12)
	j.Record(2, jpg(ps, 1))
	j.NewSegment()
	j.Record(3, jpg(ps, 2))
	j.SetHeader(0, 7, 12, 512, ps)
	s = scanJ(t, j.Bytes(), ps, sqlitefile.Options{})
	if len(s.Records) != 1 || len(s.Info.Segments) != 2 || s.Info.TrailingBytes != int64(4+ps+4) {
		t.Errorf("records after an empty segment: %d records, %d segments, trailing %d", len(s.Records), len(s.Info.Segments), s.Info.TrailingBytes)
	}
}

func TestJournalMultiSegment(t *testing.T) {
	const ps = 1024
	j := newJ(ps, 512, 7, 12)
	counts := []int{2, 1, 3}
	page := uint32(2)
	for si, n := range counts {
		if si > 0 {
			j.NewSegment()
		}
		for range n {
			j.Record(page, jpg(ps, byte(page)))
			page++
		}
	}
	b := j.Bytes()
	s := scanJ(t, b, ps, sqlitefile.Options{})
	// segment 1: header at 0, records end at 512+2*1032 = 2576, next header at 3072;
	// segment 2: records end at 3072+512+1032 = 4616, next at 5120.
	wantOff := []int64{0, 3072, 5120}
	if len(s.Info.Segments) != 3 || len(s.Records) != 6 {
		t.Fatalf("segments %+v, %d records", s.Info.Segments, len(s.Records))
	}
	for i, sg := range s.Info.Segments {
		if sg.Offset != wantOff[i] || int(sg.Records) != counts[i] || int(sg.DeclaredRecords) != counts[i] || !sg.HeaderOK {
			t.Errorf("segment %d: %+v", i, sg)
		}
	}
	idx := 0
	for si, n := range counts {
		for k := range n {
			r := s.Records[idx]
			off := wantOff[si] + 512 + int64(k)*(4+ps+4) + 4
			if r.Index != idx || r.Segment != si || r.Offset != off || r.Page != uint32(idx+2) || !r.ChecksumOK {
				t.Errorf("record %d: %+v, want segment %d offset %d", idx, r, si, off)
			}
			idx++
		}
	}
	if s.Info.TrailingBytes != 0 || !s.Info.Applied || s.Info.AppliedRecords != 6 {
		t.Errorf("%+v", s.Info)
	}
}

func TestJournalSectorSizeValidated(t *testing.T) {
	const ps = 1024
	for _, sec := range []uint32{0, 3, 31, 70000, 0xffffffff} {
		j := newJ(ps, 512, 7, 12)
		j.Record(2, jpg(ps, 1))
		j.SetHeader(1, 7, 12, sec, ps)
		s := scanJ(t, j.Bytes(), ps, sqlitefile.Options{})
		if s.Info.HeaderValid || s.Info.SectorValid || s.Info.SectorSize != sec || s.Info.Applied || len(s.Records) != 0 || len(s.Info.Segments) > 1 {
			t.Errorf("sector %d: %+v", sec, s.Info)
		}
		if !jwarns(s, sqlitefile.WarnJournalSectorInvalid) {
			t.Errorf("sector %d: journal-sector-invalid missing: %v", sec, jcodes(s))
		}
		if s.Info.NotAppliedReason != "header-invalid" {
			t.Errorf("sector %d: reason %q", sec, s.Info.NotAppliedReason)
		}
	}
	// The smallest and largest sectors are valid (the engine takes 32..65536).
	for _, sec := range []int{32, 65536} {
		j := newJ(ps, sec, 7, 12)
		j.Record(2, jpg(ps, 1))
		s := scanJ(t, j.Bytes(), ps, sqlitefile.Options{})
		if !s.Info.SectorValid || !s.Info.HeaderValid || len(s.Records) != 1 || !s.Info.Applied {
			t.Errorf("sector %d: %+v", sec, s.Info)
		}
	}
	// A long chain of the smallest sectors terminates at MaxJournalSegments.
	j := newJ(ps, 32, 7, 12)
	for range 40 {
		j.NewSegment()
	}
	s := scanJ(t, j.Bytes(), ps, sqlitefile.Options{Limits: sqlitefile.Limits{MaxJournalSegments: 5}})
	if len(s.Info.Segments) != 5 || !jwarns(s, sqlitefile.WarnLimitReached) {
		t.Errorf("%d segments, %v", len(s.Info.Segments), jcodes(s))
	}
}

func TestJournalPageSizeInvalid(t *testing.T) {
	const ps = 1024
	for _, hps := range []uint32{100, 513, 70000, 0x80000000} {
		j := newJ(ps, 512, 7, 12)
		j.Record(2, jpg(ps, 1))
		j.SetHeader(1, 7, 12, 512, hps)
		s := scanJ(t, j.Bytes(), ps, sqlitefile.Options{})
		if s.Info.HeaderValid || s.Info.Applied || len(s.Records) != 0 || !jwarns(s, sqlitefile.WarnJournalHeaderInvalid) || s.Info.NotAppliedReason != "header-invalid" {
			t.Errorf("page size %d: %+v %v", hps, s.Info, jcodes(s))
		}
	}
}

// TestJournalPageSizeZeroMeansDatabasePageSize: the engine accepts a header
// page size of 0 and reads it as the database's page size (TestEngineJournalRules,
// "header page size 0": the journal is applied).
func TestJournalPageSizeZeroMeansDatabasePageSize(t *testing.T) {
	const ps = 1024
	j := newJ(ps, 512, 7, 12)
	j.Record(2, jpg(ps, 1))
	j.SetHeader(1, 7, 12, 512, 0)
	s := scanJ(t, j.Bytes(), ps, sqlitefile.Options{})
	if !s.Info.HeaderValid || s.Info.PageSize != ps || !s.Info.PageSizeMatchesDB || len(s.Records) != 1 || !s.Records[0].ChecksumOK || !s.Info.Applied {
		t.Errorf("%+v", s.Info)
	}
	// With no database page size there is nothing to read it as.
	s = scanJ(t, j.Bytes(), 0, sqlitefile.Options{})
	if s.Info.HeaderValid || len(s.Records) != 0 || s.Info.Applied || !jwarns(s, sqlitefile.WarnJournalNoPageSize) {
		t.Errorf("no database page size: %+v %v", s.Info, jcodes(s))
	}
}

func TestJournalHotDetection(t *testing.T) {
	const ps = 1024
	good := func() []byte {
		j := newJ(ps, 512, 7, 12)
		j.Record(2, jpg(ps, 1))
		return j.Bytes()
	}
	zeroFirst := good()
	zeroFirst[0] = 0
	badMagic := good()
	badMagic[3] ^= 0xff
	empty := newJ(ps, 512, 7, 12).Bytes()
	cases := []struct {
		name                         string
		data                         []byte
		absent                       bool
		present, hot, valid, applied bool
		reason                       string
		warn                         string
		records                      int
		appliedRecords               uint32
	}{
		{name: "absent", absent: true, reason: "not-hot"},
		{name: "size 0", data: []byte{}, present: true, reason: "not-hot"},
		{name: "first byte 0", data: zeroFirst, present: true, reason: "not-hot"},
		{name: "magic mismatch, first byte non-zero", data: badMagic, present: true, hot: true, reason: "header-invalid", warn: sqlitefile.WarnJournalHeaderInvalid},
		{name: "valid header, nRec 0", data: empty, present: true, hot: true, valid: true, applied: true},
		{name: "valid with a record", data: good(), present: true, hot: true, valid: true, applied: true, records: 1, appliedRecords: 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var s *sqlitefile.JournalScan
			var err error
			if c.absent {
				s, err = sqlitefile.ScanJournal(nil, 0, ps, sqlitefile.Options{})
			} else {
				s, err = sqlitefile.ScanJournal(bytes.NewReader(c.data), int64(len(c.data)), ps, sqlitefile.Options{})
			}
			if err != nil || s == nil {
				t.Fatalf("%v %v", s, err)
			}
			in := s.Info
			if in.Present != c.present || in.Hot != c.hot || in.HeaderValid != c.valid || in.Applied != c.applied || in.NotAppliedReason != c.reason {
				t.Errorf("%+v", in)
			}
			if len(s.Records) != c.records || in.AppliedRecords != c.appliedRecords || in.FirstBadRecord != -1 {
				t.Errorf("%d records, %+v", len(s.Records), in)
			}
			if c.warn != "" && !jwarns(s, c.warn) {
				t.Errorf("want %s, got %v", c.warn, jcodes(s))
			}
			if c.reason == "not-hot" && jwarns(s, sqlitefile.WarnJournalHot) {
				t.Errorf("journal-hot warned for a journal that is not hot")
			}
		})
	}
}

func TestJournalBadChecksumRecordKept(t *testing.T) {
	const ps = 1024
	j := newJ(ps, 512, 7, 12)
	for i := range 5 {
		if i == 2 {
			j.RawRecord(uint32(i+2), jpg(ps, byte(i)), 0x1234)
		} else {
			j.Record(uint32(i+2), jpg(ps, byte(i)))
		}
	}
	s := scanJ(t, j.Bytes(), ps, sqlitefile.Options{})
	if len(s.Records) != 5 || s.Info.RecordsTotal != 5 || s.Info.RecordsValid != 4 || s.Info.RecordsBadChecksum != 1 || s.Info.FirstBadRecord != 2 {
		t.Fatalf("%d records, %+v", len(s.Records), s.Info)
	}
	for i, r := range s.Records {
		if r.ChecksumOK != (i != 2) || r.Applied != (i < 2) {
			t.Errorf("record %d: %+v (applied is true before the first bad checksum only)", i, r)
		}
	}
	if s.Info.AppliedRecords != 2 || !s.Info.Applied || !jwarns(s, sqlitefile.WarnJournalPageInvalid) {
		t.Errorf("%+v %v", s.Info, jcodes(s))
	}
}

func TestJournalPersistZeroedHeader(t *testing.T) {
	const ps = 1024
	nonce := uint32(0xABCD1234)
	build := func(sector, n int) []byte {
		j := newJ(ps, sector, nonce, 12)
		for i := range n {
			j.Record(uint32(i+2), jpg(ps, byte(i+9)))
		}
		j.Zero()
		return j.Bytes()
	}
	for _, sector := range []int{512, 4096} {
		s := scanJ(t, build(sector, 3), ps, sqlitefile.Options{})
		in := s.Info
		if !in.ZeroedHeader || in.Hot || in.HeaderValid || in.PageSize != ps || in.SectorSize != uint32(sector) || !in.NonceDerived || in.Nonce != nonce {
			t.Errorf("sector %d: %+v", sector, in)
		}
		if len(s.Records) != 3 || in.RecordsValid != 3 || in.Applied || in.AppliedRecords != 0 || in.NotAppliedReason != "zeroed-header" {
			t.Errorf("sector %d: %d records, %+v", sector, len(s.Records), in)
		}
		for i, r := range s.Records {
			if !r.ChecksumOK || r.Applied || r.Page != uint32(i+2) {
				t.Errorf("sector %d record %d: %+v", sector, i, r)
			}
		}
		if len(in.Segments) != 1 || in.Segments[0].HeaderOK {
			t.Errorf("sector %d: segments %+v", sector, in.Segments)
		}
	}
	// One record cannot make a consensus: it is listed, unverified.
	s := scanJ(t, build(512, 1), ps, sqlitefile.Options{})
	if len(s.Records) != 1 || s.Records[0].ChecksumOK || s.Info.NonceDerived || s.Info.RecordsValid != 0 || s.Info.Applied {
		t.Errorf("one record: %+v %+v", s.Info, s.Records)
	}
	// Records whose checksums disagree make no consensus either.
	j := newJ(ps, 512, nonce, 12)
	for i := range 3 {
		j.RawRecord(uint32(i+2), jpg(ps, byte(i)), uint32(i*7919+13))
	}
	j.Zero()
	s = scanJ(t, j.Bytes(), ps, sqlitefile.Options{})
	if len(s.Records) != 3 || s.Info.NonceDerived {
		t.Fatalf("disagreeing records: %+v", s.Info)
	}
	for i, r := range s.Records {
		if r.ChecksumOK {
			t.Errorf("record %d verified without a consensus", i)
		}
	}
	// Without a database page size no record can be cut.
	s = scanJ(t, build(512, 3), 0, sqlitefile.Options{})
	if len(s.Records) != 0 || !jwarns(s, sqlitefile.WarnJournalNoPageSize) {
		t.Errorf("no page size: %d records, %v", len(s.Records), jcodes(s))
	}
}

func TestJournalSuperJournalTrailer(t *testing.T) {
	const ps = 1024
	j := newJ(ps, 512, 7, 12)
	j.Record(2, jpg(ps, 1))
	j.Record(3, jpg(ps, 2))
	j.SuperJournal("/some/dir/main.db-mj1234ABCD")
	b := j.Bytes()
	s := scanJ(t, b, ps, sqlitefile.Options{})
	if !s.Info.HasSuperJournal || len(s.Records) != 2 || s.Info.TrailingBytes != 0 || s.Info.RecordsBadChecksum != 0 {
		t.Errorf("%+v", s.Info)
	}
	// Not applied by default: the named file is not in evidence.
	if s.Info.Applied || s.Info.NotAppliedReason != "super-journal-unknown" || !jwarns(s, sqlitefile.WarnJournalSuperUnknown) {
		t.Errorf("default: %+v %v", s.Info, jcodes(s))
	}
	for _, r := range s.Records {
		if r.Applied {
			t.Errorf("a record is applied under an unknown super-journal: %+v", r)
		}
	}
	s = scanJ(t, b, ps, sqlitefile.Options{SuperJournalPresent: true})
	if !s.Info.Applied || s.Info.AppliedRecords != 2 || jwarns(s, sqlitefile.WarnJournalSuperUnknown) {
		t.Errorf("declared present: %+v %v", s.Info, jcodes(s))
	}
	// A corrupted trailer is not a trailer: the bytes are trailing.
	bad := bytes.Clone(b)
	bad[len(bad)-1] ^= 0xff
	s = scanJ(t, bad, ps, sqlitefile.Options{})
	if s.Info.HasSuperJournal || s.Info.TrailingBytes == 0 {
		t.Errorf("a trailer with a bad magic: %+v", s.Info)
	}
}

func TestJournalCaps(t *testing.T) {
	const ps = 1024
	j := newJ(ps, 512, 7, 12)
	for i := range 5 {
		j.Record(uint32(i+2), jpg(ps, byte(i)))
	}
	s := scanJ(t, j.Bytes(), ps, sqlitefile.Options{Limits: sqlitefile.Limits{MaxJournalRecords: 3}})
	if len(s.Records) != 3 || !jwarns(s, sqlitefile.WarnLimitReached) {
		t.Errorf("%d records, %v", len(s.Records), jcodes(s))
	}
	// A partial rollback would mix two states: past the cap nothing is applied.
	if s.Info.Applied || s.Info.AppliedRecords != 0 || s.Info.NotAppliedReason != "limit-reached" {
		t.Errorf("capped journal applied: %+v", s.Info)
	}
	for _, r := range s.Records {
		if r.Applied {
			t.Errorf("record applied from a capped journal: %+v", r)
		}
	}
	j = newJ(ps, 512, 7, 12)
	for i := range 5 {
		if i > 0 {
			j.NewSegment()
		}
		j.Record(uint32(i+2), jpg(ps, byte(i)))
	}
	s = scanJ(t, j.Bytes(), ps, sqlitefile.Options{Limits: sqlitefile.Limits{MaxJournalSegments: 2}})
	if len(s.Info.Segments) != 2 || len(s.Records) != 2 || !jwarns(s, sqlitefile.WarnLimitReached) || s.Info.Applied || s.Info.NotAppliedReason != "limit-reached" {
		t.Errorf("segment cap: %d segments, %d records, %v, %+v", len(s.Info.Segments), len(s.Records), jcodes(s), s.Info)
	}
}

func TestJournalScanBudgetCharged(t *testing.T) {
	const ps = 1024
	j := newJ(ps, 512, 7, 12)
	for i := range 50 {
		j.Record(uint32(i+2), jpg(ps, byte(i)))
	}
	b := j.Bytes()
	pb := &peakBudget{}
	scanJ(t, b, ps, sqlitefile.Options{Budget: pb})
	if pb.peak < 50*16 {
		t.Errorf("50 records charged only %d bytes", pb.peak)
	}
	// A budget too small for the arrays is a budget error, and the charge is given back.
	nb := &nthBudget{n: 1}
	_, err := sqlitefile.ScanJournal(bytes.NewReader(b), int64(len(b)), ps, sqlitefile.Options{Budget: nb})
	if !errors.Is(err, sqlitefile.ErrBudget) {
		t.Errorf("err %v, want ErrBudget", err)
	}
	if nb.used != 0 {
		t.Errorf("%d bytes still charged after the failed scan", nb.used)
	}
	// A claim of 2^32-1 records on a small file costs what the file holds.
	j = newJ(ps, 512, 7, 12)
	j.Record(2, jpg(ps, 1))
	j.SetHeader(0xffffffff, 7, 12, 512, ps)
	pb = &peakBudget{}
	scanJ(t, j.Bytes(), ps, sqlitefile.Options{Budget: pb})
	if pb.peak > 1<<20 {
		t.Errorf("a claimed 2^32-1 records cost %d bytes of budget", pb.peak)
	}
}

func TestJournalDuplicatePageWarns(t *testing.T) {
	const ps = 1024
	j := newJ(ps, 512, 7, 12)
	j.Record(3, jpg(ps, 1))
	j.Record(4, jpg(ps, 2))
	j.Record(3, jpg(ps, 3))
	s := scanJ(t, j.Bytes(), ps, sqlitefile.Options{})
	found := false
	for _, w := range s.Warnings {
		if w.Code == sqlitefile.WarnJournalDuplicatePage && w.Page == 3 && w.File == sqlitefile.FileJournal {
			found = true
		}
	}
	if !found {
		t.Errorf("journal-duplicate-page for page 3 missing: %v", s.Warnings)
	}
	// The last one wins: only the last record of page 3 is the applied one.
	if s.Records[0].Applied || !s.Records[1].Applied || !s.Records[2].Applied || s.Info.AppliedRecords != 2 {
		t.Errorf("%+v", s.Records)
	}
}

// jfailReader fails every read at or beyond from.
type jfailReader struct {
	r    io.ReaderAt
	from int64
	err  error
}

func (f jfailReader) ReadAt(p []byte, off int64) (int, error) {
	if off+int64(len(p)) > f.from {
		return 0, f.err
	}
	return f.r.ReadAt(p, off)
}

func TestJournalIOError(t *testing.T) {
	const ps = 1024
	j := newJ(ps, 512, 7, 12)
	for i := range 4 {
		j.Record(uint32(i+2), jpg(ps, byte(i)))
	}
	b := j.Bytes()
	boom := errors.New("disk on fire")
	for _, from := range []int64{0, 600, 2000} {
		s, err := sqlitefile.ScanJournal(jfailReader{bytes.NewReader(b), from, boom}, int64(len(b)), ps, sqlitefile.Options{})
		if !errors.Is(err, boom) || s != nil {
			t.Errorf("from %d: scan %v, err %v: an I/O error is returned as it is, never a verdict", from, s, err)
		}
	}
}

func TestJournalRecordPagesAreChecked(t *testing.T) {
	// A record for page 0, for the lock-byte page, or above the initial size
	// is flagged (the engine ends playback at the first two and skips the third).
	const ps = 1024
	const lock = (1<<30)/ps + 1
	j := newJ(ps, 512, 7, lock+2)
	j.Record(0, jpg(ps, 1))
	j.Record(lock, jpg(ps, 2))
	j.Record(lock+5, jpg(ps, 3))
	s := scanJ(t, j.Bytes(), ps, sqlitefile.Options{})
	if len(s.Records) != 3 || !jwarns(s, sqlitefile.WarnJournalPageInvalid) {
		t.Fatalf("%d records, %v", len(s.Records), jcodes(s))
	}
	for i, r := range s.Records {
		if r.Applied {
			t.Errorf("record %d (page %d) applied", i, r.Page)
		}
	}
}

func TestJournalRecordJustAboveInitialSizeIsSkipped(t *testing.T) {
	const ps = 1024
	j := newJ(ps, 512, 7, 12)
	j.Record(12, jpg(ps, 1)) // the last page of the initial size: applied
	j.Record(13, jpg(ps, 2)) // one above: skipped
	s := scanJ(t, j.Bytes(), ps, sqlitefile.Options{})
	if !s.Records[0].Applied || s.Records[1].Applied || s.Info.AppliedRecords != 1 {
		t.Errorf("%+v", s.Records)
	}
}

func TestJournalLockBytePageEndsPlayback(t *testing.T) {
	const ps = 1024
	const lock = (1<<30)/ps + 1
	j := newJ(ps, 512, 7, lock+1)
	j.Record(2, jpg(ps, 1))
	j.Record(lock, jpg(ps, 2))
	j.Record(3, jpg(ps, 3))
	s := scanJ(t, j.Bytes(), ps, sqlitefile.Options{})
	if !s.Records[0].Applied || s.Records[1].Applied || s.Records[2].Applied {
		t.Errorf("%+v", s.Records)
	}
}

func TestJournalTrailerChecksumIsVerified(t *testing.T) {
	const ps = 1024
	j := newJ(ps, 512, 7, 12)
	j.Record(2, jpg(ps, 1))
	j.SuperJournal("/some/main.db-mj1")
	b := j.Bytes()
	b[len(b)-12] ^= 0xff // the stored sum of the name
	s := scanJ(t, b, ps, sqlitefile.Options{})
	if s.Info.HasSuperJournal || s.Info.TrailingBytes == 0 {
		t.Errorf("a trailer whose sum fails is not a trailer: %+v", s.Info)
	}
}
