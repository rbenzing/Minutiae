package sqlitefile_test

// A hot journal whose header declares more records than the file holds is cut
// short (or its count is damaged); the scan says so (LX-4 Task 2, L125).

import (
	"runtime"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// truncWarnings returns the journal-truncated warnings of s.
func truncWarnings(s *sqlitefile.JournalScan) []sqlitefile.Warning {
	var out []sqlitefile.Warning
	for _, w := range s.Warnings {
		if w.Code == sqlitefile.WarnJournalTruncated {
			out = append(out, w)
		}
	}
	return out
}

func fiveRecordJournal(ps int) *journalBuilt {
	j := newJ(ps, 512, 7, 12)
	for i := range 5 {
		j.Record(uint32(i+2), jpg(ps, byte(i)))
	}
	return &journalBuilt{b: j.Bytes()}
}

type journalBuilt struct{ b []byte }

func TestTruncatedJournalNamesTheCause(t *testing.T) {
	const ps = 1024
	full := fiveRecordJournal(ps).b
	b := full[:512+3*(4+ps+4)+(4+ps+4)/2]
	s := scanJ(t, b, ps, sqlitefile.Options{})
	if !s.Info.Truncated {
		t.Fatalf("Truncated is false: %+v", s.Info)
	}
	sg := s.Info.Segments[0]
	if sg.DeclaredRecords != 5 || sg.Records != 3 || len(s.Records) != 3 || s.Info.AppliedRecords != 3 {
		t.Errorf("declared %d, held %d, %d records, %d applied", sg.DeclaredRecords, sg.Records, len(s.Records), s.Info.AppliedRecords)
	}
	ws := truncWarnings(s)
	if len(ws) != 1 {
		t.Fatalf("%d journal-truncated warnings: %v", len(ws), jcodes(s))
	}
	w := ws[0]
	if w.Offset != sg.Offset || w.File != sqlitefile.FileJournal || !strings.Contains(w.Msg, "declares 5") || !strings.Contains(w.Msg, "holds 3") {
		t.Errorf("warning %+v", w)
	}
	if !jwarns(s, sqlitefile.WarnJournalHot) {
		t.Errorf("journal-hot missing: %v", jcodes(s))
	}
	for _, w := range s.Warnings {
		if w.Code == sqlitefile.WarnJournalHot && !strings.Contains(w.Msg, "truncated") {
			t.Errorf("the journal-hot message does not mention the truncation: %q", w.Msg)
		}
	}
}

func TestTruncatedSecondSegmentIsNamed(t *testing.T) {
	const ps = 1024
	j := newJ(ps, 512, 7, 12)
	j.Record(2, jpg(ps, 1))
	j.Record(3, jpg(ps, 2))
	j.NewSegment()
	for i := range 3 {
		j.Record(uint32(i+4), jpg(ps, byte(i)))
	}
	b := j.Bytes()
	// segment 1 starts at 3072; keep its header, one whole record and half of the next
	b = b[:3072+512+(4+ps+4)+(4+ps+4)/2]
	s := scanJ(t, b, ps, sqlitefile.Options{})
	ws := truncWarnings(s)
	if !s.Info.Truncated || len(ws) != 1 {
		t.Fatalf("truncated %v, warnings %v", s.Info.Truncated, jcodes(s))
	}
	if ws[0].Offset != 3072 || !strings.Contains(ws[0].Msg, "segment 1 ") || !strings.Contains(ws[0].Msg, "declares 3") || !strings.Contains(ws[0].Msg, "holds 1") {
		t.Errorf("warning %+v", ws[0])
	}
	if len(s.Records) != 3 {
		t.Errorf("%d records", len(s.Records))
	}
}

func TestJournalDeclaredEqualsHeldIsNotTruncated(t *testing.T) {
	const ps = 1024
	b := fiveRecordJournal(ps).b
	b = append(b, make([]byte, 300)...) // sector padding, less than a record
	s := scanJ(t, b, ps, sqlitefile.Options{})
	if s.Info.Truncated || len(truncWarnings(s)) != 0 || len(s.Records) != 5 {
		t.Errorf("truncated %v, %v, %d records", s.Info.Truncated, jcodes(s), len(s.Records))
	}
}

func TestJournalNRecFFFFFFFFWithAPartialLastRecordIsNotTruncated(t *testing.T) {
	const ps = 1024
	j := newJ(ps, 512, 7, 12)
	for i := range 3 {
		j.Record(uint32(i+2), jpg(ps, byte(i)))
	}
	j.SetHeader(0xffffffff, 7, 12, 512, ps)
	b := j.Bytes()
	b = b[:len(b)-100]
	s := scanJ(t, b, ps, sqlitefile.Options{})
	if s.Info.Truncated || len(truncWarnings(s)) != 0 || len(s.Records) != 2 {
		t.Errorf("truncated %v, %v, %d records", s.Info.Truncated, jcodes(s), len(s.Records))
	}
}

func TestJournalWithZeroDeclaredCountIsNotTruncated(t *testing.T) {
	const ps = 1024
	j := newJ(ps, 512, 7, 12)
	for i := range 3 {
		j.Record(uint32(i+2), jpg(ps, byte(i)))
	}
	j.SetHeader(0, 7, 12, 512, ps)
	s := scanJ(t, j.Bytes(), ps, sqlitefile.Options{})
	if s.Info.Truncated || len(truncWarnings(s)) != 0 {
		t.Errorf("truncated %v, %v", s.Info.Truncated, jcodes(s))
	}
}

func TestZeroedHeaderJournalIsNotTruncated(t *testing.T) {
	const ps = 1024
	j := newJ(ps, 512, 7, 12)
	for i := range 4 {
		j.Record(uint32(i+2), jpg(ps, byte(i)))
	}
	j.Zero()
	b := j.Bytes()
	b = b[:len(b)-300]
	s := scanJ(t, b, ps, sqlitefile.Options{})
	if !s.Info.ZeroedHeader || s.Info.Truncated || len(truncWarnings(s)) != 0 {
		t.Errorf("zeroed %v, truncated %v, %v", s.Info.ZeroedHeader, s.Info.Truncated, jcodes(s))
	}
}

func TestJournalCapIsNotTruncation(t *testing.T) {
	const ps = 1024
	s := scanJ(t, fiveRecordJournal(ps).b, ps, sqlitefile.Options{Limits: sqlitefile.Limits{MaxJournalRecords: 3}})
	if !jwarns(s, sqlitefile.WarnLimitReached) || s.Info.Truncated || len(truncWarnings(s)) != 0 {
		t.Errorf("truncated %v, %v", s.Info.Truncated, jcodes(s))
	}
}

func TestTruncationWarningIsOnePerScan(t *testing.T) {
	const ps = 1024
	const size = 1 << 20
	j := newJ(ps, 512, 7, 12)
	j.Record(2, jpg(ps, 1))
	j.SetHeader(4_000_000_000, 7, 12, 512, ps)
	b := j.Bytes()
	b = append(b, make([]byte, size-len(b))...)
	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)
	s := scanJ(t, b, ps, sqlitefile.Options{})
	runtime.ReadMemStats(&m1)
	if !s.Info.Truncated || len(truncWarnings(s)) != 1 {
		t.Fatalf("truncated %v, %v", s.Info.Truncated, jcodes(s))
	}
	if int64(len(s.Records)) > size/(4+ps+4) {
		t.Errorf("%d records from a %d byte file", len(s.Records), size)
	}
	if d := m1.TotalAlloc - m0.TotalAlloc; d > 2*size+1<<20 {
		t.Errorf("%d bytes allocated for a %d byte file", d, size)
	}
}
