package parsers

import (
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/chromium"
	"github.com/rbenzing/minutiae/internal/parsers/iosaddressbook"
	"github.com/rbenzing/minutiae/internal/parsers/ioscalls"
	"github.com/rbenzing/minutiae/internal/parsers/iossms"
	"github.com/rbenzing/minutiae/internal/parsers/safari"
	"github.com/rbenzing/minutiae/internal/recordtypes/call"
	"github.com/rbenzing/minutiae/internal/recordtypes/contact"
	"github.com/rbenzing/minutiae/internal/recordtypes/message"
	"github.com/rbenzing/minutiae/internal/recordtypes/web"
)

// claimsOf lists, for every registered parser, the tables it must claim (4G-R30): an explicit table, so a
// claim is never added or dropped by accident and a registered parser missing from the table fails. A parser
// without claims is a failure (4H-R7): every first-set parser claims its claimable tables.
var claimsOf = map[string][]string{
	"android.mmssms":   {"sms", "pdu"},
	"android.calllog":  {"calls"},
	"android.contacts": {"raw_contacts", "deleted_contacts"},
	"chromium.history": {"visits", "keyword_search_terms", "downloads"},
	"safari.history":   {"history_visits"},
	"ios.calls":        {"ZCALLRECORD", "call"},
	"ios.addressbook":  {"ABPerson"},
	"ios.sms":          {"message"},
}

// 4A's registry tests ranged over an empty registry; with the Android telephony parsers in it they are real
// checks. This pins what 4D and 4G promise about every one of them: each is a valid RowMapper, and each
// declares exactly the claims listed above, all of them tables it maps.
func TestRegisteredParsersClaimExactly(t *testing.T) {
	if len(All()) == 0 {
		t.Fatal("the registry is empty, so this test would check nothing")
	}
	seen := map[string]bool{}
	for _, p := range All() {
		m := p.Meta()
		seen[m.Name] = true
		if err := parse.ValidateMeta(m); err != nil {
			t.Errorf("%s: %v", m.Name, err)
		}
		rm, ok := p.(parse.RowMapper)
		if !ok {
			t.Errorf("%s is not a RowMapper: the typed recovery pass (4G) needs the mappings", m.Name)
			continue
		}
		want, listed := claimsOf[m.Name]
		if !listed {
			t.Errorf("%s is registered but missing from the claims table of this test: list it with its claims (nil for none)", m.Name)
			continue
		}
		if len(want) == 0 {
			t.Errorf("%s claims nothing: every first-set SQLite parser claims its claimable tables (plan 4H)", m.Name)
		}
		var got []string
		for _, c := range m.Claims {
			if c.Role != "db" {
				t.Errorf("%s: claim %+v is not on the db role", m.Name, c)
			}
			got = append(got, c.Table)
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s declares claims %v, want exactly %v", m.Name, got, want)
		}
		if len(rm.Mappings()) == 0 {
			t.Errorf("%s has no mappings", m.Name)
		}
		if bad := parse.ClaimsOutsideMappings(m, rm.Mappings()); len(bad) != 0 {
			t.Errorf("%s: claims outside the mappings %v", m.Name, bad)
		}
	}
	for name := range claimsOf {
		if !seen[name] {
			t.Errorf("%s is in the claims table but not registered", name)
		}
	}
}

// TestEveryFirstSetParserClaimsItsClaimableTables (PASSES ON ARRIVAL; mutation-checked, 4H-R7): for the five
// parsers that export ClaimableTables, Meta().Claims equals it in order, on the db role. The three Android
// parsers are held by claimsOf alone.
func TestEveryFirstSetParserClaimsItsClaimableTables(t *testing.T) {
	claimable := map[string]func() []string{
		"chromium.history": chromium.ClaimableTables,
		"safari.history":   safari.ClaimableTables,
		"ios.calls":        ioscalls.ClaimableTables,
		"ios.addressbook":  iosaddressbook.ClaimableTables,
		"ios.sms":          iossms.ClaimableTables,
	}
	for name, fn := range claimable {
		var found parse.Parser
		for _, p := range All() {
			if p.Meta().Name == name {
				found = p
			}
		}
		if found == nil {
			t.Errorf("%s is not registered", name)
			continue
		}
		want := fn()
		if len(want) == 0 {
			t.Errorf("%s: ClaimableTables is empty", name)
		}
		var got []string
		for _, c := range found.Meta().Claims {
			if c.Role != "db" {
				t.Errorf("%s: claim %+v is not on the db role", name, c)
			}
			got = append(got, c.Table)
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s declares claims %v, want ClaimableTables %v", name, got, want)
		}
	}
}

func TestEveryTelephonyParserReadsCompanionRolesAndEmitsOneType(t *testing.T) {
	want := map[string]string{"android.mmssms": "message", "android.calllog": "call", "android.contacts": "contact", "ios.calls": "call", "ios.addressbook": "contact", "ios.sms": "message"}
	for _, p := range All() {
		m := p.Meta()
		typ, ok := want[m.Name]
		if !ok {
			continue
		}
		roles := []string{}
		for _, in := range m.Inputs {
			roles = append(roles, in.Role)
		}
		wantRoles := []string{"db", "wal", "journal"}
		if m.Name == "ios.addressbook" {
			wantRoles = append(wantRoles, "images", "images_wal") // the photo-presence database and its WAL (4E-R25)
		}
		if !slices.Equal(roles, wantRoles) || len(m.Emits) != 1 || m.Emits[0].Type != typ {
			t.Errorf("%s: roles %v emits %v, want %v and %s", m.Name, roles, m.Emits, wantRoles, typ)
		}
		delete(want, m.Name)
	}
	if len(want) != 0 {
		t.Errorf("parsers missing from the registry: %v", want)
	}
}

// TestBrowserParsersReadCompanionRolesAndEmitTheirTypes pins what 4F promises about the two browser
// parsers: the roles they read, the exact types and payload versions they emit, and their place in the
// registry (4F-R19: after the Android telephony parsers, Chromium before Safari). Mutation-checked
// (4F-R33): it passes on arrival, and breaking an expected list below makes it fail.
func TestBrowserParsersReadCompanionRolesAndEmitTheirTypes(t *testing.T) {
	want := []struct {
		name  string
		types []string
	}{
		{"chromium.history", []string{"web_visit", "web_search", "download"}},
		{"safari.history", []string{"web_visit"}},
	}
	all := All()
	last := -1
	for _, w := range want {
		idx := slices.IndexFunc(all, func(p parse.Parser) bool { return p.Meta().Name == w.name })
		if idx < 0 {
			t.Errorf("%s is missing from the registry", w.name)
			continue
		}
		if idx < 3 {
			t.Errorf("%s is at %d, want it after the three Android telephony parsers", w.name, idx)
		}
		if idx <= last {
			t.Errorf("%s is at %d, want it after the previous browser parser (registry order)", w.name, idx)
		}
		if idx < 3 {
			t.Errorf("%s is at %d, want it after the three Android telephony parsers", w.name, idx)
		}
		last = idx
		m := all[idx].Meta()
		roles := []string{}
		for _, in := range m.Inputs {
			roles = append(roles, in.Role)
		}
		if !slices.Equal(roles, []string{"db", "wal", "journal"}) {
			t.Errorf("%s: roles %v, want db/wal/journal", w.name, roles)
		}
		types := []string{}
		for _, e := range m.Emits {
			types = append(types, e.Type)
			if e.PayloadVersion != web.PayloadVersion {
				t.Errorf("%s: emits %s at payload version %d, want %d", w.name, e.Type, e.PayloadVersion, web.PayloadVersion)
			}
		}
		if !slices.Equal(types, w.types) {
			t.Errorf("%s: emits %v, want exactly %v", w.name, types, w.types)
		}
	}
}

// iosWant is what 4E promises about each iOS parser: its roles, the one type it emits (with that type's payload
// version) and that it is registered in this order, after the Android telephony parsers and before the browser parsers (Android, then iOS, then browsers, 4E-R27).
var iosWant = []struct {
	name, typ string
	version   int
	roles     []string
}{
	{"ios.sms", message.Type, message.PayloadVersion, []string{"db", "wal", "journal"}},
	{"ios.calls", call.Type, call.PayloadVersion, []string{"db", "wal", "journal"}},
	{"ios.addressbook", contact.Type, contact.PayloadVersion, []string{"db", "wal", "journal", "images", "images_wal"}},
}

// TestIOSParsersClaimTheirTablesAndAreRowMappers (mutation-checked, 4E-R44): the three iOS parsers
// are valid RowMappers with mappings, declare exactly the claims of the claimsOf table and run on iOS only.
func TestIOSParsersClaimTheirTablesAndAreRowMappers(t *testing.T) {
	seen := 0
	for _, w := range iosWant {
		var p parse.Parser
		for _, q := range All() {
			if q.Meta().Name == w.name {
				p = q
			}
		}
		if p == nil {
			t.Errorf("%s is not registered", w.name)
			continue
		}
		seen++
		m := p.Meta()
		var got []string
		for _, c := range m.Claims {
			got = append(got, c.Table)
		}
		if !slices.Equal(got, claimsOf[w.name]) {
			t.Errorf("%s declares claims %v, want exactly %v (claimsOf)", w.name, got, claimsOf[w.name])
		}
		if !slices.Equal(m.Platforms, []string{parse.PlatformIOS}) {
			t.Errorf("%s: platforms %v, want ios only", w.name, m.Platforms)
		}
		rm, ok := p.(parse.RowMapper)
		if !ok || len(rm.Mappings()) == 0 {
			t.Errorf("%s is not a RowMapper with mappings", w.name)
		}
	}
	if seen != len(iosWant) {
		t.Errorf("found %d of %d iOS parsers", seen, len(iosWant))
	}
}

// TestEveryIOSParserReadsCompanionRolesAndEmitsOneType (PASSES ON ARRIVAL; mutation-checked, 4E-R44) is the
// registry-wide check of 4D for the three iOS parsers: exact roles (the write-ahead log and rollback journal
// companions are read), exactly one emitted type at the current payload version, and the registry order
// ios.sms, ios.calls, ios.addressbook after the Android telephony parsers.
func TestEveryIOSParserReadsCompanionRolesAndEmitsOneType(t *testing.T) {
	all := All()
	last := -1
	for _, w := range iosWant {
		idx := slices.IndexFunc(all, func(p parse.Parser) bool { return p.Meta().Name == w.name })
		if idx < 0 {
			t.Errorf("%s is missing from the registry", w.name)
			continue
		}
		if idx <= last {
			t.Errorf("%s is at %d, want it after the previous iOS parser (registry order)", w.name, idx)
		}
		last = idx
		m := all[idx].Meta()
		roles := []string{}
		for _, in := range m.Inputs {
			roles = append(roles, in.Role)
		}
		if !slices.Equal(roles, w.roles) {
			t.Errorf("%s: roles %v, want %v", w.name, roles, w.roles)
		}
		if len(m.Emits) != 1 || m.Emits[0].Type != w.typ || m.Emits[0].PayloadVersion != w.version {
			t.Errorf("%s emits %+v, want exactly %s at payload version %d", w.name, m.Emits, w.typ, w.version)
		}
	}
}
