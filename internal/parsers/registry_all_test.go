package parsers

import (
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/parse"
)

// 4A's registry tests ranged over an empty registry; with the Android telephony parsers in it they are real
// checks. This pins what 4D promises about every one of them.
func TestTelephonyParsersMakeNoClaimsAndAreRowMappers(t *testing.T) {
	if len(All()) == 0 {
		t.Fatal("the registry is empty, so this test would check nothing")
	}
	for _, p := range All() {
		m := p.Meta()
		if err := parse.ValidateMeta(m); err != nil {
			t.Errorf("%s: %v", m.Name, err)
		}
		rm, ok := p.(parse.RowMapper)
		if !ok {
			t.Errorf("%s is not a RowMapper: the typed recovery pass (4G) needs the mappings", m.Name)
			continue
		}
		if len(m.Claims) != 0 {
			t.Errorf("%s declares claims %v before its typed recovery exists", m.Name, m.Claims)
		}
		if len(rm.Mappings()) == 0 {
			t.Errorf("%s has no mappings", m.Name)
		}
		if bad := parse.ClaimsOutsideMappings(m, rm.Mappings()); len(bad) != 0 {
			t.Errorf("%s: claims outside the mappings %v", m.Name, bad)
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
