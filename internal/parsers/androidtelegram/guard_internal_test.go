package androidtelegram

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

// openBuilt opens a Go-built cache4.db (user version 181) holding the given tables.
func openBuilt(t *testing.T, tables map[string]string) *sqlitedb.DB {
	t.Helper()
	b := sqlitetest.New(sqlitetest.Options{PageSize: 4096, UserVersion: 181})
	b.CreateTable(tableMessagesV2, "CREATE TABLE messages_v2 (mid INTEGER, uid INTEGER, date INTEGER, data BLOB, out INTEGER)")
	b.CreateTable(tableUsers, "CREATE TABLE users (uid INTEGER PRIMARY KEY, name TEXT, status INTEGER, data BLOB)")
	for name, ddl := range tables {
		b.CreateTable(name, ddl)
	}
	img := b.Bytes()
	db, err := sqlitedb.Open(t.Context(), sqlitedb.Files{DB: bytes.NewReader(img), DBSize: int64(len(img))}, parse.NewBudget(parse.DefaultLimits().MemBudget).View())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Release)
	return db
}

// The plan's "Notes and warnings" (as androidcalendar/guard.go, F29): a join table absent from a schema read whole is
// "table absent" (a note, no warning); in a schema read only in part it is guarded with reasonSchemaPartial and one
// counted warning; a join table without its column or its rowid alias is guarded and warned; the census is asked once,
// only when a table is not found, and its failure fails the job. The join tables are chats, contacts, users_data and
// enc_chats, never users (a mapped table).
func TestCheckJoinSchemaGuardsTheJoinTables(t *testing.T) {
	var names []string
	for _, jt := range joinTables() {
		names = append(names, jt.Table)
	}
	if len(names) != 4 || names[0] != tableChats || names[1] != tableContacts || names[2] != tableUsersData || names[3] != tableEncChats {
		t.Fatalf("join tables %v", names)
	}
	calls := 0
	whole := &recEmitter{notes: map[string]string{}}
	g, err := checkJoinSchema(t.Context(), openBuilt(t, nil), whole, func() (bool, error) { calls++; return false, nil })
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if whole.notes["join_undecided."+n] != reasonTableAbsent {
			t.Errorf("whole schema: note join_undecided.%s = %q", n, whole.notes["join_undecided."+n])
		}
		if _, bad := g.reason(n); bad {
			t.Errorf("whole schema: %s is guarded", n)
		}
	}
	if len(whole.warns) != 0 || calls != 1 {
		t.Errorf("whole schema: warnings %v, census asked %d times; want none and once", whole.warns, calls)
	}
	part := &recEmitter{notes: map[string]string{}}
	g, err = checkJoinSchema(t.Context(), openBuilt(t, nil), part, func() (bool, error) { return true, nil })
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		if _, bad := g.reason(n); !bad || part.notes["join_undecided."+n] != reasonSchemaPartial {
			t.Errorf("partial schema: %s guarded %v, note %q", n, bad, part.notes["join_undecided."+n])
		}
	}
	if len(part.warns) != len(names) {
		t.Errorf("partial schema: warnings %v, want one per join table", part.warns)
	}
	boom := errors.New("census failed")
	if _, err := checkJoinSchema(t.Context(), openBuilt(t, nil), &recEmitter{notes: map[string]string{}}, func() (bool, error) { return false, boom }); !errors.Is(err, boom) {
		t.Errorf("a failed census: %v, want it returned", err)
	}
	// a join table without its column, and one whose uid is not the rowid alias
	bad := &recEmitter{notes: map[string]string{}}
	g, err = checkJoinSchema(t.Context(), openBuilt(t, map[string]string{
		tableUsersData: "CREATE TABLE users_data (uid INTEGER PRIMARY KEY, bio TEXT)",
		tableContacts:  "CREATE TABLE contacts (uid INTEGER, mutual INTEGER)",
	}), bad, func() (bool, error) { return false, nil })
	if err != nil {
		t.Fatal(err)
	}
	if r, ok := g.reason(tableUsersData); !ok || r != "the table lacks column(s) about" {
		t.Errorf("users_data without about: %q, %v", r, ok)
	}
	if r, ok := g.reason(tableContacts); !ok || r != "column uid is not the rowid alias" {
		t.Errorf("contacts without the alias: %q, %v", r, ok)
	}
	want := "sqlite:schema telegram-join-undecided: table contacts: column uid is not the rowid alias; every join through it is undecided"
	if len(bad.warns) != 2 || bad.warns[0] != want {
		t.Errorf("warnings %v, want two, the first %q", bad.warns, want)
	}
}

// fakeCtx is a MapContext whose Get and Match always find one row.
type fakeCtx struct{ parse.MapContext }

func (fakeCtx) Get(string, int64) (parse.Row, bool, error) { return nil, true, nil }
func (fakeCtx) Match(string, string, parse.JoinKey) ([]parse.Row, error) {
	return []parse.Row{nil}, nil
}
func (fakeCtx) UndecidedHits(string, string) int64 { return 3 }

// A join through a guarded table is undecided (sqlitedb.ErrKeyUndecidable), never a miss; any other table reaches the
// host context, and the host's undecided count is forwarded.
func TestGuardedCtxMakesGuardedJoinsUndecided(t *testing.T) {
	g := &joinGuard{}
	g.add(tableChats, "the table could not be read")
	c := guardedCtx{MapContext: fakeCtx{}, g: g}
	if _, found, err := c.Get("Chats", 1); found || !errors.Is(err, sqlitedb.ErrKeyUndecidable) {
		t.Errorf("Get on a guarded table: found %v, err %v", found, err)
	}
	if _, err := c.Match(tableChats, "uid", parse.JoinKey{}); !errors.Is(err, sqlitedb.ErrKeyUndecidable) {
		t.Errorf("Match on a guarded table: err %v", err)
	}
	if _, found, err := c.Get(tableContacts, 1); !found || err != nil {
		t.Errorf("Get on another table: found %v, err %v", found, err)
	}
	if rows, err := c.Match(tableContacts, "uid", parse.JoinKey{}); len(rows) != 1 || err != nil {
		t.Errorf("Match on another table: %d rows, err %v", len(rows), err)
	}
	if n := c.UndecidedHits(tableContacts, "uid"); n != 3 {
		t.Errorf("UndecidedHits = %d, want the host's 3", n)
	}
	if n := (guardedCtx{MapContext: struct{ parse.MapContext }{}, g: g}).UndecidedHits("x", "y"); n != 0 {
		t.Errorf("UndecidedHits without a reporting host = %d, want 0", n)
	}
}
