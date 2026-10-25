package cli

import (
	"testing"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
)

// claimingParser is a parser that claims a table.
type claimingParser struct{ cliParser }

func (p claimingParser) Meta() parse.Meta {
	m := p.cliParser.Meta()
	m.Inputs = []parse.InputSpec{{Role: "db", Globs: m.Inputs[0].Globs, Required: true}}
	m.Claims = []parse.TableClaim{{Role: "db", Table: "t"}}
	return m
}

// A claiming parser is registered with its recovery identity, a parser without claims is not given one,
// and a claiming parser with no generated recovery hash is an error (never a registry without it).
func TestRegisterParsersWiresTheRecoveryIdentity(t *testing.T) {
	plain, claims := good("plain"), claimingParser{good("claims")}
	hashOf := func(p parse.Parser) (string, bool) {
		m := p.Meta()
		return parsertest.FakeHash(m.Name + "@" + m.Version), true
	}
	recoveryOf := func(p parse.Parser) (string, bool) {
		m := p.Meta()
		if len(m.Claims) == 0 {
			return "", false
		}
		return parsertest.FakeHash(m.Name + ".recover@" + m.Version), true
	}
	regs, err := registerParsers([]parse.Parser{plain, claims}, hashOf, recoveryOf)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := regs[0].RecoveryIdentity(); ok {
		t.Error("a parser without claims has a recovery identity")
	}
	rid, ok := regs[1].RecoveryIdentity()
	if !ok || rid.Name != "claims.recover" || rid.Version != "1.0.0" || rid.Hash != parsertest.FakeHash("claims.recover@1.0.0") {
		t.Errorf("recovery identity %+v ok %v", rid, ok)
	}
	none := func(parse.Parser) (string, bool) { return "", false }
	if _, err := registerParsers([]parse.Parser{claims}, hashOf, none); err == nil {
		t.Error("a claiming parser without a recovery hash was registered")
	}
	if _, err := registerParsers([]parse.Parser{plain}, none, none); err == nil {
		t.Error("a parser without a source hash was registered")
	}
}
