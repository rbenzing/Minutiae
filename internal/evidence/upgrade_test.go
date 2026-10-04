package evidence

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// makeV1Case builds a case directory exactly as the v1.0.0 build did: a
// case.create audit entry without schema_version and a schema v1 artifacts.db
// (migrations[:1]). It returns the case directory, closed and unlocked.
func makeV1Case(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "V1CASE")
	if err := os.MkdirAll(filepath.Join(dir, artifactsDir), 0o750); err != nil {
		t.Fatal(err)
	}
	meta := Meta{ID: "V1CASE", Examiner: "E", Created: "2026-01-01T00:00:00Z", ToolVersion: "v1.0.0", HostOS: "test", HostArch: "test"}
	if err := writeExclusiveJSON(filepath.Join(dir, caseFile), meta); err != nil {
		t.Fatal(err)
	}
	audit, err := CreateAuditLog(filepath.Join(dir, auditFile), "tester", "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := audit.Append("case.create", "", map[string]any{"id": meta.ID, "examiner": meta.Examiner, "description": ""}); err != nil {
		t.Fatal(err)
	}
	if err := audit.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := openDB(filepath.Join(dir, dbFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.migrateTo(1); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return dir
}

func openV1Case(t *testing.T) *Case {
	t.Helper()
	c, err := Open(makeV1Case(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func auditEntries(t *testing.T, c *Case) []AuditEntry {
	t.Helper()
	es, err := ReadAuditEntries(filepath.Join(c.Dir, auditFile))
	if err != nil {
		t.Fatal(err)
	}
	return es
}

func captureSmall(t *testing.T, c *Case) ManifestRecord {
	t.Helper()
	rec, err := c.Capture("dev1", "acq1", "a.bin", testSrc, func(w io.Writer) error {
		_, err := io.WriteString(w, "evidence")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func mustVersion(t *testing.T, c *Case) int {
	t.Helper()
	v, err := c.SchemaVersion()
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func detailInts(t *testing.T, e AuditEntry, keys ...string) []int {
	t.Helper()
	out := make([]int, 0, len(keys))
	for _, k := range keys {
		v, ok := auditInt(e.Details, k)
		if !ok {
			t.Fatalf("audit %s details %v: no integer %q", e.Action, e.Details, k)
		}
		out = append(out, v)
	}
	return out
}

func TestCreateRecordsSchemaVersionInAudit(t *testing.T) {
	c := newTestCase(t)
	es := auditEntries(t, c)
	if len(es) != 1 || es[0].Action != "case.create" {
		t.Fatalf("audit = %+v", es)
	}
	if got := detailInts(t, es[0], "schema_version"); got[0] != CurrentSchema {
		t.Fatalf("schema_version = %d, want %d", got[0], CurrentSchema)
	}
	if v := mustVersion(t, c); v != CurrentSchema {
		t.Fatalf("new case at v%d, want v%d", v, CurrentSchema)
	}
}

func TestOpenV1CaseStaysV1(t *testing.T) {
	dir := makeV1Case(t)
	c, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if v := mustVersion(t, c); v != 1 {
		t.Fatalf("Open migrated a v1 case to v%d", v)
	}
	rec := captureSmall(t, c) // an old command still works on v1
	if r := mustVerify(t, c); !r.OK() || r.ArtifactsChecked != 1 {
		t.Fatalf("verify = %+v", r)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c2.Close() }()
	if v := mustVersion(t, c2); v != 1 {
		t.Fatalf("reopen: v%d, want v1", v)
	}
	if recs, err := c2.Manifest(); err != nil || len(recs) != 1 || recs[0].ID != rec.ID {
		t.Fatalf("manifest = %+v, %v", recs, err)
	}
}

func TestRequireSchemaRefusesV1(t *testing.T) {
	c := openV1Case(t)
	err := c.RequireSchema(2)
	if !errors.Is(err, ErrNeedsUpgrade) {
		t.Fatalf("err = %v, want ErrNeedsUpgrade", err)
	}
	for _, want := range []string{"case schema v1 < v2", "case upgrade --case " + c.Dir} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message %q lacks %q", err, want)
		}
	}
	if err := c.RequireSchema(1); err != nil {
		t.Fatalf("RequireSchema(1) on a v1 case: %v", err)
	}
	if err := newTestCase(t).RequireSchema(2); err != nil {
		t.Fatalf("RequireSchema(2) on a v2 case: %v", err)
	}
}

func TestUpgradeIsAudited(t *testing.T) {
	c := openV1Case(t)
	rec := captureSmall(t, c)
	res, err := c.Upgrade()
	if err != nil {
		t.Fatal(err)
	}
	if res != (UpgradeResult{From: 1, To: 2, Upgraded: true}) {
		t.Fatalf("result = %+v", res)
	}
	es := auditEntries(t, c)
	var tail []AuditEntry
	for i, e := range es {
		if e.Action == "case.open" {
			tail = es[i+1:]
		}
	}
	// after case.open: artifact.create (Capture), then the upgrade pair
	var up []AuditEntry
	for _, e := range tail {
		if strings.HasPrefix(e.Action, "case.upgrade") {
			up = append(up, e)
		}
	}
	if len(up) != 2 || up[0].Action != "case.upgrade" || up[1].Action != "case.upgrade.done" {
		t.Fatalf("upgrade audit entries = %+v", up)
	}
	if up[1].Seq != up[0].Seq+1 {
		t.Fatalf("done is not directly after the upgrade entry: seq %d, %d", up[0].Seq, up[1].Seq)
	}
	for _, e := range up {
		if got := detailInts(t, e, "from", "to"); got[0] != 1 || got[1] != 2 {
			t.Fatalf("%s from/to = %v", e.Action, got)
		}
	}
	if resumed, _ := up[1].Details["resumed"].(bool); resumed {
		t.Fatal("a normal completion is marked resumed")
	}
	if v := mustVersion(t, c); v != 2 {
		t.Fatalf("version = %d", v)
	}
	h, err := c.store.ArtifactHashes()
	if err != nil || h[rec.ID] != rec.SHA256 {
		t.Fatalf("artifact after upgrade: %v, %v", h, err)
	}
	if r := mustVerify(t, c); !r.OK() || len(r.Notices) != 0 {
		t.Fatalf("verify after upgrade = %+v", r)
	}
}

func TestUpgradeAuditsBeforeMigrating(t *testing.T) {
	c := openV1Case(t)
	hookRan := false
	c.upgradeHook = func() {
		hookRan = true
		es := auditEntries(t, c)
		if last := es[len(es)-1]; last.Action != "case.upgrade" {
			t.Errorf("at migration time the last audit entry is %s, want case.upgrade", last.Action)
		}
		if v := mustVersion(t, c); v != 1 {
			t.Errorf("at migration time the db is v%d, want v1", v)
		}
	}
	if _, err := c.Upgrade(); err != nil {
		t.Fatal(err)
	}
	if !hookRan {
		t.Fatal("hook did not run")
	}
}

func TestUpgradeBlockedIsAuditedAndLeavesDBUntouched(t *testing.T) {
	c := openV1Case(t)
	if _, err := c.store.db.Exec(`INSERT INTO records (type, data) VALUES ('x', '{}')`); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(c.Dir, dbFile)
	before := fileSHA(t, dbPath)

	res, err := c.Upgrade()
	if !errors.Is(err, ErrMigrationBlocked) {
		t.Fatalf("err = %v, want ErrMigrationBlocked", err)
	}
	if res.Upgraded {
		t.Fatalf("result = %+v", res)
	}
	es := auditEntries(t, c)
	if n := len(es); n < 2 || es[n-2].Action != "case.upgrade" || es[n-1].Action != "case.upgrade.error" {
		t.Fatalf("audit tail = %+v", es)
	}
	last := es[len(es)-1]
	if msg, _ := last.Details["error"].(string); !strings.Contains(msg, "records table holds 1 row") {
		t.Fatalf("error detail = %v", last.Details)
	}
	if got := detailInts(t, last, "from", "to"); got[0] != 1 || got[1] != 2 {
		t.Fatalf("from/to = %v", got)
	}
	if after := fileSHA(t, dbPath); after != before {
		t.Fatal("a blocked upgrade changed artifacts.db")
	}
	if v := mustVersion(t, c); v != 1 {
		t.Fatalf("version = %d, want 1", v)
	}
	if r := mustVerify(t, c); !r.OK() {
		t.Fatalf("verify after a blocked upgrade = %+v", r)
	}
}

func TestUpgradeNoopWhenCurrent(t *testing.T) {
	c := newTestCase(t)
	before := len(auditEntries(t, c))
	res, err := c.Upgrade()
	if err != nil {
		t.Fatal(err)
	}
	if res != (UpgradeResult{From: 2, To: 2}) || res.Resumed {
		t.Fatalf("result = %+v", res)
	}
	if after := len(auditEntries(t, c)); after != before {
		t.Fatalf("a no-op upgrade appended %d audit entries", after-before)
	}
}

func TestUpgradeResumesUnauditedCompletion(t *testing.T) {
	c := openV1Case(t)
	// The upgrade was announced and the migration committed, but the process
	// died before case.upgrade.done reached the log.
	if _, err := c.Audit.Append(ActionCaseUpgrade, "", upgradeDetails(1, 2)); err != nil {
		t.Fatal(err)
	}
	if err := c.store.migrateTo(2); err != nil {
		t.Fatal(err)
	}
	if r := mustVerify(t, c); !r.OK() || len(r.Notices) != 1 {
		t.Fatalf("verify of the crashed upgrade = %+v", r)
	}
	res, err := c.Upgrade()
	if err != nil {
		t.Fatal(err)
	}
	if res.Upgraded || !res.Resumed || res.From != 2 || res.To != 2 {
		t.Fatalf("result = %+v", res)
	}
	es := auditEntries(t, c)
	done := es[len(es)-1]
	if done.Action != "case.upgrade.done" {
		t.Fatalf("last audit entry = %s", done.Action)
	}
	if resumed, _ := done.Details["resumed"].(bool); !resumed {
		t.Fatalf("done details = %v, want resumed=true", done.Details)
	}
	if got := detailInts(t, done, "from", "to"); got[0] != 1 || got[1] != 2 {
		t.Fatalf("from/to = %v", got)
	}
	if r := mustVerify(t, c); !r.OK() || len(r.Notices) != 0 {
		t.Fatalf("verify after resuming = %+v", r)
	}
	// and now it is a plain no-op
	n := len(auditEntries(t, c))
	if _, err := c.Upgrade(); err != nil {
		t.Fatal(err)
	}
	if len(auditEntries(t, c)) != n {
		t.Fatal("second Upgrade appended an entry")
	}
}

func TestUpgradeRefusedWhenCaseInUse(t *testing.T) {
	c := openV1Case(t)
	if _, err := Open(c.Dir); !errors.Is(err, ErrCaseInUse) {
		t.Fatalf("second Open: err = %v, want ErrCaseInUse", err)
	}
	if v := mustVersion(t, c); v != 1 {
		t.Fatalf("version = %d", v)
	}
}

func TestVerifyDetectsSchemaChangedOutsideAudit(t *testing.T) {
	c := openV1Case(t)
	db := rawDB(t, filepath.Join(c.Dir, dbFile))
	if err := applyMigrations(db, 1, 2); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	r := mustVerify(t, c)
	if r.OK() || !containsSubstr(r.Problems, "schema version") {
		t.Fatalf("report = %+v", r)
	}
}

func TestVerifyV1CaseIsCleanAndSkipsRecordChecks(t *testing.T) {
	c := openV1Case(t)
	captureSmall(t, c)
	// A v1 records row has no audit commitment and no v2 columns: verify must
	// not apply the v2 record checks to a v1 case.
	if _, err := c.store.db.Exec(`INSERT INTO records (type, data) VALUES ('x', '{}')`); err != nil {
		t.Fatal(err)
	}
	r := mustVerify(t, c)
	if !r.OK() || r.ArtifactsChecked != 1 || len(r.Notices) != 0 {
		t.Fatalf("report = %+v", r)
	}
	if r.Notices == nil || r.Problems == nil {
		t.Fatalf("Notices/Problems must be empty slices, not nil: %+v", r)
	}
	b, err := json.Marshal(r)
	if err != nil || !strings.Contains(string(b), `"notices":[]`) {
		t.Fatalf("json = %s, %v", b, err)
	}
}

func TestVerifyNoticeOnDanglingUpgrade(t *testing.T) {
	c := openV1Case(t)
	if _, err := c.Audit.Append(ActionCaseUpgrade, "", upgradeDetails(1, 2)); err != nil {
		t.Fatal(err)
	}
	r := mustVerify(t, c) // the db is still at v1
	if !r.OK() || len(r.Notices) != 1 || !strings.Contains(r.Notices[0], "case.upgrade") {
		t.Fatalf("report = %+v", r)
	}
}

func TestVerifyUpgradeErrorClosesTheUpgrade(t *testing.T) {
	c := openV1Case(t)
	if _, err := c.Audit.Append(ActionCaseUpgrade, "", upgradeDetails(1, 2)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Audit.Append(ActionCaseUpgradeError, "", upgradeErrorDetails(1, 2, errors.New("boom"))); err != nil {
		t.Fatal(err)
	}
	if r := mustVerify(t, c); !r.OK() || len(r.Notices) != 0 {
		t.Fatalf("report = %+v", r)
	}
}

func TestVerifyDoneWithoutMigrationIsAProblem(t *testing.T) {
	c := openV1Case(t)
	if _, err := c.Audit.Append(ActionCaseUpgradeDone, "", upgradeDoneDetails(1, 2, false)); err != nil {
		t.Fatal(err)
	}
	r := mustVerify(t, c) // audit says v2, db is v1
	if r.OK() || !containsSubstr(r.Problems, "schema version") {
		t.Fatalf("report = %+v", r)
	}
}

// TestUpgradeRefusesDBBehindAudit: the audit log says the schema is v2 but the
// database is v1 (an old copy restored). Upgrade must refuse as an integrity
// error, announce nothing and leave the database untouched.
func TestUpgradeRefusesDBBehindAudit(t *testing.T) {
	c := openV1Case(t)
	if _, err := c.Audit.Append(ActionCaseUpgradeDone, "", upgradeDoneDetails(1, 2, false)); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(c.Dir, dbFile)
	before := fileSHA(t, dbPath)
	n := len(auditEntries(t, c))
	res, err := c.Upgrade()
	if !errors.Is(err, ErrIntegrity) {
		t.Fatalf("err = %v, want ErrIntegrity", err)
	}
	if res.Upgraded {
		t.Fatalf("result = %+v", res)
	}
	if fileSHA(t, dbPath) != before || mustVersion(t, c) != 1 {
		t.Fatal("the database changed")
	}
	if got := len(auditEntries(t, c)); got != n {
		t.Fatalf("Upgrade appended %d audit entries before refusing", got-n)
	}
}

// TestUpgradeRefusesDBAheadOfAudit: the database is v2 but nothing in the audit
// log announced or recorded an upgrade (migrated outside Minutiae).
func TestUpgradeRefusesDBAheadOfAudit(t *testing.T) {
	c := openV1Case(t)
	if err := c.store.migrateTo(2); err != nil {
		t.Fatal(err)
	}
	n := len(auditEntries(t, c))
	if _, err := c.Upgrade(); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("err = %v, want ErrIntegrity", err)
	}
	if got := len(auditEntries(t, c)); got != n {
		t.Fatalf("Upgrade appended %d audit entries before refusing", got-n)
	}
}

// TestUpgradeAuditFailureBlocksMigration: if case.upgrade cannot be written the
// migration never starts (audit before write).
func TestUpgradeAuditFailureBlocksMigration(t *testing.T) {
	c := openV1Case(t)
	dbPath := filepath.Join(c.Dir, dbFile)
	before := fileSHA(t, dbPath)
	if err := c.Audit.Close(); err != nil {
		t.Fatal(err)
	}
	res, err := c.Upgrade()
	if err == nil {
		t.Fatal("Upgrade succeeded with a closed audit log")
	}
	if res.Upgraded {
		t.Fatalf("result = %+v", res)
	}
	if fileSHA(t, dbPath) != before {
		t.Fatal("artifacts.db changed although the audit entry could not be written")
	}
	if v := mustVersion(t, c); v != 1 {
		t.Fatalf("version = %d, want 1", v)
	}
}

// TestUpgradeAuditReadErrorClassification: only a corrupt audit log (it does not
// parse) is an integrity failure; an ordinary I/O error reading it is a plain
// error (exit 1, not 4).
func TestUpgradeAuditReadErrorClassification(t *testing.T) {
	t.Run("unreadable audit log is a plain error", func(t *testing.T) {
		c := openV1Case(t)
		dir := t.TempDir()
		if err := os.Mkdir(filepath.Join(dir, auditFile), 0o700); err != nil { // reading a directory fails
			t.Fatal(err)
		}
		c.Dir = dir
		_, err := c.checkSchemaMatchesAudit(1)
		if err == nil {
			t.Fatal("no error for an unreadable audit log")
		}
		if errors.Is(err, ErrIntegrity) {
			t.Fatalf("err = %v, an I/O error must not be an integrity error", err)
		}
	})
	t.Run("unparseable audit log is an integrity error", func(t *testing.T) {
		c := openV1Case(t)
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, auditFile), []byte("{not json\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		c.Dir = dir
		if _, err := c.checkSchemaMatchesAudit(1); !errors.Is(err, ErrIntegrity) {
			t.Fatalf("err = %v, want ErrIntegrity", err)
		}
	})
}
