package sqlitedb_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

func virtualDB(t testing.TB) *sqlitedb.DB {
	t.Helper()
	data := newBuilderDB(t, sqlitetest.Options{PageSize: 1024}, func(b *sqlitetest.Builder) {
		b.CreateTable("t", "create table t(a)").Insert(1, "x")
		b.AddSchemaRow("table", "vt", "vt", int64(0), "create virtual table vt using fts5(x)")
	})
	return openBytes(t, data, nil, nil, bigBudget())
}

func TestVirtualTableIsUnsupportedSchemaNotNoSuchTable(t *testing.T) {
	d := virtualDB(t)
	tb, err := d.Table(t.Context(), "vt", nil, nil)
	var us *sqlitedb.UnsupportedSchemaError
	if tb != nil || !errors.As(err, &us) || !errors.Is(err, sqlitedb.ErrUnsupportedSchema) || errors.Is(err, sqlitedb.ErrNoSuchTable) {
		t.Fatalf("Table = %v, %v", tb, err)
	}
	if us.Table != "vt" || !strings.Contains(us.Reason, "virtual") || len(us.Missing) != 0 {
		t.Errorf("err = %+v", us)
	}
}

func TestContextColumnOfAVirtualTableIsAStickyFailure(t *testing.T) {
	d := virtualDB(t)
	c := newCtx(t, d, &fakeNotes{})
	if got := c.Column("vt", "x"); got != -1 {
		t.Errorf("Column = %d", got)
	}
	if err := c.Err(); !errors.Is(err, sqlitedb.ErrUnsupportedSchema) {
		t.Fatalf("Err = %v", err)
	}
	if _, _, err := c.Get("t", 1); !errors.Is(err, sqlitedb.ErrUnsupportedSchema) {
		t.Errorf("Get = %v", err)
	}
	if _, err := c.Match("t", "a", sqlitedb.LiteralIntKey(1)); !errors.Is(err, sqlitedb.ErrUnsupportedSchema) {
		t.Errorf("Match = %v", err)
	}
}

// PASSES ON ARRIVAL (TestTablesListsOnlyBaseTables pins it too; this adds a
// database with only a virtual table). Mutation: list virtual tables.
func TestTablesStillExcludesVirtualTables(t *testing.T) {
	data := newBuilderDB(t, sqlitetest.Options{PageSize: 1024}, func(b *sqlitetest.Builder) {
		b.AddSchemaRow("table", "vt", "vt", int64(0), "create virtual table vt using fts5(x)")
	})
	d := openBytes(t, data, nil, nil, bigBudget())
	got, err := d.Tables(t.Context())
	if err != nil || len(got) != 0 {
		t.Errorf("Tables = %v, %v", got, err)
	}
}

// PASSES ON ARRIVAL. Mutation: map every Table error to unsupported schema.
func TestMissingTableStaysNoSuchTable(t *testing.T) {
	d := virtualDB(t)
	for _, n := range []string{"nothing", ""} {
		tb, err := d.Table(t.Context(), n, nil, nil)
		if tb != nil || !errors.Is(err, sqlitedb.ErrNoSuchTable) || errors.Is(err, sqlitedb.ErrUnsupportedSchema) {
			t.Errorf("Table(%q) = %v, %v", n, tb, err)
		}
	}
}

// U37: a decoder user learns that a virtual table exists without knowing its
// name (unknown is never absent).
func TestVirtualTablesListsTheVirtualTables(t *testing.T) {
	d := virtualDB(t)
	got, err := d.VirtualTables(t.Context())
	if err != nil || len(got) != 1 || got[0] != "vt" {
		t.Fatalf("VirtualTables = %v, %v", got, err)
	}
	base, err := d.Tables(t.Context())
	if err != nil || len(base) != 1 || base[0] != "t" {
		t.Errorf("Tables = %v, %v", base, err)
	}
}

func TestVirtualTablesIsEmptyWithoutOne(t *testing.T) {
	data := newBuilderDB(t, sqlitetest.Options{PageSize: 1024}, func(b *sqlitetest.Builder) {
		b.CreateTable("t", "create table t(a)").Insert(1, "x")
	})
	d := openBytes(t, data, nil, nil, bigBudget())
	got, err := d.VirtualTables(t.Context())
	if err != nil || got == nil || len(got) != 0 {
		t.Errorf("VirtualTables = %#v, %v", got, err)
	}
}

// U38: the unsupported-schema error of a virtual table is also the reader's
// ErrVirtualTable.
func TestVirtualTableErrorUnwrapsToTheReaderSentinel(t *testing.T) {
	d := virtualDB(t)
	_, err := d.Table(t.Context(), "vt", nil, nil)
	if !errors.Is(err, sqlitefile.ErrVirtualTable) || !errors.Is(err, sqlitedb.ErrUnsupportedSchema) {
		t.Fatalf("Table err = %v", err)
	}
	// a table that merely lacks a column is not a virtual table
	_, err = d.Table(t.Context(), "t", []string{"nope"}, nil)
	if !errors.Is(err, sqlitedb.ErrUnsupportedSchema) || errors.Is(err, sqlitefile.ErrVirtualTable) {
		t.Errorf("missing-column err = %v", err)
	}
}
