package fstest

import (
	"fmt"
	"slices"
	"time"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// Recoverable implements filesys.Recoverer. The record is found by e.ID ONLY (the name, type, size and
// flags of e are ignored); an id held by two records is a *CorruptError, never a guess. A live record
// is ErrNotDeleted and an unknown id ErrNotFound (a map lookup: no read is made). The answer is a copy
// of the maps planted with Node.Recover, so a caller cannot change the next one.
func (f *mtfs) Recoverable(e filesys.Entry) ([]filesys.Candidate, error) {
	idx := f.byID[e.ID]
	switch len(idx) {
	case 0:
		return nil, fmt.Errorf("%w: %q", filesys.ErrNotFound, e.ID)
	case 1:
	default:
		return nil, corrupt(structEntry, headerLen, "duplicate entry id %q (%d records)", e.ID, len(idx))
	}
	rec := &f.recs[idx[0]]
	if !rec.Deleted {
		return nil, fmt.Errorf("%w: %q", filesys.ErrNotDeleted, rec.Name)
	}
	out := make([]filesys.Candidate, 0, len(rec.Recover))
	for _, rm := range rec.Recover {
		c := filesys.Candidate{
			Method:      rm.Method,
			Size:        rm.Size,
			Basis:       slices.Clone(rm.Basis),
			Assumptions: slices.Clone(rm.Assumptions),
			Warnings:    slices.Clone(rm.Warnings),
			Mode:        rm.Mode,
			Encrypted:   rm.Encrypted,
		}
		for _, ru := range rm.Runs {
			c.Runs = append(c.Runs, filesys.Run{Offset: ru.Offset, Length: ru.Length})
		}
		if rec.MTime != 0 {
			c.Times.Modified = filesys.Timestamp{T: time.Unix(rec.MTime, 0).UTC(), ZoneKnown: true}
		}
		out = append(out, c)
	}
	return out, nil
}
