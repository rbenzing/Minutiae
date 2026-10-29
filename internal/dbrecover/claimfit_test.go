package dbrecover

import (
	"testing"

	"github.com/rbenzing/minutiae/internal/records/recordstest"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// The exclusion of a claimed table applies to table rows of basis schema or fit
// whose values fit the live definition strictly (sqlitefile.ClaimMatch); a
// guess is written as a db_row.
func TestClaimExclusionNeedsLiveFit(t *testing.T) {
	c, in := fixtureCase(t, "sqlite3-freelist")
	res := mustRun(t, c, in, Options{ExcludeTables: []ExcludedTable{{Table: "people", Parser: "examiner"}}})
	// the oracle of the fixture (sqlitefile TestClaimMatch): 12 deleted rows of d_fit fit people
	// strictly (basis fit); the 12 of d_guess fit it only loosely (basis guess)
	if got := res.Counts.SkippedByReason["excluded-by-claim"]; got != 12 {
		t.Errorf("excluded-by-claim %d, want 12 (the d_fit rows)", got)
	}
	var guess, fit int
	for _, f := range storedRecords(t, c) {
		p := payloadOf(t, f)
		if p["table"] != "people" {
			continue
		}
		switch p["table_basis"] {
		case "guess":
			guess++
		case "fit":
			fit++
		}
	}
	if guess != 12 {
		t.Errorf("%d db_row records of people at basis guess, want 12 (a guess is never excluded)", guess)
	}
	if fit != 0 {
		t.Errorf("%d db_row records of people at basis fit, want 0 (claimed)", fit)
	}
}

// A row carved from page slack is never claimed (4G-R22): it is written as a
// db_row and counted by no exclusion, whatever basis its table was named at.
// The hook sets the basis by hand, so the rule is tested on a slack row that
// would be claimed were it not a slack row (its values fit the live table t).
func TestSlackRowsAreNeverClaimed(t *testing.T) {
	for _, basis := range []sqlitefile.TableBasis{sqlitefile.BasisSchema, sqlitefile.BasisFit} {
		t.Run(string(basis), func(t *testing.T) {
			defer SetRowHookForTest(func(r sqlitefile.RecoveredRow) sqlitefile.RecoveredRow {
				if r.Method == sqlitefile.MethodPageSlack {
					r.Table, r.TableBasis = "t", basis
				}
				return r
			})()
			c := recordstest.NewCase(t)
			db := addFile(t, c, "/data/slack.db", slackScenePlanted(t).b.Bytes())
			res := mustRun(t, c, Inputs{DB: db.ID}, Options{ExcludeTables: []ExcludedTable{{Table: "t", Parser: "examiner"}}})
			if res.Counts.ByMethod[sqlitefile.MethodPageSlack] != 1 {
				t.Fatalf("slack rows written: %v (skipped %v)", res.Counts.ByMethod, res.Counts.SkippedByReason)
			}
			if n := res.Counts.SkippedByReason["excluded-by-claim"]; n != 0 {
				t.Errorf("excluded-by-claim %d, want 0", n)
			}
			var slack int
			for _, f := range storedRecords(t, c) {
				if f.Method == sqlitefile.MethodPageSlack {
					slack++
				}
			}
			if slack != 1 {
				t.Errorf("%d stored slack records, want 1", slack)
			}
		})
	}
}
