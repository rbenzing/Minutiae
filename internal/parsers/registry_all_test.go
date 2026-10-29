package parsers

import (
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/recordtypes/web"
)

// claimsOf lists, for every registered parser, the tables it must claim (4G-R30): an explicit table, so a
// claim is never added or dropped by accident and a registered parser missing from the table fails. A parser
// without claims is listed with nil. Tasks 9 and 10 set the three Android telephony parsers, Task 10b the 4F browsers
// (the 4E parsers follow in plan 4G Task 14).
var claimsOf = map[string][]string{
	"android.mmssms":   {"sms", "pdu"},
	"android.calllog":  {"calls"},
	"android.contacts": {"raw_contacts", "deleted_contacts"},
	"chromium.history": {"visits", "keyword_search_terms", "downloads"},
	"safari.history":   {"history_visits"},
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

func TestEveryTelephonyParserReadsCompanionRolesAndEmitsOneType(t *testing.T) {
	want := map[string]string{"android.mmssms": "message", "android.calllog": "call", "android.contacts": "contact"}
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
		if !slices.Equal(roles, []string{"db", "wal", "journal"}) || len(m.Emits) != 1 || m.Emits[0].Type != typ {
			t.Errorf("%s: roles %v emits %v, want db/wal/journal and %s", m.Name, roles, m.Emits, typ)
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
