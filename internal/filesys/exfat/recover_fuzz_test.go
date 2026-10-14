package exfat_test

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/exfat"
	"github.com/rbenzing/minutiae/internal/filesys/exfat/exfattest"
)

const fuzzRecoverEntries = 200 // deleted entries asked per input

// deletedHeavySeed holds deleted files with short and long names, an empty one, a FAT-chained one and a
// fragmented one, in the root and in a subdirectory.
func deletedHeavySeed() []byte {
	files := []exfattest.File{
		{Path: "/LIVE.TXT", Data: []byte("live")},
		{Path: "/D", Dir: true},
		{Path: "/GONE.BIN", Data: bytes.Repeat([]byte{1}, 9000), Deleted: true},
		{Path: "/empty.bin", Deleted: true},
		{Path: "/a deleted file with a long name that takes three entries.txt", Data: bytes.Repeat([]byte{2}, 700), Deleted: true},
		{Path: "/chained.bin", Data: bytes.Repeat([]byte{4}, 9000), Deleted: true, FatChain: true},
		{Path: "/D/frag.bin", Data: bytes.Repeat([]byte{3}, 9000), Deleted: true, Fragmented: true},
	}
	return trimTail(exfattest.Build(exfattest.Options{ClusterCount: 256}, files))
}

// FuzzExfatRecoverable asks Recoverable for every deleted entry of arbitrary bytes. It calls exfat directly
// (not through detect), so a panic fails the fuzz.
func FuzzExfatRecoverable(f *testing.F) {
	for _, s := range fuzzBuilderSeeds() {
		f.Add(s)
	}
	f.Add(deletedHeavySeed())
	for _, path := range fixturePaths(f) {
		f.Add(trimmedFixture(f, path))
	}
	f.Add(make([]byte, 2048))
	f.Fuzz(func(t *testing.T, b []byte) {
		fsys, err := exfat.Open(bytes.NewReader(b), int64(len(b)))
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
				var sum int64
				for _, r := range c.Runs {
					if r.Offset == -1 || r.Offset < 0 || r.Length < 0 || r.Offset+r.Length > size {
						t.Fatalf("%s: run %+v outside the %d-byte filesystem", e.ID, r, size)
					}
					sum += r.Length
				}
				if c.Size != sum && c.Size != 0 && !declaredShort(c) {
					t.Fatalf("%s: Size %d but the runs add up to %d and the map is not declared short: %+v", e.ID, c.Size, sum, c)
				}
			}
			again, err := fsys.Recoverable(e)
			if err != nil || !reflect.DeepEqual(again, cs) {
				t.Fatalf("%s: second call %+v, %v differs from the first %+v", e.ID, again, err, cs)
			}
			forged := filesys.Entry{
				ID: e.ID, Name: "forged", RawName: []byte("forged"), Size: 7, Type: filesys.TypeDir, Deleted: !e.Deleted, Mode: 0o777,
				Attrs: []filesys.KV{{Key: "first_cluster", Value: "9"}, {Key: "no_fat_chain", Value: "false"}, {Key: "valid_data_length", Value: "1"}},
			}
			got, err := fsys.Recoverable(forged)
			if err != nil || !reflect.DeepEqual(got, cs) {
				t.Fatalf("%s: call with forged fields %+v, %v differs from the first %+v", e.ID, got, err, cs)
			}
		}
	})
}

// declaredShort reports a candidate whose map stops before its Size and says so.
func declaredShort(c filesys.Candidate) bool {
	for _, a := range c.Assumptions {
		if strings.HasPrefix(a, "stale-chain-stop=") {
			return true
		}
	}
	return false
}
