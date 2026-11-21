package androidgooglemessages_test

import (
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/androidgooglemessages"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

const (
	remoteApp      = "/data/data/com.google.android.apps.messaging/databases/bugle_db"
	remoteAOSP     = "/data/data/com.android.messaging/databases/bugle_db"
	remoteUser10   = "/data/user/10/com.google.android.apps.messaging/databases/bugle_db"
	remoteOther    = "/sdcard/x/bugle_db"
	reasonDeferred = "schema checked at parse time: a -wal or -journal companion is present"
	reasonNotBugle = "not a bugle_db"
	reasonAppPfx   = "no bugle_db signature at the Messages app path: "
)

// The DDL of the Go-built databases: the signature of spec 5.3 and what the tests take away from it.
const (
	ddlMessages     = `CREATE TABLE messages (_id INTEGER PRIMARY KEY AUTOINCREMENT, conversation_id INT, sender_id INT, sent_timestamp INT, received_timestamp INT, message_protocol INT, message_status INT, mms_subject TEXT)`
	ddlParts        = `CREATE TABLE parts (_id INTEGER PRIMARY KEY AUTOINCREMENT, message_id INT, text TEXT, uri TEXT, content_type TEXT, width INT, height INT, timestamp INT, conversation_id INT)`
	ddlPartsNoAlias = `CREATE TABLE parts (_id INTEGER, message_id INT, text TEXT, uri TEXT, content_type TEXT, width INT, height INT, timestamp INT, conversation_id INT)`
	ddlParticipants = `CREATE TABLE participants (_id INTEGER PRIMARY KEY AUTOINCREMENT, sub_id INT, normalized_destination TEXT, send_destination TEXT, display_destination TEXT)`
	ddlParticipNoD  = `CREATE TABLE participants (_id INTEGER PRIMARY KEY AUTOINCREMENT, sub_id INT, full_name TEXT)`
	ddlConvs        = `CREATE TABLE conversations (_id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT)`
	ddlMembers      = `CREATE TABLE conversation_participants (_id INTEGER PRIMARY KEY AUTOINCREMENT, conversation_id INT, participant_id INT)`
)

type table struct{ name, ddl string }

func goBuilt(userVersion int32, tables ...table) []byte {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 4096, UserVersion: userVersion})
	for _, t := range tables {
		b.CreateTable(t.name, t.ddl)
	}
	return b.Bytes()
}

func v2Tables() []table {
	return []table{{"messages", ddlMessages}, {"parts", ddlParts}, {"participants", ddlParticipants}, {"conversations", ddlConvs}, {"conversation_participants", ddlMembers}}
}

func inputAt(t *testing.T, h *parsertest.Harness, p parse.Parser, remote string, files map[string][]byte, forProbe bool) *parse.Input {
	t.Helper()
	primary := h.Artifact(parsertest.AddAndroidFile(t, h.Case, "acq1", remote, files["db"]), files["db"])
	others := map[string]parse.Artifact{}
	for role, suffix := range map[string]string{"wal": "-wal", "journal": "-journal"} {
		if data, ok := files[role]; ok {
			others[role] = h.Artifact(parsertest.AddAndroidFile(t, h.Case, "acq1", remote+suffix, data), data)
		}
	}
	return h.Input(p, primary, others, forProbe)
}

func probeAt(t *testing.T, remote string, files map[string][]byte) parse.Applicability {
	t.Helper()
	h := parsertest.NewHarness(t)
	p := androidgooglemessages.Parser{}
	got, err := p.Probe(t.Context(), inputAt(t, h, p, remote, files, true))
	if err != nil {
		t.Fatalf("Probe(%s): %v", remote, err)
	}
	return got
}

func parseAt(t *testing.T, remote string, files map[string][]byte) parsertest.Result {
	t.Helper()
	h := parsertest.NewHarness(t)
	p := androidgooglemessages.Parser{}
	return h.RunLenient(p, inputAt(t, h, p, remote, files, false))
}

func fixtureFiles(t *testing.T, variant string) map[string][]byte {
	t.Helper()
	return parsertest.LoadFixture(t, "testdata", variant).Files
}

func dbOnly(db []byte) map[string][]byte { return map[string][]byte{"db": db} }

func withWAL(db []byte) map[string][]byte { return map[string][]byte{"db": db, "wal": {}} }

func compileAll(t *testing.T, globs []string) func(string) bool {
	t.Helper()
	var gs []*parse.Glob
	for _, s := range globs {
		g, err := parse.CompileGlob(s)
		if err != nil {
			t.Fatalf("%s: %v", s, err)
		}
		gs = append(gs, g)
	}
	return func(p string) bool {
		for _, g := range gs {
			if g.Match(p) {
				return true
			}
		}
		return false
	}
}

// Spec 5.1: Meta names the two primary globs, the companion roles and the one emitted type.
func TestAndroidGoogleMessagesMetaIsValid(t *testing.T) {
	m := androidgooglemessages.Parser{}.Meta()
	if err := parse.ValidateMeta(m); err != nil {
		t.Fatal(err)
	}
	if m.Name != "android.googlemessages" || m.Version != "1.0.0" || len(m.Emits) != 1 || m.Emits[0].Type != "message" || m.Emits[0].PayloadVersion != 1 {
		t.Errorf("meta %+v", m)
	}
	if len(m.Platforms) != 1 || m.Platforms[0] != parse.PlatformAndroid {
		t.Errorf("platforms %v", m.Platforms)
	}
	if len(m.Claims) != 2 || m.Claims[0].Table != "messages" || m.Claims[1].Table != "parts" {
		t.Errorf("claims %v, want messages and parts (spec 8.1)", m.Claims)
	}
	if len(m.Inputs) != 3 || m.Inputs[0].Role != "db" || !m.Inputs[0].Required || m.Inputs[1].Role != "wal" || m.Inputs[2].Role != "journal" {
		t.Errorf("inputs %+v", m.Inputs)
	}
}

// Spec 5.1: both globs compile and together match the two packages' databases directory file bugle_db and nothing else.
func TestAndroidGoogleMessagesGlobsCompileAndMatch(t *testing.T) {
	match := compileAll(t, androidgooglemessages.Parser{}.Meta().Inputs[0].Globs)
	for _, p := range []string{
		"android:/data/data/com.google.android.apps.messaging/databases/bugle_db",
		"android:/data/user/10/com.google.android.apps.messaging/databases/bugle_db",
		"android:/data/data/com.android.messaging/databases/bugle_db",
	} {
		if !match(p) {
			t.Errorf("no glob matches %s", p)
		}
	}
	for _, p := range []string{
		"android:/data/data/com.google.android.apps.messaging/databases/bugle_db-wal",
		"android:/data/data/com.google.android.apps.messaging/databases/bugle_db-journal",
		"android:/data/data/com.google.android.apps.messaging/databases/bugle_db.bak",
		"android:/data/data/com.google.android.apps.messaging/files/bugle_db",
		"android:/data/data/com.google.android.apps.messagingx/databases/bugle_db",
	} {
		if match(p) {
			t.Errorf("a glob matches %s", p)
		}
	}
}

// 4I-R21: no primary glob matches a companion; the companions reach the job only through their roles.
func TestAndroidGoogleMessagesCompanionIsNeverAPrimary(t *testing.T) {
	m := androidgooglemessages.Parser{}.Meta()
	match := compileAll(t, m.Inputs[0].Globs)
	for _, pkg := range []string{"com.google.android.apps.messaging", "com.android.messaging"} {
		for _, f := range []string{"bugle_db-wal", "bugle_db-journal", "bugle_db-shm"} {
			if match("android:/data/data/" + pkg + "/databases/" + f) {
				t.Errorf("a primary glob matches the companion %s/%s", pkg, f)
			}
		}
	}
	roles := map[string][]string{}
	for _, in := range m.Inputs[1:] {
		roles[in.Role] = in.Companions
	}
	if len(roles) != 2 || len(roles["wal"]) != 1 || roles["wal"][0] != "-wal" || len(roles["journal"]) != 1 || roles["journal"][0] != "-journal" {
		t.Errorf("companion roles %v", roles)
	}
}

// Spec 5.4, 5.3: Probe at the app path is loud, elsewhere quiet; every refusal names tables and columns only.
func TestGoogleMessagesProbePlaces(t *testing.T) {
	for _, v := range []string{"gm-aosp-v2", "gm-aosp-v1", "gm-proprietary-shaped"} {
		for _, remote := range []string{remoteApp, remoteAOSP, remoteUser10, remoteOther} {
			if got := probeAt(t, remote, fixtureFiles(t, v)); got.Status != parse.Applicable {
				t.Errorf("%s at %s: Probe = %+v", v, remote, got)
			}
		}
	}
	other := fixtureFiles(t, "gm-other-kind")
	if got := probeAt(t, remoteApp, other); got.Status != parse.UnsupportedSchema || got.Reason != reasonAppPfx+"missing column messages.conversation_id" {
		t.Errorf("gm-other-kind at the app path: Probe = %+v", got)
	}
	if got := probeAt(t, remoteOther, other); got.Status != parse.NotApplicable || got.Reason != reasonNotBugle {
		t.Errorf("gm-other-kind elsewhere: Probe = %+v", got)
	}
	ns := fixtureFiles(t, "gm-not-sqlite")
	if got := probeAt(t, remoteApp, ns); got.Status != parse.UnsupportedSchema || got.Reason != "not a SQLite file at the Messages app path" {
		t.Errorf("gm-not-sqlite at the app path: Probe = %+v", got)
	}
	if got := probeAt(t, remoteOther, ns); got.Status != parse.NotApplicable || got.Reason != "not an SQLite database" {
		t.Errorf("gm-not-sqlite elsewhere: Probe = %+v", got)
	}
	if got := probeAt(t, remoteApp, withWAL(ns["db"])); got.Status != parse.UnsupportedSchema ||
		got.Reason != "the main file is not a database but a -wal or -journal companion exists" {
		t.Errorf("not SQLite beside a companion: Probe = %+v", got)
	}
	cases := []struct {
		name   string
		db     []byte
		reason string
	}{
		{
			"no destination column", goBuilt(2, table{"messages", ddlMessages}, table{"parts", ddlParts}, table{"participants", ddlParticipNoD}, table{"conversations", ddlConvs}),
			"missing one of participants.normalized_destination, participants.send_destination, participants.display_destination",
		},
		{"no parts", goBuilt(2, table{"messages", ddlMessages}, table{"participants", ddlParticipants}, table{"conversations", ddlConvs}), "missing table parts"},
	}
	for _, c := range cases {
		if got := probeAt(t, remoteApp, dbOnly(c.db)); got.Status != parse.UnsupportedSchema || got.Reason != reasonAppPfx+c.reason {
			t.Errorf("%s at the app path: Probe = %+v", c.name, got)
		}
		if got := probeAt(t, remoteOther, dbOnly(c.db)); got.Status != parse.NotApplicable || got.Reason != reasonNotBugle {
			t.Errorf("%s elsewhere: Probe = %+v", c.name, got)
		}
	}
	// a key that is not the rowid alias is unsupported-schema at both places (rowrun's rule), never not-applicable
	noAlias := dbOnly(goBuilt(2, table{"messages", ddlMessages}, table{"parts", ddlPartsNoAlias}, table{"participants", ddlParticipants}, table{"conversations", ddlConvs}))
	for _, remote := range []string{remoteApp, remoteOther} {
		if got := probeAt(t, remote, noAlias); got.Status != parse.UnsupportedSchema || !strings.Contains(got.Reason, "column _id of table parts is not the rowid alias") {
			t.Errorf("parts._id not the alias at %s: Probe = %+v", remote, got)
		}
	}
	// a companion defers every schema finding to Parse
	if got := probeAt(t, remoteApp, withWAL(other["db"])); got.Status != parse.Applicable || got.Reason != reasonDeferred {
		t.Errorf("other kind beside a companion: Probe = %+v", got)
	}
}

// highEntropy is n deterministic bytes that look encrypted (a SHA-256 chain).
func highEntropy(n int) []byte {
	var out []byte
	sum := sha256.Sum256([]byte("4i5-encrypted"))
	for len(out) < n {
		out = append(out, sum[:]...)
		sum = sha256.Sum256(sum[:])
	}
	return out[:n]
}

// B9: "encrypted" is the shared detection (sqlitedb.ErrEncrypted), at the app path loud and elsewhere quiet; the package
// holds no entropy computation of its own.
func TestGoogleMessagesEncryptedUsesTheSharedDetection(t *testing.T) {
	enc := dbOnly(highEntropy(64 << 10))
	if got := probeAt(t, remoteApp, enc); got.Status != parse.Encrypted || got.Reason != "the file looks encrypted" {
		t.Errorf("encrypted at the app path: Probe = %+v", got)
	}
	if got := probeAt(t, remoteOther, enc); got.Status != parse.NotApplicable || got.Reason != "the file looks encrypted" {
		t.Errorf("encrypted elsewhere: Probe = %+v", got)
	}
	files, err := filepath.Glob("*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("glob: %v, %d files", err, len(files))
	}
	re := regexp.MustCompile(`(?i)entropy`)
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if loc := re.Find(src); loc != nil {
			t.Errorf("%s holds the identifier %q: encryption is detected by sqlitedb.Open alone (B9)", f, loc)
		}
	}
}

// Spec 5.4: a database of another kind beside a companion is deferred by Probe; Parse then states it elsewhere (a note,
// one warning, no record) and fails at the app path.
func TestGoogleMessagesOtherKindElsewhereIsNotApplicable(t *testing.T) {
	files := withWAL(fixtureFiles(t, "gm-other-kind")["db"])
	if got := probeAt(t, remoteOther, files); got.Status != parse.Applicable || got.Reason != reasonDeferred {
		t.Fatalf("Probe = %+v, want the deferral", got)
	}
	res := parseAt(t, remoteOther, files)
	if res.Err != nil || len(res.Records) != 0 {
		t.Fatalf("Parse: err %v, %d records", res.Err, len(res.Records))
	}
	if res.Notes["not_applicable"] != reasonNotBugle {
		t.Errorf("note not_applicable = %q", res.Notes["not_applicable"])
	}
	if len(res.Warnings) != 1 || res.Warnings[0].Locator != "sqlite:schema" || res.Warnings[0].Reason != "not applicable at parse time: "+reasonNotBugle {
		t.Errorf("warnings %+v", res.Warnings)
	}
	res = parseAt(t, remoteApp, files)
	if !errors.Is(res.Err, androidgooglemessages.ErrNoSignature) || len(res.Records) != 0 {
		t.Errorf("Parse at the app path: err %v, %d records, want ErrNoSignature", res.Err, len(res.Records))
	}
}

// partialSchema is a Go-built v2 database whose first schema cell pointer points outside page 1: the schema is read only
// in part, and a signature table is not in what was read.
func partialSchema() []byte {
	db := goBuilt(2, v2Tables()...)
	db[108], db[109] = 0xff, 0xff
	return db
}

// Unknown is never absent: a signature that cannot be decided because the schema was read in part is stated as that,
// at the app path ErrSchemaIncomplete and never ErrNoSignature, elsewhere not applicable with its own reason.
func TestGoogleMessagesPartialSchemaIsUndecidedNotAbsent(t *testing.T) {
	db := dbOnly(partialSchema())
	res := parseAt(t, remoteApp, db)
	if !errors.Is(res.Err, androidgooglemessages.ErrSchemaIncomplete) || errors.Is(res.Err, androidgooglemessages.ErrNoSignature) {
		t.Errorf("Parse at the app path: err %v, want ErrSchemaIncomplete", res.Err)
	}
	res = parseAt(t, remoteOther, db)
	if res.Err != nil || res.Notes["not_applicable"] == "" || res.Notes["not_applicable"] == reasonNotBugle {
		t.Errorf("Parse elsewhere: err %v, note not_applicable %q, want an undecided reason", res.Err, res.Notes["not_applicable"])
	}
}
