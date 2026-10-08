package parse

import (
	"context"
	"reflect"
	"testing"

	"github.com/rbenzing/minutiae/internal/records"
)

// Test doubles: compile-time proof that the interfaces can be implemented
// outside this package, with only the types the contract exports.
type fakeRow struct{ table string }

func (r fakeRow) Table() string                  { return r.table }
func (fakeRow) Rowid() (int64, bool)             { return 0, false }
func (fakeRow) IsNull(int) bool                  { return true }
func (fakeRow) Int(int) (int64, bool)            { return 0, false }
func (fakeRow) Float(int) (float64, bool)        { return 0, false }
func (fakeRow) Text(int) ([]byte, bool)          { return nil, false }
func (fakeRow) Blob(int) ([]byte, bool)          { return nil, false }
func (fakeRow) Range() (records.Range, FileRole) { return records.Range{}, RoleDB }

type fakeMapContext struct {
	in    *Input
	notes map[string]string
}

func (c *fakeMapContext) Input() *Input          { return c.in }
func (c *fakeMapContext) Note(key, value string) { c.notes[key] = value }

type fakeMapper struct {
	meta     Meta
	mappings []TableMapping
}

func (m fakeMapper) Meta() Meta { return m.meta }
func (fakeMapper) Probe(context.Context, *Input) (Applicability, error) {
	return Applicability{Status: Applicable}, nil
}
func (fakeMapper) Parse(context.Context, *Input, Emitter) error { return nil }
func (m fakeMapper) Mappings() []TableMapping                   { return m.mappings }

var (
	_ Row        = fakeRow{}
	_ MapContext = (*fakeMapContext)(nil)
	_ Parser     = fakeMapper{}
	_ RowMapper  = fakeMapper{}
)

func TestFileRoles(t *testing.T) {
	if RoleDB != "db" || RoleWAL != "wal" || RoleJournal != "journal" {
		t.Fatalf("roles = %q %q %q", RoleDB, RoleWAL, RoleJournal)
	}
}

func TestRowMapperDoubleRuns(t *testing.T) {
	var called bool
	mp := fakeMapper{meta: validMeta(), mappings: []TableMapping{{
		Table:   "sms",
		Columns: []ColumnSpec{{Name: "body", Required: true, Affinity: "TEXT"}},
		Map: func(m MapContext, row Row) ([]records.Record, error) {
			called = true
			m.Note("k", row.Table())
			return []records.Record{{Type: "message"}}, nil
		},
	}}}
	ctx := &fakeMapContext{in: fullInput(), notes: map[string]string{}}
	recs, err := mp.Mappings()[0].Map(ctx, fakeRow{table: "sms"})
	if err != nil || len(recs) != 1 || !called || ctx.notes["k"] != "sms" {
		t.Fatalf("Map = %v, %v, called=%v, notes=%v", recs, err, called, ctx.notes)
	}
	if ctx.Input().Primary.ID != "a1" {
		t.Fatal("MapContext.Input does not return the input")
	}
}

func TestClaimsOutsideMappings(t *testing.T) {
	maps := func(tables ...string) []TableMapping {
		var out []TableMapping
		for _, tb := range tables {
			out = append(out, TableMapping{Table: tb})
		}
		return out
	}
	claims := func(tables ...string) Meta {
		m := validMeta()
		m.Claims = nil
		for _, tb := range tables {
			m.Claims = append(m.Claims, TableClaim{Role: "db", Table: tb})
		}
		return m
	}
	tests := []struct {
		name     string
		meta     Meta
		mappings []TableMapping
		want     []TableClaim
	}{
		{"subset passes", claims("sms", "threads"), maps("sms", "threads", "extra"), nil},
		{"equal passes", claims("sms"), maps("sms"), nil},
		{"no claims", claims(), maps("sms"), nil},
		{"no mappings", claims("sms"), nil, []TableClaim{{Role: "db", Table: "sms"}}},
		{"a stray claim is reported", claims("sms", "ghost"), maps("sms"), []TableClaim{{Role: "db", Table: "ghost"}}},
		{"several strays keep their order", claims("b", "sms", "a"), maps("sms"), []TableClaim{{Role: "db", Table: "b"}, {Role: "db", Table: "a"}}},
		{"ASCII case-insensitive", claims("SMS", "Threads"), maps("sms", "THREADS"), nil},
		{"non-ASCII letters are not folded", claims("Été"), maps("été"), []TableClaim{{Role: "db", Table: "Été"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ClaimsOutsideMappings(tc.meta, tc.mappings)
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ClaimsOutsideMappings = %v, want %v", got, tc.want)
			}
		})
	}
}
