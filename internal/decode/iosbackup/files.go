package iosbackup

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
)

const (
	// MaxFilesRows is the most Files rows ReadFiles reads (spec 5.2, 4E-R8); more is ErrTooManyRows,
	// never a partial mapping.
	MaxFilesRows = 2_000_000
	// MaxManifestBytes is the largest Manifest.db ReadFiles opens.
	MaxManifestBytes = 1 << 30
	// FlagFile is the Files.flags value of a regular file (from memory, docs/limitations.md).
	FlagFile = 1
)

var (
	// ErrTooManyRows: Files has more rows than the limit.
	ErrTooManyRows = errors.New("iosbackup: more Files rows than the limit")
	// ErrTooLarge: Manifest.db is larger than MaxManifestBytes.
	ErrTooLarge = errors.New("iosbackup: Manifest.db is larger than the limit")
	// ErrBadOptions: an Options value would loosen a bound.
	ErrBadOptions = errors.New("iosbackup: option would loosen a bound")
)

// entryCharge is what each kept entry costs beyond its strings.
const entryCharge = 64

// Entry is the domain and relative path a Files row gives a file.
type Entry struct {
	Domain, RelativePath string
	Flags                int64
}

// State is the answer of Files.Lookup. The zero value is not a state.
type State uint8

// The lookup states.
const (
	Mapped    State = iota + 1 // listed once with flags 1 and a usable domain and path
	NotListed                  // the scan was complete and the ID is not in Files
	Undecided                  // not found, but a row or page could not be read, or the ID was not requested
	Duplicate                  // listed more than once
	NotAFile                   // listed with flags other than 1
	BadEntry                   // listed, but the domain or path is not usable
)

// String names the state.
func (s State) String() string {
	switch s {
	case Mapped:
		return "mapped"
	case NotListed:
		return "not-listed"
	case Undecided:
		return "undecided"
	case Duplicate:
		return "duplicate"
	case NotAFile:
		return "not-a-file"
	case BadEntry:
		return "bad-entry"
	}
	return "invalid"
}

// kept is what is retained of the first row of a wanted ID.
type kept struct {
	entry  Entry
	state  State
	reason string
	seen   int
}

// Files is the retained part of a Manifest.db Files table: the rows of the IDs the caller asked
// for, how often each was listed, and whether the whole table was read without loss.
type Files struct {
	kept     map[string]*kept
	want     func(string) bool
	complete bool
	rows     int64
	b        sqlitedb.Budget
	charged  int64
}

// Options can only tighten the bounds: MaxRows 0 means MaxFilesRows; a value above MaxFilesRows or
// below 0 is ErrBadOptions (nothing exported may loosen a bound).
type Options struct{ MaxRows int }

// damage are the row flags that say the row itself is suspect. FlagExtraValues (more values than
// declared columns) and FlagUnknownValues (judged per column below) do not.
const damage = sqlitedb.FlagLengthMismatch | sqlitedb.FlagKeyRange | sqlitedb.FlagOverflowMixed | sqlitedb.FlagShortRecord | sqlitedb.FlagBeyondEngineDepth

// ReadFiles scans the Files table of the Manifest.db in f (the WAL and journal of f applied, as
// the live view does) and keeps the rows whose fileID want accepts. want must stay valid and give
// the same answers for as long as the result is used: Lookup asks it to tell "not listed" from
// "never requested". A nil want requests nothing.
//
// Errors: ErrBadOptions, ErrTooLarge (checked before any read), ErrTooManyRows,
// sqlitedb.ErrEncrypted, sqlitedb.ErrNotSQLite, sqlitedb.ErrNoSuchTable,
// *sqlitedb.UnsupportedSchemaError (a missing fileID, domain, relativePath or flags column), a
// budget error, ctx.Err(). A nil *Files comes with every error. The kept entries stay charged to
// b until Release.
func ReadFiles(ctx context.Context, f sqlitedb.Files, b sqlitedb.Budget, want func(fileID string) bool, opt Options) (out *Files, err error) {
	maxRows := int64(MaxFilesRows)
	switch {
	case opt.MaxRows < 0 || opt.MaxRows > MaxFilesRows:
		return nil, fmt.Errorf("%w: MaxRows %d (allowed 0..%d)", ErrBadOptions, opt.MaxRows, MaxFilesRows)
	case opt.MaxRows > 0:
		maxRows = int64(opt.MaxRows)
	}
	if f.DBSize > MaxManifestBytes {
		return nil, fmt.Errorf("%w: %d bytes, limit %d", ErrTooLarge, f.DBSize, int64(MaxManifestBytes))
	}
	if want == nil {
		want = func(string) bool { return false }
	}
	db, err := sqlitedb.Open(ctx, f, b)
	if err != nil {
		return nil, err
	}
	defer db.Release()
	t, err := db.Table(ctx, "Files", []string{"fileID", "domain", "relativePath", "flags"}, nil)
	if err != nil {
		return nil, err
	}
	res := &Files{kept: map[string]*kept{}, want: want, complete: true, b: b}
	defer func() {
		if err != nil {
			res.Release()
			out = nil
		}
	}()
	cID, cDomain, cRel, cFlags := t.Col("fileID"), t.Col("domain"), t.Col("relativePath"), t.Col("flags")
	err = t.Scan(ctx, func(r sqlitedb.Row) error {
		res.rows++
		if res.rows > maxRows {
			return fmt.Errorf("%w: more than %d", ErrTooManyRows, maxRows)
		}
		damaged := r.Flags()&damage != 0
		if damaged {
			res.complete = false
		}
		idb, ok := r.Text(cID)
		if !ok {
			// A fileID that is not text (or not readable) could be any wanted ID.
			res.complete = false
			return nil
		}
		id := string(idb)
		if !want(id) {
			// A non-canonical spelling of a wanted ID may be that file: absence is then undecidable.
			if len(id) == 40 && !ValidFileID(id) && want(strings.ToLower(id)) {
				res.complete = false
			}
			return nil
		}
		if k := res.kept[id]; k != nil {
			k.seen++
			return nil
		}
		k := classify(r, cDomain, cRel, cFlags)
		if damaged {
			// Partial data never maps a file.
			k = &kept{state: Undecided, reason: "the Files row is damaged"}
		}
		k.seen = 1
		cost := int64(len(id)+len(k.entry.Domain)+len(k.entry.RelativePath)) + entryCharge
		if err := b.Alloc(cost); err != nil {
			return err
		}
		res.charged += cost
		res.kept[id] = k
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(db.Warnings()) > 0 {
		res.complete = false
	}
	return res, nil
}

// classify decides the state of the first row of a wanted ID. The flags come first: a directory
// row legitimately has an empty relative path and is a NotAFile, not a BadEntry.
func classify(r sqlitedb.Row, cDomain, cRel, cFlags int) *kept {
	flags, ok := r.Int(cFlags)
	if !ok {
		return &kept{state: Undecided, reason: "flags is not readable as an integer"}
	}
	k := &kept{entry: Entry{Flags: flags}}
	if flags != FlagFile {
		k.state, k.reason = NotAFile, fmt.Sprintf("not a regular file (flags %d)", flags)
		return k
	}
	domain, st, why := textCell(r, cDomain, "domain")
	if st != 0 {
		k.state, k.reason = st, why
		return k
	}
	rel, st, why := textCell(r, cRel, "relativePath")
	if st != 0 {
		k.state, k.reason = st, why
		return k
	}
	if !utf8.ValidString(domain) {
		k.state, k.reason = BadEntry, "domain is not valid UTF-8"
		return k
	}
	if !utf8.ValidString(rel) {
		k.state, k.reason = BadEntry, "relativePath is not valid UTF-8"
		return k
	}
	if !ValidDomain(domain) {
		k.state, k.reason = BadEntry, "unusable domain"
		return k
	}
	if !ValidRelativePath(rel) {
		k.state, k.reason = BadEntry, "relativePath is not canonical"
		return k
	}
	k.entry.Domain, k.entry.RelativePath, k.state = domain, rel, Mapped
	return k
}

// textCell reads a text column: st is zero when the value is text, else the state of the row.
func textCell(r sqlitedb.Row, col int, name string) (s string, st State, why string) {
	if b, ok := r.Text(col); ok {
		return string(b), 0, ""
	}
	switch {
	case r.IsNull(col):
		return "", BadEntry, name + " is NULL"
	case r.State(col).Known():
		return "", BadEntry, name + " is not text"
	}
	return "", Undecided, name + " is not readable (" + r.State(col).String() + ")"
}

// Lookup answers for fileID. The string is the reason for every state but Mapped. An ID that was
// not requested was not kept, so it is Undecided; "not listed" is answered only for a requested ID
// after a scan that lost nothing.
func (f *Files) Lookup(fileID string) (Entry, State, string) {
	if k, ok := f.kept[fileID]; ok {
		if k.seen > 1 {
			return Entry{}, Duplicate, fmt.Sprintf("fileID is listed %d times in Files", k.seen)
		}
		if k.state == Mapped {
			return k.entry, Mapped, ""
		}
		return Entry{}, k.state, k.reason
	}
	switch {
	case !f.want(fileID):
		return Entry{}, Undecided, "fileID was not requested from this Files table"
	case !f.complete:
		return Entry{}, Undecided, "fileID was not found, but Files could not be read completely"
	}
	return Entry{}, NotListed, "fileID is not listed in Files"
}

// Rows is the number of Files rows the scan counted.
func (f *Files) Rows() int64 { return f.rows }

// Release gives the memory of the kept entries back to the budget; it is safe to call twice.
func (f *Files) Release() {
	if f.charged > 0 {
		f.b.Free(f.charged)
		f.charged = 0
	}
}
