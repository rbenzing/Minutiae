package artparse_test

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/artparse"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
	"github.com/rbenzing/minutiae/internal/recordtypes/message"
)

const recArtSize = 8192

// recMember describes one member of a recovery job's bundle.
type recMember struct {
	role     string
	recovery *parse.RecoveryInfo
}

// renv is a real ingest with a recovery emitter over a bundle of members.
type renv struct {
	c   *evidence.Case
	spy *spy
	e   *artparse.RecoveryEmitter
	ids map[string]string // role -> artifact id
}

func newRenv(t *testing.T, minConfidence int, members ...recMember) *renv {
	t.Helper()
	c := newCase(t)
	reg := register(t, "emit", "1.0.0")
	w, err := records.NewWriter(c, reg.Identity(), records.WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = w.Abort(context.Background(), errors.New("test over")) })
	v := &renv{c: c, ids: map[string]string{}}
	arts := map[string]parse.Artifact{}
	var ids []string
	for _, m := range members {
		rec := recordstest.AddArtifact(t, c, m.role+".bin", []byte(strings.Repeat("x", recArtSize)))
		v.ids[m.role] = rec.ID
		ids = append(ids, rec.ID)
		a := parse.Artifact{ID: rec.ID, Platform: parse.PlatformAndroid, Size: recArtSize, Recovery: m.recovery}
		a.Source.RemotePath = "/data/app/" + m.role
		arts[m.role] = a
	}
	if err := w.Start(context.Background(), records.StartOptions{AnalysisID: "an-r", Artifacts: ids}); err != nil {
		t.Fatal(err)
	}
	v.spy = &spy{IngestWriter: w}
	v.e, err = artparse.NewRecoveryEmitter(context.Background(), v.spy, reg, arts, parse.DefaultLimits(), nil, nil, minConfidence)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// mapped is a record as a mapper builds it: the primary artifact, a live position, no locator, live.
func (v *renv) mapped() records.Record {
	return records.Record{
		Type: message.Type, ArtifactID: v.ids["db"], Range: &records.Range{Offset: 1, Length: 2},
		Summary: "hello", Payload: recordstest.ValidPayload(message.Type),
	}
}

func walFacts() parse.RecoveredFacts {
	return parse.RecoveredFacts{
		Table: "sms", Method: "sqlite-wal-prior", Confidence: 75, Basis: "schema", Relation: "superseded-version",
		Origin: "db-under-wal", Role: parse.RoleWAL, Range: records.Range{Offset: 4120, Length: 61},
		Locator: "sqlite:table=sms;page=3;cell=1;wal_frame=2;file=wal", Page: 3, Cell: 1,
		WAL:   &parse.RecoveredWAL{Frame: 2, Salt1: 0xA, Salt2: 0xB, Committed: true},
		Notes: []string{"joins-from-live-view"},
	}
}

func dbFacts() parse.RecoveredFacts {
	f := walFacts()
	f.Role, f.WAL, f.Origin = parse.RoleDB, nil, "live-db"
	f.Locator = "sqlite:table=sms;page=3;cell=1;file=db"
	return f
}

func (v *renv) stored(t *testing.T, n int) records.Record {
	t.Helper()
	if len(v.spy.added) != n {
		t.Fatalf("%d records reached the writer, want %d", len(v.spy.added), n)
	}
	return v.spy.added[n-1]
}

func recOf(t *testing.T, r records.Record) map[string]any {
	t.Helper()
	m, ok := r.Payload["recovery"].(map[string]any)
	if !ok {
		t.Fatalf("payload.recovery = %#v", r.Payload["recovery"])
	}
	return m
}

func rejections(v *renv) int { _, rej, _ := v.e.Counts(); return rej }

func TestTypedRecoveryStampsRowFacts(t *testing.T) {
	v := newRenv(t, 0, recMember{role: "db"}, recMember{role: "wal"})
	r := v.mapped()
	want := maps.Clone(r.Payload)
	if err := v.e.EmitRecovered(ctx0(), r, walFacts()); err != nil {
		t.Fatal(err)
	}
	got := v.stored(t, 1)
	if !got.Deleted || got.Recovery != "sqlite-wal-prior" || got.Confidence == nil || *got.Confidence != 75 {
		t.Errorf("record %+v, want deleted, sqlite-wal-prior, confidence 75", got)
	}
	if got.ArtifactID != v.ids["wal"] || got.SourcePath != "/data/app/wal" || got.Locator != walFacts().Locator {
		t.Errorf("artifact %s source %q locator %q, want the wal's", got.ArtifactID, got.SourcePath, got.Locator)
	}
	if got.Range == nil || *got.Range != (records.Range{Offset: 4120, Length: 61}) {
		t.Errorf("range %+v, want the cell's", got.Range)
	}
	wantRec := map[string]any{
		"relation": "superseded-version", "table_basis": "schema", "origin": "db-under-wal", "via": "typed-recovery",
		"page": int64(3), "cell": int64(1), "file": "wal",
		"wal":               map[string]any{"frame": int64(2), "salt1": int64(10), "salt2": int64(11), "committed": true},
		"sibling_artifacts": []any{v.ids["db"]},
		"notes":             []any{"joins-from-live-view"},
	}
	if rec := recOf(t, got); !reflect.DeepEqual(rec, wantRec) {
		t.Errorf("payload.recovery = %#v\nwant %#v", rec, wantRec)
	}
	for k, w := range want {
		if !reflect.DeepEqual(got.Payload[k], w) {
			t.Errorf("mapper payload field %q changed: %v, want %v", k, got.Payload[k], w)
		}
	}
	if _, touched := r.Payload["recovery"]; touched || r.Deleted || r.Recovery != "" {
		t.Error("the caller's record was modified")
	}
}

func TestTypedRecoveryStampsJournalAndTruncation(t *testing.T) {
	v := newRenv(t, 0, recMember{role: "db"}, recMember{role: "journal"})
	f := walFacts()
	f.Role, f.WAL, f.Origin, f.Truncated = parse.RoleJournal, nil, "db-rolled-back", true
	f.Journal = &parse.RecoveredJournal{Record: 4, Segment: 1, Hot: true, Applied: false}
	f.Locator = "sqlite:table=sms;page=3;cell=1;journal_rec=4;file=journal"
	if err := v.e.EmitRecovered(ctx0(), v.mapped(), f); err != nil {
		t.Fatal(err)
	}
	rec := recOf(t, v.stored(t, 1))
	wantJ := map[string]any{"record": int64(4), "segment": int64(1), "hot": true, "applied": false}
	if rec["file"] != "journal" || rec["truncated"] != true || !reflect.DeepEqual(rec["journal"], wantJ) {
		t.Errorf("payload.recovery = %#v", rec)
	}
	if _, ok := rec["wal"]; ok {
		t.Errorf("a journal row carries wal facts: %v", rec)
	}
}

func TestTypedRecoveryOnRecoveredArtifactTakesTheLower(t *testing.T) {
	c40 := 40
	v := newRenv(t, 0, recMember{role: "db", recovery: &parse.RecoveryInfo{Class: "carved", Method: "carve-x", Confidence: &c40}})
	f := dbFacts()
	f.Confidence = 60
	if err := v.e.EmitRecovered(ctx0(), v.mapped(), f); err != nil {
		t.Fatal(err)
	}
	got := v.stored(t, 1)
	if got.Confidence == nil || *got.Confidence != 40 || got.Recovery != "sqlite-wal-prior" || !got.Deleted {
		t.Errorf("record %+v, want confidence 40 and the row's method", got)
	}
	rec := recOf(t, got)
	if rec["artifact_method"] != "carve-x" || rec["artifact_confidence"] != 40 || rec["artifact_relation"] != "from-recovered-artifact" ||
		rec["relation"] != "superseded-version" || rec["cap_from"] != v.ids["db"] {
		t.Errorf("payload.recovery = %#v", rec)
	}
}

func TestTypedRecoveryMixedBundleCapsByBundle(t *testing.T) {
	c30 := 30
	v := newRenv(t, 0, recMember{role: "db"}, recMember{role: "wal", recovery: &parse.RecoveryInfo{Class: "carved", Method: "carve-x", Confidence: &c30}})
	f := dbFacts()
	f.Confidence = 60
	if err := v.e.EmitRecovered(ctx0(), v.mapped(), f); err != nil {
		t.Fatal(err)
	}
	got := v.stored(t, 1)
	if got.Confidence == nil || *got.Confidence != 30 || !got.Deleted || got.ArtifactID != v.ids["db"] {
		t.Errorf("record %+v, want confidence 30 on the live database", got)
	}
	rec := recOf(t, got)
	notes, _ := rec["notes"].([]any)
	if rec["cap_from"] != v.ids["wal"] || rec["artifact_relation"] != "from-recovered-bundle" || rec["bundle_member"] != v.ids["wal"] ||
		rec["relation"] != "superseded-version" || !slices.Contains(notes, any("mixed-bundle")) || rec["artifact_method"] != "carve-x" {
		t.Errorf("payload.recovery = %#v", rec)
	}
	// a row already below the bundle cap is not attributed to it
	f.Confidence = 20
	if err := v.e.EmitRecovered(ctx0(), v.mapped(), f); err != nil {
		t.Fatal(err)
	}
	low := v.stored(t, 2)
	if low.Confidence == nil || *low.Confidence != 20 {
		t.Errorf("confidence %v, want the row's own 20", low.Confidence)
	}
	if _, ok := recOf(t, low)["cap_from"]; ok {
		t.Errorf("cap_from set on a record the cap did not lower: %v", recOf(t, low))
	}
}

func TestTypedRecoveryCapUnknownIs25(t *testing.T) {
	c60 := 60
	for name, ri := range map[string]*parse.RecoveryInfo{
		"no confidence":               {Class: "carved", Method: "carve-x", CapUnknown: true},
		"cut chain with a confidence": {Class: "carved", Method: "carve-x", Confidence: &c60, CapUnknown: true},
	} {
		v := newRenv(t, 0, recMember{role: "db", recovery: ri})
		if err := v.e.EmitRecovered(ctx0(), v.mapped(), dbFacts()); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got := v.stored(t, 1)
		notes, _ := recOf(t, got)["notes"].([]any)
		if got.Confidence == nil || *got.Confidence != 25 || !slices.Contains(notes, any("recovery-cap-unknown")) {
			t.Errorf("%s: record %+v notes %v, want 25 and recovery-cap-unknown", name, got, notes)
		}
	}
}

func TestTypedRecoveryRangeOutsideArtifact(t *testing.T) {
	for name, tc := range map[string]struct {
		rng     records.Range
		outside bool
	}{
		"fits to the last byte": {records.Range{Offset: recArtSize - 61, Length: 61}, false},
		"one byte past":         {records.Range{Offset: recArtSize - 60, Length: 61}, true},
		"offset past the end":   {records.Range{Offset: 9000, Length: 61}, true},
		"sum overflows":         {records.Range{Offset: 1<<63 - 2, Length: 61}, true},
		"empty":                 {records.Range{Offset: 10, Length: 0}, true},
		"negative offset":       {records.Range{Offset: -1, Length: 4}, true},
	} {
		v := newRenv(t, 0, recMember{role: "db"})
		f := dbFacts()
		f.Range = tc.rng
		if err := v.e.EmitRecovered(ctx0(), v.mapped(), f); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got := v.stored(t, 1)
		notes, _ := recOf(t, got)["notes"].([]any)
		has := slices.Contains(notes, any("range-outside-artifact"))
		if tc.outside {
			if got.Range != nil || !has || len(v.spy.warns) != 1 {
				t.Errorf("%s: range %v notes %v warnings %d, want nil, the note and one warning", name, got.Range, notes, len(v.spy.warns))
			}
		} else if got.Range == nil || *got.Range != tc.rng || has || len(v.spy.warns) != 0 {
			t.Errorf("%s: range %v notes %v warnings %d, want the cell range", name, got.Range, notes, len(v.spy.warns))
		}
	}
}

func TestTypedRecoveryRefusesBadFacts(t *testing.T) {
	guess := dbFacts()
	guess.Basis = "guess"
	noRole := walFacts() // a wal row, but the bundle holds no wal
	manyNotes := dbFacts()
	for i := 0; i < 33; i++ {
		manyNotes.Notes = append(manyNotes.Notes, "n"+string(rune('a'+i%26))+string(rune('a'+i/26)))
	}
	longNote := dbFacts()
	longNote.Notes = []string{strings.Repeat("n", 129)}
	emptyNote := dbFacts()
	emptyNote.Notes = []string{""}
	for name, f := range map[string]parse.RecoveredFacts{
		"basis guess": guess, "role not in the bundle": noRole, "too many notes": manyNotes,
		"a note over 128 bytes": longNote, "an empty note": emptyNote,
	} {
		v := newRenv(t, 0, recMember{role: "db"})
		if err := v.e.EmitRecovered(ctx0(), v.mapped(), f); err != nil {
			t.Fatalf("%s: EmitRecovered = %v, want nil (a counted rejection)", name, err)
		}
		if v.spy.adds.Load() != 0 || rejections(v) != 1 || len(v.spy.rejects) != 1 {
			t.Errorf("%s: adds %d rejected %d rejects %v, want one rejection and no write", name, v.spy.adds.Load(), rejections(v), v.spy.rejects)
		}
	}
	// the limits themselves are accepted
	ok := dbFacts()
	ok.Notes = nil
	for i := 0; i < 32; i++ {
		ok.Notes = append(ok.Notes, strings.Repeat("a", 127)+string(rune('a'+i%26)))
	}
	v := newRenv(t, 0, recMember{role: "db"})
	if err := v.e.EmitRecovered(ctx0(), v.mapped(), ok); err != nil || v.spy.adds.Load() != 1 {
		t.Errorf("32 notes of 128 bytes: err %v adds %d, want one write", err, v.spy.adds.Load())
	}
}

// Facts are validated before anything else, and a record the mapper already marked is refused too.
func TestEmitRecoveredRefusesMapperMarkedRecords(t *testing.T) {
	c50 := 50
	for name, mod := range map[string]func(*records.Record){
		"recovery":   func(r *records.Record) { r.Recovery = "sqlite-freelist" },
		"confidence": func(r *records.Record) { r.Confidence = &c50 },
		"payload.recovery as a map": func(r *records.Record) {
			r.Payload["recovery"] = map[string]any{"relation": "absent-from-live"}
		},
		"payload.recovery as text": func(r *records.Record) { r.Payload["recovery"] = "live" },
	} {
		v := newRenv(t, 0, recMember{role: "db"})
		r := v.mapped()
		mod(&r)
		if err := v.e.EmitRecovered(ctx0(), r, dbFacts()); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if v.spy.adds.Load() != 0 || rejections(v) != 1 {
			t.Errorf("%s: adds %d rejected %d, want a counted rejection and no write", name, v.spy.adds.Load(), rejections(v))
		}
	}
}

func TestRecoveryEmitterRefusesLiveEmit(t *testing.T) {
	v := newRenv(t, 0, recMember{role: "db"})
	if err := v.e.Emit(ctx0(), v.mapped()); err != nil {
		t.Fatalf("Emit = %v, want nil (a counted rejection)", err)
	}
	if v.spy.adds.Load() != 0 || rejections(v) != 1 || len(v.spy.reasons) != 1 || !strings.Contains(v.spy.reasons[0], parse.ErrLiveEmitInRecovery.Error()) {
		t.Errorf("adds %d rejected %d reasons %v, want one rejection naming ErrLiveEmitInRecovery", v.spy.adds.Load(), rejections(v), v.spy.reasons)
	}
	// the embedded emitter's own Emit is guarded as well: no promoted path writes a live record
	if err := v.e.Base().Emit(ctx0(), v.mapped()); err != nil {
		t.Fatal(err)
	}
	if v.spy.adds.Load() != 0 || rejections(v) != 2 {
		t.Errorf("embedded Emit: adds %d rejected %d, want it refused too", v.spy.adds.Load(), rejections(v))
	}
}

func TestRecoveryEmitterMinConfidence(t *testing.T) {
	c40 := 40
	v := newRenv(t, 50, recMember{role: "db", recovery: &parse.RecoveryInfo{Class: "carved", Method: "carve-x", Confidence: &c40}})
	if err := v.e.EmitRecovered(ctx0(), v.mapped(), dbFacts()); err != nil { // capped to 40
		t.Fatal(err)
	}
	if v.spy.adds.Load() != 0 || v.e.BelowMin() != 1 || rejections(v) != 0 {
		t.Errorf("adds %d belowMin %d rejected %d, want it dropped and counted, not a rejection", v.spy.adds.Load(), v.e.BelowMin(), rejections(v))
	}
	v2 := newRenv(t, 50, recMember{role: "db"})
	f := dbFacts()
	f.Confidence = 50
	if err := v2.e.EmitRecovered(ctx0(), v2.mapped(), f); err != nil {
		t.Fatal(err)
	}
	if v2.spy.adds.Load() != 1 || v2.e.BelowMin() != 0 {
		t.Errorf("confidence 50 with minimum 50: adds %d belowMin %d, want it written", v2.spy.adds.Load(), v2.e.BelowMin())
	}
}

func TestRecoveryEmitterUnmapped(t *testing.T) {
	v := newRenv(t, 0, recMember{role: "db"})
	f := dbFacts()
	for i := 0; i < 2; i++ { // the same locator twice is remembered once
		if err := v.e.Unmapped(ctx0(), f, "no usable address"); err != nil {
			t.Fatal(err)
		}
	}
	f2 := dbFacts()
	f2.Locator = "sqlite:table=sms;page=4;cell=0;file=db"
	if err := v.e.Unmapped(ctx0(), f2, "x"); err != nil {
		t.Fatal(err)
	}
	if v.e.UnmappedCount() != 3 || len(v.spy.warns) != 3 || v.spy.warns[0] != f.Locator {
		t.Errorf("count %d warns %v, want 3 warnings at the locator", v.e.UnmappedCount(), v.spy.warns)
	}
	ws, err := v.c.ReadAudit()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range ws {
		if r, _ := e.Details["reason"].(string); e.Action == evidence.ActionAnalysisWarning && strings.Contains(r, "recovered row not mapped: no usable address") {
			found = true
		}
	}
	if !found {
		t.Error("no analysis.warning names the unmapped row and its reason")
	}
	locs, overflow := v.e.UnmappedLocators()
	if overflow || !slices.Equal(locs, []string{f.Locator, f2.Locator}) {
		t.Errorf("locators %v overflow %v, want the two distinct ones", locs, overflow)
	}
	bad := dbFacts()
	bad.Locator = ""
	if err := v.e.Unmapped(ctx0(), bad, "x"); err != nil {
		t.Fatal(err)
	}
	if rejections(v) != 1 || v.e.UnmappedCount() != 3 {
		t.Errorf("invalid facts: rejected %d count %d, want a rejection and no count", rejections(v), v.e.UnmappedCount())
	}
}

func TestRecoveryEmitterUnmappedLocatorsAreBounded(t *testing.T) {
	defer artparse.SetMaxUnmappedLocators(3)()
	v := newRenv(t, 0, recMember{role: "db"})
	for i := 0; i < 5; i++ {
		f := dbFacts()
		f.Locator = "sqlite:table=sms;page=" + string(rune('1'+i)) + ";cell=0;file=db"
		if err := v.e.Unmapped(ctx0(), f, "x"); err != nil {
			t.Fatal(err)
		}
	}
	locs, overflow := v.e.UnmappedLocators()
	if len(locs) != 3 || !overflow || v.e.UnmappedCount() != 5 {
		t.Errorf("%d locators overflow %v count %d, want 3, true, 5 (every row counted)", len(locs), overflow, v.e.UnmappedCount())
	}
}

// Each stored Confidence is its own pointer: mutating one record's never changes another, and the
// bundle's cap is never shared with a record.
func TestStoredConfidenceIsAFreshPointer(t *testing.T) {
	c30 := 30
	rec := &parse.RecoveryInfo{Class: "carved", Method: "carve-x", Confidence: &c30}
	v := newRenv(t, 0, recMember{role: "db", recovery: rec})
	for i := 0; i < 3; i++ {
		if err := v.e.EmitRecovered(ctx0(), v.mapped(), dbFacts()); err != nil {
			t.Fatal(err)
		}
	}
	if len(v.spy.added) != 3 {
		t.Fatalf("%d records reached the writer, want 3", len(v.spy.added))
	}
	a, b, c := v.spy.added[0], v.spy.added[1], v.spy.added[2]
	if a.Confidence == b.Confidence || b.Confidence == c.Confidence {
		t.Fatal("two records share one Confidence pointer")
	}
	*a.Confidence = 99
	if *b.Confidence != 30 || *c.Confidence != 30 || c30 != 30 {
		t.Errorf("mutating the first record's confidence changed another: %d, %d, source %d", *b.Confidence, *c.Confidence, c30)
	}

	// the same for the live emitter on a recovered artifact
	lv, le := recoveredEnv(t)
	for i := 0; i < 2; i++ {
		if err := le.Emit(ctx0(), lv.rec()); err != nil {
			t.Fatal(err)
		}
	}
	x, y := lv.spy.added[0], lv.spy.added[1]
	if x.Confidence == y.Confidence {
		t.Fatal("live emitter: two records share one Confidence pointer")
	}
	*x.Confidence = 1
	if *y.Confidence != 60 {
		t.Errorf("live emitter: mutating one confidence changed another: %d", *y.Confidence)
	}
}

// A payload.recovery or notes the parser set with a shape the host cannot extend is refused, not
// replaced: nothing a parser wrote is silently dropped.
func TestLiveStampRefusesMalformedParserRecoveryPayload(t *testing.T) {
	for name, val := range map[string]any{
		"payload.recovery is text":   "live",
		"payload.recovery is a list": []any{"a"},
		"notes is text":              map[string]any{"notes": "oops"},
		"notes is a string list":     map[string]any{"notes": []string{"a"}},
	} {
		v, e := recoveredEnv(t)
		r := v.rec()
		r.Payload["recovery"] = val
		if err := e.Emit(ctx0(), r); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if v.spy.adds.Load() != 0 || len(v.spy.rejects) != 1 {
			t.Errorf("%s: adds %d rejects %v, want a counted rejection and no write", name, v.spy.adds.Load(), v.spy.rejects)
		}
	}
	// well-formed ones are still extended
	v, e := recoveredEnv(t)
	r := v.rec()
	r.Payload["recovery"] = map[string]any{"notes": []any{"parser-note"}}
	if err := e.Emit(ctx0(), r); err != nil {
		t.Fatal(err)
	}
	notes, _ := v.spy.added[0].Payload["recovery"].(map[string]any)["notes"].([]any)
	if !slices.Contains(notes, any("parser-note")) {
		t.Errorf("notes %v, want the parser's note kept", notes)
	}
}

// A recovery method in the manifest that is not [a-z][a-z0-9-]{1,31} cannot be stamped on a record: the job
// reports refused with the reason, and nothing is written.
func TestManifestRecoveryMethodViolatingTheRuleRefusesTheJob(t *testing.T) {
	c55 := 55
	f := newRecoveredFixture(t)
	d := &evidence.Derivation{
		ParentID: "missing", FSType: "ext4", FSPath: dbPath,
		Recovery: &evidence.Recovery{Class: evidence.ClassDeletedFile, Method: "Bad Method", Confidence: &c55},
	}
	put(t, f.c, "D1", "A1", "rec", evidence.Source{Kind: "recover", RemotePath: dbPath, Derived: d}, "x")
	sum := f.run(f.build(), artparse.Selection{IncludeRecovered: true})
	j := jobOf(t, sum, "bundle")
	if j.Outcome != artparse.OutcomeRefused || !strings.Contains(j.Reason, "recovery method") {
		t.Fatalf("job %+v, want refused with a reason naming the recovery method", j)
	}
	if n := len(f.rows(true)); n != 0 {
		t.Errorf("%d records were stored, want none", n)
	}
}
