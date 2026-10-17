package fat_test

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/fat"
	"github.com/rbenzing/minutiae/internal/filesys/fat/fattest"
)

const (
	fuzzRecoverEntries = 200 // deleted entries asked per input
	fuzzRecoverSeedLen = 600 // clusters kept by a trimmed deleted-heavy seed
)

// deletedHeavySeed is an image whose root and a subdirectory hold deleted files with short and long
// names, an empty one and a multi-cluster one.
func deletedHeavySeed(typ int) []byte {
	o := fattest.Options{Type: typ}
	files := []fattest.File{
		{Path: "/LIVE.TXT", Data: []byte("live")},
		{Path: "/D", Dir: true},
		{Path: "/GONE.BIN", Data: bytes.Repeat([]byte{1}, 3000), Deleted: true},
		{Path: "/empty.bin", Deleted: true},
		{Path: "/a deleted file with a long name.txt", Data: bytes.Repeat([]byte{2}, 700), Deleted: true, LongName: true},
		{Path: "/éclair deleted long name.txt", Data: []byte("x"), Deleted: true, LongName: true},
		{Path: "/D/inner deleted with a long name.dat", Data: bytes.Repeat([]byte{3}, 1500), Deleted: true, LongName: true},
	}
	img := fattest.Build(o, files)
	return trimTail(img[:min(int64(len(img)), fattest.Layout(o).ClusterOffset(fuzzRecoverSeedLen))])
}

// FuzzFATRecoverable asks Recoverable for every deleted entry of arbitrary bytes. It calls fat directly
// (not through detect), so a panic fails the fuzz.
func FuzzFATRecoverable(f *testing.F) {
	for _, s := range fuzzBuilderSeeds() {
		f.Add(s)
	}
	for _, typ := range []int{12, 16, 32} {
		f.Add(deletedHeavySeed(typ))
	}
	for _, path := range fixturePaths(f) {
		f.Add(trimmedFixture(f, path))
	}
	f.Add(make([]byte, 2048))
	f.Fuzz(func(t *testing.T, b []byte) {
		fsys, err := fat.Open(bytes.NewReader(b), int64(len(b)))
		if err != nil {
			return
		}
		size := fsys.Info().Size
		var deleted []filesys.Entry
		visited := 0
		_ = filesys.Walk(fsys, fsys.Root(), "/", func(_ string, e filesys.Entry, err error) error {
			if err != nil {
				return nil
			}
			if visited++; visited > fuzzWalkBudget {
				return errFuzzBudget
			}
			if e.Deleted {
				deleted = append(deleted, e)
			}
			return nil
		})
		for i, e := range deleted {
			if i >= fuzzRecoverEntries {
				break
			}
			cs, err := fsys.Recoverable(e)
			if err != nil {
				if cs != nil {
					t.Fatalf("%s: candidates together with the error %v", e.ID, err)
				}
				continue
			}
			if len(cs) > filesys.MaxCandidatesPerEntry {
				t.Fatalf("%s: %d candidates, more than %d", e.ID, len(cs), filesys.MaxCandidatesPerEntry)
			}
			for _, c := range cs {
				if _, err := filesys.CheckCandidate(c, size); err != nil {
					t.Fatalf("%s: malformed candidate %+v: %v", e.ID, c, err)
				}
				for _, r := range c.Runs {
					if r.Offset < 0 || r.Length < 0 || r.Offset+r.Length > size {
						t.Fatalf("%s: run %+v outside the %d-byte filesystem", e.ID, r, size)
					}
				}
			}
			again, err := fsys.Recoverable(e)
			if err != nil || !reflect.DeepEqual(again, cs) {
				t.Fatalf("%s: second call %+v, %v differs from the first %+v", e.ID, again, err, cs)
			}
			forged, err := fsys.Recoverable(filesys.Entry{ID: e.ID})
			if err != nil || !reflect.DeepEqual(forged, cs) {
				t.Fatalf("%s: call with only the id %+v, %v differs from the first %+v", e.ID, forged, err, cs)
			}
		}
	})
}
