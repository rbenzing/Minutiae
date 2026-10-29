package cli

import (
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/rbenzing/minutiae/internal/dbrecover"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/evidence/evidencetest"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/detect"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// A database deleted from an ext4 filesystem and recovered as a file (3A), then parsed (4D), type-recovered
// (4G) and verified (3A R7): the end-to-end chain of spec 4G section 12.4.

const recoveredTelephonyDir = "/data/user_de/0/com.android.providers.telephony/databases/"

type recoveredChain struct {
	dir     string
	db, wal evidence.ManifestRecord
	deps    Deps
}

func newRecoveredChain(t *testing.T) *recoveredChain {
	t.Helper()
	base := "../parsers/androidmms/testdata/wal-uncheckpointed"
	db, ok := readGzFixture(t, base+".db.gz")
	wal, ok2 := readGzFixture(t, base+".db-wal.gz")
	if !ok || !ok2 {
		t.Fatal("the wal-uncheckpointed fixture is missing its database or WAL")
	}
	img, at := freedDisk(t, db, wal)
	dir := newCLICase(t)
	c, err := evidence.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	parent := evidencetest.AddImage(t, c, img)
	add := func(file, fsName string, data []byte, off int64) evidence.ManifestRecord {
		return evidencetest.AddRecovered(t, c, parent, img, evidencetest.RecoveredSpec{
			Path: "recovered/p1-ext4/" + file, FSType: "ext4", FSPath: recoveredTelephonyDir + fsName,
			PartitionOffset: freedPartStart, Runs: []evidence.Run{{Offset: off, Length: int64(len(data))}},
		})
	}
	e := &recoveredChain{dir: dir, deps: Deps{FSDrivers: ext4Drivers}}
	e.db = add("000001-mmssms.db", "mmssms.db", db, at[0])
	e.wal = add("000002-mmssms.db-wal", "mmssms.db-wal", wal, at[1])
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *recoveredChain) run(t *testing.T, args ...string) (int, string) {
	t.Helper()
	return run(t, e.deps, args...)
}

func (e *recoveredChain) verify(t *testing.T) (int, string) {
	t.Helper()
	return e.run(t, "case", "verify", "--case", e.dir)
}

func requireVerifyClean(t *testing.T, e *recoveredChain) {
	t.Helper()
	if code, out := e.verify(t); code != 0 || strings.Contains(out, "PROBLEM:") {
		t.Fatalf("case verify: %d\n%s", code, out)
	}
}

func (e *recoveredChain) show(t *testing.T, id int64) showJSON {
	t.Helper()
	code, out := e.run(t, "records", "show", "--case", e.dir, "--json", "--payload", strconv.FormatInt(id, 10))
	if code != 0 {
		t.Fatalf("records show %d: %d\n%s", id, code, out)
	}
	var sh showJSON
	if err := json.Unmarshal(jsonPart(out), &sh); err != nil {
		t.Fatalf("show json: %v\n%s", err, out)
	}
	return sh
}

func (e *recoveredChain) artifactConfidence(t *testing.T) (method string, conf int) {
	t.Helper()
	r := e.db.Source.Derived.Recovery
	if r == nil || r.Confidence == nil || r.Method == "" {
		t.Fatalf("recovered database %+v has no method and confidence", e.db.Source)
	}
	return r.Method, *r.Confidence
}

func TestRecoveredDatabaseLiveParseIsRecovered(t *testing.T) {
	e := newRecoveredChain(t)
	method, conf := e.artifactConfidence(t)
	if code, out := e.run(t, "parse", "run", "--case", e.dir, "--include-recovered"); code != 0 {
		t.Fatalf("parse run --include-recovered: %d\n%s", code, out)
	}
	msgs := listAllRecords(t, e.dir, "--type", "message")
	if len(msgs) == 0 {
		t.Fatal("no message record was parsed from the recovered database")
	}
	for _, r := range msgs {
		if !r.Deleted || !r.Recovered || r.RecoveryMethod != method || r.Confidence == nil || *r.Confidence > conf {
			t.Errorf("message %d: deleted %v recovered %v method %q confidence %v, want deleted, recovered, %q and <= %d",
				r.ID, r.Deleted, r.Recovered, r.RecoveryMethod, r.Confidence, method, conf)
		}
	}
	requireVerifyClean(t, e)
}

func TestRecoveredDatabaseTypedRecoveryTakesTheLower(t *testing.T) {
	e := newRecoveredChain(t)
	_, conf := e.artifactConfidence(t)
	if code, out := e.run(t, "parse", "recover", "--case", e.dir, "--include-recovered"); code != 0 {
		t.Fatalf("parse recover --include-recovered: %d\n%s", code, out)
	}
	s := splitCase(t, e.dir)
	if len(s.typedAll) == 0 {
		t.Fatal("no typed record was recovered")
	}
	lowered := false
	for _, r := range s.typedAll {
		// every table of this fixture is proven by its schema, so the row method alone sets the cap
		rowCap := sqlitefile.Confidence(sqlitefile.RecoveredRow{Method: r.RecoveryMethod, TableBasis: sqlitefile.BasisSchema})
		want := min(rowCap, conf)
		lowered = lowered || rowCap > conf
		if r.Confidence == nil || *r.Confidence != want {
			t.Errorf("typed record %d (%s): confidence %v, want min(%d, %d) = %d", r.ID, r.RecoveryMethod, r.Confidence, rowCap, conf, want)
		}
		var p struct {
			Recovery map[string]any `json:"recovery"`
		}
		if err := json.Unmarshal(e.show(t, r.ID).Payload, &p); err != nil {
			t.Fatalf("payload of %d: %v", r.ID, err)
		}
		if m, _ := p.Recovery["artifact_method"].(string); m != e.db.Source.Derived.Recovery.Method {
			t.Errorf("typed record %d: payload.recovery.artifact_method %v, want %q", r.ID, p.Recovery["artifact_method"], e.db.Source.Derived.Recovery.Method)
		}
	}
	if !lowered {
		t.Error("no row method is above the artifact's confidence: the test does not show the artifact lowering a row")
	}
	requireVerifyClean(t, e)
}

func TestRecoveredDatabaseSQLiteRecoverCoversOnce(t *testing.T) {
	e := newRecoveredChain(t)
	code, out := e.run(t, "sqlite", "recover", "--case", e.dir, e.db.ID)
	if code != 0 {
		t.Fatalf("sqlite recover: %d\n%s", code, out)
	}
	got := splitCase(t, e.dir)
	if len(got.typed) == 0 {
		t.Fatalf("%d typed records and %d db_rows, want typed records of the claimed tables", len(got.typed), len(got.db))
	}
	for loc := range got.typed {
		if _, both := got.db[loc]; both {
			t.Errorf("row %s is both a typed record and a db_row", loc)
		}
	}
	// the same recovered files with no claiming parser: the whole history, as db_rows
	plain := newRecoveredChain(t)
	code, out, errOut := runP(t, emptyRegistry(), "sqlite", "recover", "--case", plain.dir, plain.db.ID)
	if code != 0 {
		t.Fatalf("uncomposed sqlite recover: %d\n%s\n%s", code, out, errOut)
	}
	want := splitCase(t, plain.dir)
	if len(want.db) == 0 || len(want.typed) != 0 {
		t.Fatalf("uncomposed run: %d db_rows, %d typed", len(want.db), len(want.typed))
	}
	for loc := range want.db {
		_, typed := got.typed[loc]
		_, db := got.db[loc]
		if !typed && !db {
			t.Errorf("row %s of the history is not covered by the composed run", loc)
		}
	}
	if n := len(got.typed) + len(got.db); n != len(want.db) {
		t.Errorf("the composed run covers %d rows, the history has %d", n, len(want.db))
	}
	requireVerifyClean(t, e)
}

func TestRecoveredDatabaseLiveRecordForgedIsExit4(t *testing.T) {
	e := newRecoveredChain(t)
	if code, out := e.run(t, "parse", "recover", "--case", e.dir, "--include-recovered"); code != 0 {
		t.Fatalf("parse recover: %d\n%s", code, out)
	}
	s := splitCase(t, e.dir)
	if len(s.typedAll) == 0 {
		t.Fatal("no typed record")
	}
	requireVerifyClean(t, e)
	recordstest.SetRecordColumns(t, e.dir, s.typedAll[0].ID, map[string]any{"recovered": 0, "recovery_method": nil})
	code, out := e.verify(t)
	if code != ExitIntegrity || !strings.Contains(out, "PROBLEM:") || !strings.Contains(out, "live record (recovered=0) on artifact") {
		t.Errorf("case verify after forging a live record: %d, want 4 with the R7 problem\n%s", code, out)
	}
}

func TestRecoveredDatabaseTamperedArtifactExits4(t *testing.T) {
	e := newRecoveredChain(t)
	flipByte(t, e.dir, e.db.Path, 3000)
	code, out := e.run(t, "parse", "recover", "--case", e.dir, "--include-recovered")
	if code != ExitIntegrity {
		t.Fatalf("parse recover on a tampered artifact: %d, want 4\n%s", code, out)
	}
	if rows := listAllRecords(t, e.dir); len(rows) != 0 {
		t.Errorf("%d records written from a tampered recovered artifact", len(rows))
	}
}

// 4G-R28: the database changes between the typed pass's hash and its re-hash. The composed command exits 4,
// the generic run is never started and no claim is honored.
func TestSQLiteRecoverTypedIntegrityFailureExits4(t *testing.T) {
	e := newRecoveredChain(t)
	var once atomic.Bool
	reg := claimingRegistry(mapWrap{wrap: func(orig func(parse.MapContext, parse.Row) ([]records.Record, error)) func(parse.MapContext, parse.Row) ([]records.Record, error) {
		return func(m parse.MapContext, row parse.Row) ([]records.Record, error) {
			if once.CompareAndSwap(false, true) {
				flipByte(t, e.dir, e.db.Path, 3000)
			}
			return orig(m, row)
		}
	}})
	code, out, errOut := runP(t, reg, "sqlite", "recover", "--case", e.dir, "--json", e.db.ID)
	if !once.Load() {
		t.Fatal("the typed pass never mapped a row: the hook did not run")
	}
	if code != ExitIntegrity {
		t.Fatalf("exit %d, want 4\n%s\n%s", code, out, errOut)
	}
	for _, en := range auditOf(t, e.dir) {
		if en.Action == "analysis.start" && en.Details["op"] == dbrecover.Op {
			t.Error("the generic run was started after an integrity failure")
		}
	}
	for _, r := range listAllRecords(t, e.dir, "--all-runs") {
		if r.Type == "db_row" {
			t.Errorf("db_row %d written after an integrity failure", r.ID)
		}
	}
	if strings.Contains(out, `"honored":[{`) {
		t.Errorf("the report honors a claim after an integrity failure:\n%s", out)
	}
}

// ext4Stand is the MTFS stand-in reporting itself as ext4, so a recovered artifact that records "ext4" (the
// platform namer wants a real type) still re-opens in case verify (R9).
type ext4Stand struct{ filesys.FileSystem }

func (e ext4Stand) Info() filesys.Info {
	i := e.FileSystem.Info()
	i.Type = "ext4"
	return i
}

var ext4Drivers = []detect.Driver{{Name: "ext4", Probe: fstest.Probe, Open: func(r io.ReaderAt, size int64) (filesys.FileSystem, error) {
	f, err := fstest.Open(r, size)
	if err != nil {
		return nil, err
	}
	return ext4Stand{f}, nil
}}}
