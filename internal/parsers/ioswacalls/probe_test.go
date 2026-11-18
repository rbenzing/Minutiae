package ioswacalls_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/ioswacalls"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

const (
	metaName   = ".com.apple.mobile_container_manager.metadata.plist"
	appGroupID = "5A0B4C3D-0000-4000-8000-00000000A001"
	domainBiz  = "AppDomainGroup-group.net.whatsapp.WhatsAppSMB.shared"
)

// goStore is a minimal Go-built CallHistory: both tables and one call on one aggregate. The call's text mentions WhatsAppSMB on
// purpose: the database's own text never labels the app.
func goStore() []byte {
	db, _ := buildCalls(nil, func(tb callTables) {
		tb["ZWAAGGREGATECALLEVENT"].put(1, aggRow(int64(1), nil))
		tb["ZWACDCALLEVENT"].put(1, callRow(map[string]any{"Z1CALLEVENTS": int64(1), "ZCALLIDSTRING": "WhatsAppSMB call", "ZGROUPJIDSTRING": "x@WhatsAppSMB"}))
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
	p := ioswacalls.Parser{}
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
	p := ioswacalls.Parser{}
	got, err := p.Probe(t.Context(), b.Input(h, p, true))
	if err != nil {
		t.Fatalf("%s: %v", v, err)
	}
	return got
}

func TestIOSWhatsAppCallsGlobsCompileAndMatch(t *testing.T) { // 4I2-R2
	m := ioswacalls.Parser{}.Meta()
	if err := parse.ValidateMeta(m); err != nil {
		t.Fatal(err)
	}
	if m.Name != "ios.whatsapp.calls" || m.Version != "1.0.0" || len(m.Claims) != 1 || m.Claims[0].Table != "ZWACDCALLEVENT" || m.Claims[0].Role != "db" {
		t.Errorf("identity or claims: %+v", m)
	}
	if len(m.Platforms) != 1 || m.Platforms[0] != parse.PlatformIOS || len(m.Emits) != 1 || m.Emits[0].Type != "call" {
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
		"ios:AppDomainGroup-group.net.whatsapp.WhatsApp.shared/CallHistory.sqlite",
		"ios:AppDomainGroup-group.net.whatsapp.WhatsAppSMB.shared/CallHistory.sqlite",
		"ios:HomeDomain/Containers/Shared/AppGroup/*/CallHistory.sqlite",
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
		"ios:" + domainConsumer + "/CallHistory.sqlite", "ios:" + domainBiz + "/CallHistory.sqlite",
		grp + appGroupID + "/CallHistory.sqlite",
	} {
		if !matches(p) {
			t.Errorf("%s is not matched", p)
		}
	}
	for _, p := range []string{
		grp + "X/Y/CallHistory.sqlite", "ios:" + domainConsumer + "/sub/CallHistory.sqlite",
		"ios:AppDomain-net.whatsapp.WhatsApp/Documents/CallHistory.sqlite", "ios:MediaDomain/CallHistory.sqlite",
		"ios:HomeDomain/Library/CallHistory.sqlite", "ios:" + domainConsumer + "/ChatStorage.sqlite",
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
	if !mg.MatchRelative(grp+appGroupID+"/CallHistory.sqlite", grp+appGroupID+"/"+metaName) {
		t.Error("the metadata plist beside an app-group primary is not matched")
	}
	if mg.MatchRelative(grp+appGroupID+"/CallHistory.sqlite", grp+appGroupID+"/x/"+metaName) {
		t.Error("a metadata plist in a subdirectory is matched")
	}
}

func TestIOSWhatsAppCallsCompanionIsNeverAPrimary(t *testing.T) { // 4I-R21
	m := ioswacalls.Parser{}.Meta()
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
				if g.Match(dir + "/CallHistory.sqlite" + suffix) {
					t.Errorf("%s matches %s/CallHistory.sqlite%s as a primary", s, dir, suffix)
				}
			}
		}
	}
}

func TestIOSWhatsAppCallsProbeByPlace(t *testing.T) { // 4I2-R2, PF3, PF9
	if got := probeVariant(t, "ios-callhistory"); got.Status != parse.Applicable {
		t.Errorf("ios-callhistory: %+v", got)
	}
	if got := probeVariant(t, "ios-callhistory-extract"); got.Status != parse.Applicable {
		t.Errorf("ios-callhistory-extract: %+v", got)
	}
	if got := probeVariant(t, "ios-callhistory-otherapp"); got.Status != parse.NotApplicable {
		t.Errorf("ios-callhistory-otherapp: %+v", got)
	}
	other := parsertest.LoadFixture(t, "testdata", "ios-callhistory-otherapp").Files
	got := probeDomain(t, other)
	if got.Status != parse.UnsupportedSchema || got.Reason != "no WhatsApp CallHistory tables at the WhatsApp app group path" {
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
	// the identifying table present makes the rest required, at an app group and at a domain: the aggregate table is part of the
	// signature (PF3), and so are the columns of the call table
	noAgg, _ := buildCalls(map[string]string{"ZWAAGGREGATECALLEVENT": ""}, func(tb callTables) { tb["ZWACDCALLEVENT"].put(1, callRow(nil)) })
	got, err = probeGroup(t, map[string][]byte{"db": noAgg})
	if err != nil || got.Status != parse.UnsupportedSchema || !strings.Contains(got.Reason, "ZWAAGGREGATECALLEVENT") {
		t.Errorf("no aggregate table at an app group: %+v, %v", got, err)
	}
	if got := probeDomain(t, map[string][]byte{"db": noAgg}); got.Status != parse.UnsupportedSchema || !strings.Contains(got.Reason, "ZWAAGGREGATECALLEVENT") {
		t.Errorf("no aggregate table at a domain: %+v", got)
	}
	noDur, _ := buildCalls(map[string]string{
		"ZWACDCALLEVENT": "CREATE TABLE ZWACDCALLEVENT (Z_PK INTEGER PRIMARY KEY, Z1CALLEVENTS INTEGER, ZOUTCOME INTEGER, ZDATE TIMESTAMP)",
	}, func(tb callTables) { tb["ZWACDCALLEVENT"].t.Insert(1, nil, int64(0), int64(0), goodDate) })
	got, err = probeGroup(t, map[string][]byte{"db": noDur})
	if err != nil || got.Status != parse.UnsupportedSchema || !strings.Contains(got.Reason, "ZDURATION") {
		t.Errorf("no ZDURATION at an app group: %+v, %v", got, err)
	}
	// Z1CALLEVENTS and ZVIDEO are optional: their absence is stated per call, never a finding
	noAggCol, _ := buildCalls(map[string]string{
		"ZWACDCALLEVENT": "CREATE TABLE ZWACDCALLEVENT (Z_PK INTEGER PRIMARY KEY, ZOUTCOME INTEGER, ZDATE TIMESTAMP, ZDURATION FLOAT)",
	}, func(tb callTables) { tb["ZWACDCALLEVENT"].t.Insert(1, int64(0), goodDate, float64(0)) })
	if got, err := probeGroup(t, map[string][]byte{"db": noAggCol}); err != nil || got.Status != parse.Applicable {
		t.Errorf("no Z1CALLEVENTS: %+v, %v", got, err)
	}
	// a table that is not the rowid alias is a finding at Probe (the joins go through the rowid)
	noAlias, _ := buildCalls(map[string]string{
		"ZWAAGGREGATECALLEVENT": "CREATE TABLE ZWAAGGREGATECALLEVENT (Z_PK INTEGER, ZINCOMING INTEGER)",
	}, func(tb callTables) { tb["ZWACDCALLEVENT"].put(1, callRow(nil)) })
	if got, err := probeGroup(t, map[string][]byte{"db": noAlias}); err != nil || got.Status != parse.UnsupportedSchema {
		t.Errorf("aggregate table without a rowid alias: %+v, %v", got, err)
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
	if got := probeDomain(t, map[string][]byte{"db": noAgg, "wal": {}}); got.Status != parse.Applicable {
		t.Errorf("no aggregate table with an empty -wal: %+v, want applicable (deferred)", got)
	}
}

func TestParseRefusesWhatNoGlobMatches(t *testing.T) {
	h := parsertest.NewHarness(t)
	b := parsertest.Fixture{Name: "x", Files: map[string][]byte{"db": goStore()}}.BundleIOS(h, "/private/var/mobile/Library/CallHistory.sqlite", "ios:HomeDomain/Library/CallHistory.sqlite")
	p := ioswacalls.Parser{}
	if res := runLenient(t, h, p, b.Input(h, p, false)); !errors.Is(res.Err, ioswacalls.ErrNotWhatsAppPath) || len(res.Records) != 0 {
		t.Errorf("err %v", res.Err)
	}
	got, err := p.Probe(t.Context(), b.Input(h, p, true))
	if err != nil || got.Status != parse.UnsupportedSchema || got.Reason != "not a WhatsApp app group path" {
		t.Errorf("Probe: %+v, %v", got, err)
	}
	// a domain database without the identifying table, under a companion, is refused at Parse
	h2 := parsertest.NewHarness(t)
	files := map[string][]byte{"db": parsertest.LoadFixture(t, "testdata", "ios-callhistory-otherapp").Files["db"], "wal": {}}
	b2 := parsertest.Fixture{Name: "x", Files: files}.BundleIOSBackup(h2, domainConsumer, dbName)
	if res := runLenient(t, h2, p, b2.Input(h2, p, false)); !errors.Is(res.Err, ioswacalls.ErrNoWhatsAppTables) || len(res.Records) != 0 {
		t.Errorf("domain without the identifying table: %v", res.Err)
	}
	// the same file at an app group is benign: no error, no record
	h3 := parsertest.NewHarness(t)
	b3 := parsertest.LoadFixture(t, "testdata", "ios-callhistory-otherapp").BundleIOS(h3, extractRemote, extractLogical)
	if res := runLenient(t, h3, p, b3.Input(h3, p, false), "not applicable at parse time: table ZWACDCALLEVENT is absent"); res.Err != nil || len(res.Records) != 0 {
		t.Errorf("another app's database at an app group: err %v, %d records", res.Err, len(res.Records))
	}
}
