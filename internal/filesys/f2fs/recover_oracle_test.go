package f2fs_test

import (
	"bytes"
	"errors"
	"fmt"
	"path"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs"
)

// leadingRuns returns the first n blocks of the oracle's true_blocks (file order) as runs, cut at size.
func leadingRuns(ranges [][2]int64, n int64, size int64) []filesys.Run {
	var out []filesys.Run
	left := size
	for _, r := range ranges {
		if n <= 0 || left <= 0 {
			break
		}
		blocks := min(r[1]-r[0]+1, n)
		length := min(blocks*4096, left)
		out = append(out, filesys.Run{Offset: r[0] * 4096, Length: length})
		n -= blocks
		left -= length
	}
	return coalesce(out)
}

// TestF2FSRecoverMatchesOracle (spec 16): for every deleted entry of the two kernel-written images the reader
// answers what the independent oracle says. Where the oracle says recover (or a free-space cut that examine makes,
// reuse), there is one candidate whose runs are the oracle's true blocks up to the stop, whose basis names the
// stale inode block, and whose stop assumption is the oracle's. Every other answer is the typed error the skip
// reason stands for.
func TestF2FSRecoverMatchesOracle(t *testing.T) {
	for _, name := range recoverImages {
		t.Run(name, func(t *testing.T) {
			img, o := loadRecoverFixture(t, name)
			fsys, err := f2fs.Open(bytes.NewReader(img), int64(len(img)))
			if err != nil {
				t.Fatal(err)
			}
			listed := listedDeleted(t, fsys)
			answered := 0
			for _, d := range o.Deleted {
				if !d.Listed {
					continue
				}
				e, ok := listed[deletedKey{d.DirNID, d.Ino, path.Base(d.Path)}]
				if !ok {
					t.Errorf("%s: not listed by the reader", d.Path)
					continue
				}
				answered++
				cands, err := fsys.Recoverable(e)
				x := d.Expect
				switch {
				case x.Outcome == "recover" || (x.Outcome == "skip" && x.Skip == "no-free-bytes"):
					// no-free-bytes is examine's decision about the free space; the reader still maps the file
					if err != nil || len(cands) != 1 {
						t.Errorf("%s: Recoverable = %v, %v; want one candidate", d.Path, cands, err)
						continue
					}
					c := cands[0]
					if c.Method != f2fs.MethodNodeScan {
						t.Errorf("%s: method %q", d.Path, c.Method)
					}
					wantBlocks := rangeBlocks(d.TrueBlocks)
					if x.Stop != "" {
						rest, ok := strings.CutPrefix(x.Stop, "hole@")
						n, perr := strconv.ParseInt(rest, 10, 64)
						if !ok || perr != nil {
							t.Fatalf("%s: oracle stop %q is not hole@<n>", d.Path, x.Stop)
						}
						wantBlocks = n // the data blocks before the first hole
					}
					want := leadingRuns(d.TrueBlocks, wantBlocks, d.Size)
					if got := coalesce(c.Runs); !slices.Equal(got, want) {
						t.Errorf("%s: runs %v, want the oracle's true blocks up to the stop %v", d.Path, got, want)
					}
					if x.Stop == "" && c.Size != d.Size {
						t.Errorf("%s: size %d, want %d", d.Path, c.Size, d.Size)
					}
					if want := fmt.Sprintf("node:nid=%d:blk=0x%x", d.Ino, d.Stale.Addr); !slices.Contains(c.Basis, want) {
						t.Errorf("%s: basis %v lacks %q", d.Path, c.Basis, want)
					}
					hasStop := ""
					for _, a := range c.Assumptions {
						if rest, ok := strings.CutPrefix(a, "stale-chain-stop="); ok {
							hasStop = rest
						}
					}
					if x.Stop == "" && hasStop != "" {
						t.Errorf("%s: unexpected stop assumption %q", d.Path, hasStop)
					}
					if x.Stop != "" && hasStop != x.Stop {
						t.Errorf("%s: stop assumption %q, want stale-chain-stop=%s", d.Path, hasStop, x.Stop)
					}
				case x.Skip == "not-found":
					if !errors.Is(err, filesys.ErrNotFound) || len(cands) != 0 {
						t.Errorf("%s: Recoverable = %v, %v; want ErrNotFound", d.Path, cands, err)
					}
				case x.Skip == "reused":
					if !errors.Is(err, filesys.ErrReused) || len(cands) != 0 {
						t.Errorf("%s: Recoverable = %v, %v; want ErrReused", d.Path, cands, err)
					} else if want := fmt.Sprintf("inode %d is live (NAT ", d.Ino); !strings.Contains(err.Error(), want) {
						t.Errorf("%s: error %q lacks %q", d.Path, err, want)
					}
				case x.Skip == "unsupported" || x.Skip == "not-a-file":
					if !errors.Is(err, filesys.ErrUnsupported) || len(cands) != 0 {
						t.Errorf("%s: Recoverable = %v, %v; want ErrUnsupported", d.Path, cands, err)
					}
				case x.Skip == "empty":
					if err != nil || len(cands) != 1 || len(cands[0].Runs) != 0 || cands[0].Size != 0 {
						t.Errorf("%s: Recoverable = %v, %v; want the empty candidate", d.Path, cands, err)
					}
				default:
					t.Errorf("%s: oracle outcome %q skip %q has no reader-level meaning", d.Path, x.Outcome, x.Skip)
				}
			}
			if answered != o.DeletedEntryCount {
				t.Errorf("%d deleted entries answered, the oracle counts %d", answered, o.DeletedEntryCount)
			}
		})
	}
}
