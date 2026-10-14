package exfat_test

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/exfat"
	"github.com/rbenzing/minutiae/internal/filesys/exfat/exfattest"
)

func u32p(v uint32) *uint32 { return &v }

func TestExfatRecoverableHostile(t *testing.T) {
	data := bytes.Repeat([]byte{5}, 3*4096-11)
	maxU64 := ^uint64(0)
	for _, c := range []struct {
		name  string
		patch func(l *exfattest.Layout) exfattest.StreamPatch
	}{
		{"first cluster 0 with data", func(*exfattest.Layout) exfattest.StreamPatch { return exfattest.StreamPatch{First: u32p(0)} }},
		{"first cluster 1", func(*exfattest.Layout) exfattest.StreamPatch { return exfattest.StreamPatch{First: u32p(1)} }},
		{"first cluster past the heap", func(l *exfattest.Layout) exfattest.StreamPatch {
			return exfattest.StreamPatch{First: u32p(l.ClusterCount + 2)}
		}},
		{"DataLength larger than the heap", func(l *exfattest.Layout) exfattest.StreamPatch {
			d := uint64(l.ClusterCount+1) * uint64(l.ClusterSize)
			return exfattest.StreamPatch{DataLen: &d}
		}},
		{"ValidDataLength above DataLength", func(*exfattest.Layout) exfattest.StreamPatch {
			v := uint64(3*4096 + 1)
			return exfattest.StreamPatch{Valid: &v}
		}},
		{"DataLength and ValidDataLength at MaxUint64", func(*exfattest.Layout) exfattest.StreamPatch {
			return exfattest.StreamPatch{DataLen: &maxU64, Valid: &maxU64}
		}},
		{"NoFatChain range past the heap", func(l *exfattest.Layout) exfattest.StreamPatch {
			return exfattest.StreamPatch{First: u32p(l.ClusterCount + 1)} // the last cluster, 3 clusters needed
		}},
		{"wrong set checksum", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			img, l := exfattest.BuildLayout(exfattest.Options{ClusterCount: 256}, []exfattest.File{{Path: "/G.BIN", Data: data, Deleted: true}})
			loc := l.Find("/G.BIN", true)
			if c.patch != nil {
				exfattest.PatchStream(img, loc, c.patch(l))
			} else {
				exfattest.CorruptSetChecksum(img, loc)
			}
			rr := &recReader{r: bytes.NewReader(img)}
			fsys, err := exfat.Open(rr, int64(len(img)))
			if err != nil {
				t.Fatal(err)
			}
			base := rr.calls.Load()
			cs, err := fsys.Recoverable(filesys.Entry{ID: fmt.Sprintf("dirent:%d:%d", loc.DirCluster, loc.Index)})
			var ce *filesys.CorruptError
			if !errors.Is(err, filesys.ErrCorrupt) || !errors.As(err, &ce) || ce.Structure != "recovery map" || cs != nil {
				t.Fatalf("Recoverable = %v, %v; want nil and a *CorruptError of structure \"recovery map\"", cs, err)
			}
			if got := rr.calls.Load() - base; got > 8 { // one set read plus the cached directory: a fixed small bound, no walk of a declared size
				t.Errorf("%d reads for a refused map, want at most 8", got)
			}
		})
	}
}

// recoverableOf opens img and asks Recoverable for the deleted entry at loc.
func recoverableOf(t *testing.T, img []byte, loc exfattest.EntryLoc, forge func(*filesys.Entry)) []filesys.Candidate {
	t.Helper()
	fsys, err := exfat.Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	e := filesys.Entry{ID: fmt.Sprintf("dirent:%d:%d", loc.DirCluster, loc.Index)}
	if forge != nil {
		forge(&e)
	}
	cs, err := fsys.Recoverable(e)
	if err != nil {
		t.Fatalf("Recoverable: %v", err)
	}
	return cs
}

// The allocation bitmap says nothing about a deleted file (the kernel driver clears it on delete and
// examine proves freeness): flipping every bit of its clusters changes no candidate.
func TestExfatRecoverableDoesNotConsultBitmap(t *testing.T) {
	data := bytes.Repeat([]byte{9}, 3*4096-5)
	for _, fatChain := range []bool{false, true} {
		t.Run(fmt.Sprintf("fatchain=%v", fatChain), func(t *testing.T) {
			img, l := exfattest.BuildLayout(exfattest.Options{ClusterCount: 256}, []exfattest.File{{Path: "/G.BIN", Data: data, Deleted: true, FatChain: fatChain}})
			loc := l.Find("/G.BIN", true)
			want := recoverableOf(t, img, loc, nil)
			if len(want) != 1 || len(want[0].Runs) == 0 {
				t.Fatalf("baseline candidates %+v", want)
			}
			for _, allocated := range []bool{true, false} {
				flipped := bytes.Clone(img)
				for _, c := range loc.Clusters {
					exfattest.SetBitmapBit(flipped, l, c, allocated)
				}
				if got := recoverableOf(t, flipped, loc, nil); !reflect.DeepEqual(got, want) {
					t.Errorf("bitmap bits=%v: candidates %+v differ from %+v", allocated, got, want)
				}
			}
		})
	}
}

// The generic fields are covered by the contract; this adds the exFAT attributes: a caller cannot steer
// the map by passing a first cluster, a chain flag or a valid length in Attrs.
func TestExfatRecoverableForgedFieldsAcrossAttrs(t *testing.T) {
	img, l := exfattest.BuildLayout(exfattest.Options{ClusterCount: 256}, []exfattest.File{{Path: "/G.BIN", Data: bytes.Repeat([]byte{4}, 2*4096+3), Deleted: true}})
	loc := l.Find("/G.BIN", true)
	want := recoverableOf(t, img, loc, nil)
	got := recoverableOf(t, img, loc, func(e *filesys.Entry) {
		e.Size, e.Type, e.Deleted = 1, filesys.TypeDir, false
		e.Attrs = []filesys.KV{{Key: "first_cluster", Value: "9"}, {Key: "no_fat_chain", Value: "false"}, {Key: "valid_data_length", Value: "1"}}
	})
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("forged Attrs changed the candidates:\n got %+v\nwant %+v", got, want)
	}
}

// An empty map has no runs but is still a well-formed candidate: it names a method.
func TestExfatRecoverableEmptyFileNamesAMethod(t *testing.T) {
	for _, chain := range []bool{false, true} {
		img, l := exfattest.BuildLayout(exfattest.Options{ClusterCount: 256}, []exfattest.File{{Path: "/E.BIN", Deleted: true, FatChain: chain}})
		cs := recoverableOf(t, img, l.Find("/E.BIN", true), nil)
		if len(cs) != 1 {
			t.Fatalf("candidates %+v", cs)
		}
		if _, err := filesys.CheckCandidate(cs[0], int64(len(img))); err != nil {
			t.Errorf("fatchain=%v: malformed empty candidate %+v: %v", chain, cs[0], err)
		}
	}
}
