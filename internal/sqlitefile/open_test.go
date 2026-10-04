package sqlitefile_test

import (
	"bytes"
	"errors"
	"io"
	"math"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

var errEOF = io.EOF

// failingReader returns err for every read.
type failingReader struct{ err error }

func (f failingReader) ReadAt([]byte, int64) (int, error) { return 0, f.err }

func openBytes(t *testing.T, data []byte) *sqlitefile.DB {
	t.Helper()
	db, err := sqlitefile.Open(bytes.NewReader(data), int64(len(data)), sqlitefile.Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return db
}

func warningCodes(db *sqlitefile.DB) []string {
	var out []string
	for _, w := range db.Warnings() {
		out = append(out, w.Code)
	}
	return out
}

func hasWarning(db *sqlitefile.DB, code string) bool {
	for _, c := range warningCodes(db) {
		if c == code {
			return true
		}
	}
	return false
}

func TestOpenHeaderFields(t *testing.T) {
	encs := map[int]sqlitefile.Encoding{1: sqlitefile.EncUTF8, 2: sqlitefile.EncUTF16LE, 3: sqlitefile.EncUTF16BE}
	for _, ps := range []int{512, 1024, 4096, 32768, 65536} {
		for _, res := range []int{0, 8, 32} {
			for enc := 1; enc <= 3; enc++ {
				for av := 0; av <= 2; av++ {
					o := sqlitetest.Options{PageSize: ps, Reserved: res, Encoding: enc, AutoVacuum: av, UserVersion: -42, AppID: 0x4d494e55, ChangeCounter: 7}
					data := sqlitetest.New(o).Bytes()
					db := openBytes(t, data)
					got := db.Info()
					wantAV := sqlitefile.AutoVacuum(av) // AVNone, AVFull, AVIncremental = 0, 1, 2
					wantRoot := uint32(0)
					if av != 0 {
						wantRoot = 1
					}
					want := sqlitefile.Info{
						FileSize: int64(ps), PageSize: ps, Reserved: res, UsableSize: ps - res,
						Encoding: encs[enc], EncodingValid: true,
						HeaderPages: 1, HeaderPagesValid: true, FilePages: 1, PageCount: 1,
						ChangeCounter: 7, VersionValidFor: 7, SQLiteVersion: sqlitetest.SQLiteVersion,
						SchemaFormat: 4, AutoVacuum: wantAV, LargestRoot: wantRoot,
						UserVersion: -42, ApplicationID: 0x4d494e55, WriteVersion: 1, ReadVersion: 1,
					}
					if !infoEqual(got, want) {
						t.Fatalf("page %d reserved %d enc %d av %d:\n got %+v\nwant %+v", ps, res, enc, av, got, want)
					}
				}
			}
		}
	}
}

// infoEqual compares two Infos (EngineRefuses is a slice).
func infoEqual(a, b sqlitefile.Info) bool {
	if len(a.EngineRefuses) != len(b.EngineRefuses) {
		return false
	}
	for i := range a.EngineRefuses {
		if a.EngineRefuses[i] != b.EngineRefuses[i] {
			return false
		}
	}
	a.EngineRefuses, b.EngineRefuses = nil, nil
	return a.FileSize == b.FileSize && a.PageSize == b.PageSize && a.Reserved == b.Reserved &&
		a.UsableSize == b.UsableSize && a.Encoding == b.Encoding && a.EncodingValid == b.EncodingValid &&
		a.HeaderPages == b.HeaderPages && a.HeaderPagesValid == b.HeaderPagesValid &&
		a.FilePages == b.FilePages && a.PageCount == b.PageCount && a.ChangeCounter == b.ChangeCounter &&
		a.VersionValidFor == b.VersionValidFor && a.SQLiteVersion == b.SQLiteVersion &&
		a.SchemaCookie == b.SchemaCookie && a.SchemaFormat == b.SchemaFormat &&
		a.FreelistTrunk == b.FreelistTrunk && a.FreelistCount == b.FreelistCount &&
		a.AutoVacuum == b.AutoVacuum && a.LargestRoot == b.LargestRoot && a.UserVersion == b.UserVersion &&
		a.ApplicationID == b.ApplicationID && a.WriteVersion == b.WriteVersion &&
		a.ReadVersion == b.ReadVersion && a.LockBytePage == b.LockBytePage
}

func TestEncodingString(t *testing.T) {
	for e, want := range map[sqlitefile.Encoding]string{
		sqlitefile.EncUTF8: "UTF-8", sqlitefile.EncUTF16LE: "UTF-16le", sqlitefile.EncUTF16BE: "UTF-16be",
	} {
		if e.String() != want {
			t.Errorf("Encoding(%d).String() = %q, want %q", e, e.String(), want)
		}
	}
	if s := sqlitefile.Encoding(9).String(); !strings.Contains(s, "9") {
		t.Errorf("unknown encoding string %q does not carry the value", s)
	}
	if sqlitefile.EncUTF8 != 1 || sqlitefile.EncUTF16LE != 2 || sqlitefile.EncUTF16BE != 3 {
		t.Error("Encoding values must be 1, 2, 3")
	}
	if sqlitefile.AVNone != 0 || sqlitefile.AVFull != 1 || sqlitefile.AVIncremental != 2 {
		t.Error("AutoVacuum values must be 0, 1, 2")
	}
}

func TestOpenCleanDatabaseHasNoWarnings(t *testing.T) {
	for _, o := range []sqlitetest.Options{
		{},
		{PageSize: 512},
		{PageSize: 65536},
		{Reserved: 32},
		{Encoding: 2},
		{Encoding: 3},
		{AutoVacuum: 1},
		{AutoVacuum: 2},
		{UserVersion: -1, AppID: 7},
	} {
		db := openBytes(t, sqlitetest.New(o).Bytes())
		if w := db.Warnings(); len(w) != 0 {
			t.Errorf("%+v: unexpected warnings %+v", o, w)
		}
		if r := db.Info().EngineRefuses; len(r) != 0 {
			t.Errorf("%+v: engine refuses a clean database: %v", o, r)
		}
	}
}

func TestOpenHeaderPagesRules(t *testing.T) {
	pad := func(b []byte, pages int) []byte { return append(b, make([]byte, pages*4096)...) }
	t.Run("valid counter: the header count wins over a longer file", func(t *testing.T) {
		db := openBytes(t, pad(sqlitetest.New(sqlitetest.Options{}).Bytes(), 2))
		i := db.Info()
		if i.PageCount != 1 || i.FilePages != 3 || !i.HeaderPagesValid || i.HeaderPages != 1 {
			t.Errorf("Info = %+v, want PageCount 1, FilePages 3, header pages valid", i)
		}
		if w := db.Warnings(); len(w) != 0 {
			t.Errorf("warnings %+v", w)
		}
	})
	t.Run("counter mismatch: the file size wins", func(t *testing.T) {
		b := sqlitetest.New(sqlitetest.Options{})
		b.SetHeaderPages(1, false)
		db := openBytes(t, pad(b.Bytes(), 2))
		i := db.Info()
		if i.PageCount != 3 || i.HeaderPagesValid || i.HeaderPages != 1 || i.FilePages != 3 {
			t.Errorf("Info = %+v, want PageCount 3 from the file, header count 1 not valid", i)
		}
		if !hasWarning(db, sqlitefile.WarnHdrCounterMismatch) {
			t.Errorf("warnings %v lack %s", warningCodes(db), sqlitefile.WarnHdrCounterMismatch)
		}
	})
	t.Run("header count zero: the file size wins, silently", func(t *testing.T) {
		b := sqlitetest.New(sqlitetest.Options{})
		b.SetHeaderPages(0, true)
		db := openBytes(t, pad(b.Bytes(), 1))
		if i := db.Info(); i.PageCount != 2 || i.HeaderPagesValid {
			t.Errorf("Info = %+v, want PageCount 2, header pages not valid", i)
		}
		if w := db.Warnings(); len(w) != 0 {
			t.Errorf("warnings %+v", w)
		}
	})
	t.Run("header count larger than the file", func(t *testing.T) {
		b := sqlitetest.New(sqlitetest.Options{})
		b.SetHeaderPages(10, true)
		db := openBytes(t, pad(b.Bytes(), 1))
		i := db.Info()
		if i.PageCount != 10 || i.FilePages != 2 || !i.HeaderPagesValid {
			t.Errorf("Info = %+v, want PageCount 10 (an upper bound), FilePages 2", i)
		}
		if !hasWarning(db, sqlitefile.WarnTruncatedFile) {
			t.Errorf("warnings %v lack %s", warningCodes(db), sqlitefile.WarnTruncatedFile)
		}
		if len(i.EngineRefuses) != 1 || !strings.Contains(i.EngineRefuses[0], "10") {
			t.Errorf("EngineRefuses = %q, want one reason naming the page count", i.EngineRefuses)
		}
	})
	t.Run("trailing partial page is ignored with a warning", func(t *testing.T) {
		db := openBytes(t, append(sqlitetest.New(sqlitetest.Options{}).Bytes(), make([]byte, 100)...))
		if i := db.Info(); i.FilePages != 1 || i.PageCount != 1 || i.FileSize != 4196 {
			t.Errorf("Info = %+v", i)
		}
		if !hasWarning(db, sqlitefile.WarnTruncatedFile) {
			t.Errorf("warnings %v lack %s", warningCodes(db), sqlitefile.WarnTruncatedFile)
		}
	})
}

func TestOpenFatalHeaders(t *testing.T) {
	setPage := func(v uint16) []byte {
		b := sqlitetest.New(sqlitetest.Options{})
		b.Patch(16, byte(v>>8), byte(v))
		return b.Bytes()
	}
	for _, v := range []uint16{0, 100, 513, 3000, 0xffff, 256} {
		data := setPage(v)
		db, err := sqlitefile.Open(bytes.NewReader(data), int64(len(data)), sqlitefile.Options{})
		var ce *sqlitefile.CorruptError
		if !errors.Is(err, sqlitefile.ErrCorrupt) || errors.Is(err, sqlitefile.ErrNotSQLite) || !errors.As(err, &ce) || db != nil {
			t.Errorf("page size field %#x: Open = (%v, %v), want a *CorruptError and no DB", v, db, err)
			continue
		}
		if ce.File != sqlitefile.FileDB || ce.Page != 1 {
			t.Errorf("page size field %#x: error names %s page %d", v, ce.File, ce.Page)
		}
	}
	// 0x0001 is the valid spelling of 65536: the file must then hold the page
	data := append(setPage(1), make([]byte, 65536-4096)...)
	if i := openBytes(t, data).Info(); i.PageSize != 65536 {
		t.Errorf("page size field 1: PageSize = %d, want 65536", i.PageSize)
	}
	// ... and 4096 bytes are not a 65536-byte page 1
	if _, err := sqlitefile.Open(bytes.NewReader(setPage(1)), 4096, sqlitefile.Options{}); !errors.Is(err, sqlitefile.ErrCorrupt) {
		t.Errorf("page size field 1 in a 4096-byte file: %v, want ErrCorrupt", err)
	}
	// usable size below 480
	for _, tc := range []struct {
		res int
		ok  bool
	}{{32, true}, {33, false}, {255, false}} {
		b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
		b.Patch(20, byte(tc.res))
		_, err := sqlitefile.Open(bytes.NewReader(b.Bytes()), 512, sqlitefile.Options{})
		if tc.ok != (err == nil) || (!tc.ok && !errors.Is(err, sqlitefile.ErrCorrupt)) {
			t.Errorf("512-byte page, reserved %d: err = %v", tc.res, err)
		}
	}
	// page 1 not wholly inside the file
	full := sqlitetest.New(sqlitetest.Options{}).Bytes()
	for _, n := range []int{101, 2000, 4095} {
		_, err := sqlitefile.Open(bytes.NewReader(full[:n]), int64(n), sqlitefile.Options{})
		if !errors.Is(err, sqlitefile.ErrCorrupt) {
			t.Errorf("file of %d bytes: %v, want ErrCorrupt", n, err)
		}
	}
	// size is authoritative: a longer reader does not help
	if _, err := sqlitefile.Open(bytes.NewReader(full), 4000, sqlitefile.Options{}); !errors.Is(err, sqlitefile.ErrCorrupt) {
		t.Errorf("size 4000 over a 4096-byte reader: %v, want ErrCorrupt", err)
	}
	if _, err := sqlitefile.Open(bytes.NewReader(full), -1, sqlitefile.Options{}); err == nil {
		t.Error("negative size accepted")
	}
}

// toleratedHeader is a header the engine may refuse but the reader opens.
type toleratedHeader struct {
	name    string
	patch   func(b *sqlitetest.Builder)
	code    string // warning expected; "" for none
	refuses bool   // the engine refuses this file (TestEngineRefusesToleratedHeaders)
	check   func(t *testing.T, i sqlitefile.Info)
}

var toleratedHeaders = []toleratedHeader{
	{"fractions 64/32/31", func(b *sqlitetest.Builder) { b.Patch(23, 31) }, sqlitefile.WarnHdrFractions, true, nil},
	{"fractions 63/32/32", func(b *sqlitetest.Builder) { b.Patch(21, 63) }, sqlitefile.WarnHdrFractions, true, nil},
	{"fractions 64/31/32", func(b *sqlitetest.Builder) { b.Patch(22, 31) }, sqlitefile.WarnHdrFractions, true, nil},
	{"encoding 0 (unset)", func(b *sqlitetest.Builder) { b.Patch(56, 0, 0, 0, 0) }, "", false, func(t *testing.T, i sqlitefile.Info) {
		if i.Encoding != sqlitefile.EncUTF8 || i.EncodingValid {
			t.Errorf("encoding 0: %v valid=%v, want UTF-8 not valid", i.Encoding, i.EncodingValid)
		}
	}},
	{"encoding 7", func(b *sqlitetest.Builder) { b.Patch(56, 0, 0, 0, 7) }, sqlitefile.WarnHdrEncodingInvalid, false, func(t *testing.T, i sqlitefile.Info) {
		if i.Encoding != sqlitefile.EncUTF8 || i.EncodingValid {
			t.Errorf("encoding 7: %v valid=%v, want UTF-8 not valid", i.Encoding, i.EncodingValid)
		}
	}},
	{"read version 3", func(b *sqlitetest.Builder) { b.Patch(19, 3) }, sqlitefile.WarnHdrVersionBytes, true, func(t *testing.T, i sqlitefile.Info) {
		if i.ReadVersion != 3 {
			t.Errorf("ReadVersion = %d", i.ReadVersion)
		}
	}},
	{"write version 3", func(b *sqlitetest.Builder) { b.Patch(18, 3) }, sqlitefile.WarnHdrVersionBytes, false, func(t *testing.T, i sqlitefile.Info) {
		if i.WriteVersion != 3 {
			t.Errorf("WriteVersion = %d", i.WriteVersion)
		}
	}},
}

func TestOpenToleratedHeaders(t *testing.T) {
	for _, c := range toleratedHeaders {
		t.Run(c.name, func(t *testing.T) {
			b := sqlitetest.New(sqlitetest.Options{})
			c.patch(b)
			db := openBytes(t, b.Bytes())
			i := db.Info()
			if c.code == "" {
				if len(db.Warnings()) != 0 {
					t.Errorf("warnings %+v, want none", db.Warnings())
				}
			} else if !hasWarning(db, c.code) {
				t.Errorf("warnings %v lack %s", warningCodes(db), c.code)
			}
			if c.refuses != (len(i.EngineRefuses) > 0) {
				t.Errorf("EngineRefuses = %q, want non-empty = %v", i.EngineRefuses, c.refuses)
			}
			if c.check != nil {
				c.check(t, i)
			}
		})
	}
}

func TestOpenIOErrorIsNotCorrupt(t *testing.T) {
	boom := errors.New("device gone")
	db, err := sqlitefile.Open(failingReader{err: boom}, 8192, sqlitefile.Options{})
	if db != nil || !errors.Is(err, boom) {
		t.Fatalf("Open = (%v, %v), want the reader's error wrapped", db, err)
	}
	if errors.Is(err, sqlitefile.ErrCorrupt) || errors.Is(err, sqlitefile.ErrNotSQLite) {
		t.Errorf("an I/O error became a verdict: %v", err)
	}
}

func TestOpenShortReadBeforeSizeIsIOError(t *testing.T) {
	data := sqlitetest.New(sqlitetest.Options{}).Bytes()
	// the caller says 3 pages but the reader ends after 150 bytes: its size lied
	db, err := sqlitefile.Open(&countingReader{data: data[:150]}, 4096*3, sqlitefile.Options{})
	if db != nil || err == nil {
		t.Fatalf("Open = (%v, %v), want an I/O error", db, err)
	}
	if errors.Is(err, sqlitefile.ErrCorrupt) || errors.Is(err, sqlitefile.ErrNotSQLite) {
		t.Errorf("a short read before size became a verdict: %v", err)
	}
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("error %v does not wrap io.ErrUnexpectedEOF", err)
	}
	// a page read after Open fails the same way
	open, err := sqlitefile.Open(bytes.NewReader(data), 4096*3, sqlitefile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = open.RawPage(2)
	if err == nil || errors.Is(err, sqlitefile.ErrCorrupt) || errors.Is(err, sqlitefile.ErrPageUnavailable) {
		t.Errorf("RawPage(2) with a reader that lied about its size: %v, want a plain I/O error", err)
	}
}

func TestOpenGeometryBeyondSizeIsPageUnavailable(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{})
	b.SetHeaderPages(10, true)
	data := append(b.Bytes(), make([]byte, 2*4096)...)
	data[4096+7] = 0xaa // page 2 has recognizable content
	r := &countingReader{data: data}
	db, err := sqlitefile.Open(r, int64(len(data)), sqlitefile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !hasWarning(db, sqlitefile.WarnTruncatedFile) || db.Info().PageCount != 10 {
		t.Errorf("warnings %v, PageCount %d", warningCodes(db), db.Info().PageCount)
	}
	p2, err := db.RawPage(2)
	if err != nil || len(p2) != 4096 || p2[7] != 0xaa {
		t.Errorf("RawPage(2) = (%d bytes, %v)", len(p2), err)
	}
	for _, pg := range []uint32{0, 4, 10, 11, math.MaxUint32} {
		if _, err := db.RawPage(pg); !errors.Is(err, sqlitefile.ErrPageUnavailable) {
			t.Errorf("RawPage(%d) = %v, want ErrPageUnavailable", pg, err)
		}
	}
	if r.maxAt > int64(len(data)) {
		t.Errorf("a read reached offset %d, size is %d", r.maxAt, len(data))
	}
	// a smaller declared size hides pages the reader still holds
	r2 := &countingReader{data: data}
	db2, err := sqlitefile.Open(r2, 4096*2, sqlitefile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db2.RawPage(3); !errors.Is(err, sqlitefile.ErrPageUnavailable) {
		t.Errorf("RawPage(3) with size of 2 pages = %v, want ErrPageUnavailable", err)
	}
	if r2.maxAt > 4096*2 {
		t.Errorf("read past the declared size: offset %d", r2.maxAt)
	}
	// MaxPages is a hard limit on addressable pages
	db3, err := sqlitefile.Open(bytes.NewReader(data), int64(len(data)), sqlitefile.Options{Limits: sqlitefile.Limits{MaxPages: 2}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db3.RawPage(2); err != nil {
		t.Errorf("RawPage(2) under MaxPages 2: %v", err)
	}
	if _, err := db3.RawPage(3); !errors.Is(err, sqlitefile.ErrPageUnavailable) {
		t.Errorf("RawPage(3) under MaxPages 2 = %v, want ErrPageUnavailable", err)
	}
}

func TestOpenRecoversPanic(t *testing.T) {
	data := sqlitetest.New(sqlitetest.Options{}).Bytes()
	db, err := sqlitefile.OpenWithHook(bytes.NewReader(data), int64(len(data)), sqlitefile.Options{}, func(string) { panic("boom") })
	var pe *sqlitefile.PanicError
	if db != nil || !errors.As(err, &pe) || !errors.Is(err, sqlitefile.ErrInternal) {
		t.Errorf("Open = (%v, %v), want a recovered *PanicError and no DB", db, err)
	}
}

func TestInfoAndWarningsAreCopies(t *testing.T) {
	b := sqlitetest.New(sqlitetest.Options{})
	b.Patch(23, 31)
	db := openBytes(t, b.Bytes())
	i := db.Info()
	if len(i.EngineRefuses) == 0 {
		t.Fatal("setup: no EngineRefuses")
	}
	i.EngineRefuses[0] = "changed"
	if db.Info().EngineRefuses[0] == "changed" {
		t.Error("Info shares its EngineRefuses slice with the DB")
	}
	w := db.Warnings()
	if len(w) == 0 {
		t.Fatal("setup: no warnings")
	}
	w[0].Msg = "changed"
	if db.Warnings()[0].Msg == "changed" {
		t.Error("Warnings shares its slice with the DB")
	}
}

func TestLockBytePageGolden(t *testing.T) {
	for ps, want := range map[int]uint32{512: 2097153, 1024: 1048577, 4096: 262145, 65536: 16385} {
		if got := sqlitefile.LockBytePage(ps); got != want {
			t.Errorf("LockBytePage(%d) = %d, want %d", ps, got, want)
		}
	}
	if sqlitefile.LockBytePage(0) != 0 || sqlitefile.LockBytePage(-4096) != 0 {
		t.Error("LockBytePage of a non-positive size must be 0")
	}
}

func TestPageOffset(t *testing.T) {
	for _, tc := range []struct {
		ps   int
		pg   uint32
		want int64
	}{
		{4096, 0, 0},
		{4096, 1, 0},
		{4096, 2, 4096},
		{512, 1000, 999 * 512},
		{65536, math.MaxUint32, int64(math.MaxUint32-1) * 65536},
	} {
		if got := sqlitefile.PageOffset(tc.ps, tc.pg); got != tc.want {
			t.Errorf("PageOffset(%d, %d) = %d, want %d", tc.ps, tc.pg, got, tc.want)
		}
	}
}

// sparseReader serves head and zeros elsewhere, as a huge file; it records
// the highest offset read.
type sparseReader struct {
	head  []byte
	size  int64
	maxAt int64
}

func (s *sparseReader) ReadAt(p []byte, off int64) (int, error) {
	s.maxAt = max(s.maxAt, off+int64(len(p)))
	if off < 0 || off >= s.size {
		return 0, io.EOF
	}
	n := int(min(int64(len(p)), s.size-off))
	clear(p[:n])
	if off < int64(len(s.head)) {
		copy(p[:n], s.head[off:])
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

func TestInfoLockBytePageAndHugeFiles(t *testing.T) {
	head := sqlitetest.New(sqlitetest.Options{}).Bytes()
	const gib = int64(1) << 30
	for _, tc := range []struct {
		size int64
		want uint32
	}{{gib, 0}, {gib + 1, 262145}, {gib + 4096, 262145}, {gib - 4096, 0}} {
		r := &sparseReader{head: head, size: tc.size}
		db, err := sqlitefile.Open(r, tc.size, sqlitefile.Options{})
		if err != nil {
			t.Fatal(err)
		}
		if got := db.Info().LockBytePage; got != tc.want {
			t.Errorf("size %d: LockBytePage = %d, want %d", tc.size, got, tc.want)
		}
		if r.maxAt > 4096 {
			t.Errorf("size %d: Open read up to offset %d", tc.size, r.maxAt)
		}
	}
	// more whole pages than a uint32 holds: clamped, and said
	small := sqlitetest.New(sqlitetest.Options{PageSize: 512})
	size := int64(512) * (int64(math.MaxUint32) + 5)
	r := &sparseReader{head: small.Bytes(), size: size}
	db, err := sqlitefile.Open(r, size, sqlitefile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := db.Info().FilePages; got != math.MaxUint32 {
		t.Errorf("FilePages = %d, want it clamped to %d", got, uint32(math.MaxUint32))
	}
	if !hasWarning(db, sqlitefile.WarnPageCountClamped) {
		t.Errorf("warnings %v lack %s", warningCodes(db), sqlitefile.WarnPageCountClamped)
	}
}
