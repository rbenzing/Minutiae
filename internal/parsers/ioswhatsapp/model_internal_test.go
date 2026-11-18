package ioswhatsapp

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

func modelOf(t *testing.T, db []byte, names ...string) (model, error) {
	t.Helper()
	b := parse.NewBudget(parse.DefaultLimits().MemBudget).View()
	return readModel(t.Context(), sqlitedb.Files{DB: bytes.NewReader(db), DBSize: int64(len(db))}, b, names...)
}

func zpk(ddl string, fill func(z *sqlitetest.Table)) []byte { return zpkEnc(1, ddl, fill) }

func zpkEnc(enc int, ddl string, fill func(z *sqlitetest.Table)) []byte {
	b := sqlitetest.New(sqlitetest.Options{PageSize: 4096, Encoding: enc})
	z := b.CreateTable("Z_PRIMARYKEY", ddl)
	fill(z)
	return b.Bytes()
}

const zpkDDL = "CREATE TABLE Z_PRIMARYKEY (Z_ENT INTEGER PRIMARY KEY, Z_NAME VARCHAR, Z_SUPER INTEGER, Z_MAX INTEGER)"

func wantReason(t *testing.T, label string, err error, reason string) {
	t.Helper()
	var me *modelError
	if !errors.As(err, &me) || me.Reason != reason || err.Error() != reason {
		t.Errorf("%s: err %v, want modelError %q", label, err, reason)
	}
}

func TestReadModelBounds(t *testing.T) { // 4I2-R3
	// the cap: 4097 rows is refused whatever they name; 4096 are read
	many := func(n int) []byte {
		return zpk(zpkDDL, func(z *sqlitetest.Table) {
			z.Insert(1, nil, "WAMessage", int64(0), int64(0))
			z.Insert(2, nil, "WAChatSession", int64(0), int64(0))
			for i := 3; i <= n; i++ {
				z.Insert(int64(i), nil, "E"+strconv.Itoa(i), int64(0), int64(0))
			}
		})
	}
	if _, err := modelOf(t, many(maxModelRows+1), "WAMessage", "WAChatSession"); err == nil {
		t.Error("4097 rows were accepted")
	} else {
		wantReason(t, "4097 rows", err, "Z_PRIMARYKEY has more than 4096 rows")
	}
	m, err := modelOf(t, many(maxModelRows), "WAMessage", "WAChatSession")
	if err != nil || m.ByName["WAMessage"].Ent != 1 || m.ByName["WAChatSession"].Ent != 2 || len(m.ByName) != 2 {
		t.Errorf("4096 rows: %+v, %v", m, err)
	}

	// Z_ENT must be an INTEGER
	text := zpk("CREATE TABLE Z_PRIMARYKEY (Z_ENT INTEGER, Z_NAME VARCHAR)", func(z *sqlitetest.Table) {
		z.Insert(1, "9", "WAMessage")
		z.Insert(2, int64(4), "WAChatSession")
	})
	_, err = modelOf(t, text, "WAMessage", "WAChatSession")
	wantReason(t, "text Z_ENT", err, "Z_PRIMARYKEY entity WAMessage has no integer Z_ENT")
	null := zpk("CREATE TABLE Z_PRIMARYKEY (Z_ENT INTEGER, Z_NAME VARCHAR)", func(z *sqlitetest.Table) {
		z.Insert(1, nil, "WAMessage")
	})
	_, err = modelOf(t, null, "WAMessage")
	wantReason(t, "NULL Z_ENT", err, "Z_PRIMARYKEY entity WAMessage has no integer Z_ENT")

	// names compare byte-exact, and only the names asked for are kept
	exact := zpk(zpkDDL, func(z *sqlitetest.Table) {
		z.Insert(1, nil, "wamessage", int64(0), int64(0))
		z.Insert(2, nil, "WAMessage ", int64(0), int64(0))
		z.Insert(3, nil, "WAMessage", int64(7), int64(0))
		z.Insert(4, nil, "Other", int64(0), int64(0))
	})
	m, err = modelOf(t, exact, "WAMessage")
	if err != nil || len(m.ByName) != 1 || m.ByName["WAMessage"] != (entity{Ent: 3, Super: 7}) {
		t.Errorf("byte-exact: %+v, %v", m, err)
	}
	_, err = modelOf(t, exact, "WAMessage", "WAChatSession")
	wantReason(t, "absent", err, "Z_PRIMARYKEY does not name the entity WAChatSession")

	// no table; no columns beyond Z_ENT and Z_NAME is fine (Z_SUPER is optional)
	_, err = modelOf(t, sqlitetest.New(sqlitetest.Options{PageSize: 4096}).Bytes(), "WAMessage")
	wantReason(t, "no table", err, "no Z_PRIMARYKEY table: the Core Data model cannot be read")
	slim := zpk("CREATE TABLE Z_PRIMARYKEY (Z_ENT INTEGER PRIMARY KEY, Z_NAME VARCHAR)", func(z *sqlitetest.Table) {
		z.Insert(5, nil, "WAMessage")
	})
	if m, err = modelOf(t, slim, "WAMessage"); err != nil || m.ByName["WAMessage"] != (entity{Ent: 5}) {
		t.Errorf("slim: %+v, %v", m, err)
	}

	// a cancelled context and a refused budget pass through, never a model error
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	b := parse.NewBudget(parse.DefaultLimits().MemBudget).View()
	if _, err = readModel(ctx, sqlitedb.Files{DB: bytes.NewReader(slim), DBSize: int64(len(slim))}, b, "WAMessage"); err == nil || isModelError(err) {
		t.Errorf("cancelled context: %v", err)
	}
	if _, err = readModel(t.Context(), sqlitedb.Files{DB: bytes.NewReader(slim), DBSize: int64(len(slim))}, &parse.BudgetView{}, "WAMessage"); err == nil || isModelError(err) {
		t.Errorf("no budget: %v", err)
	}
}

func isModelError(err error) bool {
	var me *modelError
	return errors.As(err, &me)
}

// Unknown is never absent (review M1): a cell the reader cannot vouch for decides nothing, whatever the counts.
func TestReadModelUnreadableCellsAreNeverDecided(t *testing.T) {
	const reason = "Z_PRIMARYKEY could not be read whole"
	// another row's name is unreadable: "exactly once" is undecidable, so the model is not accepted
	name := zpkEnc(2, zpkDDL, func(z *sqlitetest.Table) {
		z.Insert(1, nil, "WAMessage", int64(0), int64(0))
		z.Insert(2, nil, "WAChatSession", int64(0), int64(0))
		z.InsertRaw(3, []uint64{0, 19, 0, 0}, []byte{0x41, 0x00, 0x42}) // odd-length UTF-16: undecodable, so unknown
	})
	_, err := modelOf(t, name, "WAMessage", "WAChatSession")
	wantReason(t, "unreadable name beside two found once", err, reason)
	// the wanted entity's own Z_ENT is unreadable: not "has no integer Z_ENT"
	ent := zpk("CREATE TABLE Z_PRIMARYKEY (Z_NAME VARCHAR, Z_ENT INTEGER DEFAULT (1+1))", func(z *sqlitetest.Table) {
		z.InsertRaw(1, []uint64{31}, []byte("WAMessage")) // the record ends before Z_ENT, whose default is an expression
	})
	_, err = modelOf(t, ent, "WAMessage")
	wantReason(t, "unreadable Z_ENT", err, reason)
	// a NULL or TEXT Z_ENT is still a decided "no integer" (TestReadModelBounds)
}
