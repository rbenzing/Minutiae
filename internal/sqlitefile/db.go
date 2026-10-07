package sqlitefile

import (
	"fmt"
	"io"
	"sync"
)

// engineRefusesBadEncoding records what the engine does with a header
// encoding field outside 0..3: it opens the file and reads text in the encoding
// the field masked with 3 names (measured by TestEngineTextEncodingHeaderField
// for the fields 4..7 and 0x102, and by TestEngineRefusesToleratedHeaders).
const engineRefusesBadEncoding = false

// DB is an opened database file, read only. The file's pages are reached
// through readRawPage, the as-found layer: later views (the live view with
// the write-ahead log and journal applied, the raw as-found view) are built
// on top of it and never change what it returns.
type DB struct {
	env   *env
	r     io.ReaderAt
	size  int64
	info  Info
	warns *warnings

	mu  sync.RWMutex
	wal *attachedWAL     // nil until AttachWAL
	jr  *attachedJournal // nil until AttachJournal
}

// Open reads the header of the database file read through db. size is
// authoritative: nothing at or past it is ever read. A file that is not a
// database is a *NotSQLiteError; a header that leaves page 1 unreadable is a
// *CorruptError; an I/O error is returned wrapped and is never a verdict on
// the content. Open reads at most 4096 bytes.
func Open(db io.ReaderAt, size int64, opts Options) (*DB, error) {
	return openWith(db, size, opts, nil)
}

func openWith(r io.ReaderAt, size int64, opts Options, hook func(site string)) (d *DB, err error) {
	defer guard(&err)
	e := newEnv(opts, hook)
	e.at("open")
	res, err := sniff(r, size)
	if err != nil {
		return nil, err
	}
	if res.Kind != SniffSQLite {
		return nil, res.notSQLite()
	}
	w := newWarnings(e.opts.Limits.MaxWarnings)
	e.reportClamps(w)
	e.at("open.header")
	info, err := parseHeader(res.buf[:headerSize], size, w)
	if err != nil {
		return nil, err
	}
	return &DB{env: e, r: r, size: size, info: info, warns: w}, nil
}

// Info returns what the header says. The result is a copy.
func (d *DB) Info() Info {
	i := d.info
	i.EngineRefuses = append([]string(nil), d.info.EngineRefuses...)
	return i
}

// Warnings returns the anomalies found so far, in the order first seen. The
// result is a copy; the list grows as scans run.
func (d *DB) Warnings() []Warning { return d.warns.snapshot() }

// readRawPage reads page pgno of the database file exactly as found. A page
// number of 0, above Limits.MaxPages, or a page with no byte below the declared
// size is ErrPageUnavailable. The trailing page of a file whose size is not a
// whole number of pages is returned short: only the bytes that are in the file,
// never padded with zeros. A read that fails or comes up short below the size
// is an I/O error, never ErrCorrupt.
func (d *DB) readRawPage(pgno uint32) ([]byte, error) {
	d.env.at("rawpage")
	if pgno == 0 || int64(pgno) > d.env.opts.Limits.MaxPages {
		return nil, fmt.Errorf("%w: page %d is not addressable", ErrPageUnavailable, pgno)
	}
	off := PageOffset(d.info.PageSize, pgno)
	if off >= d.size {
		return nil, fmt.Errorf("%w: page %d lies beyond the %d bytes of the file", ErrPageUnavailable, pgno, d.size)
	}
	p := make([]byte, min(int64(d.info.PageSize), d.size-off))
	if err := readFull(d.r, p, off); err != nil {
		return nil, err
	}
	return p, nil
}
