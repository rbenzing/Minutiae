package sqlitefile_test

// Engine oracle for C45: databases written by the engine, with the live rows
// of the engine's own dump as the judge of every annotation.

import (
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// scenFitLive: live table t holds rows 1..40. The dropped table gq has the
// shape of t only, so its freed leaves are named t by the fit. Its rows 1..20
// are copies of the live rows, rows 21..30 carry a live rowid and another
// value, rows 41..50 carry no live rowid. The log files every gq row under t
// (the table the fit names) so the precision check accepts them.
func scenFitLive(t *testing.T) *histFixture {
	g, path := newHistGen(t, "fitlive", 1024, "delete")
	g.exec("create table t(a integer, b text)")
	g.exec("create table pad(z)")
	g.exec("create table gq(a integer, b text)")
	g.begin()
	g.exec("insert into pad values(1)")
	for id := int64(1); id <= 40; id++ {
		g.ins("t", id, 0, false)
	}
	gq := func(id int64, a int64, b string) {
		g.exec("insert into gq(rowid,a,b) values(?,?,?)", id, a, b)
		g.log.add("t", id, a, b)
	}
	for id := int64(1); id <= 20; id++ {
		gq(id, id*1000, marker("t", id, 0))
	}
	for id := int64(21); id <= 30; id++ {
		gq(id, id*1000+9, marker("gq", id, 0))
	}
	for id := int64(41); id <= 50; id++ {
		gq(id, id*1000, marker("gq", id, 0))
	}
	g.commit()
	g.exec("drop table pad")
	g.exec("drop table gq")
	_ = g.db.Close()
	return finishFixture(t, &histFixture{name: "fitlive", db: readAll(t, path), log: g.log, expectMethodsPresent: []string{sqlitefile.MethodFreelist}})
}

// TestEngineOracleFitRowsAnnotation: a fit-only row carries equals-live-row
// exactly when the engine's live table holds a row of that rowid with those
// values; the relation stays unknown and the row is delivered either way.
func TestEngineOracleFitRowsAnnotation(t *testing.T) {
	f := scenFitLive(t)
	rows := checkHistory(t, f)
	annotated, plain := 0, 0
	for _, r := range rows {
		if r.Method != sqlitefile.MethodFreelist || r.TableBasis != sqlitefile.BasisFit || r.Index != "" {
			continue
		}
		want := false
		for _, lr := range f.live[r.Table] {
			if !lr.HasRowid || r.Rowid == nil || lr.Rowid != *r.Rowid || len(lr.Vals) != len(r.Values) {
				continue
			}
			same := true
			for i, v := range vals(r) {
				if !sameValue(lr.Vals[i], v) {
					same = false
				}
			}
			want = want || same
		}
		if got := hasNote(r, sqlitefile.NoteEqualsLiveRow); got != want {
			t.Errorf("rowid %v %s: equals-live-row %v, the engine's live table says %v", r.Rowid, rowText(r), got, want)
		}
		if r.Relation != sqlitefile.RelUnknown {
			t.Errorf("rowid %v: relation %s", r.Rowid, r.Relation)
		}
		if want {
			annotated++
		} else {
			plain++
		}
	}
	if annotated == 0 || plain == 0 {
		t.Errorf("annotated %d, plain %d: the scenario must hold both kinds", annotated, plain)
	}
}
