package fstest

import (
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
)

// RecovererSubject is what the Recoverer contract needs from a reader. The recovery readers of
// the later plans fill it from their own builder images and run RecovererContract.
type RecovererSubject struct {
	FS        filesys.FileSystem
	Deleted   filesys.Entry  // a deleted entry the reader has maps for
	Live      filesys.Entry  // a live entry
	ForgedIDs []string       // ids that exist nowhere (leading zero, sign, wrong prefix, huge number, empty)
	Reads     func() int64   // bytes/reads counter of the underlying image, for the "rejected quickly" rule; nil skips it
	Duplicate *filesys.Entry // an entry whose id is held by two objects, when the reader can have that (nil skips)
}

// Reporter is the part of testing.TB the contract uses, so a test can run the contract against a
// recorder and prove that each rule can fail.
type Reporter interface {
	Helper()
	Errorf(format string, args ...any)
}

// RecovererContract asserts the filesys.Recoverer contract on s:
//
//   - check-candidate: every candidate for Deleted passes filesys.CheckCandidate, and there is at least one;
//   - forged-fields: a call with Size, Type, Deleted, Attrs, Name, RawName, LinkTarget or Encrypted
//     forged (each alone, and all together), or only the ID set, returns the same candidates;
//   - live-not-deleted: Live is ErrNotDeleted, also with its Deleted flag forged;
//   - forged-id-not-found / forged-id-read: each ForgedID is ErrNotFound with Reads() unchanged;
//   - duplicate-corrupt: Duplicate is a corrupt-structure error (never a guess);
//   - open-deleted: Open of the deleted entry is ErrDeleted, also with the flag cleared;
//   - max-candidates: at most filesys.MaxCandidatesPerEntry candidates;
//   - deterministic / aliasing: repeated calls are equal, and mutating an answer does not change the next.
func RecovererContract(t *testing.T, s RecovererSubject) {
	t.Helper()
	CheckRecoverer(t, s)
}

// CheckRecoverer is RecovererContract over a Reporter.
func CheckRecoverer(r Reporter, s RecovererSubject) {
	r.Helper()
	rec, ok := filesys.As[filesys.Recoverer](s.FS)
	if !ok {
		r.Errorf("no-recoverer: the filesystem (and nothing it wraps) implements filesys.Recoverer")
		return
	}
	base, err := rec.Recoverable(s.Deleted)
	if err != nil {
		r.Errorf("check-candidate: Recoverable(deleted) failed: %v", err)
		return
	}
	if len(base) == 0 {
		r.Errorf("no-candidates: Recoverable(deleted) returned no candidates, the subject must have maps")
		return
	}
	if len(base) > filesys.MaxCandidatesPerEntry {
		r.Errorf("max-candidates: Recoverable(deleted) returned %d candidates, more than filesys.MaxCandidatesPerEntry (%d)", len(base), filesys.MaxCandidatesPerEntry)
	}
	size := s.FS.Info().Size
	for i, c := range base {
		if _, err := filesys.CheckCandidate(c, size); err != nil {
			r.Errorf("check-candidate: candidate %d (%q) fails CheckCandidate: %v", i, c.Method, err)
		}
	}
	checkForged(r, rec, s, base)
	checkLive(r, rec, s)
	checkForgedIDs(r, rec, s)
	if s.Duplicate != nil {
		if cs, err := rec.Recoverable(*s.Duplicate); !errors.Is(err, filesys.ErrCorrupt) || cs != nil {
			r.Errorf("duplicate-corrupt: Recoverable(duplicate id %q) = %v, %v; want a corrupt-structure error and no candidates", s.Duplicate.ID, cs, err)
		}
	}
	for _, deleted := range []bool{true, false} {
		e := s.Deleted
		e.Deleted = deleted
		if f, err := s.FS.Open(e); !errors.Is(err, filesys.ErrDeleted) || f != nil {
			r.Errorf("open-deleted: Open(deleted entry, Deleted=%v) = %v, %v; want ErrDeleted", deleted, f, err)
		}
	}
	checkRepeatable(r, rec, s, base)
}

func checkForged(r Reporter, rec filesys.Recoverer, s RecovererSubject, base []filesys.Candidate) {
	r.Helper()
	id := s.Deleted.ID
	forge := map[string]filesys.Entry{
		"only the ID": {ID: id},
		"Size":        withEntry(s.Deleted, func(e *filesys.Entry) { e.Size = 1<<40 + 7 }),
		"Type":        withEntry(s.Deleted, func(e *filesys.Entry) { e.Type = filesys.TypeDir }),
		"Deleted":     withEntry(s.Deleted, func(e *filesys.Entry) { e.Deleted = false }),
		"Attrs": withEntry(s.Deleted, func(e *filesys.Entry) {
			e.Attrs = []filesys.KV{{Key: "first_cluster", Value: "9"}, {Key: "inode", Value: "1"}}
		}),
		"Name":          withEntry(s.Deleted, func(e *filesys.Entry) { e.Name = "zz-forged" }),
		"RawName":       withEntry(s.Deleted, func(e *filesys.Entry) { e.RawName = []byte("zz-forged") }),
		"LinkTarget":    withEntry(s.Deleted, func(e *filesys.Entry) { e.LinkTarget = "/etc/passwd" }),
		"Encrypted":     withEntry(s.Deleted, func(e *filesys.Entry) { e.Encrypted = !e.Encrypted }),
		"every field":   {ID: id, Name: "zz-forged", RawName: []byte{0xff}, Type: filesys.TypeSymlink, Size: -1, Mode: 0o7777, Deleted: false, Encrypted: !s.Deleted.Encrypted, LinkTarget: "x", Attrs: []filesys.KV{{Key: "k", Value: "v"}}},
		"Live's fields": withEntry(s.Live, func(e *filesys.Entry) { e.ID = id }),
	}
	for name, e := range forge {
		got, err := rec.Recoverable(e)
		if err != nil || !reflect.DeepEqual(got, base) {
			r.Errorf("forged-fields: Recoverable with %s forged = %v, %v; want the same candidates as the real entry", name, got, err)
		}
	}
}

func withEntry(e filesys.Entry, f func(*filesys.Entry)) filesys.Entry {
	f(&e)
	return e
}

func checkLive(r Reporter, rec filesys.Recoverer, s RecovererSubject) {
	r.Helper()
	for name, e := range map[string]filesys.Entry{
		"live":                  s.Live,
		"live with Deleted set": withEntry(s.Live, func(e *filesys.Entry) { e.Deleted = true }),
		"only the live ID":      {ID: s.Live.ID},
	} {
		if cs, err := rec.Recoverable(e); !errors.Is(err, filesys.ErrNotDeleted) || cs != nil {
			r.Errorf("live-not-deleted: Recoverable(%s) = %v, %v; want nil and ErrNotDeleted", name, cs, err)
		}
	}
}

func checkForgedIDs(r Reporter, rec filesys.Recoverer, s RecovererSubject) {
	r.Helper()
	for _, id := range s.ForgedIDs {
		var before int64
		if s.Reads != nil {
			before = s.Reads()
		}
		cs, err := rec.Recoverable(filesys.Entry{ID: id, Deleted: true, Name: s.Deleted.Name, Size: s.Deleted.Size})
		if !errors.Is(err, filesys.ErrNotFound) || cs != nil {
			r.Errorf("forged-id-not-found: Recoverable(forged id %q) = %v, %v; want nil and ErrNotFound", id, cs, err)
		}
		if s.Reads != nil {
			if after := s.Reads(); after != before {
				r.Errorf("forged-id-read: Recoverable(forged id %q) read the image (%d -> %d): a forged id must be rejected without a search", id, before, after)
			}
		}
	}
}

func checkRepeatable(r Reporter, rec filesys.Recoverer, s RecovererSubject, base []filesys.Candidate) {
	r.Helper()
	want := cloneCandidates(base)
	for i := range 2 {
		got, err := rec.Recoverable(s.Deleted)
		if err != nil || !reflect.DeepEqual(got, want) {
			r.Errorf("deterministic: call %d of Recoverable(deleted) = %v, %v; want the first answer %v", i+2, got, err, want)
			return
		}
	}
	// Scribble over everything the reader handed out, then ask again.
	first, _ := rec.Recoverable(s.Deleted)
	for i := range first {
		for j := range first[i].Runs {
			first[i].Runs[j] = filesys.Run{Offset: -7, Length: -7}
		}
		for j := range first[i].Basis {
			first[i].Basis[j] = fmt.Sprint("scribble ", j)
		}
		for j := range first[i].Assumptions {
			first[i].Assumptions[j] = "scribble"
		}
		for j := range first[i].Warnings {
			first[i].Warnings[j] = "scribble"
		}
		first[i].Method = "scribble"
		first[i].Size = -1
	}
	if len(first) > 0 {
		first[0] = filesys.Candidate{Method: "scribble"}
	}
	if got, err := rec.Recoverable(s.Deleted); err != nil || !reflect.DeepEqual(got, want) {
		r.Errorf("aliasing: after the caller changed a returned answer, Recoverable(deleted) = %v, %v; want the first answer %v (the reader must hand out copies)", got, err, want)
	}
}

func cloneCandidates(in []filesys.Candidate) []filesys.Candidate {
	if in == nil {
		return nil
	}
	out := make([]filesys.Candidate, len(in))
	for i, c := range in {
		out[i] = c
		out[i].Runs = cloneOrNil(c.Runs)
		out[i].Basis = cloneOrNil(c.Basis)
		out[i].Assumptions = cloneOrNil(c.Assumptions)
		out[i].Warnings = cloneOrNil(c.Warnings)
	}
	return out
}

func cloneOrNil[T any](s []T) []T {
	if s == nil {
		return nil
	}
	return append([]T{}, s...)
}
