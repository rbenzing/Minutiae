package ioswhatsapp

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

func (e *recEmitter) Emit(context.Context, records.Record) error { return nil }
func (e *recEmitter) Warn(_ context.Context, locator, reason string) error {
	e.warns = append(e.warns, locator+" "+reason)
	return nil
}
func (e *recEmitter) Note(k, v string)    { e.notes[k] = v }
func (e *recEmitter) Progress(_, _ int64) {}

func guardDB(t *testing.T, messageDDL string) *sqlitedb.DB {
	t.Helper()
	b := sqlitetest.New(sqlitetest.Options{PageSize: 4096})
	b.CreateTable("ZWAMESSAGE", messageDDL)
	b.CreateTable("ZWACHATSESSION", "CREATE TABLE ZWACHATSESSION (Z_PK INTEGER PRIMARY KEY, ZCONTACTJID VARCHAR)")
	img := b.Bytes()
	db, err := sqlitedb.Open(t.Context(), sqlitedb.Files{DB: bytes.NewReader(img), DBSize: int64(len(img))}, parse.NewBudget(parse.DefaultLimits().MemBudget).View())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Release)
	return db
}

// The identifying table whose definition cannot be read is the database's damage: no alias is proven, so every join is
// undecided and stated (a note and a warning per table), never skipped silently (review M4). A cancelled context is the
// job's failure, never a state.
func TestCheckJoinSchemaStatesDamageAndFailsOnOtherErrors(t *testing.T) {
	db := guardDB(t, "CREATE TABLE ZWAMESSAGE (Z_PK INTEGER PRIMARY KEY, ")
	e := &recEmitter{notes: map[string]string{}}
	g, err := checkJoinSchema(t.Context(), db, e)
	if err != nil {
		t.Fatalf("damaged message table: %v", err)
	}
	for _, tb := range []string{"ZWACHATSESSION", "ZWAGROUPMEMBER", "ZWAMEDIAITEM", "ZWAMESSAGEINFO"} {
		if _, bad := g.reason(tb); !bad {
			t.Errorf("%s is not guarded", tb)
		}
		if e.notes["join_undecided."+tb] == "" {
			t.Errorf("no note for %s: %v", tb, e.notes)
		}
	}
	if len(e.warns) != 4 {
		t.Errorf("%d warnings, want one per table: %v", len(e.warns), e.warns)
	}

	db2 := guardDB(t, "CREATE TABLE ZWAMESSAGE (Z_PK INTEGER PRIMARY KEY)")
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := checkJoinSchema(cctx, db2, &recEmitter{notes: map[string]string{}}); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled context: %v, want context.Canceled", err)
	}
}
