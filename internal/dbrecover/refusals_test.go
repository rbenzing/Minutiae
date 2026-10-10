package dbrecover

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/evidence/evidencetest"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

func requireVerifyClean(t testing.TB, c *evidence.Case) {
	t.Helper()
	rep, err := c.Verify()
	if err != nil {
		t.Fatal(err)
	}
	evidencetest.RequireClean(t, rep)
}

func noIngest(t *testing.T, c *evidence.Case) {
	t.Helper()
	es := readAudit(t, c)
	if n := count(es, evidence.ActionIngestStart); n != 0 {
		t.Errorf("%d records.ingest.start for a refused run", n)
	}
	if got := storedRecords(t, c); len(got) != 0 {
		t.Errorf("%d records stored by a refused run", len(got))
	}
}

func TestNotSQLiteIsRefusedNotEmptySuccess(t *testing.T) {
	enc, ok := gunzipFile(t, filepath.Join("..", "sqlitefile", "testdata", "encrypted-like.db.gz"))
	if !ok {
		t.Fatal("no encrypted-like fixture")
	}
	random := make([]byte, 4096)
	for i := range random {
		random[i] = byte(i*131 + i>>3 + 7)
	}
	for name, data := range map[string][]byte{
		"random": random, "empty": {}, "text": bytes.Repeat([]byte("hello world, not a database. "), 100), "encrypted-like": enc,
	} {
		t.Run(name, func(t *testing.T) {
			c := recordstest.NewCase(t)
			a := addFile(t, c, "/data/n.db", data)
			_, err := Run(context.Background(), c, Inputs{DB: a.ID}, Options{})
			var re *RefusalError
			if !errors.Is(err, ErrNotSQLite) || !errors.As(err, &re) || re.Reason != ReasonNotSQLite {
				t.Fatalf("err %v, want a not-sqlite refusal", err)
			}
			es := readAudit(t, c)
			if count(es, "analysis.start") != 1 || count(es, "analysis.error") != 1 || count(es, "analysis.end") != 0 {
				t.Errorf("start %d error %d end %d", count(es, "analysis.start"), count(es, "analysis.error"), count(es, "analysis.end"))
			}
			if r, _ := lastEntry(t, es, "analysis.error").Details["reason"].(string); r != ReasonNotSQLite {
				t.Errorf("reason %q", r)
			}
			noIngest(t, c)
		})
	}
}

// pageSizeMismatchJournal is a hot journal whose header claims 2048-byte pages
// against the 1024-byte database: the engine would roll it back, the library
// cannot, so the live state is unavailable.
func pageSizeMismatchJournal(t testing.TB) (dbFiles, []byte) {
	t.Helper()
	f := newBuilderDB(t)
	b := buildTable(0)
	j := b.NewJournal(512, 0x1234, 6)
	j.Record(4, b.PageBytes(4))
	j.SetHeader(1, 0x1234, 6, 512, 2048)
	return f, j.Bytes()
}

func TestLiveUnavailableFailsClosed(t *testing.T) {
	c := recordstest.NewCase(t)
	f, j := pageSizeMismatchJournal(t)
	db := addFile(t, c, "/data/x.db", f.db)
	addFile(t, c, "/data/x.db-journal", j)
	_, err := Run(context.Background(), c, Inputs{DB: db.ID}, Options{})
	var re *RefusalError
	if !errors.Is(err, ErrLiveUnavailable) || !errors.Is(err, sqlitefile.ErrLiveUnavailable) || !errors.As(err, &re) || re.Reason != ReasonLiveUnavailable {
		t.Fatalf("err %v, want a live-unavailable refusal", err)
	}
	es := readAudit(t, c)
	if count(es, "analysis.error") != 1 {
		t.Errorf("analysis.error %d", count(es, "analysis.error"))
	}
	noIngest(t, c)
}

func TestExplicitUnusableSiblingIsRefused(t *testing.T) {
	f := newBuilderDB(t)
	badMagic := bytes.Repeat([]byte{0x5a}, 1024)
	for name, tc := range map[string]struct {
		role string
		data []byte
	}{
		"zeroed-header WAL":           {"wal", make([]byte, 2000)},
		"non-WAL artifact as the WAL": {"wal", f.db},
		"bad-magic journal":           {"journal", badMagic},
	} {
		t.Run(name, func(t *testing.T) {
			c := recordstest.NewCase(t)
			db := addFile(t, c, "/data/x.db", f.db)
			sib := addFile(t, c, "/data/zzz.bin", tc.data)
			in := Inputs{DB: db.ID, WAL: sib.ID}
			if tc.role == "journal" {
				in = Inputs{DB: db.ID, Journal: sib.ID}
			}
			_, err := Run(context.Background(), c, in, Options{})
			var re *RefusalError
			if !errors.As(err, &re) || re.Reason != ReasonSiblingUnusable {
				t.Fatalf("err %v, want sibling-unusable", err)
			}
			es := readAudit(t, c)
			if count(es, "analysis.error") != 1 || count(es, "analysis.start") != 1 {
				t.Errorf("error %d start %d", count(es, "analysis.error"), count(es, "analysis.start"))
			}
			noIngest(t, c)
		})
	}
}

func TestEveryRefusalIsAudited(t *testing.T) {
	f := newBuilderDB(t)
	hdr := slices.Clone(f.db)
	hdr[21] = 99 // a payload fraction the engine refuses
	_, hotMismatch := pageSizeMismatchJournal(t)
	type tc struct {
		reason    string
		withStart bool
		run       func(t *testing.T, c *evidence.Case) error
	}
	cases := []tc{
		{ReasonNotSQLite, true, func(t *testing.T, c *evidence.Case) error {
			a := addFile(t, c, "/data/n.db", bytes.Repeat([]byte("zz"), 3000))
			_, err := Run(context.Background(), c, Inputs{DB: a.ID}, Options{})
			return err
		}},
		{ReasonIneligibleKind, false, func(t *testing.T, c *evidence.Case) error {
			a := recordstest.AddArtifact(t, c, "app.db", f.db)
			man, err := c.Manifest()
			if err != nil {
				t.Fatal(err)
			}
			for i := range man {
				if man[i].ID == a.ID {
					man[i].Source.Kind = "report"
				}
			}
			_, err = openInputs(context.Background(), c, man, a.ID, "", "")
			return err
		}},
		{ReasonLiveUnavailable, true, func(t *testing.T, c *evidence.Case) error {
			db := addFile(t, c, "/data/x.db", f.db)
			addFile(t, c, "/data/x.db-journal", hotMismatch)
			_, err := Run(context.Background(), c, Inputs{DB: db.ID}, Options{})
			return err
		}},
		{ReasonEngineRefuses, true, func(t *testing.T, c *evidence.Case) error {
			db := addFile(t, c, "/data/x.db", hdr)
			_, err := Run(context.Background(), c, Inputs{DB: db.ID}, Options{})
			return err
		}},
		{ReasonSiblingUnusable, true, func(t *testing.T, c *evidence.Case) error {
			db := addFile(t, c, "/data/x.db", f.db)
			w := addFile(t, c, "/data/zzz.wal", make([]byte, 2000))
			_, err := Run(context.Background(), c, Inputs{DB: db.ID, WAL: w.ID}, Options{})
			return err
		}},
		{ReasonAmbiguousSibling, false, func(t *testing.T, c *evidence.Case) error {
			db := addFile(t, c, "/data/x.db", f.db)
			addFile(t, c, "/data/x.db-wal", f.wal)
			src := evidence.Source{Kind: "file", DeviceID: "dev1", RemotePath: "/data/x.db-wal"}
			if _, err := c.Capture("dev1", "acq1", "copy2/x.db-wal", src, func(w io.Writer) error {
				_, err := w.Write(f.wal)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			_, err := Run(context.Background(), c, Inputs{DB: db.ID}, Options{})
			return err
		}},
		{ReasonBadOption, false, func(t *testing.T, c *evidence.Case) error {
			db := addFile(t, c, "/data/x.db", f.db)
			_, err := Run(context.Background(), c, Inputs{DB: db.ID}, Options{MinConfidence: 101})
			return err
		}},
	}
	seen := map[string]bool{}
	for _, tc := range cases {
		seen[tc.reason] = true
		t.Run(tc.reason, func(t *testing.T) {
			c := recordstest.NewCase(t)
			err := tc.run(t, c)
			if got := refusalReason(err); got != tc.reason {
				t.Fatalf("refusal reason %q (err %v), want %q", got, err, tc.reason)
			}
			if tc.reason == ReasonIneligibleKind {
				return // a kind Run cannot reach through a case is pinned at the openInputs seam
			}
			es := readAudit(t, c)
			if n := count(es, "analysis.error"); n != 1 {
				t.Fatalf("%d analysis.error entries, want exactly 1", n)
			}
			if got := count(es, "analysis.start"); (got == 1) != tc.withStart || got > 1 {
				t.Errorf("analysis.start %d, want it present: %v", got, tc.withStart)
			}
			if r, _ := lastEntry(t, es, "analysis.error").Details["reason"].(string); r != tc.reason {
				t.Errorf("audited reason %q, want %q", r, tc.reason)
			}
			if count(es, "analysis.end") != 0 {
				t.Error("analysis.end for a refused run")
			}
			noIngest(t, c)
		})
	}
	for _, r := range []string{ReasonNotSQLite, ReasonIneligibleKind, ReasonLiveUnavailable, ReasonEngineRefuses, ReasonSiblingUnusable, ReasonAmbiguousSibling, ReasonBadOption} {
		if !seen[r] {
			t.Errorf("reason %q has no case", r)
		}
	}
}

func TestRerunIsRefusedUnlessAllowed(t *testing.T) {
	e := newRunEnv(t)
	mustRun(t, e.c, e.in(), Options{})
	before := len(storedRecords(t, e.c))
	_, err := Run(context.Background(), e.c, e.in(), Options{})
	if !errors.Is(err, records.ErrAlreadyIngested) {
		t.Fatalf("err %v", err)
	}
	if after := len(storedRecords(t, e.c)); after != before {
		t.Errorf("a refused re-run changed the records: %d -> %d", before, after)
	}
	mustRun(t, e.c, e.in(), Options{AllowReingest: true})
	es := readAudit(t, e.c)
	starts := indexes(es, "analysis.start")
	opts, _ := es[starts[len(starts)-1]].Details["options"].(map[string]any)
	if opts["allow_reingest"] != true {
		t.Errorf("the re-run start entry does not say it was allowed: %v", opts)
	}
	requireVerifyClean(t, e.c)
}

// changeWAL flips a byte of the WAL on disk from the after-flush hook.
func changeWAL(t *testing.T, e *runEnv) func() {
	return SetAfterFlushHookForTest(func() { flipByteAt(t, diskPath(e.c, e.wal), 40) })
}

func TestInputChangedBeforeEndAbortsWithIntegrityError(t *testing.T) {
	e := newRunEnv(t)
	restore := SetVersionForTest("0.9.0")
	old := mustRun(t, e.c, e.in(), Options{})
	restore()
	defer changeWAL(t, e)()
	res, err := Run(context.Background(), e.c, e.in(), Options{})
	var ch *InputChangedError
	if !errors.Is(err, evidence.ErrIntegrity) || !errors.As(err, &ch) {
		t.Fatalf("err %v, want an integrity error", err)
	}
	if ch.ArtifactID != e.wal.ID || ch.Want != e.wal.SHA256 || ch.Got == "" || ch.Got == ch.Want {
		t.Errorf("error names %+v, want the WAL %s and both digests", ch, e.wal.ID)
	}
	if res.Ingest.Outcome != "incomplete" || res.Counts.Written == 0 {
		t.Errorf("ingest %+v written %d: the flushed rows are kept and flagged incomplete", res.Ingest, res.Counts.Written)
	}
	es := readAudit(t, e.c)
	if n := count(es, evidence.ActionIngestEnd); n != 1 { // the 0.9.0 run's own end
		t.Errorf("%d records.ingest.end, want only the older run's", n)
	}
	var oldLive, newKept int
	for _, f := range storedRecords(t, e.c) {
		switch f.IngestID {
		case old.Ingest.IngestID:
			if f.Superseded {
				t.Errorf("record %d of the older complete run was superseded by an incomplete one", f.ID)
			}
			oldLive++
		case res.Ingest.IngestID:
			newKept++
		}
	}
	if oldLive == 0 || int64(newKept) != res.Counts.Written {
		t.Errorf("older records %d, incomplete run's kept records %d, written %d", oldLive, newKept, res.Counts.Written)
	}
	if r, _ := lastEntry(t, es, "analysis.error").Details["reason"].(string); r != "input-changed" {
		t.Errorf("audited reason %q", r)
	}
}

func TestInputReplacedBeforeEndAbortsWithIntegrityError(t *testing.T) {
	e := newRunEnv(t)
	path := diskPath(e.c, e.wal)
	replaced := false
	defer SetAfterFlushHookForTest(func() {
		if err := os.Remove(path); err != nil {
			t.Logf("cannot remove an open file here: %v", err)
			return
		}
		repl := slices.Clone(e.f.wal)
		repl[len(repl)-1] ^= 0xFF
		if err := os.WriteFile(path, repl, 0o600); err != nil {
			t.Error(err)
			return
		}
		replaced = true
	})()
	_, err := Run(context.Background(), e.c, e.in(), Options{})
	if !replaced {
		t.Skip("the platform cannot remove a file held open; the fresh re-open is pinned by the injected-reader tests (J44)")
	}
	var ch *InputChangedError
	if !errors.Is(err, evidence.ErrIntegrity) || !errors.As(err, &ch) || ch.ArtifactID != e.wal.ID {
		t.Fatalf("err %v", err)
	}
	if n := count(readAudit(t, e.c), evidence.ActionIngestEnd); n != 0 {
		t.Errorf("%d records.ingest.end after a replaced input", n)
	}
}

func cancelAtRow(cancel context.CancelFunc, rows int64) func(Progress) {
	return func(p Progress) {
		if p.Rows >= rows {
			cancel()
		}
	}
}

func TestCancelledRunKeepsPartialRowsAndVerifies(t *testing.T) {
	e := newRunEnv(t)
	defer SetProgressEveryForTest(1)()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res, err := Run(ctx, e.c, e.in(), Options{Progress: cancelAtRow(cancel, 2)})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err %v, want context canceled", err)
	}
	if res.Ingest.Outcome != "incomplete" || res.Counts.Written < 2 {
		t.Errorf("ingest %+v written %d", res.Ingest, res.Counts.Written)
	}
	if got := int64(len(storedRecords(t, e.c))); got != res.Counts.Written {
		t.Errorf("%d records stored, result says %d", got, res.Counts.Written)
	}
	es := readAudit(t, e.c)
	if count(es, evidence.ActionIngestEnd) != 0 || count(es, evidence.ActionIngestError) != 1 || count(es, "analysis.error") != 1 {
		t.Errorf("ingest.end %d ingest.error %d analysis.error %d", count(es, evidence.ActionIngestEnd), count(es, evidence.ActionIngestError), count(es, "analysis.error"))
	}
	if msg, _ := lastEntry(t, es, "analysis.error").Details["error"].(string); !strings.Contains(msg, "context canceled") {
		t.Errorf("analysis.error %q", msg)
	}
	requireVerifyClean(t, e.c)
}

func TestIntegrityFailureIsNeverMaskedByCancel(t *testing.T) {
	e := newRunEnv(t)
	defer SetProgressEveryForTest(1)()
	defer changeWAL(t, e)()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := Run(ctx, e.c, e.in(), Options{Progress: cancelAtRow(cancel, 2)})
	var ch *InputChangedError
	if !errors.Is(err, evidence.ErrIntegrity) || !errors.As(err, &ch) || ch.ArtifactID != e.wal.ID {
		t.Fatalf("err %v: a cancelled run still reports an input that changed", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err %v: the cancel is also reported", err)
	}
	if r, _ := lastEntry(t, readAudit(t, e.c), "analysis.error").Details["reason"].(string); r != "input-changed" {
		t.Errorf("audited reason %q", r)
	}
}

func TestWriterFailureIsNeverDowngradedToWarning(t *testing.T) {
	e := newRunEnv(t)
	defer SetProgressEveryForTest(1)()
	closed := false
	res, err := Run(context.Background(), e.c, e.in(), Options{BatchRows: 1, Progress: func(Progress) {
		if !closed {
			closed = true
			if cerr := e.c.Audit.Close(); cerr != nil {
				t.Error(cerr)
			}
		}
	}})
	if err == nil {
		t.Fatal("Run succeeded with a closed audit log")
	}
	if !strings.Contains(err.Error(), "audit") {
		t.Errorf("err %v does not name the audit failure", err)
	}
	if !strings.Contains(err.Error(), "ingest error") {
		t.Errorf("err %v: the writer was not aborted (Abort not attempted)", err)
	}
	if n := sumMap(res.Counts.SkippedByReason); n != 0 {
		t.Errorf("%d rows counted as skipped after an audit failure: %v", n, res.Counts.SkippedByReason)
	}
}

func TestAddRejectionIsCountedNotFatal(t *testing.T) {
	e := newRunEnv(t)
	total := sumMap(libraryRowsByMethod(t, e.f))
	n := 0
	defer SetCheckForTest(func(p map[string]any) error {
		if n++; n <= 3 {
			return records.ErrRecordTooLarge
		}
		return checkPayload(p)
	})()
	res := mustRun(t, e.c, e.in(), Options{})
	if res.Counts.SkippedByReason["writer-rejected"] != 3 || res.Counts.Written != total-3 {
		t.Errorf("skipped %v written %d of %d", res.Counts.SkippedByReason, res.Counts.Written, total)
	}
	if res.Ingest.Rejected != 3 || res.Ingest.Outcome != "incomplete" { // rejected rows narrow the run (J112)
		t.Errorf("ingest %+v", res.Ingest)
	}
}

func TestMapperDefectAborts(t *testing.T) {
	e := newRunEnv(t)
	defer SetCheckForTest(func(map[string]any) error { return records.ErrRecoveredArtifactLiveRecord })()
	res, err := Run(context.Background(), e.c, e.in(), Options{})
	if !errors.Is(err, records.ErrRecoveredArtifactLiveRecord) {
		t.Fatalf("err %v, want the mapper defect", err)
	}
	if res.Ingest.Outcome != "incomplete" {
		t.Errorf("ingest %+v", res.Ingest)
	}
	if n := sumMap(res.Counts.SkippedByReason); n != 0 {
		t.Errorf("a mapper defect was counted as skipped: %v", res.Counts.SkippedByReason)
	}
	es := readAudit(t, e.c)
	if count(es, evidence.ActionIngestEnd) != 0 || count(es, "analysis.error") != 1 {
		t.Errorf("ingest.end %d analysis.error %d", count(es, evidence.ActionIngestEnd), count(es, "analysis.error"))
	}
}

func TestMaxRecordsStopsCleanly(t *testing.T) {
	e := newRunEnv(t)
	res := mustRun(t, e.c, e.in(), Options{MaxRecords: 4})
	if res.Counts.Written != 4 || !slices.Contains(res.Counts.LimitsHit, "max-records") || res.Ingest.Outcome != "incomplete" {
		t.Fatalf("counts %+v ingest %+v", res.Counts, res.Ingest)
	}
	es := readAudit(t, e.c)
	if count(es, "analysis.end") != 1 || count(es, evidence.ActionIngestEnd) != 0 {
		t.Errorf("analysis.end %d ingest.end %d", count(es, "analysis.end"), count(es, evidence.ActionIngestEnd))
	}
	requireLimitWarning(t, es, "max-records")
	requireVerifyClean(t, e.c)
}

func TestRunByteBudgetStopsCleanly(t *testing.T) {
	e := newRunEnv(t)
	res := mustRun(t, e.c, e.in(), Options{MaxBytes: 3000})
	if res.Counts.Written == 0 || !slices.Contains(res.Counts.LimitsHit, "max-bytes") || res.Ingest.Outcome != "incomplete" {
		t.Fatalf("counts %+v ingest %+v", res.Counts, res.Ingest)
	}
	es := readAudit(t, e.c)
	requireLimitWarning(t, es, "max-bytes")
	requireVerifyClean(t, e.c)
}

func TestRunTimeoutStopsCleanly(t *testing.T) {
	e := newRunEnv(t)
	defer SetProgressEveryForTest(1)()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	res, err := Run(ctx, e.c, e.in(), Options{Progress: func(p Progress) {
		if p.Rows == 2 {
			<-ctx.Done() // the run's time is up while rows are being mapped
		}
	}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err %v, want the deadline", err)
	}
	if res.Ingest.Outcome != "incomplete" || res.Counts.Written < 2 {
		t.Errorf("ingest %+v written %d", res.Ingest, res.Counts.Written)
	}
	es := readAudit(t, e.c)
	if count(es, "analysis.error") != 1 || count(es, evidence.ActionIngestEnd) != 0 {
		t.Errorf("analysis.error %d ingest.end %d", count(es, "analysis.error"), count(es, evidence.ActionIngestEnd))
	}
	requireVerifyClean(t, e.c)
}

func TestMaxRecordsCeilingIsTheLibrarysCap(t *testing.T) {
	if lim := sqlitefile.DefaultLimits().MaxHistoryRows; int64(maxRecordsCeiling) != lim {
		t.Fatalf("ceiling %d, the library's MaxHistoryRows is %d", int64(maxRecordsCeiling), lim)
	}
	e := newRunEnv(t)
	if _, err := Run(context.Background(), e.c, e.in(), Options{MaxRecords: maxRecordsCeiling + 1}); refusalReason(err) != ReasonBadOption {
		t.Errorf("above the ceiling: %v", err)
	}
	if _, err := Run(context.Background(), e.c, e.in(), Options{MaxRecords: maxRecordsCeiling}); err != nil {
		t.Errorf("at the ceiling: %v", err)
	}
}

func poisonRow(_ sqlitefile.RecoveredRow) sqlitefile.RecoveredRow { panic("poisoned row") }

func TestPanicInMappingIsRecovered(t *testing.T) {
	e := newRunEnv(t)
	n := 0
	restore := SetRowHookForTest(func(r sqlitefile.RecoveredRow) sqlitefile.RecoveredRow {
		if n++; n == 3 {
			return poisonRow(r)
		}
		return r
	})
	res, err := Run(context.Background(), e.c, e.in(), Options{})
	restore()
	var pe *PanicError
	if !errors.Is(err, ErrInternal) || !errors.As(err, &pe) {
		t.Fatalf("err %v, want a PanicError", err)
	}
	es := readAudit(t, e.c)
	if count(es, evidence.ActionIngestError) != 1 || count(es, evidence.ActionIngestEnd) != 0 || count(es, "analysis.error") != 1 {
		t.Errorf("ingest.error %d ingest.end %d analysis.error %d", count(es, evidence.ActionIngestError), count(es, evidence.ActionIngestEnd), count(es, "analysis.error"))
	}
	if res.Ingest.Outcome != "incomplete" {
		t.Errorf("ingest %+v", res.Ingest)
	}
	if r, _ := lastEntry(t, es, "analysis.error").Details["reason"].(string); r != "error" {
		t.Errorf("audited reason %q", r)
	}
	requireVerifyClean(t, e.c)
	mustRun(t, e.c, e.in(), Options{}) // the case is usable afterwards
}

func TestRecoverPublicEntryPointsRecoverPanics(t *testing.T) {
	t.Run("Run", func(t *testing.T) {
		e := newRunEnv(t)
		defer SetRowHookForTest(poisonRow)()
		_, err := Run(context.Background(), e.c, e.in(), Options{})
		if !errors.Is(err, ErrInternal) {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("Run, after the pass", func(t *testing.T) {
		e := newRunEnv(t)
		defer SetAfterFlushHookForTest(func() { panic("poisoned hook") })()
		res, err := Run(context.Background(), e.c, e.in(), Options{})
		var pe *PanicError
		if !errors.As(err, &pe) {
			t.Fatalf("err %v", err)
		}
		es := readAudit(t, e.c)
		if count(es, evidence.ActionIngestError) != 1 || count(es, evidence.ActionIngestEnd) != 0 || res.Ingest.Outcome != "incomplete" || res.Counts.Written == 0 {
			t.Errorf("ingest.error %d ingest.end %d result %+v", count(es, evidence.ActionIngestError), count(es, evidence.ActionIngestEnd), res.Ingest)
		}
		requireVerifyClean(t, e.c)
	})
	t.Run("Plan after the pass", func(t *testing.T) {
		e := newRunEnv(t)
		_, err := Plan(context.Background(), e.c, e.in(), Options{Progress: func(Progress) { panic("poisoned progress") }})
		if !errors.Is(err, ErrInternal) {
			t.Fatalf("err %v", err)
		}
	})
	t.Run("Plan", func(t *testing.T) {
		e := newRunEnv(t)
		before := len(readAudit(t, e.c))
		defer SetRowHookForTest(poisonRow)()
		_, err := Plan(context.Background(), e.c, e.in(), Options{})
		if !errors.Is(err, ErrInternal) {
			t.Fatalf("err %v", err)
		}
		if after := len(readAudit(t, e.c)); after != before {
			t.Errorf("a plan audited %d entries", after-before)
		}
	})
	t.Run("FindSiblings", func(t *testing.T) {
		e := newRunEnv(t)
		defer SetSiblingScanForTest(func([]evidence.ManifestRecord, evidence.ManifestRecord) SiblingReport { panic("poisoned") })()
		man, err := e.c.Manifest()
		if err != nil {
			t.Fatal(err)
		}
		rep := FindSiblings(man, e.db)
		if rep.WAL.Status != SiblingError || rep.Journal.Status != SiblingError {
			t.Errorf("report %+v", rep)
		}
	})
}

// J102: WriterStarted is set where the writer starts: true for a run that wrote, false for a refusal.
func TestResultWriterStartedFlag(t *testing.T) {
	e := newRunEnv(t)
	if res := mustRun(t, e.c, e.in(), Options{}); !res.WriterStarted {
		t.Error("a run that ingested did not set WriterStarted")
	}
	c := recordstest.NewCase(t)
	a := addFile(t, c, "/data/n.db", []byte("not a database at all, just text"))
	res, err := Run(context.Background(), c, Inputs{DB: a.ID}, Options{})
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if res.WriterStarted {
		t.Error("a refused run set WriterStarted")
	}
}

// J108 (J100): a manifest line that does not parse is corruption; Run carries evidence.ErrIntegrity out of the
// manifest read, so the CLI maps it to exit 4 and nothing masks it.
func TestRunCorruptManifestLineIsIntegrityError(t *testing.T) {
	c := recordstest.NewCase(t)
	a := addFile(t, c, "/data/n.db", bytes.Repeat([]byte("not a database. "), 300))
	f, err := os.OpenFile(filepath.Join(c.Dir, "manifest.jsonl"), os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("garbage\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = Run(context.Background(), c, Inputs{DB: a.ID}, Options{})
	if !errors.Is(err, evidence.ErrIntegrity) {
		t.Fatalf("err %v, want evidence.ErrIntegrity", err)
	}
}
