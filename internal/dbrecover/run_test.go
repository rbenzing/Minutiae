package dbrecover

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
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

func TestRunRecoversFreelistWALAndJournalRows(t *testing.T) {
	e := newRunEnv(t)
	want := libraryRowsByMethod(t, e.f)
	if len(want) < 3 {
		t.Fatalf("the scenario yields only %v", want)
	}
	res := mustRun(t, e.c, e.in(), Options{})
	if !reflect.DeepEqual(res.Counts.ByMethod, want) {
		t.Errorf("by method %v, want %v", res.Counts.ByMethod, want)
	}
	if res.Counts.Written != sumMap(want) || res.Ingest.Records != res.Counts.Written || res.Ingest.Outcome != "complete" {
		t.Errorf("written %d ingest %+v", res.Counts.Written, res.Ingest)
	}
	var hasFree, hasWAL, hasJournal bool
	for m := range res.Counts.ByMethod {
		hasFree = hasFree || m == sqlitefile.MethodFreelist
		hasWAL = hasWAL || strings.HasPrefix(m, "sqlite-wal")
		hasJournal = hasJournal || strings.HasPrefix(m, "sqlite-journal")
	}
	if !hasFree || !hasWAL || !hasJournal {
		t.Errorf("methods %v lack a freelist, WAL or journal row", res.Counts.ByMethod)
	}
	if res.Siblings.WAL.Status != SiblingFound || res.Siblings.WAL.ArtifactID != e.wal.ID || res.Siblings.Journal.ArtifactID != e.journal.ID {
		t.Errorf("siblings %+v", res.Siblings)
	}
	if res.RulesHash != RulesHash() || res.AnalysisID == "" {
		t.Errorf("hash %q id %q", res.RulesHash, res.AnalysisID)
	}
}

func TestRunWritesOnlyRecoveredDeletedRecords(t *testing.T) {
	e := newRunEnv(t)
	res := mustRun(t, e.c, e.in(), Options{})
	got := storedRecords(t, e.c)
	if int64(len(got)) != res.Counts.Written || len(got) == 0 {
		t.Fatalf("stored %d, written %d", len(got), res.Counts.Written)
	}
	for _, f := range got {
		if f.Type != TypeName || !f.Recovered || !f.Deleted || f.Confidence == nil || f.TS != nil || f.Method == "" {
			t.Errorf("record %d: %+v", f.ID, f.Row)
		}
		if f.Parser.Name != ParserName || f.Parser.Version != Version || f.Parser.Hash != RulesHash() {
			t.Errorf("parser %+v", f.Parser)
		}
	}
}

func TestAuditOrderPinsFreshHashBeforeEnd(t *testing.T) {
	e := newRunEnv(t)
	hookLen := -1
	defer SetAfterFlushHookForTest(func() { hookLen = len(readAudit(t, e.c)) })()
	mustRun(t, e.c, e.in(), Options{})
	if hookLen < 0 {
		t.Fatal("the fresh-hash hook never ran")
	}
	es := readAudit(t, e.c)
	iStart, iIng, iBatch := firstIndex(es, "analysis.start"), firstIndex(es, evidence.ActionIngestStart), firstIndex(es, "records.batch")
	iIngEnd, iEnd := firstIndex(es, evidence.ActionIngestEnd), firstIndex(es, "analysis.end")
	warns := indexes(es, "analysis.warning")
	if len(warns) == 0 {
		t.Fatal("the scenario raised no warning: the order test would be vacuous")
	}
	if iStart < 0 || iStart >= iIng || iIng >= iBatch {
		t.Fatalf("start %d ingest.start %d batch %d", iStart, iIng, iBatch)
	}
	if warns[0] < iBatch {
		t.Errorf("a warning (%d) precedes the first batch (%d)", warns[0], iBatch)
	}
	if warns[len(warns)-1] >= hookLen {
		t.Errorf("a warning (%d) follows the fresh-hash moment (%d)", warns[len(warns)-1], hookLen)
	}
	if hookLen > iIngEnd || iIngEnd >= iEnd {
		t.Errorf("fresh-hash moment %d, ingest.end %d, analysis.end %d", hookLen, iIngEnd, iEnd)
	}
}

func TestChangedInputAfterFlushNeverReachesIngestEnd(t *testing.T) {
	e := newRunEnv(t)
	defer SetAfterFlushHookForTest(func() {
		f, err := os.OpenFile(diskPath(e.c, e.wal), os.O_WRONLY, 0)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = f.Close() }()
		if _, err := f.WriteAt([]byte{0xAA}, 40); err != nil {
			t.Error(err)
		}
	})()
	res, err := Run(context.Background(), e.c, e.in(), Options{})
	var ch *InputChangedError
	if !errors.As(err, &ch) || !errors.Is(err, evidence.ErrIntegrity) || !errors.Is(err, ErrInputChanged) {
		t.Fatalf("err %v", err)
	}
	if ch.ArtifactID != e.wal.ID {
		t.Errorf("names %q, want the changed WAL %s", ch.ArtifactID, e.wal.ID)
	}
	es := readAudit(t, e.c)
	if count(es, evidence.ActionIngestEnd) != 0 || count(es, evidence.ActionIngestError) != 1 {
		t.Errorf("ingest.end %d ingest.error %d", count(es, evidence.ActionIngestEnd), count(es, evidence.ActionIngestError))
	}
	ae := lastEntry(t, es, "analysis.error")
	msg, _ := ae.Details["error"].(string)
	if !strings.Contains(msg, e.wal.ID) || !strings.Contains(msg, e.wal.SHA256) {
		t.Errorf("analysis.error %q names neither the artifact nor the manifest digest", msg)
	}
	if res.Ingest.Outcome != "incomplete" || res.Counts.Written == 0 {
		t.Errorf("result %+v: flushed rows must be kept and flagged incomplete", res.Ingest)
	}
	if count(es, "analysis.end") != 0 {
		t.Error("analysis.end written for a failed run")
	}
}

func TestRunAuditCarriesInputsSiblingsAndCompanionFacts(t *testing.T) {
	e := newRunEnv(t)
	res := mustRun(t, e.c, e.in(), Options{})
	es := readAudit(t, e.c)
	st := lastEntry(t, es, "analysis.start").Details
	if st["op"] != "sqlite-recover" || st["parent_id"] != e.db.ID || st["algorithm"] != "minutiae-sqlite-recover/1" || st["rules_hash"] != RulesHash() {
		t.Errorf("start %v", st)
	}
	ins, _ := st["inputs"].([]any)
	if len(ins) != 3 {
		t.Fatalf("inputs %v", st["inputs"])
	}
	first, _ := ins[0].(map[string]any)
	if first["role"] != "db" || first["id"] != e.db.ID || first["sha256"] != e.db.SHA256 || first["recovered"] != false || first["incomplete"] != false {
		t.Errorf("first input %v", first)
	}
	sib, _ := st["siblings"].(map[string]any)
	wal, _ := sib["wal"].(map[string]any)
	if wal["status"] != "found" || wal["searched_by"] == "" {
		t.Errorf("siblings %v", sib)
	}
	if _, ok := st["options"].(map[string]any); !ok {
		t.Errorf("options %v", st["options"])
	}
	end := lastEntry(t, es, "analysis.end").Details
	comp, _ := end["companions"].(map[string]any)
	cw, _ := comp["wal"].(map[string]any)
	cj, _ := comp["journal"].(map[string]any)
	if cw["header_valid"] != true || cw["used_by_live"] != true || cw["version_refused"] != false {
		t.Errorf("wal facts %v", cw)
	}
	if cj["header_valid"] != true || cj["hot"] != true || cj["applied"] != true || cj["has_super_journal"] != false {
		t.Errorf("journal facts %v", cj)
	}
	if s, _ := cj["super_journal_present"].(string); !strings.Contains(s, "not modelled") {
		t.Errorf("super_journal_present %v", cj["super_journal_present"])
	}
	if fmt.Sprint(end["written"]) != fmt.Sprint(res.Counts.Written) || end["ingest_id"] != res.Ingest.IngestID || end["analysis_id"] != res.AnalysisID {
		t.Errorf("end %v", end)
	}
	if res.Inputs[1].Facts["header_valid"] != true {
		t.Errorf("InputInfo facts %v", res.Inputs[1].Facts)
	}
}

func TestSQLiteRecoverNeverModifiesSource(t *testing.T) {
	e := newRunEnv(t)
	type snap struct {
		sum string
		mod time.Time
	}
	take := func() map[string]snap {
		out := map[string]snap{}
		var paths []string
		_ = filepath.WalkDir(filepath.Join(e.c.Dir, "artifacts"), func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				paths = append(paths, p)
			}
			return err
		})
		for _, p := range paths {
			b, err := os.ReadFile(p)
			fi, err2 := os.Stat(p)
			if err != nil || err2 != nil {
				t.Fatal(err, err2)
			}
			s := sha256.Sum256(b)
			out[p] = snap{hex.EncodeToString(s[:]), fi.ModTime()}
		}
		return out
	}
	before := take()
	mustRun(t, e.c, e.in(), Options{})
	if after := take(); !reflect.DeepEqual(before, after) {
		t.Errorf("artifacts changed or gained files:\nbefore %v\nafter  %v", before, after)
	}
}

func TestChainRowsInheritTheLowestInputCap(t *testing.T) {
	c := recordstest.NewCase(t)
	f := newBuilderDB(t)
	img := evidencetest.AddImage(t, c, f.db)
	cap50 := 50
	spec := carvedSpec()
	spec.Confidence = &cap50
	spec.Runs = []evidence.Run{{Offset: 0, Length: int64(len(f.db))}}
	db := evidencetest.AddRecovered(t, c, img, f.db, spec)
	wal := addFile(t, c, "/data/other.db-wal", f.wal)
	jr := addFile(t, c, "/data/other.db-journal", f.journal)
	res := mustRun(t, c, Inputs{DB: db.ID, WAL: wal.ID, Journal: jr.ID}, Options{})
	if res.Counts.Written == 0 {
		t.Fatal("no rows")
	}
	high := false
	for _, r := range storedRecords(t, c) {
		if r.Confidence == nil || *r.Confidence > 50 {
			t.Errorf("record %d confidence %v above the cap 50", r.ID, r.Confidence)
		}
		p := payloadOf(t, r)
		if p["confidence_cap"] != float64(50) || p["mixed_inputs"] != true {
			t.Errorf("payload cap %v mixed %v", p["confidence_cap"], p["mixed_inputs"])
		}
		if m, _ := p["method_confidence"].(float64); m > 50 {
			high = true
		}
	}
	if !high {
		t.Error("no row had a method confidence above the cap: the cap was not exercised")
	}
	if !res.Inputs[0].Recovered || res.Inputs[0].Cap == nil || *res.Inputs[0].Cap != 50 || res.Inputs[1].Recovered {
		t.Errorf("inputs %+v", res.Inputs)
	}
	es := readAudit(t, c)
	mixed := 0
	for _, i := range indexes(es, "analysis.warning") {
		if r, _ := es[i].Details["reason"].(string); strings.Contains(r, "mixed") {
			mixed++
		}
	}
	if mixed != 1 {
		t.Errorf("%d mixed-input warnings, want 1", mixed)
	}
}

func TestForeignWALIsWarnedAndRowsSayByName(t *testing.T) {
	e := newRunEnv(t)
	other := addFile(t, e.c, "/data/y.db", e.f.db)
	foreign := addFile(t, e.c, "/data/y.db-wal", sqlitetestWAL4k(t))
	res := mustRun(t, e.c, Inputs{DB: other.ID}, Options{})
	if res.Siblings.WAL.ArtifactID != foreign.ID {
		t.Fatalf("siblings %+v", res.Siblings)
	}
	found := false
	for _, w := range readAudit(t, e.c) {
		if r, _ := w.Details["reason"].(string); w.Action == "analysis.warning" && strings.Contains(r, sqlitefile.WarnWALPageSizeMismatch) {
			found = true
		}
	}
	if !found {
		t.Error("no analysis.warning names the foreign WAL's page size")
	}
	for _, r := range storedRecords(t, e.c) {
		if payloadOf(t, r)["sibling_pairing_wal"] != "by-name" {
			t.Errorf("record %d does not say the WAL was paired by name: %v", r.ID, payloadOf(t, r)["sibling_pairing_wal"])
		}
	}
}

func TestUnusableDiscoveredWALIsReportedNotSilent(t *testing.T) {
	for name, wal := range map[string][]byte{
		"zeroed-header": make([]byte, 32+1024+24),
		"bad-magic":     bytes.Repeat([]byte{0x5a}, 32+1024+24),
	} {
		t.Run(name, func(t *testing.T) {
			c := recordstest.NewCase(t)
			f := newBuilderDB(t)
			db := addFile(t, c, "/data/x.db", f.db)
			w := addFile(t, c, "/data/x.db-wal", wal)
			res := mustRun(t, c, Inputs{DB: db.ID}, Options{})
			s := res.Siblings.WAL
			if s.Status != SiblingFound || s.ArtifactID != w.ID || s.Unused == "" {
				t.Fatalf("sibling %+v", s)
			}
			if txt := s.Text("WAL"); !strings.Contains(txt, "found, unused") || !strings.Contains(txt, s.Unused) {
				t.Errorf("text %q", txt)
			}
			if res.Counts.Written == 0 {
				t.Fatal("the run did not proceed")
			}
			for _, r := range storedRecords(t, c) {
				un, _ := payloadOf(t, r)["sibling_unused"].([]any)
				if len(un) != 1 {
					t.Fatalf("record %d sibling_unused %v", r.ID, un)
				}
				m := un[0].(map[string]any)
				if m["id"] != w.ID || m["role"] != "wal" || m["reason"] == "" {
					t.Errorf("sibling_unused %v", m)
				}
			}
			es := readAudit(t, c)
			comp, _ := lastEntry(t, es, "analysis.end").Details["companions"].(map[string]any)
			cw, _ := comp["wal"].(map[string]any)
			if cw["header_valid"] != false || cw["used_by_live"] != false || cw["unused_reason"] == nil {
				t.Errorf("facts %v", cw)
			}
			sib, _ := lastEntry(t, es, "analysis.start").Details["siblings"].(map[string]any)
			sw, _ := sib["wal"].(map[string]any)
			if sw["status"] != "found" {
				t.Errorf("start siblings %v", sib)
			}
		})
	}
}

func TestExplicitUnusableCompanionIsARefusal(t *testing.T) {
	c := recordstest.NewCase(t)
	f := newBuilderDB(t)
	db := addFile(t, c, "/data/x.db", f.db)
	w := addFile(t, c, "/data/zzz.wal", make([]byte, 2000))
	_, err := Run(context.Background(), c, Inputs{DB: db.ID, WAL: w.ID}, Options{})
	var re *RefusalError
	if !errors.As(err, &re) || re.Reason != ReasonSiblingUnusable {
		t.Fatalf("err %v", err)
	}
	es := readAudit(t, c)
	if count(es, "analysis.error") != 1 || count(es, evidence.ActionIngestStart) != 0 {
		t.Errorf("error %d ingest.start %d", count(es, "analysis.error"), count(es, evidence.ActionIngestStart))
	}
}

func TestRunVerifiesClean(t *testing.T) {
	e := newRunEnv(t)
	mustRun(t, e.c, e.in(), Options{})
	rep, err := e.c.Verify()
	if err != nil {
		t.Fatal(err)
	}
	evidencetest.RequireClean(t, rep)
}

func TestRunSupersedesOlderVersionRun(t *testing.T) {
	e := newRunEnv(t)
	restore := SetVersionForTest("0.9.0")
	old := mustRun(t, e.c, e.in(), Options{})
	restore()
	cur := mustRun(t, e.c, e.in(), Options{})
	r, err := records.NewReader(e.c)
	if err != nil {
		t.Fatal(err)
	}
	res, err := r.List(context.Background(), records.Filter{IncludeSuperseded: true}, records.Page{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	var oldN, curN int
	for _, row := range res.Rows {
		switch row.IngestID {
		case old.Ingest.IngestID:
			oldN++
			if !row.Superseded {
				t.Errorf("record %d of the old run is not superseded", row.ID)
			}
		case cur.Ingest.IngestID:
			curN++
			if row.Superseded {
				t.Errorf("record %d of the current run is superseded", row.ID)
			}
		}
	}
	if oldN == 0 || curN == 0 {
		t.Errorf("old %d current %d", oldN, curN)
	}
}

func TestAlreadyIngestedIsRefusedUnlessAllowed(t *testing.T) {
	e := newRunEnv(t)
	mustRun(t, e.c, e.in(), Options{})
	if _, err := Run(context.Background(), e.c, e.in(), Options{}); !errors.Is(err, records.ErrAlreadyIngested) {
		t.Fatalf("err %v", err)
	}
	mustRun(t, e.c, e.in(), Options{AllowReingest: true})
}

func TestRunResultCountsMatchStoredRecords(t *testing.T) {
	check := func(t *testing.T, c *evidence.Case, res Result) {
		t.Helper()
		got := storedRecords(t, c)
		if int64(len(got)) != res.Counts.Written || res.Counts.Written != res.Ingest.Records {
			t.Errorf("stored %d, Counts.Written %d, ingest %d", len(got), res.Counts.Written, res.Ingest.Records)
		}
		if sumMap(res.Counts.ByMethod) != res.Counts.Written {
			t.Errorf("by method %v vs %d", res.Counts.ByMethod, res.Counts.Written)
		}
		by := map[string]int64{}
		for _, f := range got {
			by[f.Method]++
		}
		if !reflect.DeepEqual(by, res.Counts.ByMethod) {
			t.Errorf("stored by method %v, result %v", by, res.Counts.ByMethod)
		}
	}
	t.Run("complete", func(t *testing.T) {
		e := newRunEnv(t)
		check(t, e.c, mustRun(t, e.c, e.in(), Options{}))
	})
	t.Run("cancelled", func(t *testing.T) {
		e := newRunEnv(t)
		defer SetProgressEveryForTest(1)()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		n := 0
		res, err := Run(ctx, e.c, e.in(), Options{Progress: func(Progress) {
			if n++; n == 3 {
				cancel()
			}
		}})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err %v", err)
		}
		if res.Counts.Written < 3 || res.Ingest.Outcome != "incomplete" {
			t.Errorf("result %+v", res.Ingest)
		}
		check(t, e.c, res)
		es := readAudit(t, e.c)
		if count(es, "analysis.error") != 1 || count(es, evidence.ActionIngestError) != 1 || count(es, evidence.ActionIngestEnd) != 0 {
			t.Errorf("audit: error %d ingest.error %d ingest.end %d", count(es, "analysis.error"), count(es, evidence.ActionIngestError), count(es, evidence.ActionIngestEnd))
		}
	})
}

func TestZeroRowResultStatesWhatWasExamined(t *testing.T) {
	c := recordstest.NewCase(t)
	b := buildTable(0)
	db := addFile(t, c, "/data/clean.db", b.Bytes())
	res := mustRun(t, c, Inputs{DB: db.ID}, Options{})
	if res.Counts.Written != 0 || res.Ingest.Outcome != "complete" {
		t.Fatalf("result %+v %+v", res.Counts, res.Ingest)
	}
	if res.Inputs[0].Facts["page_count"] == nil || res.Inputs[0].Facts["page_size"] != int64(testPageSize) {
		t.Errorf("facts %v", res.Inputs[0].Facts)
	}
	if len(res.Counts.PagesByOrigin) == 0 {
		t.Errorf("a zero-row result says nothing about the pages examined: %+v", res.Counts)
	}
	end := lastEntry(t, readAudit(t, c), "analysis.end").Details
	if fmt.Sprint(end["written"]) != "0" || end["pages_by_origin"] == nil || end["freelist_pages"] == nil {
		t.Errorf("end %v", end)
	}
}

func TestLibraryWarningsAreSentOnceAfterTheLastPass(t *testing.T) {
	e := newRunEnv(t)
	res := mustRun(t, e.c, e.in(), Options{})
	es := readAudit(t, e.c)
	iIng, iBatch := firstIndex(es, evidence.ActionIngestStart), firstIndex(es, "records.batch")
	seen := map[string]int{}
	lib := 0
	for _, i := range indexes(es, "analysis.warning") {
		d := es[i].Details
		if d["ingest_id"] == nil {
			t.Errorf("warning %d not written by the writer: %v", i, d)
		}
		if i < iIng || i < iBatch {
			t.Errorf("warning %d precedes the ingest start or the first batch", i)
		}
		r, _ := d["reason"].(string)
		if strings.HasPrefix(r, "library warning") {
			lib++
			key := fmt.Sprint(d["path"], "|", r)
			if seen[key]++; seen[key] > 1 {
				t.Errorf("warning sent twice: %s", key)
			}
		}
	}
	if lib == 0 || lib != res.Counts.LibraryWarnings {
		t.Errorf("%d library warnings in the log, Counts.LibraryWarnings %d", lib, res.Counts.LibraryWarnings)
	}
	// none lost: every warning the library holds after the same attachments is in the log
	want := map[string]bool{}
	for _, w := range libraryWarnings(t, e.f) {
		_, r := warningText(w)
		want[r] = true
	}
	for _, i := range indexes(es, "analysis.warning") {
		delete(want, fmt.Sprint(es[i].Details["reason"]))
	}
	if len(want) != 0 {
		t.Errorf("library warnings missing from the log: %v", want)
	}
}

func TestOutsideRangeRowsAreAuditedWarningsAndCounted(t *testing.T) {
	e := newRunEnv(t)
	n := 0
	defer SetRowHookForTest(func(r sqlitefile.RecoveredRow) sqlitefile.RecoveredRow {
		if r.Loc.File == sqlitefile.FileDB {
			if n++; n <= 2 {
				r.Loc.Offset = e.db.Size + 1000
			}
		}
		return r
	})()
	res := mustRun(t, e.c, e.in(), Options{})
	if res.Counts.RangeOutside != 2 {
		t.Fatalf("RangeOutside %d (db rows seen %d)", res.Counts.RangeOutside, n)
	}
	es := readAudit(t, e.c)
	iBatch := firstIndex(es, "records.batch")
	w := 0
	for _, i := range indexes(es, "analysis.warning") {
		r, _ := es[i].Details["reason"].(string)
		if !strings.Contains(r, "outside") {
			continue
		}
		w++
		if i < iBatch || es[i].Details["path"] == "" || es[i].Details["ingest_id"] == nil {
			t.Errorf("entry %d: %v", i, es[i].Details)
		}
	}
	if w != 2 {
		t.Errorf("%d outside-range warnings, want 2", w)
	}
	if fmt.Sprint(lastEntry(t, es, "analysis.end").Details["range_outside"]) != "2" {
		t.Errorf("end %v", lastEntry(t, es, "analysis.end").Details)
	}
	out := 0
	for _, r := range storedRecords(t, e.c) {
		if r.Range == nil {
			out++
			if p := payloadOf(t, r); p["loc_offset"] == nil {
				t.Errorf("record %d lost its location claim", r.ID)
			}
		}
	}
	if out != 2 {
		t.Errorf("%d stored records without a range", out)
	}
}

func TestWriterRejectedRowsAreCountedAndTheRunContinues(t *testing.T) {
	e := newRunEnv(t)
	total := sumMap(libraryRowsByMethod(t, e.f))
	n := 0
	defer SetCheckForTest(func(p map[string]any) error {
		if n++; n <= 2 {
			return fmt.Errorf("%w: db_row: forced", records.ErrInvalidPayload)
		}
		return checkPayload(p)
	})()
	res := mustRun(t, e.c, e.in(), Options{})
	if res.Counts.SkippedByReason["writer-rejected"] != 2 || res.Counts.Written != total-2 {
		t.Errorf("skipped %v written %d total %d", res.Counts.SkippedByReason, res.Counts.Written, total)
	}
	if res.Ingest.Rejected != 2 {
		t.Errorf("writer rejected %d", res.Ingest.Rejected)
	}
	if res.Ingest.Outcome != "incomplete" || !slices.Contains(res.Counts.Narrowed, "writer-rejected") {
		t.Errorf("outcome %q narrowed %v: a run that lost rows is narrowed (J112)", res.Ingest.Outcome, res.Counts.Narrowed)
	}
}

func TestNarrowedRunNeverCallsEnd(t *testing.T) {
	for name, tc := range map[string]struct {
		opts   Options
		narrow string
	}{
		"methods":        {Options{Methods: []string{sqlitefile.MethodFreelist}}, "methods"},
		"min-confidence": {Options{MinConfidence: 10}, "min-confidence"},
		"exclude":        {Options{ExcludeTables: []ExcludedTable{{Table: "t", Parser: "p", Version: "1"}}}, "exclude-tables"},
	} {
		t.Run(name, func(t *testing.T) {
			e := newRunEnv(t)
			res, err := Run(context.Background(), e.c, e.in(), tc.opts)
			if err != nil {
				t.Fatalf("a narrowed run is not an error: %v", err)
			}
			if res.Ingest.Outcome != "incomplete" || !slices.Contains(res.Counts.Narrowed, tc.narrow) {
				t.Errorf("outcome %q narrowed %v", res.Ingest.Outcome, res.Counts.Narrowed)
			}
			es := readAudit(t, e.c)
			if count(es, evidence.ActionIngestEnd) != 0 || count(es, evidence.ActionIngestError) != 1 || count(es, "analysis.end") != 1 {
				t.Errorf("ingest.end %d ingest.error %d analysis.end %d", count(es, evidence.ActionIngestEnd), count(es, evidence.ActionIngestError), count(es, "analysis.end"))
			}
			end := lastEntry(t, es, "analysis.end").Details
			if nl, _ := end["narrowed"].([]any); len(nl) == 0 {
				t.Errorf("end %v", end)
			}
			// a following full run is not refused
			mustRun(t, e.c, e.in(), Options{})
		})
	}
}

func TestRunLimitStopsCleanly(t *testing.T) {
	e := newRunEnv(t)
	res := mustRun(t, e.c, e.in(), Options{MaxRecords: 3})
	if res.Counts.Written != 3 || !slices.Contains(res.Counts.LimitsHit, "max-records") || res.Ingest.Outcome != "incomplete" {
		t.Errorf("result %+v %+v", res.Counts, res.Ingest)
	}
	es := readAudit(t, e.c)
	end := lastEntry(t, es, "analysis.end").Details
	if l, _ := end["limits_hit"].([]any); len(l) == 0 {
		t.Errorf("end %v", end)
	}
	requireLimitWarning(t, es, "max-records")
	// exactly meeting the cap is not a limit
	e2 := newRunEnv(t)
	total := sumMap(libraryRowsByMethod(t, e2.f))
	r2 := mustRun(t, e2.c, e2.in(), Options{MaxRecords: total})
	if len(r2.Counts.LimitsHit) != 0 || r2.Ingest.Outcome != "complete" {
		t.Errorf("exact cap: %+v", r2.Counts)
	}
}

func TestRunRefusals(t *testing.T) {
	t.Run("not sqlite", func(t *testing.T) {
		c := recordstest.NewCase(t)
		a := addFile(t, c, "/data/n.db", bytes.Repeat([]byte("hello world, not a database. "), 100))
		_, err := Run(context.Background(), c, Inputs{DB: a.ID}, Options{})
		var re *RefusalError
		if !errors.As(err, &re) || re.Reason != ReasonNotSQLite || !errors.Is(err, ErrNotSQLite) {
			t.Fatalf("err %v", err)
		}
		es := readAudit(t, c)
		if count(es, "analysis.start") != 1 || count(es, "analysis.error") != 1 || count(es, evidence.ActionIngestStart) != 0 {
			t.Errorf("audit start %d error %d ingest %d", count(es, "analysis.start"), count(es, "analysis.error"), count(es, evidence.ActionIngestStart))
		}
	})
	t.Run("bad options", func(t *testing.T) {
		e := newRunEnv(t)
		for name, o := range map[string]Options{
			"unknown method": {Methods: []string{"sqlite-bogus"}}, "confidence": {MinConfidence: 101},
			"payload": {MaxPayloadBytes: MaxPayloadBytesCeiling + 1}, "body": {MaxBodyBytes: MaxBodyBytesCeiling + 1},
			"records": {MaxRecords: 50_000_001}, "negative": {MaxBytes: -1}, "fit equality": {MaxFitEqualityRows: MaxFitEqualityRowsCeiling + 1}, "table": {ExcludeTables: []ExcludedTable{{}}},
		} {
			_, err := Run(context.Background(), e.c, e.in(), o)
			var re *RefusalError
			if !errors.As(err, &re) || re.Reason != ReasonBadOption {
				t.Errorf("%s: err %v", name, err)
			}
		}
		if n := count(readAudit(t, e.c), "analysis.start"); n != 0 {
			t.Errorf("%d analysis.start for refused options", n)
		}
	})
	t.Run("unknown artifact", func(t *testing.T) {
		e := newRunEnv(t)
		if _, err := Run(context.Background(), e.c, Inputs{DB: "nope"}, Options{}); err == nil {
			t.Error("no error")
		}
	})
}

func TestRunSiblingRefusalsComeBeforeAnalysisStart(t *testing.T) {
	noStart := func(t *testing.T, c *evidence.Case) {
		t.Helper()
		es := readAudit(t, c)
		if count(es, "analysis.error") != 1 || count(es, "analysis.start") != 0 || count(es, evidence.ActionIngestStart) != 0 {
			t.Errorf("error %d start %d ingest %d", count(es, "analysis.error"), count(es, "analysis.start"), count(es, evidence.ActionIngestStart))
		}
	}
	t.Run("ambiguous", func(t *testing.T) {
		e := newRunEnv(t)
		src := evidence.Source{Kind: "file", DeviceID: "dev1", RemotePath: "/data/x.db-wal"}
		if _, err := e.c.Capture("dev1", "acq1", "copy2/x.db-wal", src, func(w io.Writer) error { _, err := w.Write(e.f.wal); return err }); err != nil {
			t.Fatal(err)
		}
		_, err := Run(context.Background(), e.c, e.in(), Options{})
		var re *RefusalError
		if !errors.Is(err, ErrAmbiguousSibling) || !errors.As(err, &re) || re.Reason != ReasonAmbiguousSibling {
			t.Fatalf("err %v", err)
		}
		noStart(t, e.c)
		// naming one resolves it; so does turning the search off
		res := mustRun(t, e.c, Inputs{DB: e.db.ID, WAL: e.wal.ID}, Options{})
		if res.Siblings.WAL.Status != SiblingExplicit || res.Siblings.WAL.ArtifactID != e.wal.ID {
			t.Errorf("siblings %+v", res.Siblings)
		}
		for _, r := range storedRecords(t, e.c) {
			if p := payloadOf(t, r); p["sibling_pairing_wal"] != "explicit" || p["sibling_pairing_journal"] != "by-name" {
				t.Errorf("pairing wal %v journal %v: the WAL was named, the journal found by name", payloadOf(t, r)["sibling_pairing_wal"], payloadOf(t, r)["sibling_pairing_journal"])
			}
		}
		res = mustRun(t, e.c, Inputs{DB: e.db.ID, NoSiblings: true}, Options{AllowReingest: true})
		if res.Siblings.WAL.Status != SiblingDisabled || res.Siblings.Journal.Status != SiblingDisabled {
			t.Errorf("siblings %+v", res.Siblings)
		}
	})
	t.Run("conflict", func(t *testing.T) {
		e := newRunEnv(t)
		_, err := Run(context.Background(), e.c, Inputs{DB: e.db.ID, WAL: e.wal.ID, NoSiblings: true}, Options{})
		var re *RefusalError
		if !errors.As(err, &re) || re.Reason != ReasonBadOption {
			t.Fatalf("err %v", err)
		}
		noStart(t, e.c)
	})
	t.Run("search error is a refusal, never none", func(t *testing.T) {
		e := newRunEnv(t)
		defer SetSiblingScanForTest(func([]evidence.ManifestRecord, evidence.ManifestRecord) SiblingReport { panic("poisoned") })()
		_, err := Run(context.Background(), e.c, e.in(), Options{})
		var re *RefusalError
		if !errors.As(err, &re) || re.Reason != ReasonSiblingUnusable || !errors.Is(err, ErrSiblingSearch) {
			t.Fatalf("err %v", err)
		}
		noStart(t, e.c)
	})
}

func TestPlanMatchesRun(t *testing.T) {
	ran := 0
	for _, name := range fixtureNames(t) {
		t.Run(name, func(t *testing.T) {
			c, in := fixtureCase(t, name)
			as, perr := Plan(context.Background(), c, in, Options{})
			res, rerr := Run(context.Background(), c, in, Options{})
			if refusalReason(perr) != refusalReason(rerr) {
				t.Fatalf("plan error %v, run error %v", perr, rerr)
			}
			if rerr != nil {
				return
			}
			ran++
			if !reflect.DeepEqual(nonEmpty(as.Counts.ByMethod), nonEmpty(res.Counts.ByMethod)) ||
				!reflect.DeepEqual(nonEmptyS(as.Counts.SkippedByReason), nonEmptyS(res.Counts.SkippedByReason)) ||
				!reflect.DeepEqual(as.Counts.ConfidenceRange, res.Counts.ConfidenceRange) {
				t.Errorf("plan %+v\nrun  %+v", as.Counts, res.Counts)
			}
			stored := map[string]int64{}
			lo, hi := map[string]int{}, map[string]int{}
			for _, f := range storedRecords(t, c) {
				stored[f.Method]++
				v := *f.Confidence
				if l, ok := lo[f.Method]; !ok || v < l {
					lo[f.Method] = v
				}
				if v > hi[f.Method] {
					hi[f.Method] = v
				}
			}
			if !reflect.DeepEqual(stored, nonEmpty(as.Counts.ByMethod)) {
				t.Errorf("stored %v, plan %v", stored, as.Counts.ByMethod)
			}
			for m, r := range as.Counts.ConfidenceRange {
				if r.Min != lo[m] || r.Max != hi[m] {
					t.Errorf("%s: plan range %+v, stored %d..%d", m, r, lo[m], hi[m])
				}
			}
			if !strings.Contains(as.Note, "nothing was written") {
				t.Errorf("note %q", as.Note)
			}
		})
	}
	if ran == 0 {
		t.Error("no fixture ran to completion")
	}
}

func nonEmptyS(m map[string]int64) map[string]int64 { return nonEmpty(m) }

func TestPlanWritesNothing(t *testing.T) {
	e := newRunEnv(t)
	files := []string{"audit.jsonl", "artifacts.db", "manifest.jsonl"}
	digest := func() map[string]string {
		out := map[string]string{}
		for _, f := range files {
			b, err := os.ReadFile(filepath.Join(e.c.Dir, f))
			if err != nil {
				t.Fatal(err)
			}
			s := sha256.Sum256(b)
			out[f] = hex.EncodeToString(s[:])
		}
		return out
	}
	before := digest()
	as, err := Plan(context.Background(), e.c, e.in(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if sumMap(as.Counts.ByMethod) == 0 {
		t.Error("the plan found no rows in a scenario that has them")
	}
	if after := digest(); !reflect.DeepEqual(before, after) {
		t.Errorf("Plan changed the case: %v -> %v", before, after)
	}
	if n := len(storedRecords(t, e.c)); n != 0 {
		t.Errorf("%d records after a Plan", n)
	}
}
