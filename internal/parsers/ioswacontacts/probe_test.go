package ioswacontacts_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/ioswacontacts"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

const (
	metaName   = ".com.apple.mobile_container_manager.metadata.plist"
	appGroupID = "5A0B4C3D-0000-4000-8000-00000000A001"
	domainBiz  = "AppDomainGroup-group.net.whatsapp.WhatsAppSMB.shared"
)

// goStore is a minimal Go-built ContactsV2: one contact. Its text mentions WhatsAppSMB on purpose: the database's own text never
// labels the app.
func goStore() []byte {
	db, _ := buildContacts(nil, func(put putRow) {
		put(1, contactRow(map[string]any{"ZFULLNAME": "WhatsAppSMB contact", "ZWHATSAPPID": "15550000001@s.whatsapp.net", "ZABOUTTEXT": "net.whatsapp.WhatsAppSMB"}))
	})
	return db
}

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
	p := ioswacontacts.Parser{}
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
	p := ioswacontacts.Parser{}
	got, err := p.Probe(t.Context(), b.Input(h, p, true))
	if err != nil {
		t.Fatalf("%s: %v", v, err)
	}
	return got
}

func TestIOSWhatsAppContactsGlobsCompileAndMatch(t *testing.T) { // 4I2-R2, 4I2-R10
	m := ioswacontacts.Parser{}.Meta()
	if err := parse.ValidateMeta(m); err != nil {
		t.Fatal(err)
	}
	if m.Name != "ios.whatsapp.contacts" || m.Version != "1.0.1" || len(m.Claims) != 1 || m.Claims[0].Table != "ZWAADDRESSBOOKCONTACT" || m.Claims[0].Role != "db" {
		t.Errorf("identity or claims: %+v", m)
	}
	if len(m.Platforms) != 1 || m.Platforms[0] != parse.PlatformIOS || len(m.Emits) != 1 || m.Emits[0].Type != "contact" {
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
	if got, want := m.Inputs[0].Globs, []string{
		"ios:AppDomainGroup-group.net.whatsapp.WhatsApp.shared/ContactsV2.sqlite",
		"ios:AppDomainGroup-group.net.whatsapp.WhatsAppSMB.shared/ContactsV2.sqlite",
		"ios:HomeDomain/Containers/Shared/AppGroup/*/ContactsV2.sqlite",
	}; len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Errorf("db globs %v, want exactly %v", got, want)
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
		"ios:" + domainConsumer + "/ContactsV2.sqlite", "ios:" + domainBiz + "/ContactsV2.sqlite",
		grp + appGroupID + "/ContactsV2.sqlite",
	} {
		if !matches(p) {
			t.Errorf("%s is not matched", p)
		}
	}
	for _, p := range []string{
		grp + "X/Y/ContactsV2.sqlite", "ios:" + domainConsumer + "/sub/ContactsV2.sqlite",
		"ios:AppDomain-net.whatsapp.WhatsApp/Documents/ContactsV2.sqlite", "ios:MediaDomain/ContactsV2.sqlite",
		"ios:HomeDomain/Library/ContactsV2.sqlite", "ios:" + domainConsumer + "/ChatStorage.sqlite",
		"ios:" + domainConsumer + "/CallHistory.sqlite",
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
	if !mg.MatchRelative(grp+appGroupID+"/ContactsV2.sqlite", grp+appGroupID+"/"+metaName) {
		t.Error("the metadata plist beside an app-group primary is not matched")
	}
	if mg.MatchRelative(grp+appGroupID+"/ContactsV2.sqlite", grp+appGroupID+"/x/"+metaName) {
		t.Error("a metadata plist in a subdirectory is matched")
	}
}

func TestIOSWhatsAppContactsCompanionIsNeverAPrimary(t *testing.T) { // 4I-R21
	m := ioswacontacts.Parser{}.Meta()
	if len(m.Inputs) == 0 {
		t.Fatal("no inputs")
	}
	for _, dir := range []string{"ios:" + domainConsumer, "ios:" + domainBiz, "ios:HomeDomain/Containers/Shared/AppGroup/" + appGroupID} {
		for _, suffix := range []string{"-wal", "-journal", "-shm"} {
			for _, s := range m.Inputs[0].Globs {
				g, err := parse.CompileGlob(s)
				if err != nil {
					t.Fatal(err)
				}
				if g.Match(dir + "/ContactsV2.sqlite" + suffix) {
					t.Errorf("%s matches %s/ContactsV2.sqlite%s as a primary", s, dir, suffix)
				}
			}
		}
	}
}

func TestIOSWhatsAppContactsProbeByPlace(t *testing.T) { // 4I2-R2, PF3, PF9
	for _, v := range []string{"ios-contactsv2", "ios-contactsv2-nolid", "ios-contactsv2-nofullname", "ios-contactsv2-extract", "ios-contactsv2-deleted"} {
		if got := probeVariant(t, v); got.Status != parse.Applicable {
			t.Errorf("%s: %+v", v, got)
		}
	}
	if got := probeVariant(t, "ios-contactsv2-otherapp"); got.Status != parse.NotApplicable {
		t.Errorf("ios-contactsv2-otherapp: %+v", got)
	}
	other := parsertest.LoadFixture(t, "testdata", "ios-contactsv2-otherapp").Files
	got := probeDomain(t, other)
	if got.Status != parse.UnsupportedSchema || got.Reason != "no WhatsApp ContactsV2 tables at the WhatsApp app group path" {
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
	// the Business domain names the app as well
	if got, err := probeAt(t, map[string][]byte{"db": goStore()}, domainBiz, dbName, ""); err != nil || got.Status != parse.Applicable {
		t.Errorf("business domain: %+v, %v", got, err)
	}
	if got, err := probeAt(t, text, domainBiz, dbName, ""); err != nil || got.Status != parse.UnsupportedSchema {
		t.Errorf("text at the business domain: %+v, %v", got, err)
	}
	// the identifying table present makes the rest required, at an app group and at a domain: the JID column is part of the
	// signature (spec 6.11), and so is the rowid key
	noJID, _ := buildContacts([]string{"ZWHATSAPPID"}, func(put putRow) { put(1, contactRow(map[string]any{"ZFULLNAME": "x"})) })
	got, err = probeGroup(t, map[string][]byte{"db": noJID})
	if err != nil || got.Status != parse.UnsupportedSchema || !strings.Contains(got.Reason, "ZWHATSAPPID") {
		t.Errorf("no ZWHATSAPPID at an app group: %+v, %v", got, err)
	}
	if got := probeDomain(t, map[string][]byte{"db": noJID}); got.Status != parse.UnsupportedSchema || !strings.Contains(got.Reason, "ZWHATSAPPID") {
		t.Errorf("no ZWHATSAPPID at a domain: %+v", got)
	}
	// every other column is optional: its absence is stated per row, never a finding
	for _, col := range []string{"ZFULLNAME", "ZPHONENUMBER", "ZGIVENNAME", "ZHIGHLIGHTEDNAME", "ZLID", "ZABOUTTEXT", "ZIDENTIFIER"} {
		db, _ := buildContacts([]string{col}, func(put putRow) { put(1, contactRow(map[string]any{"ZWHATSAPPID": "1@s.whatsapp.net"})) })
		if got, err := probeGroup(t, map[string][]byte{"db": db}); err != nil || got.Status != parse.Applicable {
			t.Errorf("no %s: %+v, %v", col, got, err)
		}
	}
	for _, place := range []string{"domain", "app group"} {
		var got parse.Applicability
		if place == "domain" {
			got = probeDomain(t, map[string][]byte{"db": goStore()})
		} else {
			got, err = probeGroup(t, map[string][]byte{"db": goStore()})
			if err != nil {
				t.Fatal(err)
			}
		}
		if got.Status != parse.Applicable {
			t.Errorf("a Go-built store at %s: %+v", place, got)
		}
	}
	// a companion defers every schema finding to Parse (the main file proves nothing then)
	if got := probeDomain(t, map[string][]byte{"db": noJID, "wal": {}}); got.Status != parse.Applicable {
		t.Errorf("no ZWHATSAPPID with an empty -wal: %+v, want applicable (deferred)", got)
	}
}

func TestParseRefusesWhatNoGlobMatches(t *testing.T) {
	h := parsertest.NewHarness(t)
	b := parsertest.Fixture{Name: "x", Files: map[string][]byte{"db": goStore()}}.BundleIOS(h, "/private/var/mobile/Library/ContactsV2.sqlite", "ios:HomeDomain/Library/ContactsV2.sqlite")
	p := ioswacontacts.Parser{}
	if res := runLenient(t, h, p, b.Input(h, p, false)); !errors.Is(res.Err, ioswacontacts.ErrNotWhatsAppPath) || len(res.Records) != 0 {
		t.Errorf("err %v", res.Err)
	}
	got, err := p.Probe(t.Context(), b.Input(h, p, true))
	if err != nil || got.Status != parse.UnsupportedSchema || got.Reason != "not a WhatsApp app group path" {
		t.Errorf("Probe: %+v, %v", got, err)
	}
	// a domain database without the identifying table, under a companion, is refused at Parse
	h2 := parsertest.NewHarness(t)
	files := map[string][]byte{"db": parsertest.LoadFixture(t, "testdata", "ios-contactsv2-otherapp").Files["db"], "wal": {}}
	b2 := parsertest.Fixture{Name: "x", Files: files}.BundleIOSBackup(h2, domainConsumer, dbName)
	if res := runLenient(t, h2, p, b2.Input(h2, p, false)); !errors.Is(res.Err, ioswacontacts.ErrNoWhatsAppTables) || len(res.Records) != 0 {
		t.Errorf("domain without the identifying table: %v", res.Err)
	}
	// the same file at an app group is benign: no error, no record
	h3 := parsertest.NewHarness(t)
	b3 := parsertest.LoadFixture(t, "testdata", "ios-contactsv2-otherapp").BundleIOS(h3, extractRemote, extractLogical)
	if res := runLenient(t, h3, p, b3.Input(h3, p, false), "not applicable at parse time: table ZWAADDRESSBOOKCONTACT is absent"); res.Err != nil || len(res.Records) != 0 {
		t.Errorf("another app's database at an app group: err %v, %d records", res.Err, len(res.Records))
	}
}
