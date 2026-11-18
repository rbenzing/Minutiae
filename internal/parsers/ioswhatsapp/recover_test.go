package ioswhatsapp_test

import (
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/ioswhatsapp"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// parsedVariants are the variants Parse accepts: the typed pass maps their rows. The other three (nomodel, othermodel,
// otherapp) are refused by Parse, and a refused database is never typed-recovered.
var parsedVariants = []string{
	"ios-chatstorage", "ios-chatstorage-business", "ios-chatstorage-extract", "ios-chatstorage-renumbered",
	"ios-chatstorage-nooptional", "ios-chatstorage-deleted", "ios-chatstorage-deleted-freelist",
}

// recoverVariants hold history in ZWAMESSAGE and have a recover oracle (PF2).
var recoverVariants = []string{"ios-chatstorage-deleted", "ios-chatstorage-deleted-freelist"}

// 4I2-R10: the claims are exactly the claimable tables, which are ZWAMESSAGE only: ZWAMEDIAITEM and ZWACHATSESSION are
// joins of its mapping (a recovered media row alone is no message), and the other tables are not mapped.
func TestClaimsEqualClaimableTables(t *testing.T) {
	p := ioswhatsapp.Parser{}
	var got []string
	for _, c := range p.Meta().Claims {
		if c.Role != "db" {
			t.Errorf("claim %+v is not on the db role", c)
		}
		got = append(got, c.Table)
	}
	if want := ioswhatsapp.ClaimableTables(); !slices.Equal(got, want) || !slices.Equal(got, []string{"ZWAMESSAGE"}) {
		t.Errorf("Meta().Claims = %v, want exactly ClaimableTables() = %v = [ZWAMESSAGE]", got, want)
	}
	if bad := parse.ClaimsOutsideMappings(p.Meta(), p.Mappings()); len(bad) != 0 {
		t.Errorf("claims outside the mappings: %v", bad)
	}
}

// Spec 14, D8 with PF13: every recovered ZWAMESSAGE row is a typed record, never a silent drop, checked against an oracle
// computed by an independent page walker from the committed files. A variant where nothing was deleted has no oracle file:
// it is checked against the empty set, which is a real claim (a typed record there would be invented).
func TestClaimedTablesMapRecoveredRows(t *testing.T) {
	for _, v := range parsedVariants {
		t.Run(v, func(t *testing.T) {
			h := parsertest.NewHarness(t)
			b := bundleOf(h, parsertest.LoadFixture(t, "testdata", v), v)
			want := parsertest.RecoverExpect{Variant: v, Claimed: ioswhatsapp.ClaimableTables()}
			if slices.Contains(recoverVariants, v) {
				want = parsertest.LoadRecoverExpect(t, "testdata", v)
			}
			parsertest.AssertClaimedTablesMapRecoveredRows(t, h, ioswhatsapp.Parser{}, b, want)
		})
	}
}

// The oracles are not vacuous: they list what the scenarios were built to leave behind (a WAL frame that deleted rows, and
// freed pages with no WAL), and every want has a digest.
func TestRecoverOracleShape(t *testing.T) {
	for _, v := range recoverVariants {
		x := parsertest.LoadRecoverExpect(t, "testdata", v)
		if len(x.Records) < 5 || len(x.Digests) < len(x.Records) {
			t.Errorf("%s lists %d records and %d digests: the check proves nothing", v, len(x.Records), len(x.Digests))
		}
		for _, r := range x.Records {
			if r.Table != "ZWAMESSAGE" || r.Record.Type == "" {
				t.Fatalf("%s lists %+v: a typed record of another table or with no type", v, r)
			}
		}
	}
	wal := parsertest.LoadRecoverExpect(t, "testdata", "ios-chatstorage-deleted")
	var inWAL int
	for _, r := range wal.Records {
		if r.Method == "sqlite-wal-prior" {
			inWAL++
		}
	}
	if inWAL == 0 {
		t.Errorf("ios-chatstorage-deleted lists no version the WAL deleted")
	}
	fl := parsertest.LoadRecoverExpect(t, "testdata", "ios-chatstorage-deleted-freelist")
	for _, r := range fl.Records {
		if r.Method == "sqlite-wal-prior" {
			t.Fatalf("ios-chatstorage-deleted-freelist has no WAL, and lists %+v", r)
		}
	}
}

// 4I2-P8 and PF5: a recovered row has no Parse context. At a backup domain its place names the app; at an app group the
// metadata plist is never evaluated for it, so it states app unknown and app_state not-evaluated, never a guessed label.
// The deleted fixture bundled at an app group is the same database, so only the place differs.
func TestIOSWhatsAppChatsRecoveredAppGroupRowIsNotEvaluated(t *testing.T) {
	fx := parsertest.LoadFixture(t, "testdata", "ios-chatstorage-deleted")
	p := ioswhatsapp.Parser{}
	for _, c := range []struct {
		name, app, basis, state string
		bundle                  func(h *parsertest.Harness) parsertest.Bundle
	}{
		{"domain", "whatsapp", "backup_domain", "", func(h *parsertest.Harness) parsertest.Bundle { return bundleOf(h, fx, "ios-chatstorage-deleted") }},
		{"app group", "unknown", "", "not-evaluated", func(h *parsertest.Harness) parsertest.Bundle {
			return parsertest.Fixture{Name: "group", Files: fx.Files}.BundleIOS(h, extractRemote, extractLogical)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := parsertest.NewHarness(t)
			b := c.bundle(h)
			rr, ok := parsertest.FirstClaimedRow(t, h, p, b, "ZWAMESSAGE", nil)
			if !ok {
				t.Fatal("no recovered ZWAMESSAGE row: the check proves nothing")
			}
			res := parsertest.MapRecoveredRow(t, h, p, b, rr)
			if res.Err != nil || res.Unmapped != "" || len(res.Records) != 1 {
				t.Fatalf("err %v, unmapped %q, %d records", res.Err, res.Unmapped, len(res.Records))
			}
			raw := rawOf(res.Records[0])
			if raw["app"] != c.app || raw["app_basis"] != orAbsent(c.basis) || raw["app_state"] != orAbsent(c.state) {
				t.Errorf("app %v basis %v state %v, want %q %q %q (\"\" = absent)", raw["app"], raw["app_basis"], raw["app_state"], c.app, c.basis, c.state)
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
