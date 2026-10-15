package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/evidence/evidencetest"
	"github.com/rbenzing/minutiae/internal/filesys/fstest"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// The end-to-end chain of spec 15.3 on the MTFS stand-in: a deleted database and its deleted WAL, recovered
// as files (3A), the rows recovered from them (3J), then case verify. The ext4/FAT chain waits for 3K.

const (
	chainMarker = "oldmarkerzq"
	chainCap    = 55
)

func chainTable(rev byte, prefix string) *sqlitetest.Builder {
	b := sqlitetest.New(sqlitetest.Options{PageSize: sqlPageSize})
	tb := b.CreateTable("t", "create table \"t\"(a, b)")
	for id := int64(1); id <= 12; id++ {
		tb.Insert(id, id*3, prefix+sqlFiller(100, byte(id)+rev))
	}
	return b
}

// chainFiles is a database whose freelist leaf keeps a stale leaf carrying the marker, with a WAL of two
// committed frames and one uncommitted update.
func chainFiles() (db, wal []byte) {
	b1, b2 := chainTable(0, chainMarker+"-"), chainTable(40, "")
	b1.SetFreelist([][]uint32{{5, 6}})
	db = b1.Bytes()
	old3 := slices.Clone(db[2*sqlPageSize : 3*sqlPageSize])
	n2 := b2.Bytes()
	new3, new4 := n2[2*sqlPageSize:3*sqlPageSize], n2[3*sqlPageSize:4*sqlPageSize]
	copy(db[5*sqlPageSize:], old3)
	w := b1.NewWAL(false, 0x1000, 0x1001, 0)
	w.Frame(3, old3, 6)
	w.Frame(3, new3, 6)
	w.Frame(4, new4, 0) // never committed
	return db, w.Bytes()
}

type chainEnv struct {
	*imgEnv
	db, wal evidence.ManifestRecord // the recovered artifacts
}

func newChainEnv(t *testing.T) *chainEnv {
	t.Helper()
	db, wal := chainFiles()
	e := newRecoverEnv(t,
		recNode("/data/app.db", db, fstest.RecoverMap{Method: recMethod}),
		recNode("/data/app.db-wal", wal, fstest.RecoverMap{Method: recMethod}))
	if code, out := e.recover(t, "--all"); code != 0 {
		t.Fatalf("image recover --all: %d\n%s", code, out)
	}
	ce := &chainEnv{imgEnv: e}
	recs := recoveredRecords(t, e.c)
	if len(recs) != 2 {
		t.Fatalf("%d recover artifacts, want 2", len(recs))
	}
	for _, r := range recs {
		switch r.Source.Derived.FSPath {
		case "/data/app.db":
			ce.db = r
		case "/data/app.db-wal":
			ce.wal = r
		}
	}
	if ce.db.ID == "" || ce.wal.ID == "" {
		t.Fatalf("recovered artifacts %v do not name the database and the WAL", recs)
	}
	return ce
}

func (e *chainEnv) sqlite(t *testing.T, args ...string) (int, string) {
	t.Helper()
	return run(t, e.d, append([]string{"sqlite", "recover", "--case", e.c}, args...)...)
}

func (e *chainEnv) verify(t *testing.T) (int, string) {
	t.Helper()
	return run(t, e.d, "case", "verify", "--case", e.c)
}

func chainRows(t *testing.T, dir string, extra ...string) []rowJSON {
	t.Helper()
	args := append([]string{"records", "list", "--case", dir, "--json", "--type", "db_row", "--limit", "1000"}, extra...)
	code, out := run(t, Deps{}, args...)
	if code != 0 {
		t.Fatalf("records list: %d\n%s", code, out)
	}
	var l struct {
		Records []rowJSON `json:"records"`
	}
	if err := json.Unmarshal(jsonPart(out), &l); err != nil {
		t.Fatalf("records list json: %v\n%s", err, out)
	}
	return l.Records
}

func flipByte(t *testing.T, caseDir, rel string, at int) {
	t.Helper()
	p := filepath.Join(caseDir, filepath.FromSlash(rel))
	b, err := os.ReadFile(p) //nolint:gosec // inside the test case
	if err != nil {
		t.Fatal(err)
	}
	b[at%len(b)] ^= 0xff
	if err := os.WriteFile(p, b, 0o600); err != nil { //nolint:gosec // inside the test case
		t.Fatal(err)
	}
}

func TestSQLiteRecoverChainFromDeletedDatabase(t *testing.T) {
	e := newChainEnv(t)
	// (1) two recover artifacts, kept at the recover map's confidence
	for _, r := range []evidence.ManifestRecord{e.db, e.wal} {
		if r.Source.Kind != evidence.KindRecover || r.Source.Derived.Recovery == nil || r.Source.Derived.Recovery.Confidence == nil ||
			*r.Source.Derived.Recovery.Confidence != chainCap {
			t.Fatalf("artifact %s is not a recover artifact of confidence %d: %+v", r.ID, chainCap, r.Source)
		}
	}
	// (2) the recovered WAL is found by parent, partition and path; exit 0
	code, out := e.sqlite(t, e.db.ID, "--json")
	if code != 0 {
		t.Fatalf("sqlite recover: %d\n%s", code, out)
	}
	var rep sqliteReportJSON
	if err := json.Unmarshal(jsonPart(out), &rep); err != nil {
		t.Fatalf("json: %v\n%s", err, out)
	}
	roles := map[string]string{}
	for _, in := range rep.Inputs {
		roles[in.Role] = in.ArtifactID
		if !in.Recovered || in.ConfidenceCap == nil || *in.ConfidenceCap > chainCap {
			t.Errorf("input %s: recovered %v cap %v, want recovered and a cap <= %d", in.Role, in.Recovered, in.ConfidenceCap, chainCap)
		}
	}
	if roles["db"] != e.db.ID || roles["wal"] != e.wal.ID {
		t.Fatalf("inputs %v, want the recovered database %s and WAL %s", roles, e.db.ID, e.wal.ID)
	}
	// (3) every stored row is recovered history, capped, and points inside its artifact
	rows := chainRows(t, e.c)
	if len(rows) == 0 {
		t.Fatal("no db_row records stored")
	}
	size := map[string]int64{e.db.ID: e.db.Size, e.wal.ID: e.wal.Size}
	fromWAL := 0
	for _, r := range rows {
		sz, ok := size[r.ArtifactID]
		if !ok {
			t.Errorf("row %d: artifact %s is neither the database nor the WAL", r.ID, r.ArtifactID)
			continue
		}
		if r.ArtifactID == e.wal.ID {
			fromWAL++
		}
		if !r.Recovered || !r.Deleted || r.Confidence == nil || *r.Confidence > chainCap {
			t.Errorf("row %d: recovered %v deleted %v confidence %v, want recovered, deleted and <= %d", r.ID, r.Recovered, r.Deleted, r.Confidence, chainCap)
		}
		if r.Range == nil || r.Range.Offset < 0 || r.Range.Length < 0 || r.Range.Offset+r.Range.Length > sz {
			t.Errorf("row %d: range %+v outside the %d-byte artifact", r.ID, r.Range, sz)
		}
	}
	if fromWAL == 0 {
		t.Error("no row came from the recovered WAL")
	}
	// (4) --recovered only lists them all; show resolves every hop and never calls them live
	if only := chainRows(t, e.c, "--recovered", "only"); len(only) != len(rows) {
		t.Errorf("--recovered only lists %d of %d rows", len(only), len(rows))
	}
	if none := chainRows(t, e.c, "--recovered", "none"); len(none) != 0 {
		t.Errorf("--recovered none lists %d rows, want 0", len(none))
	}
	seen := map[string]bool{}
	for _, r := range rows {
		if seen[r.ArtifactID] {
			continue
		}
		seen[r.ArtifactID] = true
		code, out := run(t, Deps{}, "records", "show", "--case", e.c, "--json", strconv.FormatInt(r.ID, 10))
		if code != 0 {
			t.Fatalf("records show %d: %d\n%s", r.ID, code, out)
		}
		var sh showJSON
		if err := json.Unmarshal(jsonPart(out), &sh); err != nil {
			t.Fatalf("show json: %v\n%s", err, out)
		}
		p := sh.Provenance
		if sh.Artifact.ID != r.ArtifactID || p.Status != statusRecovered || !p.ReachedRoot || p.Recovery == nil {
			t.Errorf("show %d: artifact %s status %q root %v recovery %v", r.ID, sh.Artifact.ID, p.Status, p.ReachedRoot, p.Recovery)
			continue
		}
		if len(p.Chain) != 2 || p.Chain[0].Artifact.ID != r.ArtifactID || p.Chain[1].Artifact.ID != e.rec.ID {
			t.Errorf("show %d: chain does not list the recovered file and the image: %+v", r.ID, p.Chain)
		}
		if p.Recovery.MinConfidence == nil || *p.Recovery.MinConfidence > chainCap || sh.Confidence == nil || *sh.Confidence > *p.Recovery.MinConfidence {
			t.Errorf("show %d: confidence %v rises above the file's %v", r.ID, sh.Confidence, p.Recovery.MinConfidence)
		}
	}
	// (5) a recovered text marker is found
	code, out = run(t, Deps{}, "records", "search", "--case", e.c, "--json", "--type", "db_row", chainMarker)
	if code != 0 {
		t.Fatalf("records search: %d\n%s", code, out)
	}
	var hits struct {
		Records []hitJSON `json:"records"`
	}
	if err := json.Unmarshal(jsonPart(out), &hits); err != nil || len(hits.Records) == 0 {
		t.Fatalf("search found nothing for %q: %v\n%s", chainMarker, err, out)
	}
	for _, h := range hits.Records {
		if !h.Recovered || !h.Deleted {
			t.Errorf("hit %d is shown as live", h.ID)
		}
	}
	// (6) case verify is clean, counts the recovered artifacts and audits ok
	code, out = e.verify(t)
	if code != 0 || strings.Contains(out, "PROBLEM:") || !strings.Contains(out, "2 recovered (2 reproduced)") {
		t.Fatalf("case verify: %d\n%s", code, out)
	}
	if ok, _ := lastVerifyRun(t, e.c)["ok"].(bool); !ok {
		t.Errorf("verify.run ok %v, want true", lastVerifyRun(t, e.c)["ok"])
	}
	// (7) a second run is refused
	if code, out := e.sqlite(t, e.db.ID); code != ExitError || !strings.Contains(out, "already") {
		t.Errorf("second run: %d, want 1 naming the earlier ingest:\n%s", code, out)
	}
	// (8) --list changes nothing but the open
	before, nBefore := caseFiles(t, e.c), len(auditActionsOf(t, e.c))
	if code, out := e.sqlite(t, e.db.ID, "--list"); code != 0 {
		t.Fatalf("--list: %d\n%s", code, out)
	}
	if acts := auditActionsOf(t, e.c)[nBefore:]; !slices.Equal(acts, []string{"case.open"}) {
		t.Errorf("--list appended %v, want only case.open", acts)
	}
	for name, h := range caseFiles(t, e.c) {
		if name != "audit.jsonl" && before[name] != h {
			t.Errorf("--list changed %s", name)
		}
	}
}

// The cap of the recovered database holds the rows of a LIVE WAL to the recovered confidence; the same
// WAL beside a live database is not capped.
func TestSQLiteRecoverChainCapsLiveWALByRecoveredDatabase(t *testing.T) {
	dbBytes, walBytes := chainFiles()
	maxWAL := func(dir, walID string) int {
		m := -1
		for _, r := range chainRows(t, dir) {
			if r.ArtifactID == walID && r.Confidence != nil {
				m = max(m, *r.Confidence)
			}
		}
		return m
	}
	// baseline: everything live
	base := newCLICase(t)
	c, err := evidence.Open(base)
	if err != nil {
		t.Fatal(err)
	}
	ldb := sqlCapture(t, c, "acq1", "app.db", "file", "dev1", "/data/app.db", dbBytes)
	lwal := sqlCapture(t, c, "acq1", "app.db-wal", "file", "dev1", "/data/app.db-wal", walBytes)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if code, out := run(t, Deps{}, "sqlite", "recover", "--case", base, ldb.ID); code != 0 {
		t.Fatalf("baseline: %d\n%s", code, out)
	}
	liveMax := maxWAL(base, lwal.ID)
	if liveMax <= chainCap {
		t.Fatalf("baseline WAL rows reach only %d; the scenario cannot show a cap above %d", liveMax, chainCap)
	}
	// recovered database, live WAL given by name
	db, _ := chainFiles()
	e := newRecoverEnv(t, recNode("/data/app.db", db, fstest.RecoverMap{Method: recMethod}))
	if code, out := e.recover(t, "--all"); code != 0 {
		t.Fatalf("image recover: %d\n%s", code, out)
	}
	rdb := recoveredRecords(t, e.c)[0]
	c, err = evidence.Open(e.c)
	if err != nil {
		t.Fatal(err)
	}
	wal := sqlCapture(t, c, "acq1", "app.db-wal", "file", "dev1", "/data/other.db-wal", walBytes)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	code, out := run(t, e.d, "sqlite", "recover", "--case", e.c, rdb.ID, "--wal", wal.ID)
	if code != 0 {
		t.Fatalf("recovered database with a live WAL: %d\n%s", code, out)
	}
	if got := maxWAL(e.c, wal.ID); got < 0 || got > chainCap {
		t.Errorf("WAL rows reach confidence %d beside a recovered database, want 0..%d (live baseline %d)", got, chainCap, liveMax)
	}
	for _, r := range chainRows(t, e.c) {
		if !r.Recovered || r.Confidence == nil || *r.Confidence > chainCap {
			t.Errorf("row %d: recovered %v confidence %v", r.ID, r.Recovered, r.Confidence)
		}
	}
	if code, out := run(t, e.d, "case", "verify", "--case", e.c); code != 0 || strings.Contains(out, "PROBLEM:") {
		t.Errorf("case verify: %d\n%s", code, out)
	}
}

// A byte flipped in the recovered database: sqlite recover exits 4 before writing, case verify exits 4.
func TestSQLiteRecoverChainTamperedRecoveredArtifactExits4(t *testing.T) {
	e := newChainEnv(t)
	flipByte(t, e.c, e.db.Path, 3*sqlPageSize+17)
	nBefore := len(auditActionsOf(t, e.c))
	code, out := e.sqlite(t, e.db.ID)
	if code != ExitIntegrity {
		t.Fatalf("sqlite recover on a tampered database: %d, want 4\n%s", code, out)
	}
	if n := len(chainRows(t, e.c)); n != 0 {
		t.Errorf("%d rows were written from a tampered input", n)
	}
	for _, a := range auditActionsOf(t, e.c)[nBefore:] {
		if strings.HasPrefix(a, "records.") {
			t.Errorf("audit %q: records were announced for a tampered input", a)
		}
	}
	code, out = e.verify(t)
	if code != ExitIntegrity || !strings.Contains(out, "PROBLEM:") || (!strings.Contains(out, "reproduce") && !strings.Contains(out, "hash")) {
		t.Errorf("case verify: %d, want 4 with the reproduce or hash problem:\n%s", code, out)
	}
}

// After a successful chain, tampering with any artifact in it makes verify exit 4.
func TestSQLiteRecoverChainTamperAnyArtifactFailsVerify(t *testing.T) {
	for _, which := range []string{"database", "wal", "image"} {
		t.Run(which, func(t *testing.T) {
			e := newChainEnv(t)
			if code, out := e.sqlite(t, e.db.ID); code != 0 {
				t.Fatalf("sqlite recover: %d\n%s", code, out)
			}
			if code, out := e.verify(t); code != 0 {
				t.Fatalf("clean chain: verify %d\n%s", code, out)
			}
			rel := map[string]string{"database": e.db.Path, "wal": e.wal.Path, "image": e.rec.Path}[which]
			flipByte(t, e.c, rel, 5000)
			if code, out := e.verify(t); code != ExitIntegrity || !strings.Contains(out, "PROBLEM:") {
				t.Errorf("verify after tampering the %s: %d, want 4 with a problem:\n%s", which, code, out)
			}
		})
	}
}

// A run that was cancelled leaves a case that verifies, and a later run still works.
func TestSQLiteRecoverChainVerifyAfterCancel(t *testing.T) {
	e := newChainEnv(t)
	if code, out := e.sqlite(t, e.db.ID, "--timeout", "1ns"); code != ExitError {
		t.Fatalf("cancelled run: %d, want 1\n%s", code, out)
	}
	if code, out := e.verify(t); code != 0 || strings.Contains(out, "PROBLEM:") {
		t.Fatalf("verify after the cancel: %d\n%s", code, out)
	}
	if code, out := e.sqlite(t, e.db.ID); code != 0 {
		t.Fatalf("run after the cancel: %d\n%s", code, out)
	}
	if code, out := e.verify(t); code != 0 || strings.Contains(out, "PROBLEM:") {
		t.Fatalf("verify after the run: %d\n%s", code, out)
	}
}

// An input that changed after a clean run: exit 4, nothing written, nothing superseded.
func TestSQLiteRecoverChainChangedInputIsExit4AndSupersedesNothing(t *testing.T) {
	e := newChainEnv(t)
	if code, out := e.sqlite(t, e.db.ID); code != 0 {
		t.Fatalf("first run: %d\n%s", code, out)
	}
	before := chainRows(t, e.c)
	if len(before) == 0 {
		t.Fatal("no rows after the first run")
	}
	flipByte(t, e.c, e.db.Path, 2*sqlPageSize+9)
	if code, out := e.sqlite(t, e.db.ID, "--allow-reingest"); code != ExitIntegrity {
		t.Fatalf("run on a changed input: %d, want 4\n%s", code, out)
	}
	after := chainRows(t, e.c, "--all-runs")
	if len(after) != len(before) {
		t.Errorf("%d rows after the failed re-run, want the %d of the first run", len(after), len(before))
	}
	for _, r := range after {
		if r.Superseded {
			t.Errorf("row %d was superseded by a run that failed", r.ID)
		}
	}
}

// The same chain with artifacts written through evidencetest.AddRecovered, and the WAL named explicitly.
func TestEvidencetestRecoveredFallback(t *testing.T) {
	dbBytes, walBytes := chainFiles()
	img := slices.Concat(dbBytes, walBytes)
	dir := newCLICase(t)
	c, err := evidence.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	parent := evidencetest.AddImage(t, c, img)
	db := evidencetest.AddRecovered(t, c, parent, img, evidencetest.RecoveredSpec{
		Path: "recovered/p1-mtfs/000001-app.db", Runs: []evidence.Run{{Offset: 0, Length: int64(len(dbBytes))}},
	})
	wal := evidencetest.AddRecovered(t, c, parent, img, evidencetest.RecoveredSpec{
		Path: "recovered/p1-mtfs/000002-app.db-wal", Runs: []evidence.Run{{Offset: int64(len(dbBytes)), Length: int64(len(walBytes))}},
	})
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if code, out := run(t, Deps{}, "sqlite", "recover", "--case", dir, db.ID, "--wal", wal.ID); code != 0 {
		t.Fatalf("sqlite recover: %d\n%s", code, out)
	}
	rows := chainRows(t, dir)
	if len(rows) == 0 {
		t.Fatal("no rows")
	}
	for _, r := range rows {
		if !r.Recovered || !r.Deleted || r.Confidence == nil || *r.Confidence > chainCap {
			t.Errorf("row %d: recovered %v deleted %v confidence %v", r.ID, r.Recovered, r.Deleted, r.Confidence)
		}
	}
	if code, out := run(t, Deps{}, "case", "verify", "--case", dir); code != 0 || strings.Contains(out, "PROBLEM:") || !strings.Contains(out, "2 recovered (2 reproduced)") {
		t.Errorf("case verify: %d\n%s", code, out)
	}
}
