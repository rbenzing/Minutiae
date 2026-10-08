package dbrecover

import (
	"context"
	"errors"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// bigScenario is a database of n small rows whose pages changed after a hot
// journal saved their earlier image: the journal yields about n recovered rows.
func bigScenario(t testing.TB, n int) (db, journal []byte) {
	t.Helper()
	build := func(rev byte) *sqlitetest.Builder {
		b := sqlitetest.New(sqlitetest.Options{PageSize: testPageSize})
		tb := b.CreateTable("t", "create table t(a, b)")
		for id := int64(1); id <= int64(n); id++ {
			tb.Insert(id, id, filler(8, rev+byte(id%5)))
		}
		return b
	}
	old, cur := build(0), build(40)
	db = cur.Bytes()
	pages := len(db) / testPageSize
	j := old.NewJournal(512, 0xabcdef, uint32(pages))
	for p := 2; p <= pages; p++ {
		j.Record(uint32(p), old.PageBytes(uint32(p)))
	}
	return db, j.Bytes()
}

func TestWriterWarningCapBoundsTheAuditLog(t *testing.T) {
	db, journal := bigScenario(t, 14000)
	c := recordstest.NewCase(t)
	d := addFile(t, c, "/data/big.db", db)
	addFile(t, c, "/data/big.db-journal", journal)
	defer SetCheckForTest(func(map[string]any) error { return records.ErrInvalidPayload })()
	res, err := Run(context.Background(), c, Inputs{DB: d.ID}, Options{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	rej := res.Counts.SkippedByReason["writer-rejected"]
	if rej <= records.MaxWarningsPerIngest {
		t.Fatalf("only %d rejected rows: the scenario does not pass the cap of %d", rej, records.MaxWarningsPerIngest)
	}
	if res.Counts.Written != 0 || res.Ingest.Rejected != int(rej) {
		t.Errorf("written %d, writer rejected %d, counted %d", res.Counts.Written, res.Ingest.Rejected, rej)
	}
	es := readAudit(t, c)
	var warns, notes int
	for _, i := range indexes(es, "analysis.warning") {
		warns++
		if _, ok := es[i].Details[evidence.WarnKeySuppression]; ok {
			notes++
			if r, _ := es[i].Details["reason"].(string); !strings.Contains(r, "suppressed") {
				t.Errorf("suppression note %q", r)
			}
		}
	}
	if warns > records.MaxWarningsPerIngest+2 { // the cap, the "suppression began" note and the final note
		t.Errorf("%d analysis.warning entries: the audit log is not bounded", warns)
	}
	if notes != 1 || res.Ingest.WarningsSuppressed == 0 {
		t.Errorf("suppression notes %d, suppressed %d", notes, res.Ingest.WarningsSuppressed)
	}
	requireVerifyClean(t, c)
}

// hostileOK checks what every run on hostile input must satisfy.
func hostileOK(t testing.TB, c *evidence.Case, err error) {
	t.Helper()
	hostileOKExpecting(t, c, err, false)
}

// hostileOKExpecting is hostileOK for a case that may expect ErrInternal.
func hostileOKExpecting(t testing.TB, c *evidence.Case, err error, expectInternal bool) {
	t.Helper()
	var pe *PanicError
	if errors.As(err, &pe) && !expectInternal {
		t.Fatalf("a panic reached the safety net: %v", pe)
	}
	if errors.Is(err, ErrInternal) && !expectInternal {
		t.Fatalf("an internal error on hostile input is a bug: %v", err)
	}
	if errors.Is(err, evidence.ErrIntegrity) {
		t.Fatalf("an integrity error on untouched inputs: %v", err)
	}
	requireVerifyClean(t, c)
}

func runHostile(t testing.TB, db, wal, journal []byte) {
	t.Helper()
	c := recordstest.NewCase(t)
	d := addFile(t, c, "/data/h.db", db)
	if wal != nil {
		addFile(t, c, "/data/h.db-wal", wal)
	}
	if journal != nil {
		addFile(t, c, "/data/h.db-journal", journal)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_, err := Run(ctx, c, Inputs{DB: d.ID}, Options{})
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a hostile database ran out of time")
	}
	hostileOK(t, c, err)
}

func mutateBytes(rng *rand.Rand, b []byte) []byte {
	out := slices.Clone(b)
	if len(out) == 0 {
		return out
	}
	switch rng.IntN(4) {
	case 0: // flip bytes
		for range 1 + rng.IntN(16) {
			out[rng.IntN(len(out))] = byte(rng.IntN(256))
		}
	case 1: // truncate
		out = out[:rng.IntN(len(out))]
	case 2: // zero a span
		a := rng.IntN(len(out))
		clear(out[a:min(len(out), a+1+rng.IntN(512))])
	default: // set a span to 0xff
		a := rng.IntN(len(out))
		s := out[a:min(len(out), a+1+rng.IntN(512))]
		for i := range s {
			s[i] = 0xff
		}
	}
	return out
}

func TestHostileDatabasesNeverPanic(t *testing.T) {
	f := newBuilderDB(t)
	rng := rand.New(rand.NewPCG(7, 11)) //nolint:gosec // a fixed seed: the hostile mutations must repeat
	for i := range 40 {
		db, wal, journal := f.db, f.wal, f.journal
		switch i % 3 {
		case 0:
			db = mutateBytes(rng, db)
		case 1:
			wal = mutateBytes(rng, wal)
		default:
			journal = mutateBytes(rng, journal)
		}
		if i%5 == 0 {
			db = mutateBytes(rng, db)
		}
		runHostile(t, db, wal, journal)
	}
	for _, name := range fixtureNames(t) {
		t.Run(name, func(t *testing.T) {
			c, in := fixtureCase(t, name)
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			_, err := Run(ctx, c, in, Options{})
			hostileOK(t, c, err)
		})
	}
}

// FuzzRun adds arbitrary database, WAL and journal bytes as artifacts and runs the
// recovery: no panic, bounded time, and the case verifies clean afterwards.
func FuzzRun(f *testing.F) {
	s := newBuilderDB(f)
	f.Add(s.db, s.wal, s.journal)
	f.Add(s.db, s.persist, []byte{})
	f.Add(s.db[:2048], s.wal[:100], s.journal[:600])
	f.Add([]byte("not a database"), []byte{}, []byte{})
	f.Fuzz(func(t *testing.T, db, wal, journal []byte) {
		if len(db) > 1<<20 || len(wal) > 1<<20 || len(journal) > 1<<20 {
			t.Skip()
		}
		var w, j []byte
		if len(wal) > 0 {
			w = wal
		}
		if len(journal) > 0 {
			j = journal
		}
		runHostile(t, db, w, j)
	})
}
