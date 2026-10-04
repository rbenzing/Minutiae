package sqlitefile_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"math/rand/v2"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// pseudoRandom returns n deterministic high-entropy bytes.
func pseudoRandom(n int, seed uint64) []byte {
	r := rand.New( //nolint:gosec // deterministic test data, not security
		rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	return b
}

func withPrefix(prefix []byte, n int, seed uint64) []byte {
	return append(append([]byte(nil), prefix...), pseudoRandom(n, seed)...)
}

// countingReader records every ReadAt.
type countingReader struct {
	data  []byte
	calls int
	bytes int
	maxAt int64 // highest offset+length requested
}

func (c *countingReader) ReadAt(p []byte, off int64) (int, error) {
	c.calls++
	c.bytes += len(p)
	c.maxAt = max(c.maxAt, off+int64(len(p)))
	if off >= int64(len(c.data)) {
		return 0, errEOF
	}
	n := copy(p, c.data[off:])
	if n < len(p) {
		return n, errEOF
	}
	return n, nil
}

func TestSniffTable(t *testing.T) {
	validDB := sqlitetest.New(sqlitetest.Options{}).Bytes()
	page1Bad := sqlitetest.New(sqlitetest.Options{})
	page1Bad.Patch(100, 0x00) // not a table b-tree flag
	walLE := make([]byte, 32)
	binary.BigEndian.PutUint32(walLE[0:], 0x377f0682)
	binary.BigEndian.PutUint32(walLE[8:], 4096)
	walBE := bytes.Clone(walLE)
	binary.BigEndian.PutUint32(walBE[0:], 0x377f0683)
	journal := make([]byte, 512)
	copy(journal, []byte{0xd9, 0xd5, 0x05, 0xf9, 0x20, 0xa1, 0x63, 0xd7})
	binary.BigEndian.PutUint32(journal[24:], 4096)
	magicRandom := bytes.Clone(validDB[:100]) // a valid header, then junk
	magicRandom = append(magicRandom, 0x11)   // not 0x05 or 0x0d
	magicRandom = append(magicRandom, pseudoRandom(4000, 77)...)

	cases := []struct {
		name       string
		data       []byte
		kind       sqlitefile.SniffKind
		pageSize   int
		openReason sqlitefile.NotSQLiteReason // "" when Open succeeds
		encrypted  bool
	}{
		{"empty", nil, sqlitefile.SniffEmpty, 0, sqlitefile.ReasonEmpty, false},
		{"99 bytes of a database header", validDB[:99], sqlitefile.SniffTooSmall, 0, sqlitefile.ReasonTooSmall, false},
		{"valid empty database", validDB, sqlitefile.SniffSQLite, 4096, "", false},
		{"non b-tree flag at 100", page1Bad.Bytes(), sqlitefile.SniffNotSQLite, 0, sqlitefile.ReasonPage1Invalid, false},
		{"wal magic, little-endian words", walLE, sqlitefile.SniffWAL, 4096, sqlitefile.ReasonBadMagic, false},
		{"wal magic, big-endian words", walBE, sqlitefile.SniffWAL, 4096, sqlitefile.ReasonBadMagic, false},
		{"journal magic", journal, sqlitefile.SniffJournal, 4096, sqlitefile.ReasonBadMagic, false},
		{"text file", bytes.Repeat([]byte("hello, evidence\n"), 100), sqlitefile.SniffNotSQLite, 0, sqlitefile.ReasonBadMagic, false},
		{"zeros", make([]byte, 4096), sqlitefile.SniffNotSQLite, 0, sqlitefile.ReasonBadMagic, false},
		{"json", []byte(`{"a":1}`), sqlitefile.SniffTooSmall, 0, sqlitefile.ReasonTooSmall, false},
		{"4 KiB of random bytes", pseudoRandom(4096, 1), sqlitefile.SniffLooksEncrypted, 0, sqlitefile.ReasonBadMagic, true},
		{"magic, invalid page 1, random body", magicRandom, sqlitefile.SniffLooksEncrypted, 0, sqlitefile.ReasonPage1Invalid, true},
		{"gzip then random", withPrefix([]byte{0x1f, 0x8b, 0x08, 0x00}, 4096, 2), sqlitefile.SniffNotSQLite, 0, sqlitefile.ReasonBadMagic, false},
		{"zip then random", withPrefix([]byte("PK\x03\x04"), 4096, 3), sqlitefile.SniffNotSQLite, 0, sqlitefile.ReasonBadMagic, false},
		{"png then random", withPrefix([]byte("\x89PNG\r\n\x1a\n"), 4096, 4), sqlitefile.SniffNotSQLite, 0, sqlitefile.ReasonBadMagic, false},
		{"jpeg then random", withPrefix([]byte{0xff, 0xd8, 0xff, 0xe0}, 4096, 5), sqlitefile.SniffNotSQLite, 0, sqlitefile.ReasonBadMagic, false},
		{"bplist then random", withPrefix([]byte("bplist00"), 4096, 6), sqlitefile.SniffNotSQLite, 0, sqlitefile.ReasonBadMagic, false},
		{"xml then random", withPrefix([]byte("<?xml version"), 4096, 7), sqlitefile.SniffNotSQLite, 0, sqlitefile.ReasonBadMagic, false},
		{"300 random bytes: below 512, no hint", pseudoRandom(300, 8), sqlitefile.SniffNotSQLite, 0, sqlitefile.ReasonBadMagic, false},
		{"511 random bytes: no hint", pseudoRandom(511, 9), sqlitefile.SniffNotSQLite, 0, sqlitefile.ReasonBadMagic, false},
		{"512 random bytes: hint", pseudoRandom(512, 9), sqlitefile.SniffLooksEncrypted, 0, sqlitefile.ReasonBadMagic, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, err := sqlitefile.Sniff(bytes.NewReader(tc.data), int64(len(tc.data)))
			if err != nil {
				t.Fatalf("Sniff: %v", err)
			}
			if s.Kind != tc.kind || s.Size != int64(len(tc.data)) || s.PageSize != tc.pageSize {
				t.Errorf("Sniff = {kind %d size %d page size %d}, want {kind %d size %d page size %d}",
					s.Kind, s.Size, s.PageSize, tc.kind, len(tc.data), tc.pageSize)
			}
			db, err := sqlitefile.Open(bytes.NewReader(tc.data), int64(len(tc.data)), sqlitefile.Options{})
			if tc.openReason == "" {
				if err != nil || db == nil {
					t.Fatalf("Open: %v", err)
				}
				return
			}
			var ne *sqlitefile.NotSQLiteError
			if !errors.As(err, &ne) || db != nil {
				t.Fatalf("Open = (%v, %v), want a *NotSQLiteError and no DB", db, err)
			}
			if ne.Reason != tc.openReason || ne.LooksEncrypted != tc.encrypted || ne.Size != int64(len(tc.data)) {
				t.Errorf("NotSQLiteError = %+v, want reason %q encrypted %v", ne, tc.openReason, tc.encrypted)
			}
			if !errors.Is(err, sqlitefile.ErrNotSQLite) || errors.Is(err, sqlitefile.ErrLooksEncrypted) != tc.encrypted {
				t.Errorf("errors.Is: ErrNotSQLite %v, ErrLooksEncrypted %v (want %v)",
					errors.Is(err, sqlitefile.ErrNotSQLite), errors.Is(err, sqlitefile.ErrLooksEncrypted), tc.encrypted)
			}
		})
	}
}

// TestSniffEncryptedLooksInvalidNeverGuessed: random bytes are reported as
// an invalid header with the hint; nothing is decoded and no DB comes back.
func TestSniffEncryptedLooksInvalidNeverGuessed(t *testing.T) {
	data := pseudoRandom(4096, 42)
	db, err := sqlitefile.Open(bytes.NewReader(data), int64(len(data)), sqlitefile.Options{})
	var ne *sqlitefile.NotSQLiteError
	if !errors.As(err, &ne) {
		t.Fatalf("Open error = %v, want *NotSQLiteError", err)
	}
	if !errors.Is(err, sqlitefile.ErrNotSQLite) || !errors.Is(err, sqlitefile.ErrLooksEncrypted) {
		t.Errorf("error does not match ErrNotSQLite and ErrLooksEncrypted: %v", err)
	}
	if db != nil {
		t.Error("Open returned a DB for random bytes")
	}
	if ne.Entropy < 7.2 {
		t.Errorf("entropy hint = %.2f, want >= 7.2", ne.Entropy)
	}
}

func TestSniffReadsAtMost4096(t *testing.T) {
	data := sqlitetest.New(sqlitetest.Options{PageSize: 65536}).Bytes()
	data = append(data, make([]byte, 1<<20)...)
	r := &countingReader{data: data}
	if _, err := sqlitefile.Sniff(r, int64(len(data))); err != nil {
		t.Fatal(err)
	}
	if r.bytes > 4096 || r.maxAt > 4096 {
		t.Errorf("Sniff requested %d bytes up to offset %d, want at most 4096", r.bytes, r.maxAt)
	}
}

// TestSniffNeverReadsPastSize: size is authoritative even if the reader holds
// more.
func TestSniffNeverReadsPastSize(t *testing.T) {
	data := sqlitetest.New(sqlitetest.Options{}).Bytes()
	r := &countingReader{data: data}
	s, err := sqlitefile.Sniff(r, 50)
	if err != nil {
		t.Fatal(err)
	}
	if s.Kind != sqlitefile.SniffTooSmall || s.Size != 50 {
		t.Errorf("Sniff = %+v, want too small, size 50", s)
	}
	if r.maxAt > 50 {
		t.Errorf("read up to offset %d with size 50", r.maxAt)
	}
}

func TestSniffIOErrors(t *testing.T) {
	boom := errors.New("device gone")
	_, err := sqlitefile.Sniff(failingReader{err: boom}, 8192)
	if !errors.Is(err, boom) || errors.Is(err, sqlitefile.ErrCorrupt) || errors.Is(err, sqlitefile.ErrNotSQLite) {
		t.Errorf("non-EOF error: got %v, want it wrapped and not a corruption or format verdict", err)
	}
	// the reader ends below the declared size: an I/O error, not a verdict
	_, err = sqlitefile.Sniff(bytes.NewReader(make([]byte, 150)), 8192)
	if err == nil || errors.Is(err, sqlitefile.ErrCorrupt) || errors.Is(err, sqlitefile.ErrNotSQLite) {
		t.Errorf("short read before size: got %v, want a plain I/O error", err)
	}
	if _, err := sqlitefile.Sniff(bytes.NewReader(nil), -1); err == nil {
		t.Error("negative size accepted")
	}
}

func TestEntropy(t *testing.T) {
	if e := sqlitefile.Entropy(nil); e != 0 {
		t.Errorf("Entropy(nil) = %v", e)
	}
	if e := sqlitefile.Entropy(make([]byte, 100)); e != 0 {
		t.Errorf("Entropy(zeros) = %v", e)
	}
	if e := sqlitefile.Entropy(bytes.Repeat([]byte{1, 2}, 50)); math.Abs(e-1) > 1e-12 {
		t.Errorf("Entropy(two equally likely values) = %v, want 1", e)
	}
	all := make([]byte, 1024)
	for i := range all {
		all[i] = byte(i)
	}
	if e := sqlitefile.Entropy(all); math.Abs(e-8) > 1e-12 {
		t.Errorf("Entropy(uniform) = %v, want 8", e)
	}
	if e := sqlitefile.Entropy(pseudoRandom(4096, 5)); e < 7.9 {
		t.Errorf("Entropy(pseudo-random 4 KiB) = %v, want > 7.9", e)
	}
}
