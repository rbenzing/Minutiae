package ioswhatsapp_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/ioswhatsapp"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

const (
	metaName   = ".com.apple.mobile_container_manager.metadata.plist"
	appGroupID = "5A0B4C3D-0000-4000-8000-00000000A001"
)

// goStore is a minimal Go-built ChatStorage: the identifying and chat tables and a Z_PRIMARYKEY naming the given entities
// (nil: no Z_PRIMARYKEY table), then pad rows of 2000 bytes. The chat row's text mentions WhatsAppSMB on purpose.
func goStore(model []string, pad int) []byte {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 4096})
	m := b.CreateTable("ZWAMESSAGE", "CREATE TABLE ZWAMESSAGE (Z_PK INTEGER PRIMARY KEY, ZCHATSESSION INTEGER, ZISFROMME INTEGER, ZMESSAGEDATE TIMESTAMP, ZMESSAGETYPE INTEGER)")
	c := b.CreateTable("ZWACHATSESSION", "CREATE TABLE ZWACHATSESSION (Z_PK INTEGER PRIMARY KEY, ZCONTACTJID VARCHAR, ZPARTNERNAME VARCHAR)")
	m.Insert(1, nil, int64(1), int64(0), int64(600000001), int64(0))
	c.Insert(1, nil, "15550000001@WhatsAppSMB", "WhatsAppSMB partner")
	if model != nil {
		z := b.CreateTable("Z_PRIMARYKEY", "CREATE TABLE Z_PRIMARYKEY (Z_ENT INTEGER PRIMARY KEY, Z_NAME VARCHAR, Z_SUPER INTEGER, Z_MAX INTEGER)")
		for i, n := range model {
			z.Insert(int64(i+1), nil, n, int64(0), int64(0))
		}
		for i := range pad {
			z.Insert(int64(len(model)+i+1), nil, strings.Repeat("P", 2000), int64(0), int64(0))
		}
	}
	return b.Bytes()
}

var okModel = []string{"WAMessage", "WAChatSession"}

func probeAt(t *testing.T, files map[string][]byte, domain, rel, logical string) (parse.Applicability, error) {
	t.Helper()
	h := parsertest.NewHarness(t)
	f := parsertest.Fixture{Name: "probe", Files: files}
	var b parsertest.Bundle
	if domain != "" {
		b = f.BundleIOSBackup(h, domain, rel)
	} else {
		b = f.BundleIOS(h, rel, logical)
	}
	p := ioswhatsapp.Parser{}
	return p.Probe(t.Context(), b.Input(h, p, true))
}

func probeDomain(t *testing.T, files map[string][]byte) parse.Applicability {
	t.Helper()
	got, err := probeAt(t, files, domainConsumer, dbName, "")
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func probeGroup(t *testing.T, files map[string][]byte) (parse.Applicability, error) {
	t.Helper()
	return probeAt(t, files, "", extractRemote, extractLogical)
}

func probeVariant(t *testing.T, v string) parse.Applicability {
	t.Helper()
	h := parsertest.NewHarness(t)
	b := bundleOf(h, parsertest.LoadFixture(t, "testdata", v), v)
	p := ioswhatsapp.Parser{}
	got, err := p.Probe(t.Context(), b.Input(h, p, true))
	if err != nil {
		t.Fatalf("%s: %v", v, err)
	}
	return got
}

func TestIOSWhatsAppChatsGlobsCompileAndMatch(t *testing.T) { // 4I2-R2
	m := ioswhatsapp.Parser{}.Meta()
	if err := parse.ValidateMeta(m); err != nil {
		t.Fatal(err)
	}
	if m.Name != "ios.whatsapp.chats" || m.Version != "1.0.0" || len(m.Claims) != 1 || m.Claims[0].Table != "ZWAMESSAGE" {
		t.Errorf("identity or claims: %+v", m)
	}
	if len(m.Platforms) != 1 || m.Platforms[0] != parse.PlatformIOS || len(m.Emits) != 1 || m.Emits[0].Type != "message" {
		t.Errorf("platforms %v emits %v", m.Platforms, m.Emits)
	}
	roles := map[string]parse.InputSpec{}
	for _, in := range m.Inputs {
		roles[in.Role] = in
		for _, g := range in.Globs {
			if _, err := parse.CompileGlob(g); err != nil {
				t.Errorf("glob %q: %v", g, err)
			}
		}
	}
	if len(roles) != 4 || m.Inputs[0].Role != "db" || !m.Inputs[0].Required {
		t.Fatalf("roles %+v", m.Inputs)
	}
	if w := roles["wal"].Companions; len(w) != 1 || w[0] != "-wal" {
		t.Errorf("wal %v", w)
	}
	if j := roles["journal"].Companions; len(j) != 1 || j[0] != "-journal" {
		t.Errorf("journal %v", j)
	}
	matches := func(logical string) bool {
		for _, s := range m.Inputs[0].Globs {
			g, _ := parse.CompileGlob(s)
			if g != nil && g.Match(logical) {
				return true
			}
		}
		return false
	}
	grp := "ios:HomeDomain/Containers/Shared/AppGroup/"
	for _, p := range []string{
		"ios:" + domainConsumer + "/ChatStorage.sqlite", "ios:" + domainBusiness + "/ChatStorage.sqlite",
		grp + appGroupID + "/ChatStorage.sqlite",
	} {
		if !matches(p) {
			t.Errorf("%s is not matched", p)
		}
	}
	for _, p := range []string{
		grp + "X/Y/ChatStorage.sqlite", "ios:" + domainConsumer + "/sub/ChatStorage.sqlite",
		"ios:AppDomain-net.whatsapp.WhatsApp/Documents/ChatStorage.sqlite", "ios:MediaDomain/ChatStorage.sqlite",
		"ios:HomeDomain/Library/ChatStorage.sqlite",
	} {
		if matches(p) {
			t.Errorf("%s is matched", p)
		}
	}
	if len(roles["container_meta"].Globs) != 1 {
		t.Fatalf("container_meta %+v", roles["container_meta"])
	}
	mg, err := parse.CompileGlob(roles["container_meta"].Globs[0])
	if err != nil || !mg.Relative() {
		t.Fatalf("container_meta glob %v: %v", roles["container_meta"].Globs, err)
	}
	if !mg.MatchRelative(grp+appGroupID+"/ChatStorage.sqlite", grp+appGroupID+"/"+metaName) {
		t.Error("the metadata plist beside an app-group primary is not matched")
	}
	if mg.MatchRelative(grp+appGroupID+"/ChatStorage.sqlite", grp+appGroupID+"/x/"+metaName) {
		t.Error("a metadata plist in a subdirectory is matched")
	}
}

func TestIOSWhatsAppChatsCompanionIsNeverAPrimary(t *testing.T) { // 4I-R21
	m := ioswhatsapp.Parser{}.Meta()
	for _, dir := range []string{"ios:" + domainConsumer, "ios:" + domainBusiness, "ios:HomeDomain/Containers/Shared/AppGroup/" + appGroupID} {
		for _, suffix := range []string{"-wal", "-journal", "-shm"} {
			for _, s := range m.Inputs[0].Globs {
				g, err := parse.CompileGlob(s)
				if err != nil {
					t.Fatal(err)
				}
				if g.Match(dir + "/ChatStorage.sqlite" + suffix) {
					t.Errorf("%s matches %s/ChatStorage.sqlite%s as a primary", s, dir, suffix)
				}
			}
		}
	}
}

func TestIOSWhatsAppChatsProbeByPlace(t *testing.T) { // 4I2-R2, PF9
	if got := probeVariant(t, "ios-chatstorage"); got.Status != parse.Applicable {
		t.Errorf("ios-chatstorage: %+v", got)
	}
	if got := probeVariant(t, "ios-chatstorage-extract"); got.Status != parse.Applicable {
		t.Errorf("ios-chatstorage-extract: %+v", got)
	}
	if got := probeVariant(t, "ios-chatstorage-otherapp"); got.Status != parse.NotApplicable {
		t.Errorf("ios-chatstorage-otherapp: %+v", got)
	}
	other := parsertest.LoadFixture(t, "testdata", "ios-chatstorage-otherapp").Files
	got := probeDomain(t, other)
	if got.Status != parse.UnsupportedSchema || got.Reason != "no WhatsApp ChatStorage tables at the WhatsApp app group path" {
		t.Errorf("other bytes at the domain: %+v", got)
	}
	text := map[string][]byte{"db": []byte(strings.Repeat("not a database, only text. ", 4)[:100])}
	got = probeDomain(t, text)
	if got.Status != parse.UnsupportedSchema || got.Reason != "not a SQLite file at the WhatsApp app group path" {
		t.Errorf("text at the domain: %+v", got)
	}
	got, err := probeGroup(t, text)
	if err != nil || got.Status != parse.NotApplicable {
		t.Errorf("text at an app group: %+v, %v", got, err)
	}
	// the identifying table present at an app group makes the rest required
	b := sqlitetest.New(sqlitetest.Options{PageSize: 4096})
	b.CreateTable("ZWAMESSAGE", "CREATE TABLE ZWAMESSAGE (Z_PK INTEGER PRIMARY KEY, ZCHATSESSION INTEGER, ZISFROMME INTEGER, ZMESSAGEDATE TIMESTAMP, ZMESSAGETYPE INTEGER)")
	got, err = probeGroup(t, map[string][]byte{"db": b.Bytes()})
	if err != nil || got.Status != parse.UnsupportedSchema || !strings.Contains(got.Reason, "ZWACHATSESSION") {
		t.Errorf("no chat table at an app group: %+v, %v", got, err)
	}
	// an absent optional table is not a finding (the nooptional variant)
	if got := probeVariant(t, "ios-chatstorage-nooptional"); got.Status != parse.Applicable {
		t.Errorf("ios-chatstorage-nooptional: %+v", got)
	}
}

func TestIOSWhatsAppChatsModelMustNameTheEntities(t *testing.T) { // 4I2-R3, PF9(b)
	for _, place := range []string{"domain", "app group"} {
		probe := func(db []byte) parse.Applicability {
			files := map[string][]byte{"db": db}
			if place == "domain" {
				return probeDomain(t, files)
			}
			got, err := probeGroup(t, files)
			if err != nil {
				t.Fatal(err)
			}
			return got
		}
		for _, c := range []struct {
			name   string
			db     []byte
			reason string
		}{
			{"no model", parsertest.LoadFixture(t, "testdata", "ios-chatstorage-nomodel").Files["db"], "no Z_PRIMARYKEY table: the Core Data model cannot be read"},
			{"other model", parsertest.LoadFixture(t, "testdata", "ios-chatstorage-othermodel").Files["db"], "Z_PRIMARYKEY does not name the entity WAMessage"},
			{"twice", goStore([]string{"WAMessage", "WAChatSession", "WAMessage"}, 0), "Z_PRIMARYKEY names the entity WAMessage more than once"},
			{"no chat entity", goStore([]string{"WAMessage"}, 0), "Z_PRIMARYKEY does not name the entity WAChatSession"},
			{"no model, Go-built", goStore(nil, 0), "no Z_PRIMARYKEY table: the Core Data model cannot be read"},
		} {
			if got := probe(c.db); got.Status != parse.UnsupportedSchema || got.Reason != c.reason {
				t.Errorf("%s at %s: %+v, want unsupported-schema %q", c.name, place, got, c.reason)
			}
		}
		if got := probe(goStore(okModel, 0)); got.Status != parse.Applicable {
			t.Errorf("a named model at %s: %+v", place, got)
		}
	}
	// with a companion the model check is deferred to Parse, which fails the job (PF9 b)
	files := map[string][]byte{"db": parsertest.LoadFixture(t, "testdata", "ios-chatstorage-othermodel").Files["db"], "wal": {}}
	if got := probeDomain(t, files); got.Status != parse.Applicable {
		t.Errorf("othermodel with an empty -wal: %+v, want applicable (deferred)", got)
	}
	h := parsertest.NewHarness(t)
	b := parsertest.Fixture{Name: "othermodel", Files: files}.BundleIOSBackup(h, domainConsumer, dbName)
	p := ioswhatsapp.Parser{}
	res := runLenient(t, h, p, b.Input(h, p, false))
	if !errors.Is(res.Err, ioswhatsapp.ErrUnsupportedModel) || len(res.Records) != 0 {
		t.Fatalf("Parse: err %v, %d records", res.Err, len(res.Records))
	}
	if !strings.Contains(res.Err.Error(), "Z_PRIMARYKEY does not name the entity WAMessage") {
		t.Errorf("the reason is lost: %v", res.Err)
	}
}

func TestIOSWhatsAppChatsEntityNumbersComeFromTheModel(t *testing.T) { // 4I2-R3, P3
	for v, want := range map[string]string{"ios-chatstorage": "9", "ios-chatstorage-renumbered": "2"} {
		h := parsertest.NewHarness(t)
		fx := parsertest.LoadFixture(t, "testdata", v)
		b := bundleOf(h, fx, v)
		p := ioswhatsapp.Parser{}
		res := runLenient(t, h, p, b.Input(h, p, false), "wal-frames-not-applied", "whatsapp-ios-join-undecided")
		if res.Err != nil {
			t.Fatalf("%s: %v", v, res.Err)
		}
		if got := res.Notes["entity.WAMessage"]; got != want {
			t.Errorf("%s: entity.WAMessage = %q, want %s", v, got, want)
		}
		for _, k := range []string{"entity.WAMessage", "entity.WAChatSession"} {
			if res.Notes[k] != fx.Expect.Notes[k] || res.Notes[k] == "" {
				t.Errorf("%s: note %s = %q, the oracle says %q", v, k, res.Notes[k], fx.Expect.Notes[k])
			}
		}
	}
}

func TestIOSWhatsAppChatsProbeModelReadIsBounded(t *testing.T) { // 4I2-R3, Limits.ProbeBytes
	db := goStore(okModel, 700) // 700 rows of 2000 bytes: more than the 1 MiB probe limit
	_, err := probeAt(t, map[string][]byte{"db": db}, domainConsumer, dbName, "")
	if !errors.Is(err, parse.ErrProbeLimit) {
		t.Errorf("domain: err %v, want parse.ErrProbeLimit", err)
	}
	_, err = probeGroup(t, map[string][]byte{"db": db})
	if !errors.Is(err, parse.ErrProbeLimit) {
		t.Errorf("app group: err %v, want parse.ErrProbeLimit", err)
	}
}

func TestParseRefusesWhatNoGlobMatches(t *testing.T) {
	h := parsertest.NewHarness(t)
	b := parsertest.Fixture{Name: "x", Files: map[string][]byte{"db": parsertest.LoadFixture(t, "testdata", "ios-chatstorage").Files["db"]}}.BundleIOS(h, "/private/var/mobile/Library/ChatStorage.sqlite", "ios:HomeDomain/Library/ChatStorage.sqlite")
	p := ioswhatsapp.Parser{}
	if res := runLenient(t, h, p, b.Input(h, p, false)); !errors.Is(res.Err, ioswhatsapp.ErrNotWhatsAppPath) {
		t.Errorf("err %v", res.Err)
	}
	got, err := p.Probe(t.Context(), b.Input(h, p, true))
	if err != nil || got.Status != parse.UnsupportedSchema || got.Reason != "not a WhatsApp app group path" {
		t.Errorf("Probe: %+v, %v", got, err)
	}
	// a domain database without the identifying table, under a companion, is refused at Parse
	h2 := parsertest.NewHarness(t)
	files := map[string][]byte{"db": parsertest.LoadFixture(t, "testdata", "ios-chatstorage-otherapp").Files["db"], "wal": {}}
	b2 := parsertest.Fixture{Name: "x", Files: files}.BundleIOSBackup(h2, domainConsumer, dbName)
	if res := runLenient(t, h2, p, b2.Input(h2, p, false)); !errors.Is(res.Err, ioswhatsapp.ErrNoWhatsAppTables) {
		t.Errorf("domain without the identifying table: %v", res.Err)
	}
}

// zpkStore is a Go-built store whose Z_PRIMARYKEY holds the given names in key order, padded between them so that the rows
// span several leaf pages. corrupt, when set, is the name whose LAST occurrence's leaf page gets an invalid page type.
func zpkStore(names []string, corrupt string) []byte {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 4096})
	m := b.CreateTable("ZWAMESSAGE", "CREATE TABLE ZWAMESSAGE (Z_PK INTEGER PRIMARY KEY, ZCHATSESSION INTEGER, ZISFROMME INTEGER, ZMESSAGEDATE TIMESTAMP, ZMESSAGETYPE INTEGER)")
	c := b.CreateTable("ZWACHATSESSION", "CREATE TABLE ZWACHATSESSION (Z_PK INTEGER PRIMARY KEY, ZCONTACTJID VARCHAR)")
	m.Insert(1, nil, int64(1), int64(0), int64(600000001), int64(0))
	c.Insert(1, nil, "15550000001@s.whatsapp.net")
	z := b.CreateTable("Z_PRIMARYKEY", "CREATE TABLE Z_PRIMARYKEY (Z_ENT INTEGER PRIMARY KEY, Z_NAME VARCHAR, Z_SUPER INTEGER, Z_MAX INTEGER)")
	for i, n := range names {
		z.Insert(int64(i+1), nil, n, int64(0), int64(0))
	}
	data := b.Bytes()
	if corrupt != "" {
		off := bytes.LastIndex(data, []byte(corrupt))
		data[off/4096*4096] = 0 // the page type byte of the leaf that holds the row
	}
	return data
}

func pad(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = strings.Repeat("P", 2000) + string(rune('a'+i))
	}
	return out
}

// A damaged Z_PRIMARYKEY leaf is a loss, not an absence and not a count: unknown is never absent (review I1).
func TestIOSWhatsAppChatsModelOnADamagedPageIsUnreadable(t *testing.T) {
	const reason = "Z_PRIMARYKEY could not be read whole"
	missing := zpkStore(append(pad(20), "WAMessage", "WAChatSession"), "WAMessage") // both entities on the lost last leaf
	dup := zpkStore(append(append([]string{"WAMessage", "WAChatSession"}, pad(20)...), "WAMessage"), "WAMessage")
	whole := zpkStore(append(append([]string{"WAMessage", "WAChatSession"}, pad(20)...), "Other"), "")
	if got := probeDomain(t, map[string][]byte{"db": whole}); got.Status != parse.Applicable {
		t.Fatalf("control: %+v", got)
	}
	for name, db := range map[string][]byte{"missing": missing, "duplicate": dup} {
		if got := probeDomain(t, map[string][]byte{"db": db}); got.Status != parse.UnsupportedSchema || got.Reason != reason {
			t.Errorf("%s: Probe %+v, want unsupported-schema %q", name, got, reason)
		}
		h := parsertest.NewHarness(t)
		b := parsertest.Fixture{Name: name, Files: map[string][]byte{"db": db}}.BundleIOSBackup(h, domainConsumer, dbName)
		p := ioswhatsapp.Parser{}
		res := runLenient(t, h, p, b.Input(h, p, false), "page-type")
		if !errors.Is(res.Err, ioswhatsapp.ErrUnsupportedModel) || !strings.Contains(res.Err.Error(), reason) || len(res.Records) != 0 {
			t.Errorf("%s: Parse err %v", name, res.Err)
		}
		if len(res.Violations) != 0 {
			t.Errorf("%s: violations %v", name, res.Violations)
		}
		if res.Notes["entity.WAMessage"] != "" {
			t.Errorf("%s: an entity note was written: %v", name, res.Notes)
		}
		// the page damage is evidence: it is forwarded although the job fails (review M3)
		found := false
		for _, w := range res.Warnings {
			found = found || strings.HasPrefix(w.Locator, "sqlite:file=db;page=")
		}
		if !found {
			t.Errorf("%s: no page warning was forwarded: %v", name, res.Warnings)
		}
	}
}

// runLenient is Harness.RunLenient plus the checks a lenient run would otherwise skip (review M5): no contract violation,
// and every warning is one of the expected kinds (by default only the WAL frames the engine does not apply, which the
// fixtures with a -wal legitimately raise; a test whose store lacks a table passes the counted join warning explicitly).
func runLenient(t *testing.T, h *parsertest.Harness, p ioswhatsapp.Parser, in *parse.Input, allowed ...string) parsertest.Result {
	t.Helper()
	if len(allowed) == 0 {
		allowed = []string{"wal-frames-not-applied"}
	}
	res := h.RunLenient(p, in)
	if len(res.Violations) != 0 {
		t.Errorf("contract violations: %v", res.Violations)
	}
	for _, w := range res.Warnings {
		ok := false
		for _, a := range allowed {
			ok = ok || strings.Contains(w.Reason, a)
		}
		if !ok {
			t.Errorf("unexpected warning %s: %s", w.Locator, w.Reason)
		}
	}
	return res
}

// A row the reader still delivers but warns about decides the model, so the warning must reach the job (review N1).
func TestIOSWhatsAppChatsModelWarningsOfADeliveredRowAreForwarded(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 4096})
	m := b.CreateTable("ZWAMESSAGE", "CREATE TABLE ZWAMESSAGE (Z_PK INTEGER PRIMARY KEY, ZCHATSESSION INTEGER, ZISFROMME INTEGER, ZMESSAGEDATE TIMESTAMP, ZMESSAGETYPE INTEGER)")
	c := b.CreateTable("ZWACHATSESSION", "CREATE TABLE ZWACHATSESSION (Z_PK INTEGER PRIMARY KEY, ZCONTACTJID VARCHAR)")
	m.Insert(1, nil, int64(1), int64(0), int64(600000001), int64(0))
	c.Insert(1, nil, "15550000001@s.whatsapp.net")
	z := b.CreateTable("Z_PRIMARYKEY", "CREATE TABLE Z_PRIMARYKEY (Z_ENT INTEGER PRIMARY KEY, Z_NAME VARCHAR, Z_SUPER INTEGER, Z_MAX INTEGER)")
	z.InsertRaw(1, []uint64{0, 31, 8, 8}, []byte("WAMessagex")) // one byte more than the header accounts for
	z.Insert(2, nil, "WAChatSession", int64(0), int64(0))
	h := parsertest.NewHarness(t)
	bd := parsertest.Fixture{Name: "mismatch", Files: map[string][]byte{"db": b.Bytes()}}.BundleIOSBackup(h, domainConsumer, dbName)
	p := ioswhatsapp.Parser{}
	res := runLenient(t, h, p, bd.Input(h, p, false), "record-length-mismatch")
	if res.Err != nil || res.Notes["entity.WAMessage"] != "1" {
		t.Fatalf("the delivered row did not decide the model: err %v notes %v", res.Err, res.Notes)
	}
	n := 0
	for _, w := range res.Warnings {
		if strings.Contains(w.Reason, "record-length-mismatch") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("record-length-mismatch warnings = %d, want 1 (the model read's, once): %v", n, res.Warnings)
	}
}
