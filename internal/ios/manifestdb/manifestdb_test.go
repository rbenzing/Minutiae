package manifestdb_test

import (
	"bytes"
	"context"
	"errors"
	"math"
	"strings"
	"testing"

	"howett.net/plist"

	"github.com/rbenzing/minutiae/internal/ios/manifestdb"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

const filesDDL = "CREATE TABLE Files (fileID TEXT PRIMARY KEY, domain TEXT, relativePath TEXT, flags INTEGER, file BLOB)"

var (
	idA = "aa" + strings.Repeat("1", 38)
	idB = "bb" + strings.Repeat("2", 38)
	idC = "cc" + strings.Repeat("3", 38)
	idD = "dd" + strings.Repeat("4", 38)
	idE = "ee" + strings.Repeat("5", 38)
	idF = "ff" + strings.Repeat("6", 38) // valid, but never wanted
)

// mbFile is the NSKeyedArchiver plist of an MBFile whose root object has Size.
func mbFile(t *testing.T, size any) []byte {
	t.Helper()
	obj := map[string]any{"$class": plist.UID(2)}
	if size != nil {
		obj["Size"] = size
	}
	b, err := plist.Marshal(map[string]any{
		"$archiver": "NSKeyedArchiver", "$version": uint64(100000),
		"$top":     map[string]any{"root": plist.UID(1)},
		"$objects": []any{"$null", obj, map[string]any{"$classname": "MBFile"}},
	}, plist.BinaryFormat)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func buildManifest(t *testing.T, rows ...[]any) []byte {
	t.Helper()
	b := sqlitetest.New(sqlitetest.Options{PageSize: 4096})
	tab := b.CreateTable("Files", filesDDL)
	for i, r := range rows {
		tab.Insert(int64(i+1), r...)
	}
	return b.Bytes()
}

func TestDeclaredSizes(t *testing.T) {
	db := buildManifest(t,
		[]any{idA, "d", "p1", int64(1), mbFile(t, uint64(1234))},
		[]any{idB, "d", "p2", int64(1), mbFile(t, nil)},        // no Size
		[]any{idC, "d", "p3", int64(1), []byte("not a plist")}, // unreadable blob
		[]any{idD, "d", "p4", int64(1), mbFile(t, uint64(5))},  // duplicated with two sizes
		[]any{idD, "d", "p4b", int64(1), mbFile(t, uint64(6))},
		[]any{idE, "d", "p5", int64(1), mbFile(t, uint64(7))}, // duplicated, same size
		[]any{idE, "d", "p5b", int64(1), mbFile(t, uint64(7))},
		[]any{idF, "d", "p6", int64(1), mbFile(t, uint64(9))}, // present, not wanted
		[]any{"not-a-file-id", "d", "p7", int64(1), mbFile(t, uint64(9))},
	)
	d, err := manifestdb.Read(context.Background(), bytes.NewReader(db), int64(len(db)), func(id string) bool { return id != idF })
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		id    string
		size  int64
		known bool
	}{
		{idA, 1234, true},
		{idB, 0, false},
		{idC, 0, false},
		{idD, 0, false},
		{idE, 7, true},
		{idF, 0, false},
		{"not-a-file-id", 0, false},
		{"zz" + strings.Repeat("9", 38), 0, false},
	} {
		if size, known := d.Size(tc.id); size != tc.size || known != tc.known {
			t.Errorf("Size(%q) = %d, %v; want %d, %v", tc.id, size, known, tc.size, tc.known)
		}
	}
}

func TestValidFileID(t *testing.T) {
	for id, want := range map[string]bool{
		idA: true, strings.Repeat("0", 40): true,
		"": false, "abc": false, strings.Repeat("a", 39): false, strings.Repeat("a", 41): false,
		strings.Repeat("A", 40): false, strings.Repeat("g", 40): false, strings.Repeat("a", 4096): false,
	} {
		if got := manifestdb.ValidFileID(id); got != want {
			t.Errorf("ValidFileID(%.12q) = %v, want %v", id, got, want)
		}
	}
}

func TestReadRefusesWhatIsNotAManifest(t *testing.T) {
	all := func(string) bool { return true }
	for name, data := range map[string][]byte{
		"encrypted-looking bytes": bytes.Repeat([]byte{0xA5}, 8192),
		"empty":                   nil,
	} {
		if _, err := manifestdb.Read(context.Background(), bytes.NewReader(data), int64(len(data)), all); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	other := sqlitetest.New(sqlitetest.Options{})
	other.CreateTable("Other", "CREATE TABLE Other (a INTEGER)")
	if _, err := manifestdb.Read(context.Background(), bytes.NewReader(other.Bytes()), int64(len(other.Bytes())), all); err == nil {
		t.Error("a database without a Files table was accepted")
	}
	// A claimed size over the cap is refused before anything is read.
	if _, err := manifestdb.Read(context.Background(), bytes.NewReader(nil), manifestdb.MaxFileBytes+1, all); !errors.Is(err, manifestdb.ErrTooLarge) {
		t.Errorf("oversized manifest err = %v", err)
	}
}

// W43: a Size that cannot be read as a non-negative int64 stays unknown; it is
// never coerced into a number.
func TestUnreadableSizesStayUnknown(t *testing.T) {
	root := func(uid plist.UID, size any) []byte {
		b, err := plist.Marshal(map[string]any{
			"$archiver": "NSKeyedArchiver", "$version": uint64(100000),
			"$top":     map[string]any{"root": uid},
			"$objects": []any{"$null", map[string]any{"Size": size, "$class": plist.UID(2)}, map[string]any{"$classname": "MBFile"}},
		}, plist.BinaryFormat)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	ids := []string{idA, idB, idC, idD, idE, idF}
	db := buildManifest(t,
		[]any{ids[0], "d", "p", int64(1), root(1, int64(-1))},
		[]any{ids[1], "d", "p", int64(1), root(1, uint64(math.MaxInt64)+1)},
		[]any{ids[2], "d", "p", int64(1), root(1, 1.5)},
		[]any{ids[3], "d", "p", int64(1), root(1, "12")},
		[]any{ids[4], "d", "p", int64(1), root(9, uint64(5))},             // $top.root past $objects
		[]any{ids[5], "d", "p", int64(1), root(1, uint64(math.MaxInt64))}, // the largest readable size
	)
	d, err := manifestdb.Read(context.Background(), bytes.NewReader(db), int64(len(db)), func(string) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	for i, id := range ids[:5] {
		if n, known := d.Size(id); known || n != 0 {
			t.Errorf("case %d: Size = %d, %v; want unknown", i, n, known)
		}
	}
	if n, known := d.Size(ids[5]); !known || n != math.MaxInt64 {
		t.Errorf("the largest int64 size = %d, %v", n, known)
	}
}

func TestTooManyRowsIsAnErrorNotAPartialRead(t *testing.T) {
	db := buildManifest(t,
		[]any{idA, "d", "p1", int64(1), mbFile(t, uint64(1))},
		[]any{idB, "d", "p2", int64(1), mbFile(t, uint64(2))},
		[]any{idC, "d", "p3", int64(1), mbFile(t, uint64(3))},
	)
	t.Cleanup(manifestdb.SetMaxRows(2))
	d, err := manifestdb.Read(context.Background(), bytes.NewReader(db), int64(len(db)), func(string) bool { return true })
	if !errors.Is(err, manifestdb.ErrTooManyRows) || d != nil {
		t.Fatalf("Read = %v, %v; want ErrTooManyRows and no partial result", d, err)
	}
}

// W44: a cancelled context stops the read.
func TestReadHonoursCancel(t *testing.T) {
	db := buildManifest(t, []any{idA, "d", "p1", int64(1), mbFile(t, uint64(1))})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d, err := manifestdb.Read(ctx, bytes.NewReader(db), int64(len(db)), func(string) bool { return true })
	if !errors.Is(err, context.Canceled) || d != nil {
		t.Fatalf("Read = %v, %v; want context.Canceled", d, err)
	}
}

// A cap equal to the row count is not an error: only more rows than the cap are.
func TestRowCapEqualToRowsIsAccepted(t *testing.T) {
	db := buildManifest(t,
		[]any{idA, "d", "p1", int64(1), mbFile(t, uint64(1))},
		[]any{idB, "d", "p2", int64(1), mbFile(t, uint64(2))},
		[]any{idC, "d", "p3", int64(1), mbFile(t, uint64(3))},
	)
	t.Cleanup(manifestdb.SetMaxRows(3))
	d, err := manifestdb.Read(context.Background(), bytes.NewReader(db), int64(len(db)), func(string) bool { return true })
	if err != nil {
		t.Fatalf("Read with the cap equal to the rows: %v", err)
	}
	if n, known := d.Size(idC); !known || n != 3 {
		t.Fatalf("Size(idC) = %d, %v", n, known)
	}
}

// W44: a cancel in the middle of the row scan stops it with no partial result.
func TestReadHonoursCancelInTheMiddleOfTheScan(t *testing.T) {
	db := buildManifest(t,
		[]any{idA, "d", "p1", int64(1), mbFile(t, uint64(1))},
		[]any{idB, "d", "p2", int64(1), mbFile(t, uint64(2))},
		[]any{idC, "d", "p3", int64(1), mbFile(t, uint64(3))},
		[]any{idD, "d", "p4", int64(1), mbFile(t, uint64(4))},
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	seen := 0
	d, err := manifestdb.Read(ctx, bytes.NewReader(db), int64(len(db)), func(string) bool {
		if seen++; seen == 2 {
			cancel()
		}
		return true
	})
	if !errors.Is(err, context.Canceled) || d != nil {
		t.Fatalf("Read = %v, %v; want context.Canceled and no result", d, err)
	}
	if seen >= 4 {
		t.Fatalf("the scan went on to row %d after the cancel", seen)
	}
}
