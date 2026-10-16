package sqlitedb_test

// Table.GetByKey (LX-4 Task 9, L105): the row of a WITHOUT ROWID table by its primary key.

import (
	"errors"
	"fmt"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// keyDB holds: kt (text key, BINARY), ki (integer key), kn (text key NOCASE), kc (a key column under
// a collation this reader does not reproduce), kc2 (composite), and rt (a rowid table whose
// columns give source keys).
func keyDB(t *testing.T) (*sqlitedb.DB, *testBudget) {
	t.Helper()
	data := newBuilderDB(t, sqlitetest.Options{PageSize: 512}, func(b *sqlitetest.Builder) {
		kt := b.CreateTableWithoutRowid("kt", "create table kt(k text primary key, v) without rowid", 1)
		for i := range 40 {
			kt.Insert(int64(i+1), fmt.Sprintf("key%03d", i), int64(i))
		}
		kt.Insert(100, "07", int64(700))
		ki := b.CreateTableWithoutRowid("ki", "create table ki(k integer primary key, v) without rowid", 1)
		for i := range 40 {
			ki.Insert(int64(i+1), int64(i*2), fmt.Sprintf("v%d", i))
		}
		kn := b.CreateTableWithoutRowid("kn", "create table kn(k text collate nocase primary key, v) without rowid", 1)
		kn.Insert(1, "abc", int64(1))
		kn.Insert(2, "xyz", int64(2))
		kc := b.CreateTableWithoutRowid("kc", "create table kc(k text collate rot13 primary key, v) without rowid", 1)
		kc.Insert(1, "abc", int64(1))
		kc.Insert(2, "xyz", int64(2))
		kc2 := b.CreateTableWithoutRowid("kc2", "create table kc2(a int, b text, v, primary key(a, b)) without rowid", 2)
		for i := range 20 {
			kc2.Insert(int64(i+1), int64(i), fmt.Sprintf("b%d", i%2), "x")
		}
		rt := b.CreateTable("rt", "create table rt(n integer, nocase text collate nocase, plain text, num integer)")
		rt.Insert(1, int64(7), "ABC", "abc", int64(7))
	})
	bud := bigBudget()
	return openBytes(t, data, nil, nil, bud), bud
}

func keyOfSource(t *testing.T, d *sqlitedb.DB, col string) parse.JoinKey {
	t.Helper()
	rt, err := d.Table(t.Context(), "rt", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	r, ok, err := rt.Get(t.Context(), 1)
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	k, ok, err := sqlitedb.KeyOf(r, rt.Col(col))
	if err != nil || !ok {
		t.Fatalf("KeyOf %s: %v %v", col, ok, err)
	}
	return k
}

func keyTbl(t *testing.T, d *sqlitedb.DB, name string) *sqlitedb.Table {
	t.Helper()
	tb, err := d.Table(t.Context(), name, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return tb
}

func TestGetByKeyFindsAndMisses(t *testing.T) {
	d, _ := keyDB(t)
	kt := keyTbl(t, d, "kt")
	for i := range 40 {
		r, ok, err := kt.GetByKey(t.Context(), sqlitedb.LiteralTextKey([]byte(fmt.Sprintf("key%03d", i))))
		if err != nil || !ok {
			t.Fatalf("key%03d: %v %v", i, ok, err)
		}
		if n, _ := r.Int(1); n != int64(i) {
			t.Fatalf("key%03d: v = %d", i, n)
		}
		if s, _ := r.Text(0); string(s) != fmt.Sprintf("key%03d", i) {
			t.Fatalf("key%03d: k = %q", i, s)
		}
		r.Release()
	}
	for _, miss := range []string{"key", "key040", "KEY001", "zzz", "", "key001 "} {
		if _, ok, err := kt.GetByKey(t.Context(), sqlitedb.LiteralTextKey([]byte(miss))); err != nil || ok {
			t.Errorf("%q: found %v, err %v, want a clean miss", miss, ok, err)
		}
	}
	ki := keyTbl(t, d, "ki")
	if r, ok, err := ki.GetByKey(t.Context(), sqlitedb.LiteralIntKey(20)); err != nil || !ok {
		t.Fatalf("integer key: %v %v", ok, err)
	} else if s, _ := r.Text(1); string(s) != "v10" {
		t.Errorf("v = %q", s)
	}
	if _, ok, err := ki.GetByKey(t.Context(), sqlitedb.LiteralIntKey(21)); err != nil || ok {
		t.Errorf("a missing integer key: %v %v", ok, err)
	}
	// the key column's affinity applies to a literal, as the engine applies it to a constant
	if _, ok, err := ki.GetByKey(t.Context(), sqlitedb.LiteralTextKey([]byte("20"))); err != nil || !ok {
		t.Errorf("'20' against an INTEGER key column: %v %v", ok, err)
	}
	// the integer 7 becomes the text '7' for a TEXT key column: it is not the stored '07'
	if _, ok, err := kt.GetByKey(t.Context(), sqlitedb.LiteralIntKey(7)); err != nil || ok {
		t.Errorf("the integer 7 against a TEXT key column holding '07' and no '7': found %v, err %v, want a clean miss", ok, err)
	}
	if f, fine := sqlitedb.LiteralFloatKey(7.5); fine {
		if _, _, err := kt.GetByKey(t.Context(), f); !errors.Is(err, sqlitedb.ErrKeyUndecidable) {
			t.Errorf("7.5 against a TEXT key column: %v, want ErrKeyUndecidable", err)
		}
	}
	kn := keyTbl(t, d, "kn")
	if _, ok, err := kn.GetByKey(t.Context(), sqlitedb.LiteralTextKey([]byte("ABC"))); err != nil || !ok {
		t.Errorf("NOCASE: %v %v", ok, err)
	}
}

func TestGetByKeyAcceptsALiteralAndASameClassKey(t *testing.T) {
	d, _ := keyDB(t)
	kt := keyTbl(t, d, "kt")
	if _, ok, err := kt.GetByKey(t.Context(), sqlitedb.LiteralTextKey([]byte("key005"))); err != nil || !ok {
		t.Errorf("a literal: %v %v", ok, err)
	}
	same := keyOfSource(t, d, "plain") // TEXT, BINARY
	if same.Class != parse.ClassText {
		t.Fatalf("class %v", same.Class)
	}
	same.S = "key007"
	if _, ok, err := kt.GetByKey(t.Context(), same); err != nil || !ok {
		t.Errorf("a same-class, same-collation key: %v %v", ok, err)
	}
	ki := keyTbl(t, d, "ki")
	num := keyOfSource(t, d, "num") // INTEGER: numeric class
	num.I = 30
	if _, ok, err := ki.GetByKey(t.Context(), num); err != nil || !ok {
		t.Errorf("a numeric source key against an integer key column: %v %v", ok, err)
	}
	kn := keyTbl(t, d, "kn")
	nc := keyOfSource(t, d, "nocase") // TEXT, NOCASE
	if _, ok, err := kn.GetByKey(t.Context(), nc); err != nil || !ok {
		t.Errorf("a NOCASE source key against a NOCASE key column: %v %v", ok, err)
	}
}

func TestGetByKeyColumnSourcedKeyOfAnotherClassIsUndecidable(t *testing.T) {
	d, _ := keyDB(t)
	kt := keyTbl(t, d, "kt")
	// a numeric source 7 against the TEXT key column holding '07': the engine applies NUMERIC to
	// the TEXT column, so '07' becomes 7 and matches; the text '7' would not match '07' and a
	// proven "absent" would be wrong, so the key is refused
	n := keyOfSource(t, d, "n")
	if n.Class != parse.ClassNumeric || n.I != 7 {
		t.Fatalf("source key %+v", n)
	}
	if _, ok, err := kt.GetByKey(t.Context(), n); !errors.Is(err, sqlitedb.ErrKeyUndecidable) || ok {
		t.Errorf("a numeric source key against a TEXT key column: found %v, err %v, want ErrKeyUndecidable", ok, err)
	}
	// the other direction: a TEXT source key against a numeric key column
	ki := keyTbl(t, d, "ki")
	txt := keyOfSource(t, d, "plain")
	txt.S = "20"
	if _, ok, err := ki.GetByKey(t.Context(), txt); !errors.Is(err, sqlitedb.ErrKeyUndecidable) || ok {
		t.Errorf("a TEXT source key against an INTEGER key column: found %v, err %v, want ErrKeyUndecidable", ok, err)
	}
}

func TestGetByKeyColumnSourcedKeyOfAnotherCollationIsUndecidable(t *testing.T) {
	d, _ := keyDB(t)
	kt := keyTbl(t, d, "kt") // BINARY
	nc := keyOfSource(t, d, "nocase")
	nc.S = "key001"
	if _, ok, err := kt.GetByKey(t.Context(), nc); !errors.Is(err, sqlitedb.ErrKeyUndecidable) || ok {
		t.Errorf("a NOCASE source key against a BINARY key column: found %v, err %v, want ErrKeyUndecidable", ok, err)
	}
	kn := keyTbl(t, d, "kn") // NOCASE
	bin := keyOfSource(t, d, "plain")
	if _, ok, err := kn.GetByKey(t.Context(), bin); !errors.Is(err, sqlitedb.ErrKeyUndecidable) || ok {
		t.Errorf("a BINARY source key against a NOCASE key column: found %v, err %v, want ErrKeyUndecidable", ok, err)
	}
	if _, ok, err := kt.GetByKey(t.Context(), parse.JoinKey{Kind: parse.JoinText, S: "key001", Collation: "rot13"}); !errors.Is(err, sqlitedb.ErrKeyUndecidable) || ok {
		t.Errorf("an unsupported source collation: found %v, err %v", ok, err)
	}
}

func TestGetByKeyUndecidableIsErrKeyUndecidable(t *testing.T) {
	d, _ := keyDB(t)
	kc := keyTbl(t, d, "kc")
	_, ok, err := kc.GetByKey(t.Context(), sqlitedb.LiteralTextKey([]byte("abc")))
	if ok || !errors.Is(err, sqlitedb.ErrKeyUndecidable) {
		t.Fatalf("a key column under an unreproducible collation: found %v, err %v, want ErrKeyUndecidable", ok, err)
	}
	if errors.Is(err, sqlitedb.ErrCorrupt) {
		t.Errorf("an undecided lookup is reported as corruption: %v", err)
	}
	if !errors.Is(err, sqlitefile.ErrKeyUndecided) {
		t.Errorf("the library error left the chain: %v", err)
	}
}

func TestGetByKeyOnARowidTableIsUnsupportedSchema(t *testing.T) {
	d, _ := keyDB(t)
	rt := keyTbl(t, d, "rt")
	_, ok, err := rt.GetByKey(t.Context(), sqlitedb.LiteralIntKey(7))
	if ok || !errors.Is(err, sqlitedb.ErrUnsupportedSchema) {
		t.Fatalf("a rowid table: found %v, err %v, want ErrUnsupportedSchema", ok, err)
	}
	kt := keyTbl(t, d, "kt")
	for name, keys := range map[string][]parse.JoinKey{
		"no key":     nil,
		"two values": {sqlitedb.LiteralTextKey([]byte("a")), sqlitedb.LiteralTextKey([]byte("b"))},
	} {
		if _, ok, err := kt.GetByKey(t.Context(), keys...); ok || !errors.Is(err, sqlitedb.ErrUnsupportedSchema) {
			t.Errorf("%s: found %v, err %v, want ErrUnsupportedSchema", name, ok, err)
		}
	}
	if _, ok, err := kt.GetByKey(t.Context(), parse.JoinKey{}); ok || err != nil {
		t.Errorf("a key that matches nothing (NULL): found %v, err %v, want a decided miss", ok, err)
	}
	kc2 := keyTbl(t, d, "kc2")
	if _, ok, err := kc2.GetByKey(t.Context(), sqlitedb.LiteralIntKey(3), sqlitedb.LiteralTextKey([]byte("b1"))); err != nil || !ok {
		t.Errorf("a composite key: %v %v", ok, err)
	}
	if _, ok, err := kc2.GetByKey(t.Context(), sqlitedb.LiteralIntKey(3), sqlitedb.LiteralTextKey([]byte("b0"))); err != nil || ok {
		t.Errorf("a composite miss: %v %v", ok, err)
	}
	if _, _, err := rt.Get(t.Context(), 1); err != nil {
		t.Errorf("Get of a rowid table: %v", err)
	}
	if _, _, err := kt.Get(t.Context(), 1); !errors.Is(err, sqlitedb.ErrWithoutRowid) {
		t.Errorf("Get of a WITHOUT ROWID table: %v, want ErrWithoutRowid", err)
	}
}

func TestGetByKeyRowIsOwnedAndCharged(t *testing.T) {
	d, b := keyDB(t)
	kt := keyTbl(t, d, "kt")
	warm, _, _ := kt.GetByKey(t.Context(), sqlitedb.LiteralTextKey([]byte("key001")))
	warm.Release()
	base := b.used
	r1, ok, err := kt.GetByKey(t.Context(), sqlitedb.LiteralTextKey([]byte("key002")))
	if err != nil || !ok {
		t.Fatal(ok, err)
	}
	if b.used <= base {
		t.Fatalf("GetByKey charged nothing (used %d, base %d)", b.used, base)
	}
	r2, _, _ := kt.GetByKey(t.Context(), sqlitedb.LiteralTextKey([]byte("key003")))
	if s, _ := r1.Text(0); string(s) != "key002" { // the clone survives the later call
		t.Errorf("the first row changed to %q", s)
	}
	r1.Release()
	r1.Release()
	r2.Release()
	if b.used != base {
		t.Errorf("used %d after releasing both rows, want %d", b.used, base)
	}
	r3, _, _ := kt.GetByKey(t.Context(), sqlitedb.LiteralTextKey([]byte("key004")))
	_ = r3
	d.Release()
	if b.used != 0 {
		t.Errorf("used %d after DB.Release, want 0", b.used)
	}
	if _, _, err := kt.GetByKey(t.Context(), sqlitedb.LiteralTextKey([]byte("key004"))); !errors.Is(err, sqlitedb.ErrReleased) {
		t.Errorf("a released DB: %v, want ErrReleased", err)
	}
}
