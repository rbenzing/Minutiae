package ioswacontacts_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/ioswacontacts"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// parsedVariants are the variants Parse accepts: the typed pass maps their rows. The other one (otherapp) is refused by Parse,
// and a refused database is never typed-recovered.
var parsedVariants = []string{"ios-contactsv2", "ios-contactsv2-nolid", "ios-contactsv2-nofullname", "ios-contactsv2-extract", "ios-contactsv2-deleted"}

// recoverVariants hold history in ZWAADDRESSBOOKCONTACT and have a recover oracle (PF2): the deleted variant, whose oracle was
// pinned by Task 9 and is run, not regenerated, here. A variant with nothing deleted has none and is checked against the empty
// set.
var recoverVariants = []string{"ios-contactsv2-deleted"}

// 4I2-R10: the claims are exactly the claimable tables, which are ZWAADDRESSBOOKCONTACT only: the mapping reads no other table.
func TestClaimsEqualClaimableTables(t *testing.T) {
	p := ioswacontacts.Parser{}
	var got []string
	for _, c := range p.Meta().Claims {
		if c.Role != "db" {
			t.Errorf("claim %+v is not on the db role", c)
		}
		got = append(got, c.Table)
	}
	if want := ioswacontacts.ClaimableTables(); !slices.Equal(got, want) || !slices.Equal(got, []string{"ZWAADDRESSBOOKCONTACT"}) {
		t.Errorf("Meta().Claims = %v, want exactly ClaimableTables() = %v = [ZWAADDRESSBOOKCONTACT]", got, want)
	}
	if bad := parse.ClaimsOutsideMappings(p.Meta(), p.Mappings()); len(bad) != 0 {
		t.Errorf("claims outside the mappings: %v", bad)
	}
}

// Spec 14, D8 with PF13: every recovered ZWAADDRESSBOOKCONTACT row is a typed record or a counted unmapped row, never a silent
// drop, checked against an oracle computed by an independent page walker from the committed files (a lower bound). A variant
// where nothing was deleted has no oracle file: it is checked against the empty set, which is a real claim (a typed record there
// would be invented).
func TestClaimedTablesMapRecoveredRows(t *testing.T) {
	for _, v := range parsedVariants {
		t.Run(v, func(t *testing.T) {
			h := parsertest.NewHarness(t)
			b := bundleOf(h, parsertest.LoadFixture(t, "testdata", v), v)
			want := parsertest.RecoverExpect{Variant: v, Claimed: ioswacontacts.ClaimableTables()}
			if slices.Contains(recoverVariants, v) {
				want = parsertest.LoadRecoverExpect(t, "testdata", v)
			}
			parsertest.AssertClaimedTablesMapRecoveredRows(t, h, ioswacontacts.Parser{}, b, want)
		})
	}
}

// The oracle is not vacuous: it lists what the scenario was built to leave behind (rows a WAL transaction deleted), and every
// want has a digest.
func TestRecoverOracleShape(t *testing.T) {
	for _, v := range recoverVariants {
		x := parsertest.LoadRecoverExpect(t, "testdata", v)
		if len(x.Records) < 5 || len(x.Digests) < len(x.Records) {
			t.Errorf("%s lists %d records and %d digests: the check proves nothing", v, len(x.Records), len(x.Digests))
		}
		inWAL := 0
		for _, r := range x.Records {
			if r.Table != "ZWAADDRESSBOOKCONTACT" || r.Record.Type != "contact" {
				t.Fatalf("%s lists %+v: a typed record of another table or type", v, r)
			}
			if r.Method == "sqlite-wal-prior" {
				inWAL++
			}
		}
		if inWAL == 0 {
			t.Errorf("%s lists no version the WAL deleted", v)
		}
	}
}

// PF5 and 4I2-P8: a recovered row has no Parse context. At a backup domain its place names the app; at an app group the
// metadata plist is never evaluated for it, so it states app unknown and app_state not-evaluated, never a guessed label. The
// deleted fixture bundled at an app group is the same database, so only the place differs.
func TestIOSWhatsAppContactsRecoveredAppGroupRowIsNotEvaluated(t *testing.T) {
	fx := parsertest.LoadFixture(t, "testdata", "ios-contactsv2-deleted")
	p := ioswacontacts.Parser{}
	for _, c := range []struct {
		name, app, basis, state string
		bundle                  func(h *parsertest.Harness) parsertest.Bundle
	}{
		{"domain", "whatsapp", "backup_domain", "", func(h *parsertest.Harness) parsertest.Bundle { return bundleOf(h, fx, "ios-contactsv2-deleted") }},
		{"app group", "unknown", "", "not-evaluated", func(h *parsertest.Harness) parsertest.Bundle {
			return parsertest.Fixture{Name: "group", Files: fx.Files}.BundleIOS(h, extractRemote, extractLogical)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := parsertest.NewHarness(t)
			b := c.bundle(h)
			rr, ok := parsertest.FirstClaimedRow(t, h, p, b, "ZWAADDRESSBOOKCONTACT", func(r sqlitefile.RecoveredRow) bool { return r.Rowid != nil && *r.Rowid == 90 })
			if !ok {
				t.Fatal("recovered contact 90 not found: the check proves nothing")
			}
			res := parsertest.MapRecoveredRow(t, h, p, b, rr)
			if res.Err != nil || res.Unmapped != "" || len(res.Records) != 1 {
				t.Fatalf("err %v, unmapped %q, %d records", res.Err, res.Unmapped, len(res.Records))
			}
			raw := rawOf(res.Records[0])
			if raw["app"] != c.app || raw["app_basis"] != orAbsent(c.basis) || raw["app_state"] != orAbsent(c.state) {
				t.Errorf("app %v basis %v state %v, want %q %q %q (\"\" = absent)", raw["app"], raw["app_basis"], raw["app_state"], c.app, c.basis, c.state)
			}
			// the row maps as a live one does: the wrapper decoded, the row's own ZLID, no time and no live-ness claim
			if raw["phone_label_stored"] != "_$!<Home>!$_" || raw["lid_map"] == nil || res.Records[0].Time != nil {
				t.Errorf("recovered contact 90: %v", res.Records[0].Payload)
			}
		})
	}
}

// orAbsent is the value a missing map key reads as for want: nil for "".
func orAbsent(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// A recovered row that names no person is reported unmapped with its reason (row 93 is a deleted group row), never typed.
func TestIOSWhatsAppContactsRecoveredNonPersonRowIsUnmapped(t *testing.T) {
	h := parsertest.NewHarness(t)
	b := bundleOf(h, parsertest.LoadFixture(t, "testdata", "ios-contactsv2-deleted"), "ios-contactsv2-deleted")
	p := ioswacontacts.Parser{}
	rr, ok := parsertest.FirstClaimedRow(t, h, p, b, "ZWAADDRESSBOOKCONTACT", func(r sqlitefile.RecoveredRow) bool { return r.Rowid != nil && *r.Rowid == 93 })
	if !ok {
		t.Fatal("recovered contact 93 not found")
	}
	res := parsertest.MapRecoveredRow(t, h, p, b, rr)
	if len(res.Records) != 0 || !strings.Contains(res.Unmapped, "ZWAADDRESSBOOKCONTACT row is not a person (group)") {
		t.Errorf("unmapped %q, %d records", res.Unmapped, len(res.Records))
	}
}
