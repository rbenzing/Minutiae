package androidtelegram_test

import (
	"crypto/sha256"
	"errors"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/androidtelegram"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

const (
	remoteApp      = "/data/data/org.telegram.messenger/files/cache4.db"
	remoteAccount1 = "/data/user/0/org.telegram.messenger/files/account1/cache4.db"
	remoteOther    = "/sdcard/x/cache4.db"
	reasonDeferred = "schema checked at parse time: a -wal or -journal companion is present"
	reasonNoTable  = "no Telegram message table at the app path"
	reasonNotTG    = "not a Telegram cache4.db"
)

// The DDL of the Go-built databases: the signature columns of spec 8.1 and what the tests take away from them.
const (
	ddlV2               = `CREATE TABLE messages_v2 (mid INTEGER, uid INTEGER, read_state INTEGER, date INTEGER, data BLOB, out INTEGER)`
	ddlV2NoOut          = `CREATE TABLE messages_v2 (mid INTEGER, uid INTEGER, read_state INTEGER, date INTEGER, data BLOB)`
	ddlV1               = `CREATE TABLE messages (mid INTEGER PRIMARY KEY, uid INTEGER, date INTEGER, data BLOB, out INTEGER)`
	ddlUsers            = `CREATE TABLE users (uid INTEGER PRIMARY KEY, name TEXT, status INTEGER, data BLOB)`
	ddlUsersData        = `CREATE TABLE users_data (uid INTEGER PRIMARY KEY, about TEXT)`
	ddlUsersDataNoAbout = `CREATE TABLE users_data (uid INTEGER PRIMARY KEY, bio TEXT)`
	ddlNotes            = `CREATE TABLE notes (_id INTEGER PRIMARY KEY, body TEXT)`
)

// table is one table of a Go-built database: its name and its DDL.
type table struct{ name, ddl string }

// goBuilt is a Go-built database at the given user version holding the tables, with no rows.
func goBuilt(userVersion int32, tables ...table) []byte {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 4096, UserVersion: userVersion})
	for _, t := range tables {
		b.CreateTable(t.name, t.ddl)
	}
	return b.Bytes()
}

func v2Tables() []table {
	return []table{{"messages_v2", ddlV2}, {"users", ddlUsers}, {"users_data", ddlUsersData}}
}

// inputAt stores files (roles db, wal, journal) at remote through parsertest.AddAndroidFile and returns the input.
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
	p := androidtelegram.Parser{}
	got, err := p.Probe(t.Context(), inputAt(t, h, p, remote, files, true))
	if err != nil {
		t.Fatalf("Probe(%s): %v", remote, err)
	}
	return got
}

func parseAt(t *testing.T, remote string, files map[string][]byte) parsertest.Result {
	t.Helper()
	h := parsertest.NewHarness(t)
	p := androidtelegram.Parser{}
	return h.RunLenient(p, inputAt(t, h, p, remote, files, false))
}

func fixtureFiles(t *testing.T, variant string) map[string][]byte {
	t.Helper()
	return parsertest.LoadFixture(t, "testdata", variant).Files
}

func dbOnly(db []byte) map[string][]byte { return map[string][]byte{"db": db} }

func withWAL(db []byte) map[string][]byte { return map[string][]byte{"db": db, "wal": {}} }

// Spec 8.1, 4I4-P7: Meta names the two primary globs and the companion roles.
func TestAndroidTelegramMetaIsValid(t *testing.T) {
	m := androidtelegram.Parser{}.Meta()
	if err := parse.ValidateMeta(m); err != nil {
		t.Fatal(err)
	}
	if m.Name != "android.telegram" || m.Version != "1.1.3" || len(m.Claims) != 3 { // the claims: TestClaimsEqualClaimableTables (Task 14)
		t.Errorf("meta %+v", m)
	}
	var emits []string
	for _, e := range m.Emits {
		emits = append(emits, e.Type)
		if e.PayloadVersion != 1 {
			t.Errorf("emit %+v: payload version", e)
		}
	}
	if strings.Join(emits, ",") != "message,call,contact" {
		t.Errorf("emits %v", emits)
	}
	if len(m.Platforms) != 1 || m.Platforms[0] != parse.PlatformAndroid {
		t.Errorf("platforms %v", m.Platforms)
	}
	// 4I4-P6: the typed pass maps both message tables, the old generation first, then users
	var tables []string
	for _, mp := range (androidtelegram.Parser{}).Mappings() {
		tables = append(tables, mp.Table)
		if mp.Map == nil {
			t.Errorf("mapping %s has no Map", mp.Table)
		}
	}
	if strings.Join(tables, ",") != "messages,messages_v2,users" {
		t.Errorf("Mappings tables %v", tables)
	}
}

// Spec 8.1, 4I4-P7, Q9: both globs compile and together match the account-0 file and the account<x> files of the
// app's files directory, and nothing else (a companion, a deeper file, another package, another directory).
func TestAndroidTelegramGlobsCompileAndMatch(t *testing.T) {
	m := androidtelegram.Parser{}.Meta()
	want := []string{"android:**/org.telegram.messenger/files/cache4.db", "android:**/org.telegram.messenger/files/account*/cache4.db"}
	if len(m.Inputs) == 0 || m.Inputs[0].Role != "db" || !m.Inputs[0].Required || strings.Join(m.Inputs[0].Globs, "|") != strings.Join(want, "|") {
		t.Fatalf("primary input %+v, want the globs %v", m.Inputs, want)
	}
	var globs []*parse.Glob
	for _, s := range want {
		g, err := parse.CompileGlob(s)
		if err != nil {
			t.Fatalf("%s: %v", s, err)
		}
		globs = append(globs, g)
	}
	match := func(p string) bool {
		for _, g := range globs {
			if g.Match(p) {
				return true
			}
		}
		return false
	}
	for _, p := range []string{
		"android:/data/data/org.telegram.messenger/files/cache4.db",
		"android:/data/user/0/org.telegram.messenger/files/account1/cache4.db",
		"android:/data/user/0/org.telegram.messenger/files/account12/cache4.db",
		"android:/data/user/0/org.telegram.messenger/files/accountX/cache4.db",
	} {
		if !match(p) {
			t.Errorf("no glob matches %s", p)
		}
	}
	for _, p := range []string{
		"android:/data/data/org.telegram.messenger/files/cache4.db-wal",
		"android:/data/data/org.telegram.messenger/files/account1/sub/cache4.db",
		"android:/data/data/org.telegram.messenger.web/files/cache4.db",
		"android:/data/data/org.telegram.messenger/databases/cache4.db",
	} {
		if match(p) {
			t.Errorf("a glob matches %s", p)
		}
	}
}

// 4I-R21: no primary glob matches a companion; the companions reach the job only through their roles.
func TestAndroidTelegramCompanionIsNeverAPrimary(t *testing.T) {
	m := androidtelegram.Parser{}.Meta()
	for _, in := range m.Inputs {
		for _, gs := range in.Globs {
			g, err := parse.CompileGlob(gs)
			if err != nil {
				t.Fatal(err)
			}
			for _, d := range []string{"files/", "files/account1/"} {
				for _, f := range []string{"cache4.db-wal", "cache4.db-journal", "cache4.db-shm"} {
					if p := "android:/data/data/org.telegram.messenger/" + d + f; g.Match(p) {
						t.Errorf("the glob %s matches the companion %s", gs, p)
					}
				}
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

// Spec 8.1, 4I4-P8, F2: at the app path every refusal is loud; elsewhere a database of another kind is not
// applicable, and the signature is checked before anything else of the schema.
func TestAndroidTelegramProbeByPlace(t *testing.T) {
	for _, v := range []string{"telegram-v181", "telegram-v159", "telegram-v84", "telegram-v64"} {
		if got := probeAt(t, remoteApp, fixtureFiles(t, v)); got.Status != parse.Applicable {
			t.Errorf("%s: Probe = %+v", v, got)
		}
	}
	noMessages := fixtureFiles(t, "telegram-nomessages")
	if got := probeAt(t, remoteApp, noMessages); got.Status != parse.UnsupportedSchema || got.Reason != reasonNoTable {
		t.Errorf("telegram-nomessages at the app path: Probe = %+v", got)
	}
	if got := probeAt(t, remoteOther, noMessages); got.Status != parse.NotApplicable || got.Reason != reasonNotTG {
		t.Errorf("telegram-nomessages at %s: Probe = %+v", remoteOther, got)
	}
	text := dbOnly([]byte(strings.Repeat("not a database. ", 7)[:100]))
	if got := probeAt(t, remoteApp, text); got.Status != parse.UnsupportedSchema || got.Reason != "not a SQLite file at the Telegram app path" {
		t.Errorf("text at the app path: Probe = %+v", got)
	}
	if got := probeAt(t, remoteOther, text); got.Status != parse.NotApplicable {
		t.Errorf("text at %s: Probe = %+v", remoteOther, got)
	}
	// F2: messages_v2 without out fails the signature, which is checked first
	noOut := goBuilt(181, table{"messages_v2", ddlV2NoOut}, table{"users", ddlUsers})
	if got := probeAt(t, remoteApp, dbOnly(noOut)); got.Status != parse.UnsupportedSchema || got.Reason != reasonNoTable {
		t.Errorf("messages_v2 without out at the app path: Probe = %+v", got)
	}
	if got := probeAt(t, remoteOther, dbOnly(noOut)); got.Status != parse.NotApplicable || got.Reason != reasonNotTG {
		t.Errorf("messages_v2 without out at %s: Probe = %+v", remoteOther, got)
	}
	// F2: a column outside the signature is rowrun's missing-column reason, unsupported-schema at both places
	noAbout := goBuilt(181, table{"messages_v2", ddlV2}, table{"users", ddlUsers}, table{"users_data", ddlUsersDataNoAbout})
	for _, remote := range []string{remoteApp, remoteOther} {
		if got := probeAt(t, remote, dbOnly(noAbout)); got.Status != parse.UnsupportedSchema || !strings.Contains(got.Reason, "about") {
			t.Errorf("users_data without about at %s: Probe = %+v", remote, got)
		}
	}
	// spec 8.1 layout: from 84 on the version needs messages_v2
	only1 := goBuilt(90, table{"messages", ddlV1}, table{"users", ddlUsers})
	if got := probeAt(t, remoteApp, dbOnly(only1)); got.Status != parse.UnsupportedSchema || got.Reason != "cache4.db user version 90 needs table messages_v2" {
		t.Errorf("version 90 with messages only at the app path: Probe = %+v", got)
	}
	if got := probeAt(t, remoteOther, dbOnly(only1)); got.Status != parse.NotApplicable || got.Reason != "cache4.db user version 90 needs table messages_v2" {
		t.Errorf("version 90 with messages only at %s: Probe = %+v", remoteOther, got)
	}
	only2 := goBuilt(70, v2Tables()...)
	if got := probeAt(t, remoteApp, dbOnly(only2)); got.Status != parse.UnsupportedSchema || got.Reason != "cache4.db user version 70 needs table messages" {
		t.Errorf("version 70 with messages_v2 only at the app path: Probe = %+v", got)
	}
	if got := probeAt(t, remoteAccount1, fixtureFiles(t, "telegram-v181")); got.Status != parse.Applicable {
		t.Errorf("telegram-v181 at %s: Probe = %+v", remoteAccount1, got)
	}
	// 4I3-P6: a companion defers the schema to Parse at either place
	for _, remote := range []string{remoteApp, remoteOther} {
		if got := probeAt(t, remote, withWAL(noOut)); got.Status != parse.Applicable || got.Reason != reasonDeferred {
			t.Errorf("a companion at %s: Probe = %+v, want the deferral", remote, got)
		}
	}
	// after the deferral Parse at the app path refuses a database without the signature loudly
	res := parseAt(t, remoteApp, withWAL(noOut))
	if !errors.Is(res.Err, androidtelegram.ErrNoMessageTable) || len(res.Records) != 0 {
		t.Errorf("messages_v2 without out and a -wal at the app path: Parse err %v, %d records", res.Err, len(res.Records))
	}
	res = parseAt(t, remoteApp, withWAL(only1))
	if !errors.Is(res.Err, androidtelegram.ErrNoMessageTable) || !strings.Contains(res.Err.Error(), "needs table messages_v2") {
		t.Errorf("version 90 with messages only and a -wal: Parse err %v", res.Err)
	}
}

// B4, 4I4-P8: at the app path a cache4.db outside the user-version band 64-181 is refused loudly by Probe and, after a
// deferral, by Parse.
func TestTelegramVersionBandIsLoudAtAppPath(t *testing.T) {
	for v, variant := range map[string]string{"182": "telegram-otherversion", "63": "telegram-oldversion"} {
		reason := "cache4.db user version " + v + " is outside the supported band 64-181"
		if got := probeAt(t, remoteApp, fixtureFiles(t, variant)); got.Status != parse.UnsupportedSchema || got.Reason != reason {
			t.Errorf("%s at the app path: Probe = %+v, want %q", variant, got, reason)
		}
	}
	for _, v := range []int32{64, 181} {
		tables := v2Tables()
		if v == 64 {
			tables = []table{{"messages", ddlV1}, {"users", ddlUsers}}
		}
		if got := probeAt(t, remoteApp, dbOnly(goBuilt(v, tables...))); got.Status != parse.Applicable {
			t.Errorf("user version %d at the app path: Probe = %+v", v, got)
		}
	}
	other := fixtureFiles(t, "telegram-otherversion")
	other["wal"] = []byte{}
	if got := probeAt(t, remoteApp, other); got.Status != parse.Applicable || got.Reason != reasonDeferred {
		t.Errorf("telegram-otherversion with an empty -wal: Probe = %+v, want the deferral", got)
	}
	res := parseAt(t, remoteApp, other)
	if !errors.Is(res.Err, androidtelegram.ErrUnsupportedVersion) || len(res.Records) != 0 {
		t.Errorf("telegram-otherversion with an empty -wal: Parse err %v, %d records; want ErrUnsupportedVersion and none", res.Err, len(res.Records))
	}
}

// Call 2, 4I3 final review I1, Q12, F3: away from the app path the signature comes first; a database of another kind
// is not applicable whatever its version, in Probe and in Parse, and gives no record.
func TestTelegramOtherKindElsewhereIsNotApplicable(t *testing.T) {
	reason := "cache4.db user version 182 is outside the supported band 64-181"
	if got := probeAt(t, remoteOther, fixtureFiles(t, "telegram-otherversion")); got.Status != parse.NotApplicable || got.Reason != reason {
		t.Errorf("telegram-otherversion at %s: Probe = %+v", remoteOther, got)
	}
	foreign := goBuilt(999, table{"notes", ddlNotes})
	if got := probeAt(t, remoteOther, dbOnly(foreign)); got.Status != parse.NotApplicable || got.Reason != reasonNotTG {
		t.Errorf("a foreign database at version 999 at %s: Probe = %+v", remoteOther, got)
	}
	// F3: messages_v2 with the signature columns but no users is of another kind, never a step on a present table
	noUsers := goBuilt(181, table{"messages_v2", ddlV2})
	for name, db := range map[string][]byte{"foreign": foreign, "no users": noUsers} {
		if got := probeAt(t, remoteOther, withWAL(db)); got.Status != parse.Applicable || got.Reason != reasonDeferred {
			t.Errorf("%s with an empty -wal: Probe = %+v, want the deferral", name, got)
		}
		res := parseAt(t, remoteOther, withWAL(db))
		if res.Err != nil || len(res.Records) != 0 {
			t.Errorf("%s with an empty -wal: Parse err %v, %d records; want not-applicable", name, res.Err, len(res.Records))
		}
		if errors.Is(res.Err, androidtelegram.ErrUnsupportedVersion) || errors.Is(res.Err, androidtelegram.ErrNoMessageTable) {
			t.Errorf("%s: Parse err %v", name, res.Err)
		}
		if got := res.Notes["not_applicable"]; got != reasonNotTG {
			t.Errorf("%s: note not_applicable = %q, want %q", name, got, reasonNotTG)
		}
		found := false
		for _, w := range res.Warnings {
			found = found || (w.Locator == "sqlite:schema" && w.Reason == "not applicable at parse time: "+reasonNotTG)
		}
		if !found {
			t.Errorf("%s: warnings %v, want the not-applicable warning", name, res.Warnings)
		}
		if _, has := res.Notes["database_version"]; has {
			t.Errorf("%s: the store notes are stated for a database of another kind (%v)", name, res.Notes)
		}
	}
}

// notApplicableAs says the run stated not-applicable with exactly this reason, in rowrun's form, and no record.
func notApplicableAs(t *testing.T, name string, res parsertest.Result, reason string) {
	t.Helper()
	if res.Err != nil || len(res.Records) != 0 {
		t.Errorf("%s: Parse err %v, %d records; want not-applicable", name, res.Err, len(res.Records))
	}
	if got := res.Notes["not_applicable"]; got != reason {
		t.Errorf("%s: note not_applicable = %q, want %q", name, got, reason)
	}
	found := false
	for _, w := range res.Warnings {
		found = found || (w.Locator == "sqlite:schema" && w.Reason == "not applicable at parse time: "+reason)
	}
	if !found {
		t.Errorf("%s: warnings %v, want the not-applicable warning %q", name, res.Warnings, reason)
	}
}

// highEntropy is n deterministic bytes that look encrypted (a SHA-256 chain).
func highEntropy(n int) []byte {
	var out []byte
	sum := sha256.Sum256([]byte("android.telegram"))
	for len(out) < n {
		out = append(out, sum[:]...)
		sum = sha256.Sum256(sum[:])
	}
	return out[:n]
}

// partialSchema is a Go-built v181 database with the signature tables whose first schema cell pointer points outside
// page 1: the schema is read only in part, and a signature table is not in what was read.
func partialSchema() []byte {
	db := goBuilt(181, table{"messages_v2", ddlV2}, table{"users", ddlUsers})
	db[108], db[109] = 0xff, 0xff
	return db
}

// virtualV2 is a Go-built v181 database whose messages_v2 is a virtual table (it exists and cannot be read).
func virtualV2() []byte {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 4096, UserVersion: 181})
	b.CreateTable("users", ddlUsers)
	b.AddSchemaRow("table", "messages_v2", "messages_v2", int64(0), "CREATE VIRTUAL TABLE messages_v2 USING fts4(data)")
	return b.Bytes()
}

// Task 6 review (I1, M2): the Probe and Parse matrix beyond the brief's rows. An undecided signature (a schema read in
// part, a signature table that cannot be read) is never stated as "not a Telegram cache4.db": unknown is never absent.
// With the signature present elsewhere, the band and the layout fail Parse after a deferral. The app path is loud for
// a non-database beside a companion and Encrypted for an encrypted file; an account<x> that is not digits is still the
// app path, with the account undecided.
func TestTelegramProbeAndParseMatrix(t *testing.T) {
	const schemaUnread = "the schema could not be read whole and no Telegram message table was found in what was read"
	partial := partialSchema()
	if got := probeAt(t, remoteOther, dbOnly(partial)); got.Status != parse.NotApplicable || got.Reason != schemaUnread {
		t.Errorf("a schema read in part at %s: Probe = %+v", remoteOther, got)
	}
	if got := probeAt(t, remoteApp, dbOnly(partial)); got.Status != parse.UnsupportedSchema || got.Reason != schemaUnread {
		t.Errorf("a schema read in part at the app path: Probe = %+v", got)
	}
	// I1: elsewhere Parse states the undecided reason, never the decided absence
	notApplicableAs(t, "a schema read in part with a -wal elsewhere", parseAt(t, remoteOther, withWAL(partial)), schemaUnread)
	res := parseAt(t, remoteApp, withWAL(partial))
	if !errors.Is(res.Err, androidtelegram.ErrSchemaIncomplete) || errors.Is(res.Err, androidtelegram.ErrNoMessageTable) || len(res.Records) != 0 {
		t.Errorf("a schema read in part with a -wal at the app path: Parse err %v; want ErrSchemaIncomplete", res.Err)
	}
	virtual := virtualV2()
	const v2Unread = "table messages_v2 could not be read"
	if got := probeAt(t, remoteOther, dbOnly(virtual)); got.Status != parse.NotApplicable || got.Reason != v2Unread {
		t.Errorf("a virtual messages_v2 at %s: Probe = %+v", remoteOther, got)
	}
	if got := probeAt(t, remoteApp, dbOnly(virtual)); got.Status != parse.UnsupportedSchema || got.Reason != v2Unread {
		t.Errorf("a virtual messages_v2 at the app path: Probe = %+v", got)
	}
	notApplicableAs(t, "a virtual messages_v2 with a -wal elsewhere", parseAt(t, remoteOther, withWAL(virtual)), v2Unread)
	res = parseAt(t, remoteApp, withWAL(virtual))
	if res.Err == nil || errors.Is(res.Err, androidtelegram.ErrNoMessageTable) || len(res.Records) != 0 {
		t.Errorf("a virtual messages_v2 with a -wal at the app path: Parse err %v; want the table's error", res.Err)
	}

	// the signature present elsewhere: the band, then the layout, after a deferral
	if res := parseAt(t, remoteOther, withWAL(goBuilt(200, v2Tables()...))); !errors.Is(res.Err, androidtelegram.ErrUnsupportedVersion) || len(res.Records) != 0 {
		t.Errorf("version 200 with the signature and a -wal elsewhere: Parse err %v", res.Err)
	}
	only1 := goBuilt(90, table{"messages", ddlV1}, table{"users", ddlUsers})
	if res := parseAt(t, remoteOther, withWAL(only1)); !errors.Is(res.Err, androidtelegram.ErrNoMessageTable) || !strings.Contains(res.Err.Error(), "needs table messages_v2") {
		t.Errorf("version 90 with messages only and a -wal elsewhere: Parse err %v", res.Err)
	}

	// not SQLite beside a companion, and an encrypted file
	text := []byte(strings.Repeat("not a database. ", 7)[:100])
	if got := probeAt(t, remoteApp, withWAL(text)); got.Status != parse.UnsupportedSchema || got.Reason != "the main file is not a database but a -wal or -journal companion exists" {
		t.Errorf("text with a companion at the app path: Probe = %+v", got)
	}
	if got := probeAt(t, remoteOther, withWAL(text)); got.Status != parse.NotApplicable {
		t.Errorf("text with a companion at %s: Probe = %+v", remoteOther, got)
	}
	enc := dbOnly(highEntropy(8192))
	if got := probeAt(t, remoteApp, enc); got.Status != parse.Encrypted || got.Reason != "the file looks encrypted" {
		t.Errorf("encrypted at the app path: Probe = %+v", got)
	}
	if got := probeAt(t, remoteOther, enc); got.Status != parse.NotApplicable {
		t.Errorf("encrypted at %s: Probe = %+v", remoteOther, got)
	}

	// 4I4-P7, F34: an account<x> that is not 1 to 6 digits is the app path with the account undecided
	for _, dir := range []string{"accountX", "account1234567"} {
		remote := "/data/user/0/org.telegram.messenger/files/" + dir + "/cache4.db"
		if got := probeAt(t, remote, dbOnly(goBuilt(70, table{"messages", ddlV1}))); got.Status != parse.UnsupportedSchema || got.Reason != reasonNoTable {
			t.Errorf("%s: Probe = %+v, want the app path's loud refusal", dir, got)
		}
		res := parseAt(t, remote, fixtureFiles(t, "telegram-v181"))
		if res.Err != nil || res.Notes["account"] != "undecided" {
			t.Errorf("%s: Parse err %v, note account = %q, want undecided", dir, res.Err, res.Notes["account"])
		}
	}
}
