package ext4_test

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/ext4"
)

// recoverImages are the kernel-written recovery images and the oracle each is held against.
var recoverImages = []string{"ext4-recover", "ext3-recover", "ext4-recover-sameboot", "ext4-recover-wiped"}

// oracleKey ties an oracle record to a reader entry: the name and the inode attribute (0 in the oracle is "unknown",
// the first record of a block whose inode number the kernel zeroed).
func oracleKey(name string, inode uint32) string {
	if inode == 0 {
		return name + "/unknown"
	}
	return name + "/" + strconv.FormatUint(uint64(inode), 10)
}

// mergeBlockRuns turns [first block, count] runs into byte runs, merging runs that touch.
func mergeBlockRuns(runs recoverRuns, bs int64) []filesys.Run {
	var out []filesys.Run
	for _, r := range runs {
		out = appendMerged(out, filesys.Run{Offset: r[0] * bs, Length: r[1] * bs})
	}
	return out
}

func appendMerged(out []filesys.Run, r filesys.Run) []filesys.Run {
	if n := len(out); n > 0 && out[n-1].Offset+out[n-1].Length == r.Offset {
		out[n-1].Length += r.Length
		return out
	}
	return append(out, r)
}

func mergeFSRuns(runs []filesys.Run) []filesys.Run {
	var out []filesys.Run
	for _, r := range runs {
		out = appendMerged(out, r)
	}
	return out
}

// walkDeleted returns every deleted entry of the image, in walk order, with the path it was seen under.
func walkDeleted(t *testing.T, f *ext4.FS) []filesys.Entry {
	t.Helper()
	var out []filesys.Entry
	err := filesys.Walk(f, f.Root(), "/", func(_ string, e filesys.Entry, werr error) error {
		if werr != nil {
			t.Errorf("walk: %v", werr)
			return nil
		}
		if e.Deleted {
			out = append(out, e)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func entryInode(e filesys.Entry) string {
	v, _ := attr(e, "inode")
	return v
}

// The reader against the kernel-image oracles: every deleted record the oracle lists is walked and answers as the
// oracle says (the candidates with their method, merged byte runs, size, stop and assumptions; the typed error of a
// skip), the walk finds exactly the oracle's number of deleted entries, and the wiped image lists none.
func TestExt4RecoverableMatchesOracle(t *testing.T) {
	for _, name := range recoverImages {
		t.Run(name, func(t *testing.T) {
			img, o := loadRecoverFixture(t, name)
			f, err := ext4.Open(bytes.NewReader(img), int64(len(img)))
			if err != nil {
				t.Fatal(err)
			}
			if err := f.PrepareRecovery(context.Background()); err != nil {
				t.Fatal(err)
			}
			bs := o.BlockSize
			byKey := map[string]filesys.Entry{}
			deleted := walkDeleted(t, f)
			for _, e := range deleted {
				k := e.Name + "/" + entryInode(e)
				if _, dup := byKey[k]; dup {
					t.Fatalf("two deleted entries share the key %q", k)
				}
				byKey[k] = e
			}
			if len(deleted) != o.DeletedEntryCount {
				t.Errorf("the walk finds %d deleted entries, the oracle says %d", len(deleted), o.DeletedEntryCount)
			}
			matched := 0
			for _, d := range o.Deleted {
				e, ok := byKey[oracleKey(d.Name, d.Inode)]
				if !ok {
					t.Errorf("%s: the walk lists no deleted entry %s", d.Path, oracleKey(d.Name, d.Inode))
					continue
				}
				matched++
				checkReaderRecord(t, f, e, d, bs)
			}
			// One defect, one message: this count is derived from the walk count and the matches, so it is checked only
			// when both already agree with the oracle.
			if unlisted := len(deleted) - matched; matched == len(o.Deleted) && len(deleted) == o.DeletedEntryCount && unlisted != o.UnlistedCount {
				t.Errorf("%d walked entries have no oracle record, the oracle says %d", unlisted, o.UnlistedCount)
			}
		})
	}
}

func checkReaderRecord(t *testing.T, f *ext4.FS, e filesys.Entry, d recoverDeleted, bs int64) {
	t.Helper()
	x := d.Expect
	if x.Skip == "not-a-file" {
		if e.Type != filesys.TypeDir {
			t.Errorf("%s: type %v, the oracle says a directory", d.Path, e.Type)
		}
		return
	}
	cands, err := f.Recoverable(e)
	if x.Outcome == "skip" {
		switch x.Skip {
		case "reused":
			if !errors.Is(err, filesys.ErrReused) {
				t.Errorf("%s: err %v, want ErrReused", d.Path, err)
			}
		case "undecided":
			if !errors.Is(err, filesys.ErrUndecided) {
				t.Errorf("%s: err %v, want ErrUndecided", d.Path, err)
			}
		case "unsupported":
			if !errors.Is(err, filesys.ErrUnsupported) {
				t.Errorf("%s: err %v, want ErrUnsupported", d.Path, err)
			}
		case "no-map":
			if err != nil || len(cands) != 0 {
				t.Errorf("%s: %d candidates, err %v, want nil, nil", d.Path, len(cands), err)
			}
		default:
			t.Errorf("%s: the oracle skip %q has no reader rule in this test", d.Path, x.Skip)
		}
		return
	}
	if err != nil {
		t.Errorf("%s: Recoverable: %v", d.Path, err)
		return
	}
	if len(cands) != len(d.Candidates) {
		t.Errorf("%s: %d candidates, the oracle says %d", d.Path, len(cands), len(d.Candidates))
	}
	for _, w := range d.Candidates {
		i := slices.IndexFunc(cands, func(c filesys.Candidate) bool { return c.Method == w.Method })
		if i < 0 {
			t.Errorf("%s: no candidate by method %s", d.Path, w.Method)
			continue
		}
		c := cands[i]
		if got, want := mergeFSRuns(c.Runs), cutRuns(mergeBlockRuns(w.Blocks, bs), w.Size); !slices.Equal(got, want) {
			t.Errorf("%s/%s: runs %v, the oracle (the true blocks up to the reader's own stop) says %v", d.Path, w.Method, got, want)
		}
		if c.Size != w.Size {
			t.Errorf("%s/%s: size %d, the oracle says %d", d.Path, w.Method, c.Size, w.Size)
		}
		stop := ""
		for _, a := range c.Assumptions {
			if v, ok := strings.CutPrefix(a, "stale-chain-stop="); ok {
				stop = v
			}
		}
		if stop != d.ReaderStop {
			t.Errorf("%s/%s: stale-chain-stop %q, the oracle says %q", d.Path, w.Method, stop, d.ReaderStop)
		}
		for _, a := range w.Assumptions {
			if !slices.Contains(c.Assumptions, a) {
				t.Errorf("%s/%s: assumption %q missing from %v", d.Path, w.Method, a, c.Assumptions)
			}
		}
	}
}

// cutRuns keeps the first size bytes of runs: the oracle lists whole blocks, the reader cuts a run at the file size.
func cutRuns(runs []filesys.Run, size int64) []filesys.Run {
	var out []filesys.Run
	for _, r := range runs {
		if size <= 0 {
			break
		}
		r.Length = min(r.Length, size)
		size -= r.Length
		out = append(out, r)
	}
	return out
}
