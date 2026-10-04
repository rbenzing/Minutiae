// Package sqlitetest builds SQLite database files byte by byte for tests of
// internal/sqlitefile. Its encoders are independent of sqlitefile's decoders
// (it imports nothing of this module), so a test that compares the two is not
// circular. It is test-only: archtest forbids any non-test package from
// importing it.
package sqlitetest

import "fmt"

// SQLiteVersion is the SQLITE_VERSION_NUMBER the builder writes at offset 96.
const SQLiteVersion = 3045000

// Options select the header fields of a new database. Zero values mean the
// defaults: page size 4096, no reserved bytes, UTF-8, no autovacuum, change
// counter 1.
type Options struct {
	PageSize, Reserved int
	Encoding           int // 1 UTF-8, 2 UTF-16le, 3 UTF-16be
	AutoVacuum         int // 0 none, 1 full, 2 incremental
	UserVersion        int32
	AppID              uint32
	ChangeCounter      uint32
}

// Builder holds a database as pages. New returns one empty page (page 1 is an
// empty table leaf: an empty schema).
type Builder struct {
	o     Options
	pages [][]byte
}

// New returns a builder for o. Invalid options (a page size that is not a
// power of two in 512..65536, usable size below 480, an encoding or an
// autovacuum mode out of range) panic: this is a test helper.
func New(o Options) *Builder {
	if o.PageSize == 0 {
		o.PageSize = 4096
	}
	if o.Encoding == 0 {
		o.Encoding = 1
	}
	if o.ChangeCounter == 0 {
		o.ChangeCounter = 1
	}
	switch {
	case o.PageSize < 512 || o.PageSize > 65536 || o.PageSize&(o.PageSize-1) != 0:
		panic(fmt.Sprintf("sqlitetest: page size %d is not a power of two in 512..65536", o.PageSize))
	case o.Reserved < 0 || o.Reserved > 255 || o.PageSize-o.Reserved < 480:
		panic(fmt.Sprintf("sqlitetest: reserved %d leaves a usable size below 480", o.Reserved))
	case o.Encoding < 1 || o.Encoding > 3:
		panic(fmt.Sprintf("sqlitetest: encoding %d is not 1..3", o.Encoding))
	case o.AutoVacuum < 0 || o.AutoVacuum > 2:
		panic(fmt.Sprintf("sqlitetest: autovacuum %d is not 0..2", o.AutoVacuum))
	}
	b := &Builder{o: o, pages: [][]byte{make([]byte, o.PageSize)}}
	writeHeader(b.pages[0], o)
	emptyTableLeaf(b.pages[0], 100, o.PageSize-o.Reserved)
	return b
}

// Bytes returns a copy of the whole file.
func (b *Builder) Bytes() []byte {
	out := make([]byte, 0, len(b.pages)*b.o.PageSize)
	for _, p := range b.pages {
		out = append(out, p...)
	}
	return out
}

// PageSize returns the page size in bytes.
func (b *Builder) PageSize() int { return b.o.PageSize }

// PageBytes returns the live slice of page n (1-based): tests mutate hostile
// cases through it. It panics for a page that does not exist.
func (b *Builder) PageBytes(n uint32) []byte {
	if n == 0 || int(n) > len(b.pages) {
		panic(fmt.Sprintf("sqlitetest: page %d does not exist (the file has %d)", n, len(b.pages)))
	}
	return b.pages[n-1]
}

// Patch overwrites file bytes at off. It panics when the range is outside the
// file.
func (b *Builder) Patch(off int, p ...byte) {
	if off < 0 || off+len(p) > len(b.pages)*b.o.PageSize {
		panic(fmt.Sprintf("sqlitetest: patch of %d bytes at %d is outside the %d-byte file", len(p), off, len(b.pages)*b.o.PageSize))
	}
	for i, v := range p {
		b.pages[(off+i)/b.o.PageSize][(off+i)%b.o.PageSize] = v
	}
}

// SetHeaderPages writes n at offset 28 (database size in pages). With valid
// the version-valid-for field (offset 92) is made equal to the file change
// counter (offset 24), so a reader trusts n; without it, it is made to
// differ.
func (b *Builder) SetHeaderPages(n uint32, valid bool) {
	h := b.pages[0]
	put32(h[28:], n)
	counter := get32(h[24:])
	if !valid {
		counter++
	}
	put32(h[92:], counter)
}
