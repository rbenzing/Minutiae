package artparse_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/artparse"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers"
)

// claimParser is a database parser (role "db") that claims tables and answers Probe with status.
type claimParser struct {
	name, status string
	claims       []string
}

func (p *claimParser) Meta() parse.Meta {
	m := metaFor(p.name, "1.0.0")
	for _, t := range p.claims {
		m.Claims = append(m.Claims, parse.TableClaim{Role: "db", Table: t})
	}
	return m
}

func (p *claimParser) Probe(context.Context, *parse.Input) (parse.Applicability, error) {
	return parse.Applicability{Status: p.status, Reason: "r"}, nil
}
func (p *claimParser) Parse(context.Context, *parse.Input, parse.Emitter) error { return nil }

// claimMapper is a claimParser that also declares row mappings.
type claimMapper struct {
	claimParser
	tables []string
}

func (p *claimMapper) Mappings() []parse.TableMapping {
	var out []parse.TableMapping
	for _, t := range p.tables {
		out = append(out, parse.TableMapping{Table: t})
	}
	return out
}

func claimHost(t *testing.T, status string, claims ...string) (*artparse.Host, evidence.ManifestRecord, artparse.Registered) {
	t.Helper()
	c := newCase(t)
	acquireStart(t, c, "D1", "A1", "logical")
	rec := putFile(t, c, "D1", "A1", dbPath, "db")
	r := regFake(t, &claimMapper{claimParser{name: "cp", status: status, claims: claims}, claims}, "cp")
	return hostWith(t, c, nil, r), rec, r
}

func TestClaimedTablesRequireApplicableProbe(t *testing.T) {
	h, rec, r := claimHost(t, parse.Applicable, "sms", "SMS_Backup")
	got, err := h.ClaimedTables(context.Background(), rec.ID)
	if err != nil {
		t.Fatal(err)
	}
	id := r.Identity()
	want := []artparse.Claimed{{Table: "sms", Role: "db", Parser: id.Name, Version: id.Version, Hash: id.Hash}, {Table: "SMS_Backup", Role: "db", Parser: id.Name, Version: id.Version, Hash: id.Hash}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("claims %+v, want %+v", got, want)
	}
	for _, st := range []string{parse.NotApplicable} { // the parser says the file is not its format: no claim
		h, rec, _ := claimHost(t, st, "sms")
		got, err := h.ClaimedTables(context.Background(), rec.ID)
		if err != nil || len(got) != 0 {
			t.Errorf("probe %s: claims %+v err %v, want none", st, got, err)
		}
	}
}

func TestClaimedTablesOnlyForPrimaryGlobMatch(t *testing.T) {
	c := newCase(t)
	acquireStart(t, c, "D1", "A1", "logical")
	other := putFile(t, c, "D1", "A1", "/data/data/com.a/databases/other.db", "x")
	r := regFake(t, &claimMapper{claimParser{name: "cp", status: parse.Applicable, claims: []string{"sms"}}, []string{"sms"}}, "cp")
	got, err := hostWith(t, c, nil, r).ClaimedTables(context.Background(), other.ID)
	if err != nil || len(got) != 0 {
		t.Fatalf("claims %+v err %v for a path the parser does not take", got, err)
	}
	if _, err := hostWith(t, c, nil, r).ClaimedTables(context.Background(), "no-such-artifact"); !errors.Is(err, evidence.ErrUnknownArtifact) {
		t.Fatalf("unknown artifact: %v", err)
	}
}

func TestClaimedTablesEmptyWithoutClaims(t *testing.T) {
	c := newCase(t)
	acquireStart(t, c, "D1", "A1", "logical")
	rec := putFile(t, c, "D1", "A1", dbPath, "db")
	// the 4A registry shape: parsers, none with claims
	h := hostWith(t, c, nil, register(t, "p", "1.0.0"))
	got, err := h.ClaimedTables(context.Background(), rec.ID)
	if err != nil || len(got) != 0 {
		t.Fatalf("claims %+v err %v", got, err)
	}
}

func TestClaimsEqualMappings(t *testing.T) {
	reg := func(p parse.Parser, name string) artparse.Registered { return regFake(t, p, name) }
	if err := reg(&claimMapper{claimParser{name: "m", claims: []string{"sms", "Calls"}}, []string{"CALLS", "sms"}}, "m").CheckClaims(); err != nil {
		t.Errorf("claims equal to mappings (case-folded): %v", err)
	}
	if err := reg(&claimParser{name: "nc"}, "nc").CheckClaims(); err != nil {
		t.Errorf("no claims, not a RowMapper: %v", err)
	}
	err := reg(&claimMapper{claimParser{name: "x", claims: []string{"sms", "calls"}}, []string{"sms"}}, "x").CheckClaims()
	if err == nil || !strings.Contains(err.Error(), "calls") {
		t.Errorf("a claim outside the mappings: %v", err)
	}
	err = reg(&claimParser{name: "y", claims: []string{"sms"}}, "y").CheckClaims()
	if err == nil || !strings.Contains(err.Error(), "RowMapper") {
		t.Errorf("claims without a RowMapper: %v", err)
	}
	// the real registry: vacuous in 4A (no parser), wired for 4D to 4G
	for _, p := range parsers.All() {
		hash, ok := parsers.HashOf(p)
		if !ok {
			t.Fatalf("no hash for %T", p)
		}
		r, err := artparse.Register(p, hash)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.CheckClaims(); err != nil {
			t.Errorf("%s: %v", r.Meta().Name, err)
		}
	}
}

func TestClaimedTablesRefuseALyingParser(t *testing.T) {
	c := newCase(t)
	acquireStart(t, c, "D1", "A1", "logical")
	rec := putFile(t, c, "D1", "A1", dbPath, "db")
	r := regFake(t, &claimMapper{claimParser{name: "liar", status: parse.Applicable, claims: []string{"sms", "calls"}}, []string{"sms"}}, "liar")
	if _, err := hostWith(t, c, nil, r).ClaimedTables(context.Background(), rec.ID); err == nil || !strings.Contains(err.Error(), "calls") {
		t.Fatalf("ClaimedTables with a claim outside the mappings: %v", err)
	}
}
