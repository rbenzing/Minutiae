package exfat_test

import (
	"bytes"
	"errors"
	"slices"
	"strconv"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/exfat"
)

func entryAttr(e filesys.Entry, key string) string {
	for _, kv := range e.Attrs {
		if kv.Key == key {
			return kv.Value
		}
	}
	return ""
}

// recKey ties a walk entry to an oracle record: the first cluster and, for a file, the size (a directory's size
// is not compared).
func recKey(first, size int64, dir bool) string {
	if dir {
		size = 0
	}
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

// The exFAT reader's recovery map for every deleted entry of the kernel-written image equals the independent
// oracle's: the method by the oracle's own rule, the same MERGED run list as a list (a fragmented exfat-fatchain
// file has two runs, ruling F16), and no deleted entry is dropped silently.
func TestExfatRecoverableMatchesOracle(t *testing.T) {
	img, o := loadRecoverFixture(t, "exfat-recover")
	fsys, err := exfat.Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	walked, paths := map[string]filesys.Entry{}, map[string]string{}
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
		first, _ := strconv.ParseInt(entryAttr(e, "first_cluster"), 10, 64)
		k := recKey(first, e.Size, e.Type == filesys.TypeDir)
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
	var nlisted int
	for _, d := range o.Deleted {
		isDir := d.Expect.Skip == "not-a-file"
		k := recKey(d.FirstCluster, d.Size, isDir)
		if !d.Listed {
			// ruling F2: the content of a deleted directory is not listed
			if e, ok := walked[k]; ok {
				t.Errorf("%s is under a deleted directory but the walk lists it (%s at %s)", d.Path, e.ID, paths[k])
			}
			continue
		}
		nlisted++
		listed[k] = true
		e, ok := walked[k]
		if !ok {
			t.Errorf("%s: no deleted entry with first cluster %d and size %d in the walk", d.Path, d.FirstCluster, d.Size)
			continue
		}
		if paths[k] != d.Path { // exFAT keeps whole names
			t.Errorf("%s: listed as %q", d.Path, paths[k])
		}
		cands, err := fsys.Recoverable(e)
		if isDir {
			if !errors.Is(err, filesys.ErrUnsupported) {
				t.Errorf("%s: a deleted directory gave %v, %v; want ErrUnsupported", d.Path, cands, err)
			}
			continue
		}
		if err != nil || len(cands) != 1 {
			t.Errorf("%s: Recoverable = %v, %v; want one candidate", d.Path, cands, err)
			continue
		}
		c := cands[0]
		wantMethod := "exfat-contiguous"
		switch {
		case d.NoFatChain:
			wantMethod = "exfat-nofatchain"
		case d.FatResidue == "chain-kept":
			wantMethod = "exfat-fatchain"
		}
		if d.Expect.Outcome == "recover" && d.Expect.Method != wantMethod {
			t.Errorf("%s: the oracle's method %q disagrees with its own rule %q", d.Path, d.Expect.Method, wantMethod)
		}
		if size := d.ValidDataLength; d.Size == 0 {
			if len(c.Runs) != 0 || c.Size != 0 {
				t.Errorf("%s: an empty file has size %d runs %v", d.Path, c.Size, c.Runs)
			}
			continue
		} else if c.Size != size {
			t.Errorf("%s: candidate size %d, want valid_data_length %d", d.Path, c.Size, size)
		}
		if c.Method != wantMethod {
			t.Errorf("%s: method %q, want %q", d.Path, c.Method, wantMethod)
		}
		if wantMethod != "exfat-nofatchain" && wantMethod != "exfat-fatchain" && !slices.Contains(c.Assumptions, "contiguous-allocation") {
			t.Errorf("%s: assumptions %v lack contiguous-allocation", d.Path, c.Assumptions)
		}
		if !slices.Contains(c.Basis, "first_cluster="+strconv.FormatInt(d.FirstCluster, 10)) {
			t.Errorf("%s: basis %v lacks the first cluster", d.Path, c.Basis)
		}
		if want := rangeRuns(o, d.Expect.Range, d.ValidDataLength); !slices.Equal(c.Runs, want) {
			t.Errorf("%s: runs %v, want the oracle's range %v", d.Path, c.Runs, want)
		}
	}
	var left int
	for k, p := range paths {
		if !listed[k] {
			left++
			t.Logf("deleted entry without an oracle record: %s (%s)", p, k)
		}
	}
	if left != o.DeletedEntryCount-nlisted {
		t.Errorf("%d unexplained deleted entries, want deleted_entry_count %d - %d listed records", left, o.DeletedEntryCount, nlisted)
	}
}
