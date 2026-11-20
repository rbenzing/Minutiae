package androidcalendar

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// recEmitter records the notes and warnings of checkJoinSchema.
type recEmitter struct {
	notes map[string]string
	warns []string
}

func (*recEmitter) Emit(context.Context, records.Record) error { return nil }
func (e *recEmitter) Warn(_ context.Context, locator, reason string) error {
	e.warns = append(e.warns, locator+" "+reason)
	return nil
}
func (e *recEmitter) Note(k, v string)  { e.notes[k] = v }
func (*recEmitter) Progress(_, _ int64) {}

// eventsOnlyDB is a calendar.db with an Events table and no join table.
func eventsOnlyDB(t *testing.T) *sqlitedb.DB {
	t.Helper()
	b := sqlitetest.New(sqlitetest.Options{PageSize: 4096, UserVersion: 601})
	b.CreateTable("Events", "CREATE TABLE Events (_id INTEGER PRIMARY KEY AUTOINCREMENT, calendar_id INTEGER, dtstart INTEGER, allDay INTEGER, deleted INTEGER)")
	img := b.Bytes()
	db, err := sqlitedb.Open(t.Context(), sqlitedb.Files{DB: bytes.NewReader(img), DBSize: int64(len(img))}, parse.NewBudget(parse.DefaultLimits().MemBudget).View())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Release)
	return db
}

// Coordinator ruling (i): a join table not found in a schema read whole is "table absent" (a note, no warning); in a
// schema read only in part it is never absent: it is guarded (every join through it undecided), with the note
// reasonSchemaPartial and one counted warning. The census is asked once, only when a table is not found; its failure
// fails the job.
func TestCheckJoinSchemaNeverCallsAPartialSchemaAbsent(t *testing.T) {
	joins := len(joinTables())
	calls := 0
	whole := &recEmitter{notes: map[string]string{}}
	g, err := checkJoinSchema(t.Context(), eventsOnlyDB(t), whole, func() (bool, error) { calls++; return false, nil })
	if err != nil {
		t.Fatal(err)
	}
	for _, jt := range joinTables() {
		if whole.notes["join_undecided."+jt.Table] != reasonTableAbsent {
			t.Errorf("whole schema: note join_undecided.%s = %q, want %q", jt.Table, whole.notes["join_undecided."+jt.Table], reasonTableAbsent)
		}
		if _, bad := g.reason(jt.Table); bad {
			t.Errorf("whole schema: %s is guarded", jt.Table)
		}
	}
	if len(whole.warns) != 0 || calls != 1 {
		t.Errorf("whole schema: warnings %v, census asked %d times; want none and once", whole.warns, calls)
	}

	part := &recEmitter{notes: map[string]string{}}
	g, err = checkJoinSchema(t.Context(), eventsOnlyDB(t), part, func() (bool, error) { return true, nil })
	if err != nil {
		t.Fatal(err)
	}
	for _, jt := range joinTables() {
		if part.notes["join_undecided."+jt.Table] != reasonSchemaPartial {
			t.Errorf("partial schema: note join_undecided.%s = %q, want %q", jt.Table, part.notes["join_undecided."+jt.Table], reasonSchemaPartial)
		}
		if _, bad := g.reason(jt.Table); !bad {
			t.Errorf("partial schema: %s is not guarded", jt.Table)
		}
	}
	if len(part.warns) != joins {
		t.Errorf("partial schema: %d warnings, want one per join table (%d): %v", len(part.warns), joins, part.warns)
	}

	boom := errors.New("census failed")
	if _, err := checkJoinSchema(t.Context(), eventsOnlyDB(t), &recEmitter{notes: map[string]string{}}, func() (bool, error) { return false, boom }); !errors.Is(err, boom) {
		t.Errorf("a failed census: %v, want it returned", err)
	}
}
