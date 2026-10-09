package dbrecover

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/records/recordstest"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// trunkScene is plainScene plus a freelist trunk (page 3, no leaves) whose
// tail holds a stale cell.
func trunkScene() *slackScene {
	return newSlackScene(func(b *sqlitetest.Builder) {
		tb := b.CreateTable("t", "create table t(a integer, b text)")
		tb.Insert(1, 10, "live-row-one-"+filler(30, 1))
		tb.Insert(2, 20, "live-row-two-"+filler(30, 2))
		b.SetFreelist([][]uint32{{3}})
	})
}

func regionOf(r sqlitefile.RecoveredRow) string {
	for _, n := range r.Notes {
		if v, ok := cutPrefix(n, noteSlackRegionPrefix); ok {
			return v
		}
	}
	return ""
}

// J85: the tail of a freelist trunk is carved like the other free space.
func TestCarveFreelistTrunkTailRecoversCell(t *testing.T) {
	s := trunkScene()
	cell := slackCell(9, 90, "stale-in-trunk-"+filler(30, 9))
	s.plant(3, 100, cell)
	rows, st := s.carve(t)
	if len(rows) != 1 {
		t.Fatalf("%d rows, want 1 (the cell in the trunk tail)", len(rows))
	}
	r := rows[0]
	if r.Origin != sqlitefile.OriginFreelistTrunk || r.Loc.Page != 3 || *r.Rowid != 9 || r.Loc.Length != int64(len(cell)) {
		t.Errorf("origin %v page %d rowid %v length %d", r.Origin, r.Loc.Page, r.Rowid, r.Loc.Length)
	}
	if regionOf(r) != "freelist-trunk" {
		t.Errorf("region %q, want freelist-trunk (notes %v)", regionOf(r), r.Notes)
	}
	if r.Loc.Offset != int64(2*1024+100) || r.Loc.PageOffset != 2*1024 {
		t.Errorf("loc %+v", r.Loc)
	}
	if st.UnparsedBytes != 0 {
		t.Errorf("UnparsedBytes %d, want 0: the tail is explained by the carved cell", st.UnparsedBytes)
	}
}

// J88: the stored payload keeps span_kind as the page origin and names the
// region of the free space.
func TestSlackPayloadNamesTheRegion(t *testing.T) {
	c := recordstest.NewCase(t)
	s := slackScenePlanted(t)
	db := addFile(t, c, "/data/slack.db", s.b.Bytes())
	mustRun(t, c, Inputs{DB: db.ID}, Options{})
	n := 0
	for _, f := range storedRecords(t, c) {
		if f.Method != "sqlite-page-slack" {
			continue
		}
		n++
		carved, _ := payloadOf(t, f)["carved"].(map[string]any)
		if carved["span_kind"] != "live-btree" || carved["slack_region"] != "gap" {
			t.Errorf("carved %v, want span_kind live-btree and slack_region gap", carved)
		}
		if slices.Contains(toStrings(payloadOf(t, f)["notes"]), noteSlackRegionPrefix+"gap") {
			t.Errorf("notes carry the internal region marker")
		}
	}
	if n != 1 {
		t.Fatalf("%d slack records, want 1", n)
	}
}

// J87: the crossed-cell count is a number on analysis.end, not only a warning.
func TestSlackDroppedCrossedIsAudited(t *testing.T) {
	c := recordstest.NewCase(t)
	s := plainScene()
	off, _ := s.gap(t, 2)
	older := slackCell(12, 120, "older-cell-"+filler(40, 12))
	newer := slackCell(13, 130, "newer-cell-"+filler(40, 13))
	s.plant(2, off+10, older)
	s.plant(2, off+10+len(older)-3, newer)
	db := addFile(t, c, "/data/crossed.db", s.b.Bytes())
	res := mustRun(t, c, Inputs{DB: db.ID}, Options{})
	if res.Counts.SlackDroppedCrossed != 1 {
		t.Errorf("SlackDroppedCrossed %d, want 1", res.Counts.SlackDroppedCrossed)
	}
	end := lastEntry(t, readAudit(t, c), "analysis.end")
	if got := fmt.Sprint(end.Details["slack_dropped_crossed"]); got != "1" {
		t.Errorf("analysis.end slack_dropped_crossed %s, want 1", got)
	}
}

func newTestCarver(t *testing.T, s *slackScene) (*slackCarver, *slackStats) {
	t.Helper()
	db := s.open(t)
	h := db.History()
	t.Cleanup(h.Release)
	k, err := newSlackCarver(context.Background(), db, func(sqlitefile.RecoveredRow) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	return k, &k.st
}

// J91: every page skipped without being carved adds its non-zero span bytes.
func TestSkippedPagesCountTheirSpanBytes(t *testing.T) {
	garbage := func() []byte {
		d := make([]byte, 1024)
		d[0] = 0xff // not a b-tree page type
		for i := 100; i < 130; i++ {
			d[i] = 0xaa
		}
		return d
	}
	spans := []sqlitefile.Span{{Kind: sqlitefile.SpanGap, Offset: 90, Length: 60}}
	img := func(mut func(*sqlitefile.PageImage)) sqlitefile.PageImage {
		i := sqlitefile.PageImage{Number: 2, Origin: sqlitefile.OriginOrphan, Spans: spans}
		mut(&i)
		return i
	}
	for name, c := range map[string]struct {
		img  sqlitefile.PageImage
		data []byte
		want int64
	}{
		"partial page":          {img(func(i *sqlitefile.PageImage) { i.Partial = true }), garbage(), 30},
		"header does not parse": {img(func(*sqlitefile.PageImage) {}), garbage(), 30},
		"no spans":              {img(func(i *sqlitefile.PageImage) { i.Spans = nil }), garbage(), 0},
	} {
		k, st := newTestCarver(t, plainScene())
		k.image(c.img, c.data)
		if st.UnparsedBytes != c.want {
			t.Errorf("%s: UnparsedBytes %d, want %d", name, st.UnparsedBytes, c.want)
		}
	}
}

// J92: a freeblock that points at itself ends the chain: carving terminates,
// and the library counts the broken chain as a warning.
func TestCarveSelfPointingFreeblockTerminatesWithWarning(t *testing.T) {
	s := plainScene()
	db := s.open(t)
	h := db.History()
	defer h.Release()
	var contentStart int
	_ = h.Pages(context.Background(), func(img sqlitefile.PageImage) bool {
		if img.Number == 2 {
			b, _ := img.Bytes()
			contentStart = int(b[5])<<8 | int(b[6])
		}
		return true
	})
	if contentStart == 0 {
		t.Fatal("no content start")
	}
	s.plant(2, 1, []byte{byte(contentStart >> 8), byte(contentStart)}) // first freeblock
	s.plant(2, contentStart, []byte{byte(contentStart >> 8), byte(contentStart), 0, 8})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	db2 := s.open(t)
	h2 := db2.History()
	defer h2.Release()
	done := make(chan error, 1)
	go func() {
		_, err := carveSpans(ctx, db2, h2, func(sqlitefile.RecoveredRow) bool { return true })
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("carveSpans did not terminate on a self-pointing freeblock")
	}
	found := false
	for _, w := range h2.Warnings() {
		found = found || w.Code == sqlitefile.WarnFreeblockChain
	}
	if !found {
		t.Errorf("no %s warning for the self-pointing freeblock", sqlitefile.WarnFreeblockChain)
	}
}

func toStrings(v any) []string {
	var out []string
	l, _ := v.([]any)
	for _, x := range l {
		s, _ := x.(string)
		out = append(out, s)
	}
	return out
}
