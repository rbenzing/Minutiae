package sqlitedb_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// pin fails to compile unless x is assignable to T.
func pin[T any](T) {}

// TestSqlitefileSurfaceUsedByTheDecoder pins, by type, exactly the library
// symbols the decoder uses, so a signature change in sqlitefile fails here.
func TestSqlitefileSurfaceUsedByTheDecoder(_ *testing.T) {
	pin[func(io.ReaderAt, int64, sqlitefile.Options) (*sqlitefile.DB, error)](sqlitefile.Open)
	pin[func(*sqlitefile.DB, io.ReaderAt, int64) (*sqlitefile.WALInfo, error)]((*sqlitefile.DB).AttachWAL)
	pin[func(*sqlitefile.DB, io.ReaderAt, int64) (*sqlitefile.JournalInfo, error)]((*sqlitefile.DB).AttachJournal)
	pin[func(*sqlitefile.DB) *sqlitefile.View]((*sqlitefile.DB).Live)
	pin[func(*sqlitefile.DB) sqlitefile.Info]((*sqlitefile.DB).Info)
	pin[func(*sqlitefile.DB) []sqlitefile.Warning]((*sqlitefile.DB).Warnings)
	pin[func(*sqlitefile.DB) sqlitefile.DBStatus]((*sqlitefile.DB).Status)
	pin[func(*sqlitefile.View, context.Context) (*sqlitefile.Schema, error)]((*sqlitefile.View).Schema)
	pin[func(*sqlitefile.View, context.Context, string) (*sqlitefile.Table, error)]((*sqlitefile.View).Table)
	pin[func(*sqlitefile.View) sqlitefile.Info]((*sqlitefile.View).Info)
	pin[func(*sqlitefile.View) []sqlitefile.Warning]((*sqlitefile.View).Warnings)
	pin[func(*sqlitefile.View)]((*sqlitefile.View).Release)
	pin[func(*sqlitefile.Table, context.Context, func(sqlitefile.Row) bool) error]((*sqlitefile.Table).Rows)
	pin[func(*sqlitefile.Table, context.Context, int64) (sqlitefile.Row, bool, error)]((*sqlitefile.Table).Get)
	pin[func(*sqlitefile.Table, sqlitefile.Row) []sqlitefile.Value]((*sqlitefile.Table).Resolve)
	pin[func(*sqlitefile.Table) sqlitefile.TableDef]((*sqlitefile.Table).Def)
	pin[func(*sqlitefile.Table) string]((*sqlitefile.Table).Name)
	pin[func(sqlitefile.Row) sqlitefile.Row](sqlitefile.Row.Clone)

	// Used by Open, Info, Tables and the budget adapter (Task 2).
	pin[sqlitefile.Budget](sqlitefile.Options{}.Budget)
	pin[bool](sqlitefile.Options{}.SuperJournalPresent)
	pin[error](sqlitefile.ErrNotSQLite)
	pin[error](sqlitefile.ErrLooksEncrypted)
	pin[error](sqlitefile.ErrCorrupt)
	pin[error](sqlitefile.ErrBudget)
	pin[error](sqlitefile.ErrLimit)
	pin[error](sqlitefile.ErrPageUnavailable)
	pin[error](sqlitefile.ErrWithoutRowid)
	pin[error](sqlitefile.ErrInternal)
	pin[error](sqlitefile.ErrEngineRefuses)
	pin[error](sqlitefile.ErrLiveUnavailable)
	pin[error](parse.ErrBudget)
	var st sqlitefile.DBStatus
	pin[sqlitefile.Info](st.Info)
	pin[*sqlitefile.WALInfo](st.WAL)
	pin[*sqlitefile.JournalInfo](st.Journal)
	var sc sqlitefile.Schema
	pin[[]sqlitefile.SchemaObject](sc.Objects)
	var so sqlitefile.SchemaObject
	pin[string](so.Type)
	pin[string](so.Name)
	pin[bool](so.Virtual)
	var w sqlitefile.Warning
	pin[string](w.Code)
	pin[string](w.Msg)

	var r sqlitefile.Row
	pin[int64](r.Rowid)
	pin[bool](r.HasRowid)
	pin[[]sqlitefile.Value](r.Values)
	pin[sqlitefile.Loc](r.Loc)
	pin[bool](r.LengthMismatch)
	pin[bool](r.KeyRangeViolation)

	var v sqlitefile.Value
	pin[sqlitefile.Kind](v.Kind)
	pin[int64](v.Int)
	pin[float64](v.Float)
	pin[[]byte](v.Bytes)
	pin[int64](v.Len)
	pin[bool](v.Omitted)
	pin[bool](v.Unread)
	pin[bool](v.Clipped)
	pin[sqlitefile.Encoding](v.Enc)

	var l sqlitefile.Loc
	pin[sqlitefile.FileKind](l.File)
	pin[uint32](l.Page)
	pin[int](l.Cell)
	pin[int64](l.Offset)
	pin[int64](l.Length)
	pin[uint32](l.Frame)
	pin[int](l.Record)
	pin[[]sqlitefile.PagePart](l.Overflow)
	pin[bool](l.OverflowMixed)

	var rr sqlitefile.RecoveredRow
	pin[string](rr.Method)
	pin[sqlitefile.Origin](rr.Origin)
	pin[string](rr.Table)
	pin[string](rr.Index)
	pin[sqlitefile.TableBasis](rr.TableBasis)
	pin[*int64](rr.Rowid)
	pin[[]sqlitefile.Value](rr.Values)
	pin[sqlitefile.Loc](rr.Loc)
	pin[*sqlitefile.WALProv](rr.WAL)
	pin[*sqlitefile.JournalProv](rr.Journal)
	pin[sqlitefile.Relation](rr.Relation)
	pin[uint32](rr.OverflowHead)
	pin[bool](rr.Truncated)
	pin[[]string](rr.Notes)

	var c sqlitefile.Column
	pin[string](c.Name)
	pin[string](c.DeclType)
	pin[sqlitefile.Affinity](c.Affinity)
	pin[bool](c.NotNull)
	pin[int](c.PKOrdinal)
	pin[string](c.Collation)
	pin[string](c.KeyCollation)
	pin[sqlitefile.Default](c.Default)
	pin[sqlitefile.GenKind](c.Generated)
	pin[int](c.RecordIndex)
}

func TestBudgetViewSatisfiesBudget(_ *testing.T) {
	var _ sqlitedb.Budget = (*parse.BudgetView)(nil)
}

func TestSentinelsAreDistinct(t *testing.T) {
	all := map[string]error{
		"ErrNoBudget": sqlitedb.ErrNoBudget, "ErrNotSQLite": sqlitedb.ErrNotSQLite,
		"ErrEncrypted": sqlitedb.ErrEncrypted, "ErrCorrupt": sqlitedb.ErrCorrupt,
		"ErrLiveUnavailable": sqlitedb.ErrLiveUnavailable, "ErrEngineRefuses": sqlitedb.ErrEngineRefuses,
		"ErrNoSuchTable": sqlitedb.ErrNoSuchTable, "ErrWithoutRowid": sqlitedb.ErrWithoutRowid,
		"ErrIndexLimit": sqlitedb.ErrIndexLimit, "ErrStop": sqlitedb.ErrStop,
		"ErrBadFiles": sqlitedb.ErrBadFiles, "ErrRowMismatch": sqlitedb.ErrRowMismatch,
		"ErrInternal": sqlitedb.ErrInternal, "ErrUnsupportedSchema": sqlitedb.ErrUnsupportedSchema,
		"ErrUnsupportedCollation": sqlitedb.ErrUnsupportedCollation,
		"ErrReleased":             sqlitedb.ErrReleased,
	}
	for n1, e1 := range all {
		if e1 == nil {
			t.Fatalf("%s is nil", n1)
		}
		for _, w := range []string{"select ", "insert ", "update ", "delete ", "create ", "drop ", "pragma"} {
			if strings.Contains(strings.ToLower(e1.Error()), w) {
				t.Errorf("%s text %q holds SQL-shaped word %q", n1, e1.Error(), w)
			}
		}
		for n2, e2 := range all {
			if n1 != n2 && errors.Is(e1, e2) {
				t.Errorf("%s is %s", n1, n2)
			}
		}
	}
	if !errors.Is(&sqlitedb.UnsupportedSchemaError{Table: "t"}, sqlitedb.ErrUnsupportedSchema) {
		t.Error("UnsupportedSchemaError does not match its sentinel")
	}
	if !errors.Is(&sqlitedb.UnsupportedCollationError{Table: "t"}, sqlitedb.ErrUnsupportedCollation) {
		t.Error("UnsupportedCollationError does not match its sentinel")
	}
}

// Used by Table (Task 3).
func TestSqlitefileSurfaceUsedByTableResolution(_ *testing.T) {
	pin[error](sqlitefile.ErrNotFound)
	var d sqlitefile.TableDef
	pin[[]sqlitefile.Column](d.Columns)
	pin[bool](d.WithoutRowid)
	pin[int](d.RowidAlias)
	pin[bool](d.ParseOK)
	pin[string](d.ParseNote)
	pin[sqlitefile.Affinity](sqlitefile.AffText)
	pin[sqlitefile.DefaultKind](sqlitefile.DefaultLiteral)
	pin[sqlitefile.GenKind](sqlitefile.GenVirtual)
	pin[func(*sqlitefile.Table) uint32]((*sqlitefile.Table).RootPage)
}
