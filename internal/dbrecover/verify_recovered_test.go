package dbrecover

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/evidence/evidencetest"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// These tests pin the recovered-ness rule of the records layer with real
// db_row records: the output of the mapper is what the writer and case verify see.

var testParser = records.Parser{Name: "dbrecover-test", Version: "1"}

func imageBytes() []byte {
	b := make([]byte, 8192)
	for i := range b {
		b[i] = byte(i*31 + 7)
	}
	return b
}

// recoveredDB stores a recovered artifact of the given spec (confidence conf)
// whose bytes are the first 2048 bytes of an imported image.
func recoveredDB(t *testing.T, c *evidence.Case, conf int, spec evidencetest.RecoveredSpec) evidence.ManifestRecord {
	t.Helper()
	data := imageBytes()
	img := evidencetest.AddImage(t, c, data)
	spec.Confidence = &conf
	spec.Runs = []evidence.Run{{Offset: 0, Length: 2048}}
	return evidencetest.AddRecovered(t, c, img, data, spec)
}

func carvedSpec() evidencetest.RecoveredSpec {
	return evidencetest.RecoveredSpec{
		Kind: evidence.KindCarve, Class: evidence.ClassCarved, Method: "carve-sqlite", Scope: "raw", Path: "carved/raw/c1.db",
	}
}

func mapperOn(art evidence.ManifestRecord, ceil *int) *mapper {
	return &mapper{
		byFile: map[sqlitefile.FileKind]inputRef{sqlitefile.FileDB: {Role: "db", ArtifactID: art.ID, SHA256: art.SHA256, Size: art.Size, Cap: ceil}},
		cap:    ceil,
	}
}

func rowWith(method string) sqlitefile.RecoveredRow {
	r := baseRow()
	r.Method = method
	r.Loc.Offset, r.Loc.Length = 100, 50
	return r
}

func startWriter(t *testing.T, c *evidence.Case, art string) *records.Writer {
	t.Helper()
	w, err := records.NewWriter(c, testParser, records.WriterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Start(context.Background(), records.StartOptions{Artifacts: []string{art}}); err != nil {
		t.Fatal(err)
	}
	return w
}

// requireRejected adds rec and requires the typed refusal, exactly one audited
// rejection note and no stored record.
func requireRejected(t *testing.T, c *evidence.Case, art string, rec records.Record, want error) {
	t.Helper()
	w := startWriter(t, c, art)
	pre, err := c.ReadAudit()
	if err != nil {
		t.Fatal(err)
	}
	err = w.Add(context.Background(), rec)
	if !errors.Is(err, want) {
		t.Fatalf("Add = %v, want %v", err, want)
	}
	all, rerr := c.ReadAudit()
	if rerr != nil {
		t.Fatal(rerr)
	}
	if len(all) != len(pre)+1 {
		t.Fatalf("audit grew from %d to %d on a refused record, want one rejection note", len(pre), len(all))
	}
	if last := all[len(all)-1]; last.Action != "analysis.warning" || last.Details["rejected"] != true {
		t.Errorf("last audit entry %s %v, want a rejection note", last.Action, last.Details)
	}
	res, err := w.End(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Records != 0 {
		t.Errorf("%d records stored, want 0", res.Records)
	}
}

func TestMappedRowOnRecoveredDatabaseIsAccepted(t *testing.T) {
	c := recordstest.NewCase(t)
	art := recoveredDB(t, c, 55, evidencetest.RecoveredSpec{})
	ceil := 55
	rec := mustRecord(t, mapperOn(art, &ceil), rowWith(sqlitefile.MethodFreelist))
	if rec.Confidence == nil || *rec.Confidence != 55 {
		t.Fatalf("confidence %v, want the artifact ceil 55", rec.Confidence)
	}
	if res := recordstest.Ingest(t, c, testParser, []string{art.ID}, []records.Record{rec}); res.Records != 1 {
		t.Fatalf("stored %d", res.Records)
	}
	rep, err := c.Verify()
	if err != nil {
		t.Fatal(err)
	}
	evidencetest.RequireClean(t, rep)
}

func TestMappedRowAboveArtifactCapIsRejected(t *testing.T) {
	c := recordstest.NewCase(t)
	art := recoveredDB(t, c, 55, evidencetest.RecoveredSpec{})
	rec := mustRecord(t, mapperOn(art, nil), rowWith(sqlitefile.MethodFreelist)) // 60, no ceil applied
	if *rec.Confidence != 60 {
		t.Fatalf("confidence %d", *rec.Confidence)
	}
	requireRejected(t, c, art.ID, rec, records.ErrConfidenceAboveArtifact)
}

func TestMappedRowWithoutRecoveryOnRecoveredDatabaseIsRejected(t *testing.T) {
	c := recordstest.NewCase(t)
	art := recoveredDB(t, c, 55, evidencetest.RecoveredSpec{})
	ceil := 55
	rec := mustRecord(t, mapperOn(art, &ceil), rowWith(sqlitefile.MethodFreelist))
	rec.Recovery, rec.Deleted, rec.Confidence = "", false, nil // a live-shaped row
	requireRejected(t, c, art.ID, rec, records.ErrRecoveredArtifactLiveRecord)
}

func TestRowOnRecoveredSiblingOfLiveDatabaseIsCapped(t *testing.T) {
	c := recordstest.NewCase(t)
	live := recordstest.AddArtifact(t, c, "app.db", imageBytes())
	// the recovered sibling (a WAL) sets the ceiling of the whole input set (D2)
	sib := recoveredDB(t, c, 50, evidencetest.RecoveredSpec{Path: "recovered/p1-mtfs/000001-app.db-wal"})
	ceil := 50
	m := mapperOn(live, &ceil)
	m.byFile[sqlitefile.FileWAL] = inputRef{Role: "wal", ArtifactID: sib.ID, SHA256: sib.SHA256, Size: sib.Size, Recovered: true, Cap: &ceil}
	rec := mustRecord(t, m, rowWith(sqlitefile.MethodWALPrior)) // 75 uncapped
	if *rec.Confidence != 50 {
		t.Fatalf("confidence %d, want the sibling ceil 50", *rec.Confidence)
	}
	if res := recordstest.Ingest(t, c, testParser, []string{live.ID}, []records.Record{rec}); res.Records != 1 {
		t.Fatalf("stored %d", res.Records)
	}
}

func TestRowOnCarvedDatabaseIsCapped(t *testing.T) {
	c := recordstest.NewCase(t)
	art := recoveredDB(t, c, 70, carvedSpec())
	ceil := 70
	rec := mustRecord(t, mapperOn(art, &ceil), rowWith(sqlitefile.MethodWALPrior)) // 75 -> 70
	if *rec.Confidence != 70 {
		t.Fatalf("confidence %d, want 70", *rec.Confidence)
	}
	if res := recordstest.Ingest(t, c, testParser, []string{art.ID}, []records.Record{rec}); res.Records != 1 {
		t.Fatalf("stored %d", res.Records)
	}
	// without the ceil the same row is above the carved ceiling and refused
	c2 := recordstest.NewCase(t)
	art2 := recoveredDB(t, c2, 70, carvedSpec())
	requireRejected(t, c2, art2.ID, mustRecord(t, mapperOn(art2, nil), rowWith(sqlitefile.MethodWALPrior)), records.ErrConfidenceAboveArtifact)
}

func TestRecoveredRowOnLiveDatabaseIsAcceptedAndVerifies(t *testing.T) {
	c := recordstest.NewCase(t)
	live := recordstest.AddArtifact(t, c, "app.db", imageBytes())
	rec := mustRecord(t, mapperOn(live, nil), rowWith(sqlitefile.MethodFreelist))
	if !rec.Deleted || rec.Recovery == "" || *rec.Confidence != 60 {
		t.Fatalf("record %+v", rec)
	}
	recordstest.Ingest(t, c, testParser, []string{live.ID}, []records.Record{rec})
	rep, err := c.Verify()
	if err != nil {
		t.Fatal(err)
	}
	evidencetest.RequireClean(t, rep)
}

func TestVerifyFlagsLiveShapedDBRowOnRecoveredArtifact(t *testing.T) {
	c := recordstest.NewCase(t)
	art := recoveredDB(t, c, 55, evidencetest.RecoveredSpec{})
	ceil := 55
	rec := mustRecord(t, mapperOn(art, &ceil), rowWith(sqlitefile.MethodFreelist))
	recordstest.Ingest(t, c, testParser, []string{art.ID}, []records.Record{rec})
	recordstest.SetRecordColumns(t, c.Dir, 1, map[string]any{"recovered": 0, "recovery_method": nil})
	rep, err := c.Verify()
	if err != nil {
		t.Fatal(err)
	}
	evidencetest.RequireProblem(t, rep, "live record (recovered=0) on artifact")
	for _, p := range rep.Problems {
		if !strings.Contains(p, "live record (recovered=0) on artifact") && !strings.Contains(p, "digest mismatch") {
			t.Errorf("unexpected problem kind: %s", p)
		}
	}
}
