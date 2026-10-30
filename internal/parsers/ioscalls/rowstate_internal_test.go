package ioscalls

import (
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
)

// flaggedFake is a fakeRow that reports the row-level damage of a decoded row (Flags), as the real decoder does.
type flaggedFake struct {
	fakeRow
	flags sqlitedb.RowFlags
}

func (r flaggedFake) Flags() sqlitedb.RowFlags { return r.flags }

// mapRow maps one row of l whose cells are given by column name; every other cell is NULL, the date is a valid
// instant.
func (l layout) mapRow(t *testing.T, row func(cells []cell) parse.Row, set map[string]cell) records.Record {
	t.Helper()
	cells := make([]cell, len(l.specs()))
	cells[l.colIndex(l.date)] = cell{kind: cFloat, f: 700000000}
	if l.table == tableLegacy {
		cells[l.colIndex(l.date)] = cell{kind: cInt, i: 1700000000}
	}
	for name, c := range set {
		cells[l.colIndex(name)] = c
	}
	in := &parse.Input{Artifacts: map[string]parse.Artifact{"db": {ID: "a1", Size: 100, Source: parse.SourceInfo{RemotePath: "/p/db"}}}}
	recs, err := l.mp(fakeCtx{in: in, table: l.table, specs: l.specs()}, row(cells))
	if err != nil || len(recs) != 1 {
		t.Fatalf("%s: %d records, err %v", l.table, len(recs), err)
	}
	return recs[0]
}

func rawOfRec(r records.Record) map[string]any {
	raw, _ := r.Payload["raw"].(map[string]any)
	return raw
}

// finish states the row-level damage the decoder saw in raw.row_flags, for both tables, and nothing for an
// undamaged row (PASSES ON ARRIVAL: the production line is finish's DamageFlags block).
func TestFinishStatesRowDamageFlags(t *testing.T) {
	all := sqlitedb.FlagLengthMismatch | sqlitedb.FlagKeyRange | sqlitedb.FlagExtraValues
	for _, l := range []layout{coreLayout, legacyLayout} {
		damaged := l.mapRow(t, func(c []cell) parse.Row {
			return flaggedFake{fakeRow: fakeRow{table: l.table, cells: c}, flags: all}
		}, nil)
		got, _ := rawOfRec(damaged)["row_flags"].([]any)
		if !slices.Equal(got, []any{"length_mismatch", "key_range", "extra_values"}) {
			t.Errorf("%s: raw.row_flags = %v, want all three flags", l.table, got)
		}
		clean := l.mapRow(t, func(c []cell) parse.Row {
			return flaggedFake{fakeRow: fakeRow{table: l.table, cells: c}}
		}, nil)
		if _, has := rawOfRec(clean)["row_flags"]; has {
			t.Errorf("%s: an undamaged row states row_flags: %v", l.table, rawOfRec(clean))
		}
	}
}

// An empty name or location is not absent: it gets a _state note, as an empty address does (raw.address_state).
func TestEmptyNameAndLocationAreStatedNotAbsent(t *testing.T) {
	plain := func(l layout) func(c []cell) parse.Row {
		return func(c []cell) parse.Row { return fakeRow{table: l.table, cells: c} }
	}
	empty := cell{kind: cText, b: []byte{}}
	text := cell{kind: cText, b: []byte("Ann")}
	for _, tc := range []struct {
		l          layout
		col, field string
	}{
		{coreLayout, "ZNAME", "name_cached"}, {coreLayout, "ZLOCATION", "geo_description"}, {legacyLayout, "name", "name_cached"},
	} {
		r := tc.l.mapRow(t, plain(tc.l), map[string]cell{tc.col: empty})
		if rawOfRec(r)[tc.field+"_state"] != "empty" {
			t.Errorf("%s.%s empty: raw %v, want %s_state empty", tc.l.table, tc.col, rawOfRec(r), tc.field)
		}
		for name, c := range map[string]cell{"text": text, "null": {kind: cNull}} {
			r := tc.l.mapRow(t, plain(tc.l), map[string]cell{tc.col: c})
			if _, has := rawOfRec(r)[tc.field+"_state"]; has {
				t.Errorf("%s.%s %s: states %s_state: %v", tc.l.table, tc.col, name, tc.field, rawOfRec(r))
			}
		}
	}
}
