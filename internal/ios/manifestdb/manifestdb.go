// Package manifestdb reads the file sizes a finished iOS backup declares in
// its own Manifest.db. It reads through internal/sqlitefile (pure, read-only,
// bounded) and decodes each file's NSKeyedArchiver blob with the validated
// plist decoder of internal/ios/mb2. It never opens the database with an SQL
// engine, never writes anything and retains only the file IDs it is asked for.
package manifestdb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"

	"howett.net/plist"

	"github.com/rbenzing/minutiae/internal/ios/mb2"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// MaxRows bounds the Files rows scanned; more is ErrTooManyRows (the manifest
// is then not used at all: a partial read would turn missing rows into
// "unknown" without saying so).
const MaxRows = 4_000_000

// MaxFileBytes bounds the Manifest.db that is parsed.
const MaxFileBytes = 1 << 30

// maxRows is MaxRows; only export_test.go changes it.
var maxRows = MaxRows

// ErrTooManyRows means the Files table has more than MaxRows rows.
var ErrTooManyRows = errors.New("manifestdb: more Files rows than the limit")

// ErrTooLarge means the Manifest.db is larger than MaxFileBytes.
var ErrTooLarge = errors.New("manifestdb: the manifest is larger than the limit")

// ValidFileID reports whether id is a backup file ID: 40 lowercase hex digits.
func ValidFileID(id string) bool {
	if len(id) != 40 {
		return false
	}
	for i := range len(id) {
		c := id[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// Declared holds the size each wanted fileID declares.
type Declared struct {
	sizes    map[string]int64
	conflict map[string]bool // fileIDs with two sizes, or an unreadable declaration
}

// Size returns the declared size of fileID; known is false when the manifest
// does not say (unwanted or missing row, unreadable declaration, conflicting rows).
func (d *Declared) Size(fileID string) (int64, bool) {
	if d.conflict[fileID] {
		return 0, false
	}
	n, ok := d.sizes[fileID]
	return n, ok
}

// Read parses the Files table of the database read through r and keeps the
// declarations of the valid file IDs for which want returns true.
func Read(ctx context.Context, r io.ReaderAt, size int64, want func(fileID string) bool) (*Declared, error) {
	if size > MaxFileBytes {
		return nil, fmt.Errorf("%w (%d bytes)", ErrTooLarge, size)
	}
	db, err := sqlitefile.Open(r, size, sqlitefile.Options{Limits: sqlitefile.DefaultLimits()})
	if err != nil {
		return nil, fmt.Errorf("manifestdb: %w", err)
	}
	t, err := db.Live().Table(ctx, "Files")
	if err != nil {
		return nil, fmt.Errorf("manifestdb: %w", err)
	}
	idCol, fileCol := -1, -1
	for i, c := range t.Def().Columns {
		switch strings.ToLower(c.Name) {
		case "fileid":
			idCol = i
		case "file":
			fileCol = i
		}
	}
	if idCol < 0 || fileCol < 0 {
		return nil, errors.New("manifestdb: the Files table has no fileID and file columns")
	}
	d := &Declared{sizes: map[string]int64{}, conflict: map[string]bool{}}
	rows := 0
	var scanErr error
	err = t.Rows(ctx, func(row sqlitefile.Row) bool {
		if scanErr = ctx.Err(); scanErr != nil {
			return false
		}
		rows++
		if rows > maxRows {
			scanErr = ErrTooManyRows
			return false
		}
		vals := t.Resolve(row)
		if idCol >= len(vals) || fileCol >= len(vals) {
			return true
		}
		id, ok := vals[idCol].Text()
		if !ok || !ValidFileID(id) || !want(id) {
			return true
		}
		blob := vals[fileCol]
		if blob.Kind != sqlitefile.KindBlob || blob.Omitted || blob.Unread || blob.Clipped {
			d.conflict[id] = true
			return true
		}
		n, ok := declaredSize(blob.Bytes)
		if !ok {
			d.conflict[id] = true
			return true
		}
		if prev, seen := d.sizes[id]; seen && prev != n {
			d.conflict[id] = true
		}
		d.sizes[id] = n
		return true
	})
	if err = errors.Join(err, scanErr); err != nil {
		return nil, fmt.Errorf("manifestdb: %w", err)
	}
	return d, nil
}

// declaredSize decodes an MBFile keyed archive and returns its root object's
// Size. The layout ($top.root -> $objects[i].Size) is from memory of the
// iOS 10+ backup format.
func declaredSize(b []byte) (int64, bool) {
	var a struct {
		Top struct {
			Root plist.UID `plist:"root"`
		} `plist:"$top"`
		Objects []any `plist:"$objects"`
	}
	if err := mb2.UnmarshalPlist(b, &a); err != nil {
		return 0, false
	}
	i := uint64(a.Top.Root)
	if i >= uint64(len(a.Objects)) {
		return 0, false
	}
	obj, ok := a.Objects[i].(map[string]any)
	if !ok {
		return 0, false
	}
	switch v := obj["Size"].(type) {
	case uint64:
		if v <= math.MaxInt64 {
			return int64(v), true
		}
	case int64:
		if v >= 0 {
			return v, true
		}
	}
	return 0, false
}
