package androidmms_test

import (
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/androidmms"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

func TestMetaIsValidAndClaimsTheMappedTables(t *testing.T) {
	m := androidmms.Parser{}.Meta()
	if err := parse.ValidateMeta(m); err != nil {
		t.Fatal(err)
	}
	// a claim is a promise of typed recovery (plan 4G): the two tables the mappings map, on the database
	if want := []parse.TableClaim{{Role: "db", Table: "sms"}, {Role: "db", Table: "pdu"}}; !slices.Equal(m.Claims, want) {
		t.Errorf("claims %v, want %v", m.Claims, want)
	}
	if m.Name != androidmms.Name || m.Version != androidmms.Version || len(m.Emits) != 1 || m.Emits[0].Type != "message" {
		t.Errorf("meta %+v", m)
	}
	roles := map[string]bool{}
	for _, in := range m.Inputs {
		roles[in.Role] = true
	}
	if !roles["db"] || !roles["wal"] || !roles["journal"] || m.Inputs[0].Role != "db" {
		t.Errorf("roles %v", roles)
	}
}

func TestProbeAnswersForEveryFixtureAndForAFileThatIsNotADatabase(t *testing.T) {
	p := androidmms.Parser{}
	for _, v := range variants {
		h := parsertest.NewHarness(t)
		b := parsertest.LoadFixture(t, "testdata", v).Bundle(h, remotePath)
		got, err := p.Probe(t.Context(), b.Input(h, p, true))
		if err != nil || got.Status != parse.Applicable {
			t.Errorf("%s: Probe = %+v, %v", v, got, err)
		}
	}
	h := parsertest.NewHarness(t)
	fx := parsertest.Fixture{Name: "text", Files: map[string][]byte{"db": []byte("this is a renamed text file, not a database\n")}}
	b := fx.Bundle(h, remotePath)
	got, err := p.Probe(t.Context(), b.Input(h, p, true))
	if err != nil || got.Status != parse.NotApplicable {
		t.Errorf("text file: Probe = %+v, %v; want not-applicable", got, err)
	}
}
