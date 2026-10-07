package sqlitefile_test

import (
	"bytes"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// FuzzJournal: arbitrary journal bytes and a database page size never panic,
// and the result obeys the structural rules, re-checked here independently.
func FuzzJournal(f *testing.F) {
	good := func(sector int) []byte {
		j := newJ(512, sector, 0xabcdef, 9)
		j.Record(2, jpg(512, 1))
		j.Record(3, jpg(512, 2))
		j.NewSegment()
		j.Record(4, jpg(512, 3))
		return j.Bytes()
	}
	f.Add(good(512), uint32(512))
	f.Add(good(4096), uint32(512))
	f.Add(good(32), uint32(0))
	bad := newJ(512, 512, 7, 9)
	bad.Record(2, jpg(512, 1))
	bad.RawRecord(3, jpg(512, 2), 1)
	bad.Record(2, jpg(512, 3))
	f.Add(bad.Bytes(), uint32(512))
	z := newJ(512, 512, 7, 9)
	for i := range 3 {
		z.Record(uint32(i+2), jpg(512, byte(i)))
	}
	z.Zero()
	f.Add(z.Bytes(), uint32(512))
	s := newJ(512, 512, 7, 9)
	s.Record(2, jpg(512, 1))
	s.SuperJournal("/x/y-mj123")
	f.Add(s.Bytes(), uint32(512))
	h := newJ(512, 512, 7, 9)
	h.Record(2, jpg(512, 1))
	h.SetHeader(0xffffffff, 7, 9, 512, 0)
	f.Add(h.Bytes(), uint32(512))
	f.Add(newJ(65536, 512, 7, 9).Bytes(), uint32(65536))
	f.Add([]byte{0xd9, 0xd5, 0x05, 0xf9, 0x20, 0xa1, 0x63, 0xd7}, uint32(512))
	f.Fuzz(func(t *testing.T, data []byte, dbPS uint32) {
		cb := &capBudget{limit: 256 << 20}
		for _, super := range []bool{false, true} {
			s, err := sqlitefile.ScanJournal(bytes.NewReader(data), int64(len(data)), int(dbPS), sqlitefile.Options{Budget: cb, SuperJournalPresent: super})
			if err != nil {
				t.Fatalf("ScanJournal: %v", err)
			}
			in := s.Info
			if in.RecordsTotal != uint32(len(s.Records)) || in.RecordsValid+in.RecordsBadChecksum != in.RecordsTotal {
				t.Fatalf("counts %+v for %d records", in, len(s.Records))
			}
			ps := int64(in.PageSize)
			var segRecs uint32
			for _, sg := range in.Segments {
				segRecs += sg.Records
				if sg.Offset < 0 || sg.Offset > int64(len(data)) {
					t.Fatalf("segment %+v outside %d bytes", sg, len(data))
				}
			}
			if segRecs != in.RecordsTotal {
				t.Fatalf("segments hold %d records, total %d", segRecs, in.RecordsTotal)
			}
			applied := uint32(0)
			first := -1
			for i, r := range s.Records {
				if r.Index != i || r.Offset < 4 || r.Offset+ps+4 > int64(len(data)) {
					t.Fatalf("record %d: %+v in %d bytes (page size %d)", i, r, len(data), ps)
				}
				if first < 0 && !r.ChecksumOK {
					first = i
				}
				if r.Applied {
					applied++
					if !r.ChecksumOK || (first >= 0 && i >= first && !in.ZeroedHeader) || !in.Applied {
						t.Fatalf("record %d applied against the rules: %+v %+v", i, r, in)
					}
				}
			}
			if !in.ZeroedHeader && in.FirstBadRecord != first {
				t.Fatalf("FirstBadRecord %d, want %d", in.FirstBadRecord, first)
			}
			if applied != in.AppliedRecords || (!in.Applied && applied != 0) || (in.Applied && in.NotAppliedReason != "") || (!in.Applied && in.NotAppliedReason == "") {
				t.Fatalf("verdict %+v (%d applied)", in, applied)
			}
			if cb.peak > 1<<20+400*int64(len(s.Records)) {
				t.Fatalf("%d bytes charged for %d records", cb.peak, len(s.Records))
			}
		}
	})
}
