package fat_test

import (
	"bytes"
	"errors"
	"slices"
	"strconv"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/fat"
)

func entryAttr(e filesys.Entry, key string) string {
	for _, kv := range e.Attrs {
		if kv.Key == key {
			return kv.Value
		}
	}
	return ""
}

// recKey ties a walk entry to an oracle record: the listing is reader output, the oracle knows the first cluster
// and the size from the image alone.
func recKey(first, size int64, dir bool) string {
	return strconv.FormatInt(first, 10) + "/" + strconv.FormatInt(size, 10) + "/" + strconv.FormatBool(dir)
}

// rangeRuns converts the oracle's inclusive cluster ranges into byte runs, the total cut at size.
func rangeRuns(o recoverOracle, ranges [][2]int64, size int64) []filesys.Run {
	var runs []filesys.Run
	left := size
	for _, r := range ranges {
		length := min((r[1]-r[0]+1)*o.ClusterSize, left)
		if length <= 0 {
			break
		}
		runs = append(runs, filesys.Run{Offset: o.DataStart + (r[0]-2)*o.ClusterSize, Length: length})
		left -= length
	}
	return runs
}

// The FAT reader's recovery map for every deleted entry of the kernel-written images equals the independent
// oracle's: the same run list (merged, compared as lists), the method, the assumptions, and no deleted entry is
// dropped silently (the walk returns as many as the oracle's own count).
func TestFATRecoverableMatchesOracle(t *testing.T) {
	for _, name := range []string{"fat12-recover", "fat16-recover", "fat32-recover"} {
		t.Run(name, func(t *testing.T) {
			img, o := loadRecoverFixture(t, name)
			fsys, err := fat.Open(bytes.NewReader(img), int64(len(img)))
			if err != nil {
				t.Fatal(err)
			}
			walked := map[string]filesys.Entry{}
			paths := map[string]string{}
			var ndeleted int
			if err := filesys.Walk(fsys, fsys.Root(), "/", func(p string, e filesys.Entry, werr error) error {
				if werr != nil {
					t.Errorf("walk %s: %v", p, werr)
					return nil
				}
				if !e.Deleted {
					return nil
				}
				ndeleted++
				size, _ := strconv.ParseInt(entryAttr(e, "size"), 10, 64)
				first, _ := strconv.ParseInt(entryAttr(e, "first_cluster"), 10, 64)
				k := recKey(first, size, e.Type == filesys.TypeDir)
				if _, dup := walked[k]; dup {
					t.Errorf("two deleted entries share first cluster/size %s", k)
				}
				walked[k], paths[k] = e, p
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if ndeleted != o.DeletedEntryCount {
				t.Errorf("the walk returns %d deleted entries, the oracle counts %d: a deleted entry was dropped or invented", ndeleted, o.DeletedEntryCount)
			}
			listed := map[string]bool{}
			for _, d := range o.Deleted {
				k := recKey(d.FirstCluster, d.Size, d.Expect.Skip == "not-a-file")
				if !d.Listed {
					// ruling F2: the content of a deleted directory is not listed
					if e, ok := walked[k]; ok {
						t.Errorf("%s is under a deleted directory but the walk lists it (%s at %s)", d.Path, e.ID, paths[k])
					}
					continue
				}
				listed[k] = true
				e, ok := walked[k]
				if !ok {
					t.Errorf("%s: no deleted entry with first cluster %d and size %d in the walk", d.Path, d.FirstCluster, d.Size)
					continue
				}
				if d.NameState == "lfn-intact" && paths[k] != d.Path {
					t.Errorf("%s: listed as %q", d.Path, paths[k])
				}
				cands, err := fsys.Recoverable(e)
				switch {
				case d.Expect.Skip == "not-a-file":
					if !errors.Is(err, filesys.ErrUnsupported) {
						t.Errorf("%s: a deleted directory gave %v, %v; want ErrUnsupported", d.Path, cands, err)
					}
					continue
				case err != nil || len(cands) != 1:
					t.Errorf("%s: Recoverable = %v, %v; want one candidate", d.Path, cands, err)
					continue
				}
				c := cands[0]
				if c.Method != "fat-contiguous" || c.Size != d.Size {
					t.Errorf("%s: method %q size %d; want fat-contiguous %d", d.Path, c.Method, c.Size, d.Size)
				}
				if !slices.Contains(c.Assumptions, "contiguous-allocation") {
					t.Errorf("%s: assumptions %v lack contiguous-allocation", d.Path, c.Assumptions)
				}
				if !slices.Contains(c.Basis, "first_cluster="+strconv.FormatInt(d.FirstCluster, 10)) {
					t.Errorf("%s: basis %v lacks the first cluster", d.Path, c.Basis)
				}
				if d.Expect.Skip == "empty" {
					if len(c.Runs) != 0 || c.Size != 0 {
						t.Errorf("%s: an empty file has runs %v", d.Path, c.Runs)
					}
					continue
				}
				if want := rangeRuns(o, d.Expect.Range, d.Size); !slices.Equal(c.Runs, want) {
					t.Errorf("%s: runs %v, want the oracle's range %v", d.Path, c.Runs, want)
				}
			}
			// kernel leftovers: deleted entries the oracle has no listed record for (not silently dropped: counted)
			var left int
			for k, p := range paths {
				if !listed[k] {
					left++
					t.Logf("deleted entry without an oracle record: %s (%s)", p, k)
				}
			}
			var nlisted int
			for _, d := range o.Deleted {
				if d.Listed {
					nlisted++
				}
			}
			if left != o.DeletedEntryCount-nlisted {
				t.Errorf("%d unexplained deleted entries, want deleted_entry_count %d - %d listed records", left, o.DeletedEntryCount, nlisted)
			}
		})
	}
}
