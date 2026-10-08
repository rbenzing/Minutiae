package sqlitefile_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// TestStatedPageNumberMustBePossible: a WAL frame or journal record that
// states page 0, the lock-byte page, a page past Limits.MaxPages (or, for a
// journal record, past the journal's initial size) is flagged: the image
// carries Note "invalid-page-number: N", its rows have relation unknown and
// NoteInvalidPageNumber, and the source file gets one counted warning (final
// review B, I-1). A possible page is untouched.
func TestStatedPageNumberMustBePossible(t *testing.T) {
	lock := sqlitefile.LockBytePage(hps)
	type src struct {
		name    string
		invalid []uint32
		build   func(s *rowScen, pgs []uint32) files
		warn    string
	}
	srcs := []src{
		{"wal", []uint32{0, lock, 4294967280, 4294967295}, func(s *rowScen, pgs []uint32) files {
			w := s.b.NewWAL(false, 0x1000, 0x1001, 0)
			for _, pg := range pgs {
				w.Frame(pg, s.v0.Page(2), 0)
			}
			return files{db: s.db0, wal: w.Bytes()}
		}, sqlitefile.WarnWALPageInvalid},
		{"journal", []uint32{0, lock, 7000, 4294967280, 4294967295}, func(s *rowScen, pgs []uint32) files {
			j := s.b.NewJournal(hps, 0xfeed, s.v0.Pages())
			for _, pg := range pgs {
				j.Record(pg, s.v0.Page(2))
			}
			return files{db: s.db0, journal: j.Bytes()}
		}, sqlitefile.WarnJournalPageInvalid},
	}
	for _, sc := range srcs {
		t.Run(sc.name, func(t *testing.T) {
			s := newRowScen(t)
			pgs := append([]uint32{2}, sc.invalid...) // page 2 is a possible page: the control
			f := sc.build(s, pgs)
			_, h := openAll(t, f.db, f.wal, f.journal)

			bad := map[uint32]bool{}
			for _, p := range sc.invalid {
				bad[p] = true
			}
			var images int
			if err := h.Pages(context.Background(), func(p sqlitefile.PageImage) bool {
				if p.WAL == nil && (p.Journal == nil || p.Origin != sqlitefile.OriginJournalBefore) {
					return true // not an image of a frame or record
				}
				images++
				flagged := strings.HasPrefix(p.Note, sqlitefile.NoteInvalidPageNumber+": ")
				if flagged != bad[p.Number] {
					t.Errorf("page %d (%v): Note %q, impossible = %v", p.Number, p.Origin, p.Note, bad[p.Number])
				}
				return true
			}); err != nil {
				t.Fatal(err)
			}
			if images != len(pgs) {
				t.Fatalf("%d images from the companion file, want %d", images, len(pgs))
			}
			rows, _ := collectRows(t, h)
			seen := 0
			for _, r := range rows {
				if r.WAL == nil && r.Origin != sqlitefile.OriginJournalBefore {
					continue
				}
				seen++
				has := false
				for _, n := range r.Notes {
					has = has || n == sqlitefile.NoteInvalidPageNumber
				}
				if has != bad[r.Loc.Page] {
					t.Errorf("row at page %d: notes %v, impossible = %v", r.Loc.Page, r.Notes, bad[r.Loc.Page])
				}
				if bad[r.Loc.Page] && r.Relation != sqlitefile.RelUnknown {
					t.Errorf("row at impossible page %d has relation %v, want unknown", r.Loc.Page, r.Relation)
				}
			}
			if seen == 0 {
				t.Fatal("no rows from the companion file")
			}
			var warns []sqlitefile.Warning
			for _, w := range h.Warnings() {
				if w.Code == sc.warn && strings.Contains(w.Msg, "impossible page number") {
					warns = append(warns, w)
				}
			}
			if len(warns) != 1 || !strings.Contains(warns[0].Msg, fmt.Sprintf("%d ", len(sc.invalid))) {
				t.Errorf("want exactly one counted warning, got %v", warns)
			}
		})
	}
}
