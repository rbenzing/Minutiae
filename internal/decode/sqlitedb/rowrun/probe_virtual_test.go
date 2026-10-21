package rowrun_test

// A virtual table is a table that exists, whose content a module defines (LX-4 U36): the probe must
// answer unsupported schema, never "not applicable" ("the table is absent") and never a hard error.

import (
	"errors"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

func virtualProbeDB() []byte {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 1024})
	b.CreateTable("sms", "create table sms(_id integer primary key, address text, date integer, body text)").Insert(1, int64(1), "a", int64(1), "b")
	b.AddSchemaRow("table", "vt", "vt", int64(0), "create virtual table vt using fts5(x)")
	return b.Bytes()
}

func TestProbeOfAVirtualTargetIsUnsupportedSchemaNotAbsent(t *testing.T) {
	for _, optional := range []bool{false, true} {
		got, err := probeOf(t, virtualProbeDB(), false, rowrun.Req{Table: "vt", Need: []string{"x"}, Optional: optional})
		if err != nil || got.Status != parse.UnsupportedSchema || !strings.Contains(got.Reason, "vt") {
			t.Errorf("optional %v: Probe = %+v, %v, want unsupported schema naming vt", optional, got, err)
		}
	}
}

func TestProbeOfATableWhoseRequiredCompanionIsVirtualIsUnsupportedSchema(t *testing.T) {
	got, err := probeOf(t, virtualProbeDB(), false, rowrun.Req{Table: "sms", Need: []string{"body"}, With: []string{"vt"}})
	if err != nil || got.Status != parse.UnsupportedSchema || !strings.Contains(got.Reason, "vt") {
		t.Errorf("Probe = %+v, %v, want unsupported schema naming vt (not an error, not absent)", got, err)
	}
}

func TestProbeOfAVirtualTargetWithACompanionDefersToParse(t *testing.T) {
	got, err := probeOf(t, virtualProbeDB(), true, rowrun.Req{Table: "vt", Need: []string{"x"}})
	if err != nil || got.Status != parse.Applicable {
		t.Errorf("Probe = %+v, %v, want the schema decision deferred to Parse", got, err)
	}
}

// Run: a virtual table is neither "not applicable" (identifying) nor "absent" (optional): the job
// fails with an unsupported-schema error that also satisfies sqlitefile.ErrVirtualTable.
func TestRunOfAVirtualTableFailsAsUnsupportedSchemaNeverAbsent(t *testing.T) {
	m := parse.TableMapping{Table: "vt", Columns: []parse.ColumnSpec{{Name: "x"}}, Map: echoMapping(nil).Map}
	for name, st := range map[string]rowrun.Step{
		"identifying": {Mapping: m, Identifying: true},
		"optional":    {Mapping: m, Optional: true},
		"required":    {Mapping: m},
	} {
		em := &recEmitter{}
		rep, err := rowrun.Run(t.Context(), dbInput(t, virtualProbeDB()), em, []rowrun.Step{st})
		var us *sqlitedb.UnsupportedSchemaError
		if !errors.As(err, &us) || !errors.Is(err, sqlitefile.ErrVirtualTable) || errors.Is(err, sqlitedb.ErrNoSuchTable) {
			t.Errorf("%s: err = %v, want an unsupported-schema error for the virtual table", name, err)
		}
		if rep.NotApplicable != "" || em.notes["table.vt"] != "" || em.notes["not_applicable"] != "" {
			t.Errorf("%s: the virtual table was reported as absent: report %+v notes %v", name, rep, em.notes)
		}
	}
}
