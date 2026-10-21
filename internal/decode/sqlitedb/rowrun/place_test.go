package rowrun_test

import (
	"errors"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
)

// stubRow answers only what Where asks.
type stubRow struct {
	parse.Row
	rng    records.Range
	role   parse.FileRole
	loc    string
	hasLoc bool
}

func (s stubRow) Table() string                          { return "t" }
func (s stubRow) Range() (records.Range, parse.FileRole) { return s.rng, s.role }
func (s stubRow) Locator() (string, bool)                { return s.loc, s.hasLoc }

// recStub is a stubRow that is a recovered row (no locator of its own).
type recStub struct{ stubRow }

func (recStub) Recovered() *sqlitedb.RecoveredInfo { return &sqlitedb.RecoveredInfo{} }

func bundleInput() *parse.Input {
	return &parse.Input{Artifacts: map[string]parse.Artifact{
		"db":  {ID: "A-DB", Size: 10000, Source: parse.SourceInfo{RemotePath: "/d/m.db"}},
		"wal": {ID: "A-WAL", Size: 5000, Source: parse.SourceInfo{RemotePath: "/d/m.db-wal"}},
	}}
}

func TestWhereMapsTheFileRoleToTheArtifactHoldingTheCell(t *testing.T) {
	row := stubRow{rng: records.Range{Offset: 100, Length: 50}, role: parse.RoleWAL, loc: "sqlite:table=t;rowid=3;wal_frame=2", hasLoc: true}
	got, err := rowrun.Where(fakeMC{bundleInput()}, row)
	if err != nil {
		t.Fatal(err)
	}
	want := rowrun.Place{ArtifactID: "A-WAL", Role: parse.RoleWAL, Range: records.Range{Offset: 100, Length: 50}, Locator: "sqlite:table=t;rowid=3;wal_frame=2", SourcePath: "/d/m.db-wal"}
	if got != want {
		t.Errorf("Where = %+v, want %+v", got, want)
	}
}

func TestWhereRefusesWhatItCannotAttribute(t *testing.T) {
	good := records.Range{Offset: 10, Length: 20}
	for name, row := range map[string]stubRow{
		"no known file":          {rng: good, role: "", loc: "l", hasLoc: true},
		"journal not in bundle":  {rng: good, role: parse.RoleJournal, loc: "l", hasLoc: true},
		"negative offset":        {rng: records.Range{Offset: -1, Length: 5}, role: parse.RoleDB, loc: "l", hasLoc: true},
		"empty cell":             {rng: records.Range{Offset: 5, Length: 0}, role: parse.RoleDB, loc: "l", hasLoc: true},
		"offset beyond the file": {rng: records.Range{Offset: 10001, Length: 1}, role: parse.RoleDB, loc: "l", hasLoc: true},
		"length past the end":    {rng: records.Range{Offset: 9995, Length: 10}, role: parse.RoleDB, loc: "l", hasLoc: true},
		"live row without rowid": {rng: good, role: parse.RoleDB, hasLoc: false},
	} {
		_, err := rowrun.Where(fakeMC{bundleInput()}, row)
		var re *rowrun.RowError
		if !errors.As(err, &re) {
			t.Errorf("%s: err = %v, want a *RowError", name, err)
		}
	}
}

func TestWhereAcceptsARecoveredRowWithoutALocator(t *testing.T) {
	row := recStub{stubRow{rng: records.Range{Offset: 10, Length: 20}, role: parse.RoleDB}}
	got, err := rowrun.Where(fakeMC{bundleInput()}, row)
	if err != nil || got.ArtifactID != "A-DB" || got.Locator != "" || got.SourcePath != "/d/m.db" {
		t.Errorf("Where = %+v, %v; want the db artifact and no locator", got, err)
	}
}

func TestWhereOnARealRow(t *testing.T) {
	db := tinyDB(t, 3)
	in := dbInput(t, db)
	d, err := sqlitedb.Open(t.Context(), sqlitedb.Files{DB: in.Primary.R, DBSize: in.Primary.Size}, in.Budget)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Release)
	tb, err := d.Table(t.Context(), "t", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var places []rowrun.Place
	err = tb.Scan(t.Context(), func(r sqlitedb.Row) error {
		p, err := rowrun.Where(fakeMC{in}, r)
		places = append(places, p)
		return err
	})
	if err != nil || len(places) != 3 {
		t.Fatalf("scan: %d places, %v", len(places), err)
	}
	for i, p := range places {
		wantLoc := "sqlite:table=t;rowid=" + string(rune('1'+i))
		if p.ArtifactID != "A-DB" || p.Role != parse.RoleDB || p.Locator != wantLoc || p.Range.Length <= 0 || p.Range.Offset+p.Range.Length > in.Primary.Size {
			t.Errorf("place %d = %+v, want locator %s inside the artifact", i, p, wantLoc)
		}
	}
}
