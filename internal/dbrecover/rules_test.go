package dbrecover

import (
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// goldenRulesHash pins the rules hash of Version 1.0.0.
const goldenRulesHash = "de25c87b5da8b51becf8fe144b342a379f2e7ea091dc2d76bfdc5850531f68d8"

func TestRulesHashGolden(t *testing.T) {
	if got := RulesHash(); got != goldenRulesHash {
		t.Fatalf("a rule changed: bump Version and update the golden together\n got  %s\n want %s\n"+
			"(after 3J releases, any change to delivered rows bumps Version, even one the hash does not cover)", got, goldenRulesHash)
	}
}

func TestRulesHashFollowsConfidenceTable(t *testing.T) {
	base := rulesHashFor(sqlitefile.Confidence)
	if base != RulesHash() {
		t.Fatal("RulesHash does not use sqlitefile.Confidence")
	}
	for _, m := range Methods() {
		for _, b := range []sqlitefile.TableBasis{sqlitefile.BasisSchema, sqlitefile.BasisFit, sqlitefile.BasisGuess, sqlitefile.BasisNone} {
			got := rulesHashFor(func(r sqlitefile.RecoveredRow) int {
				c := sqlitefile.Confidence(r)
				if r.Method == m && r.TableBasis == b {
					c++
				}
				return c
			})
			if got == base {
				t.Errorf("changing the cell %s x %s does not change the hash", m, b)
			}
		}
	}
}

func TestRulesHashFollowsVersion(t *testing.T) {
	before := RulesHash()
	defer SetVersionForTest("9.9.9")()
	if RulesHash() == before {
		t.Error("the hash ignores Version")
	}
	if ParserIdentity().Version != "9.9.9" {
		t.Error("ParserIdentity ignores the version seam")
	}
}

func TestParserIdentityShape(t *testing.T) {
	id := ParserIdentity()
	if id.Name != ParserName || id.Version != Version || id.Hash != RulesHash() || ParserName != "sqlite-recover" || Version != "1.0.0" {
		t.Fatalf("identity = %+v", id)
	}
	c := recordstest.NewCase(t)
	art := recordstest.AddArtifact(t, c, "a.db", []byte("x"))
	w, err := records.NewWriter(c, id, records.WriterOptions{})
	if err != nil {
		t.Fatalf("NewWriter refused the identity: %v", err)
	}
	if err := w.Start(t.Context(), records.StartOptions{Artifacts: []string{art.ID}}); err != nil {
		t.Fatalf("Start refused the identity: %v", err)
	}
	if _, err := w.End(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestMethodsAreExactlyTheLibrarysTokens(t *testing.T) {
	want := []string{
		sqlitefile.MethodWALPrior, sqlitefile.MethodJournalBefore, sqlitefile.MethodWALStale, sqlitefile.MethodFreelist,
		sqlitefile.MethodWALUncommitted, sqlitefile.MethodJournalRolledBack, sqlitefile.MethodJournalPersist, sqlitefile.MethodPageSlack,
	}
	slices.Sort(want)
	got := Methods()
	if !slices.Equal(got, want) || !slices.IsSorted(got) || len(got) != 8 {
		t.Fatalf("Methods() = %v, want %v", got, want)
	}
	got[0] = "mutated"
	if Methods()[0] == "mutated" {
		t.Error("Methods returns its internal slice")
	}
	for _, m := range want {
		if sqlitefile.Confidence(sqlitefile.RecoveredRow{Method: m, TableBasis: sqlitefile.BasisSchema}) == 0 {
			t.Errorf("method %q has no confidence: not a real token", m)
		}
	}
}
