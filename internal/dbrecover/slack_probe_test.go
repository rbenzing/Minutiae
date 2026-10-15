package dbrecover

// The slack probe (plan 3J Task 14, ruling J5). It builds, with an independent
// engine (secure_delete off), pages whose deleted rows leave bytes in the two
// kinds of in-page free space (the gap and freeblocks), asks a structural
// scanner what it can recover from those spans, and compares the answer with
// the engine's own log of what it wrote. The verdict is what slack.go
// implements; the numbers are logged with -v.

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// probeScan is the probe's scanner: every offset of the span at which a whole
// table-leaf cell (payload length, rowid, record whose header and body add up
// to the payload length exactly, no reserved serial, no overflow) fits inside
// the span, taken left to right without overlap. It returns the text columns it
// found, one per cell.
func probeScan(page []byte, usable int, sp sqlitefile.Span) (cells []string, steps int) {
	end := sp.Offset + sp.Length
	maxLocal := usable - 35
	for p := sp.Offset; p < end; {
		steps++
		pl, n1 := sqlitefile.GetVarint(page[p:end])
		if n1 == 0 || pl < 2 || pl > uint64(maxLocal) {
			p++
			continue
		}
		_, n2 := sqlitefile.GetVarint(page[p+n1 : end])
		if n2 == 0 || p+n1+n2+int(pl) > end {
			p++
			continue
		}
		rec := page[p+n1+n2 : p+n1+n2+int(pl)]
		serials, hl, body, err := sqlitefile.ParseRecordHeader(rec, 64)
		if err != nil || len(serials) == 0 || int64(hl)+body != int64(len(rec)) {
			p++
			continue
		}
		usable := true
		nonNull := false
		for _, s := range serials {
			usable = usable && s != 10 && s != 11
			nonNull = nonNull || s != 0
		}
		if !usable || !nonNull {
			p++
			continue
		}
		cells = append(cells, string(rec[hl:]))
		p += n1 + n2 + int(pl)
	}
	return cells, steps
}

type probeTally struct {
	pages, spans, spanBytes int
	markersInSpans          int // deleted markers whose bytes lie in a span (the raw oracle)
	carved, matched, wrong  int
}

func probeRun(t *testing.T, name string, s scenario) map[string]*probeTally {
	f := s.build(t)
	db, err := sqlitefile.Open(bytesAt{f.db}, int64(len(f.db)), sqlitefile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if f.wal != nil {
		if _, err := db.AttachWAL(bytesAt{f.wal}, int64(len(f.wal))); err != nil {
			t.Fatal(err)
		}
	}
	h := db.History()
	defer h.Release()
	tally := map[string]*probeTally{}
	inSpans := map[string]bool{}
	err = h.Pages(context.Background(), func(img sqlitefile.PageImage) bool {
		key := img.Origin.String()
		tl := tally[key]
		if tl == nil {
			tl = &probeTally{}
			tally[key] = tl
		}
		tl.pages++
		data, err := img.Bytes()
		if err != nil {
			t.Fatal(err)
		}
		for _, sp := range img.Spans {
			key := fmt.Sprintf("%s/kind%d", img.Origin, sp.Kind)
			tl := tally[key]
			if tl == nil {
				tl = &probeTally{}
				tally[key] = tl
			}
			tl.spans++
			tl.spanBytes += sp.Length
			raw := data[sp.Offset : sp.Offset+sp.Length]
			for _, m := range f.deleted {
				if m != "" && bytes.Contains(raw, []byte(m)) {
					tl.markersInSpans++
					inSpans[fmt.Sprintf("%s|%d|%d", key, sp.Kind, sp.Offset)] = true
				}
			}
			cells, _ := probeScan(data, db.Info().UsableSize, sp)
			for _, c := range cells {
				tl.carved++
				if strings.Contains(c, "EV-") {
					tl.matched++
				} else {
					tl.wrong++
					t.Logf("%s: page %d %v span@%d: carved a cell without a marker: %q raw % x", name, img.Number, sp.Kind, sp.Offset, c, data[sp.Offset:min(sp.Offset+sp.Length, sp.Offset+12)])
				}
			}
		}
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range sortedKeys(tally) {
		tl := tally[k]
		{
			t.Logf("%s: %-16s pages=%d spans=%d spanBytes=%d markersInSpans=%d carved=%d withMarker=%d withoutMarker=%d",
				name, k, tl.pages, tl.spans, tl.spanBytes, tl.markersInSpans, tl.carved, tl.matched, tl.wrong)
		}
	}
	return tally
}

// TestSlackProbe logs what the free space of engine-written pages holds.
func TestSlackProbe(t *testing.T) {
	short := func(id int64, ver int) string { return fmt.Sprintf("EV-%06d-v%d-%s", id, ver, strings.Repeat("p", 20)) }
	long := func(id int64, ver int) string {
		return fmt.Sprintf("EV-%06d-v%d-%s", id, ver, strings.Repeat("q", 150))
	}
	sum := func(m map[string]*probeTally, prefix string) (carved, wrong int) {
		for k, tl := range m {
			if strings.HasPrefix(k, prefix) {
				carved += tl.carved
				wrong += tl.wrong
			}
		}
		return carved, wrong
	}
	// The verdict (see slack.go): gap and freeblock bytes of live pages and of superseded
	// WAL images hold whole old cells, and nothing that is not an old cell parses.
	for _, c := range []struct {
		name   string
		s      scenario
		prefix string
		want   bool
	}{
		{"delete-prefix", scenario{rows: 60, deleteTo: 25, text: short}, "live-btree", true},
		{"delete-prefix-long", scenario{rows: 30, deleteTo: 10, text: long}, "live-btree", true},
		{"wal-delete", scenario{rows: 60, deleteTo: 25, text: short, wal: true, updateAll: true}, "wal-superseded", true},
		{"secure-delete", scenario{rows: 60, deleteTo: 25, text: short, pragmas: []string{"secure_delete=1"}}, "live-btree", false},
	} {
		carved, wrong := sum(probeRun(t, c.name, c.s), c.prefix)
		if wrong != 0 {
			t.Errorf("%s: %d carved cells hold no marker the engine wrote", c.name, wrong)
		}
		if (carved > 0) != c.want {
			t.Errorf("%s: %d cells carved from %s spans, want carving: %v", c.name, carved, c.prefix, c.want)
		}
	}
}

func sortedKeys(m map[string]*probeTally) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	slices.Sort(ks)
	return ks
}

// TestSlackCarverOnEnginePages runs the real carver on engine-written pages:
// every row it delivers is a tuple the engine wrote, and the rows the engine
// deleted and left in the gap are among them.
func TestSlackCarverOnEnginePages(t *testing.T) {
	short := func(id int64, ver int) string { return fmt.Sprintf("EV-%06d-v%d-%s", id, ver, strings.Repeat("p", 20)) }
	f := scenario{rows: 60, deleteTo: 25, text: short}.build(t)
	db, err := sqlitefile.Open(bytesAt{f.db}, int64(len(f.db)), sqlitefile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	h := db.History()
	defer h.Release()
	deleted := map[int64]bool{}
	var carved int
	st, err := carveSpans(context.Background(), db, h, func(r sqlitefile.RecoveredRow) bool {
		carved++
		a, _ := r.Values[0], 0
		b, ok := r.Values[1].Text()
		if r.Rowid == nil || !ok || !f.log.rows[fmt.Sprintf("%d|%d|%s", *r.Rowid, a.Int, b)] {
			t.Errorf("carved a row the engine never wrote: rowid %v values %+v", r.Rowid, r.Values)
		}
		deleted[*r.Rowid] = true
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("carved %d rows (%d distinct rowids), dropped %d copies of live cells, %d unparsed bytes", carved, len(deleted), st.DroppedLiveCopy, st.UnparsedBytes)
	recovered := 0
	for id := range f.deleted {
		if deleted[id] {
			recovered++
		}
	}
	if recovered < 20 {
		t.Errorf("only %d of the %d deleted rows were recovered from the slack, want at least 20", recovered, len(f.deleted))
	}
	t.Logf("%d of %d deleted rows recovered from slack", recovered, len(f.deleted))
}
