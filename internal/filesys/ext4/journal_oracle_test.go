package ext4_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/ext4"
)

// The jbd2 reader and the ext4-journal-inode method against the kernel-written journal fixtures (testdata/journal/, plan 3G
// Task 14) and their independent oracle (tools/fixtures/ext4_journal_oracle.py: its own decoder cross-checked with logdump,
// dumpe2fs and debugfs, and the kernel's replay of a copy of the crash image). Every figure compared is read from the oracle file;
// the only values written here are the per-family intentions of jfFamilyWant, which hold the oracle itself to the plan's table
// (an oracle regenerated into a different story fails there, not silently).

func jfOpen(t testing.TB, img []byte) *ext4.FS {
	t.Helper()
	f, err := ext4.Open(bytes.NewReader(img), int64(len(img)))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

type jfAnswer struct {
	skip, method, region string
	conf                 int
	equal                string
}

// jfFamilyWant is the plan's per-family table (README "Families"), by journal mode. skip is the oracle's skip token
// ("" = recovered); region is the journal region of an ext4-journal-inode answer; conf and equal are the confidence and
// the relation to the source the examine test checks. A family or mode missing here is not checked against the table.
var jfFamilyWant = map[string]map[string]jfAnswer{
	"jgone":     {"none": {skip: "no-map"}, "committed": {skip: "no-map"}, "stale": {skip: "no-map"}},
	"jtrunc":    {"none": {skip: "no-map"}, "committed": {skip: "no-map"}, "stale": {skip: "no-map"}},
	"jhardlink": {"none": {skip: "reused"}, "committed": {skip: "reused"}, "stale": {skip: "reused"}},
	"jinline":   {"none": {skip: "unsupported"}, "committed": {skip: "unsupported"}, "stale": {skip: "unsupported"}},
	"jsmall": {
		"none": {skip: "no-map"}, "committed": {skip: "no-map"},
		"stale": {method: "ext4-journal-inode", region: "stale", conf: 40, equal: "full"},
	},
	"jdeep": {
		"none":      {method: "ext4-extent-leaf", conf: 20, equal: "prefix-of-artifact"},
		"committed": {method: "ext4-extent-leaf", conf: 20, equal: "prefix-of-artifact"},
		"stale":     {method: "ext4-journal-inode", region: "stale", conf: 40, equal: "full"},
	},
	"jreuse": {
		"none": {skip: "no-map"}, "committed": {skip: "no-map"},
		"stale": {method: "ext4-journal-inode", region: "stale", conf: 20, equal: "artifact-is-prefix"},
	},
	"jswap": {
		"none": {skip: "no-map"}, "committed": {skip: "no-map"},
		"stale": {method: "ext4-journal-inode", region: "stale", conf: 30, equal: "no"},
	},
	"crash-live-copy": {
		"none":      {skip: "no-map"},
		"committed": {method: "ext4-journal-inode", region: "live", conf: 40, equal: "full"},
		"stale":     {method: "ext4-journal-inode", region: "live", conf: 40, equal: "full"},
	},
	"bm-small": {
		"none": {skip: "no-map"}, "committed": {skip: "no-map"},
		"stale": {method: "ext4-journal-inode", region: "stale", conf: 40, equal: "full"},
	},
	"bm-indirect": {
		"none": {skip: "no-map"}, "committed": {skip: "no-map"},
		"stale": {method: "ext4-journal-inode", region: "stale", conf: 40, equal: "full"},
	},
}

// jfCheckFamilyTable holds one oracle record to the plan's table.
func jfCheckFamilyTable(t *testing.T, d journalFixtureDeleted, mode string) {
	t.Helper()
	want, ok := jfFamilyWant[d.Family][mode]
	if !ok {
		return
	}
	x := d.Expect[mode]
	if want.skip != "" {
		if x.Outcome != "skip" || x.Skip != want.skip {
			t.Errorf("%s (%s) %s: the oracle says %s %q, the plan's table says skip %q", d.Path, d.Family, mode, x.Outcome, x.Skip, want.skip)
		}
		return
	}
	if x.Outcome != "recover" || x.Method != want.method || x.Region != want.region || x.Confidence != want.conf || x.EqualSource != want.equal {
		t.Errorf("%s (%s) %s: the oracle says %s %s/%s conf %d equal %q, the plan's table says %s/%s conf %d equal %q",
			d.Path, d.Family, mode, x.Outcome, x.Method, x.Region, x.Confidence, x.EqualSource, want.method, want.region, want.conf, want.equal)
	}
}

func jfTxnKey(seq uint32, region string, start int64) string {
	return fmt.Sprintf("%d/%s/%d", seq, region, start)
}

// TestJBD2MatchesLogdump holds the reader's journal and every transaction to the oracle's (which the oracle itself checked
// against logdump: txns[].logdump says which transactions logdump listed, with exactly these tags).
func TestJBD2MatchesLogdump(t *testing.T) {
	for _, name := range journalFixtureImages {
		t.Run(name, func(t *testing.T) {
			img, o := loadJournalFixture(t, name)
			f := jfOpen(t, img)
			info, err := f.Journal()
			if err != nil {
				t.Fatal(err)
			}
			j := o.Journal
			if info.Type != "jbd2" || info.Version != j.Version || int64(info.BlockSize) != j.BlockSize || info.Blocks != j.Blocks ||
				int64(info.First) != j.First || info.Sequence != j.Sequence || int64(info.Start) != j.Start || info.NeedsRecovery != j.NeedsRecovery {
				t.Errorf("Journal() = %+v, the oracle says %+v", info, j)
			}
			if info.Incomplete || info.Stop != "" {
				t.Errorf("the journal is reported incomplete (stop %q): the oracle read all of it", info.Stop)
			}
			// the oracle names features in bit order, compat first; the reader names them its own way, so compare as sets
			// of lower-cased names with the oracle's "csum_v3" spelled by either
			norm := func(fs []string) []string {
				out := make([]string, len(fs))
				for i, s := range fs {
					out[i] = strings.ToLower(strings.NewReplacer("-", "", "_", "").Replace(s))
				}
				sort.Strings(out)
				return out
			}
			if !slices.Equal(norm(info.Features), norm(j.Features)) {
				t.Errorf("features %v, the oracle says %v", info.Features, j.Features)
			}

			var got []filesys.JournalTxn
			if err := f.JournalTransactions(func(tx filesys.JournalTxn) bool { got = append(got, tx); return true }); err != nil {
				t.Fatal(err)
			}
			if len(got) != len(o.Txns) {
				t.Fatalf("%d transactions, the oracle lists %d", len(got), len(o.Txns))
			}
			byKey := map[string]filesys.JournalTxn{}
			for _, tx := range got {
				k := jfTxnKey(tx.Seq, tx.Region, tx.StartBlock)
				if _, dup := byKey[k]; dup {
					t.Fatalf("two transactions share %s", k)
				}
				byKey[k] = tx
			}
			logdumpTxns := 0
			for _, w := range o.Txns {
				tx, ok := byKey[jfTxnKey(w.Seq, w.Region, w.StartJBlk)]
				if !ok {
					t.Errorf("the oracle's transaction %s is not read", jfTxnKey(w.Seq, w.Region, w.StartJBlk))
					continue
				}
				if w.LogDump {
					logdumpTxns++
				}
				commit := tx.CommitBlock
				if commit <= 0 {
					commit = -1 // 0 and -1 both mean none
				}
				if commit != w.CommitJ || tx.Committed != w.Committed || tx.Checksum != w.Checksum {
					t.Errorf("seq %d %s: commit block %d committed %v checksum %q; oracle %d %v %q", w.Seq, w.Region, commit, tx.Committed, tx.Checksum, w.CommitJ, w.Committed, w.Checksum)
				}
				gotRev := append([]int64{}, int64s(tx.Revoked)...)
				if len(gotRev) != len(w.Revoked) || (len(gotRev) > 0 && !slices.Equal(gotRev, w.Revoked)) {
					t.Errorf("seq %d %s: revoked %v, the oracle says %v", w.Seq, w.Region, gotRev, w.Revoked)
				}
				if len(tx.Blocks) != len(w.Tags) {
					t.Errorf("seq %d %s: %d tags, the oracle says %d", w.Seq, w.Region, len(tx.Blocks), len(w.Tags))
					continue
				}
				for i, wt := range w.Tags {
					g := tx.Blocks[i]
					if int64(g.FSBlock) != wt.FSBlock || int64(g.JournalBlock) != wt.JBlk || int(g.Flags) != wt.Flags || g.Copy != wt.Copy || g.Revoked != wt.Revoked || g.Escaped != wt.Escaped {
						t.Errorf("seq %d %s tag %d: fsblock %d jblk %d flags %d copy %q revoked %v escaped %v; oracle %+v",
							w.Seq, w.Region, i, g.FSBlock, g.JournalBlock, g.Flags, g.Copy, g.Revoked, g.Escaped, wt)
					}
				}
			}
			if logdumpTxns == 0 {
				t.Error("the oracle marks no transaction as confirmed by logdump: the comparison would be vacuous")
			}
		})
	}
}

func int64s(u []uint64) []int64 {
	out := make([]int64, len(u))
	for i, v := range u {
		out[i] = int64(v)
	}
	return out
}

// TestJBD2StaleRegionMarkedOnFixtures: a clean image has no live region, so every transaction is stale; on the crash image
// the live ones are exactly the ones logdump -a listed, and the stale ones are the fragments outside the log.
func TestJBD2StaleRegionMarkedOnFixtures(t *testing.T) {
	for _, name := range journalFixtureImages {
		t.Run(name, func(t *testing.T) {
			img, o := loadJournalFixture(t, name)
			f := jfOpen(t, img)
			live := map[string]bool{}
			var stale int
			if err := f.JournalTransactions(func(tx filesys.JournalTxn) bool {
				switch tx.Region {
				case "live":
					live[jfTxnKey(tx.Seq, tx.Region, tx.StartBlock)] = true
				case "stale":
					stale++
				default:
					t.Errorf("seq %d: region %q", tx.Seq, tx.Region)
				}
				return true
			}); err != nil {
				t.Fatal(err)
			}
			want := map[string]bool{}
			for _, w := range o.Txns {
				if w.Region == "live" {
					if !w.LogDump {
						t.Errorf("seq %d: the oracle's live transaction was not confirmed by logdump", w.Seq)
					}
					want[jfTxnKey(w.Seq, w.Region, w.StartJBlk)] = true
				}
			}
			if !reflect.DeepEqual(live, want) {
				t.Errorf("live transactions %v, the oracle (logdump -a) lists %v", live, want)
			}
			if o.Journal.NeedsRecovery {
				if len(live) == 0 || stale == 0 {
					t.Errorf("%d live and %d stale transactions: the crash image must hold both", len(live), stale)
				}
			} else if len(live) != 0 || stale == 0 {
				t.Errorf("a clean image has %d live and %d stale transactions, want 0 live and some stale", len(live), stale)
			}
		})
	}
}

// TestJBD2NeverChangesLiveViewOnFixtures: every journal call, the preparation of the journal recovery and every recovery
// answer leave the live view, the unallocated map and the filesystem info exactly as a filesystem that was never asked.
func TestJBD2NeverChangesLiveViewOnFixtures(t *testing.T) {
	for _, name := range journalFixtureImages {
		t.Run(name, func(t *testing.T) {
			img, _ := loadJournalFixture(t, name)
			touched, untouched := jfOpen(t, img), jfOpen(t, img)
			if _, err := touched.Journal(); err != nil {
				t.Fatal(err)
			}
			var txns []filesys.JournalTxn
			if err := touched.JournalTransactions(func(tx filesys.JournalTxn) bool { txns = append(txns, tx); return true }); err != nil {
				t.Fatal(err)
			}
			blocks := 0
			for _, tx := range txns {
				for i := range tx.Blocks {
					if _, _, err := touched.JournalBlock(tx, i); err == nil {
						blocks++
					}
				}
			}
			if blocks == 0 {
				t.Fatal("no journal block was readable: the test would prove nothing")
			}
			ctx := context.Background()
			if err := touched.PrepareRecovery(ctx); err != nil {
				t.Fatal(err)
			}
			if err := touched.PrepareJournalRecovery(ctx, filesys.JournalUseStale); err != nil {
				t.Fatal(err)
			}
			answers := 0
			for _, e := range walkDeleted(t, touched) {
				if _, err := touched.Recoverable(e); err == nil {
					answers++
				}
			}
			if answers == 0 {
				t.Fatal("no deleted entry was answered")
			}
			a, b := lvCapture(t, touched), lvCapture(t, untouched)
			if len(a.Walk) < 5 || len(a.Files) < 1 {
				t.Fatalf("walk %d entries, %d files: too small to prove anything", len(a.Walk), len(a.Files))
			}
			if !reflect.DeepEqual(a, b) {
				switch {
				case !reflect.DeepEqual(a.Walk, b.Walk):
					t.Error("the walk differs after the journal calls")
				case !reflect.DeepEqual(a.Files, b.Files) || !reflect.DeepEqual(a.Lookups, b.Lookups):
					t.Error("Open or Lookup differ after the journal calls")
				case !reflect.DeepEqual(a.Unallocated, b.Unallocated):
					t.Error("Unallocated differs after the journal calls")
				default:
					t.Errorf("Info differs after the journal calls:\n%+v\n%+v", a.Info, b.Info)
				}
			}
		})
	}
}

// jfEntryKey ties an oracle record to a walked entry (the oracle's inode 0 never occurs here).
func jfEntryKey(name string, inode uint32) string { return oracleKey(name, inode) }

// jfRunsOf returns the oracle's captured blocks for a record, as merged byte runs cut at the record's size.
func jfRunsOf(x journalFixtureExpect, bs int64) []filesys.Run {
	return cutRuns(mergeBlockRuns(x.CapturedBlocks, bs), x.Size)
}

// TestExt4JournalRecoverableMatchesOracle: for every deleted entry and every journal mode, PrepareJournalRecovery then
// Recoverable give what the oracle derived: the method, the journal sequence and region, the runs (the true blocks up to the
// stop, cut at the size), the stop and the oracle's assumptions, or the typed skip.
func TestExt4JournalRecoverableMatchesOracle(t *testing.T) {
	for _, name := range journalFixtureImages {
		img, o := loadJournalFixture(t, name)
		for _, mode := range []string{"none", "committed", "stale"} {
			t.Run(name+"/"+mode, func(t *testing.T) {
				f := jfOpen(t, img)
				ctx := context.Background()
				if err := f.PrepareRecovery(ctx); err != nil {
					t.Fatal(err)
				}
				if use := filesys.JournalUse(mode); use != filesys.JournalUseNone {
					if err := f.PrepareJournalRecovery(ctx, use); err != nil {
						t.Fatal(err)
					}
				}
				byKey := map[string]filesys.Entry{}
				deleted := walkDeleted(t, f)
				for _, e := range deleted {
					byKey[e.Name+"/"+entryInode(e)] = e
				}
				if len(deleted) != o.DeletedEntryCount {
					t.Errorf("the walk finds %d deleted entries, the oracle says %d", len(deleted), o.DeletedEntryCount)
				}
				matched := 0
				for _, d := range o.Deleted {
					jfCheckFamilyTable(t, d, mode)
					e, ok := byKey[jfEntryKey(d.Name, d.Inode)]
					if !ok {
						t.Errorf("%s: the walk lists no deleted entry %s", d.Path, jfEntryKey(d.Name, d.Inode))
						continue
					}
					matched++
					jfCheckRecoverable(t, f, e, d, mode, o.BlockSize)
				}
				if matched != len(o.Deleted) {
					t.Errorf("%d of %d oracle records matched a walked entry", matched, len(o.Deleted))
				}
			})
		}
	}
}

func jfCheckRecoverable(t *testing.T, f *ext4.FS, e filesys.Entry, d journalFixtureDeleted, mode string, bs int64) {
	t.Helper()
	x := d.Expect[mode]
	cands, err := f.Recoverable(e)
	if x.Outcome == "skip" {
		switch x.Skip {
		case "reused":
			if !errors.Is(err, filesys.ErrReused) {
				t.Errorf("%s %s: err %v, want ErrReused", d.Path, mode, err)
			}
		case "unsupported":
			if !errors.Is(err, filesys.ErrUnsupported) {
				t.Errorf("%s %s: err %v, want ErrUnsupported", d.Path, mode, err)
			}
		case "undecided":
			if !errors.Is(err, filesys.ErrUndecided) {
				t.Errorf("%s %s: err %v, want ErrUndecided", d.Path, mode, err)
			}
		case "no-map":
			if err != nil || len(cands) != 0 {
				t.Errorf("%s %s: %d candidates, err %v, want nil, nil", d.Path, mode, len(cands), err)
			}
		case "no-free-bytes":
			// decided by the examiner against the free map, not by the reader: the reader offers a map
			if err != nil {
				t.Errorf("%s %s: err %v", d.Path, mode, err)
			}
		default:
			t.Errorf("%s %s: the oracle skip %q has no reader rule in this test", d.Path, mode, x.Skip)
		}
		return
	}
	if err != nil {
		t.Errorf("%s %s: Recoverable: %v", d.Path, mode, err)
		return
	}
	var methods []string
	for _, c := range cands {
		methods = append(methods, c.Method)
	}
	sort.Strings(methods)
	if want := slices.Sorted(slices.Values(x.CandidateMethods)); !slices.Equal(methods, want) {
		t.Errorf("%s %s: candidate methods %v, the oracle says %v", d.Path, mode, methods, want)
	}
	i := slices.IndexFunc(cands, func(c filesys.Candidate) bool { return c.Method == x.Method })
	if i < 0 {
		t.Errorf("%s %s: no candidate by method %s (have %v)", d.Path, mode, x.Method, methods)
		return
	}
	c := cands[i]
	got, want := mergeFSRuns(c.Runs), jfRunsOf(x, bs)
	if x.Cut {
		// the free-prefix cut is the examiner's (it needs the free map): the reader names the whole map and the oracle's
		// captured blocks are its leading bytes; the declared size is larger than what is captured
		if !jfRunsStartWith(got, want) || c.Size <= x.Size {
			t.Errorf("%s %s/%s: runs %v size %d; the oracle's captured prefix %v (size %d) must lead a longer map", d.Path, mode, x.Method, got, c.Size, want, x.Size)
		}
	} else {
		if !slices.Equal(got, want) {
			t.Errorf("%s %s/%s: runs %v, the oracle (the true blocks up to the stop, cut at the size) says %v", d.Path, mode, x.Method, got, want)
		}
		if c.Size != x.Size {
			t.Errorf("%s %s/%s: size %d, the oracle says %d", d.Path, mode, x.Method, c.Size, x.Size)
		}
	}
	for _, a := range x.Assumptions {
		if strings.HasPrefix(a, "overlap=") {
			continue // the overlap rule needs every entry's map: the examiner's (TestExt4JournalRecoverMatchesOracle)
		}
		if !slices.Contains(c.Assumptions, a) {
			t.Errorf("%s %s/%s: assumption %q missing from %v", d.Path, mode, x.Method, a, c.Assumptions)
		}
	}
	if x.Method != "ext4-journal-inode" {
		if c.Journal != nil {
			t.Errorf("%s %s/%s: a non-journal map carries a journal reference %+v", d.Path, mode, x.Method, c.Journal)
		}
		return
	}
	if c.Journal == nil || c.Journal.Seq != x.Seq || c.Journal.Region != x.Region || c.Journal.Revoked {
		t.Errorf("%s %s: journal reference %+v, the oracle says seq %d region %s", d.Path, mode, c.Journal, x.Seq, x.Region)
	}
	if c.Journal != nil && !c.Journal.Committed {
		t.Errorf("%s %s: the journal copy is not from a committed transaction", d.Path, mode)
	}
	// 3G-R13: the effective ceiling on a real image is 40
	if x.Confidence > 40 {
		t.Errorf("%s %s: the oracle's confidence %d exceeds the real-image ceiling of 40", d.Path, mode, x.Confidence)
	}
}

// jfRunsStartWith reports whether the bytes of runs begin with the bytes of prefix.
func jfRunsStartWith(runs, prefix []filesys.Run) bool {
	var total int64
	for _, p := range prefix {
		total += p.Length
	}
	var cut []filesys.Run
	left := total
	for _, r := range runs {
		if left <= 0 {
			break
		}
		r.Length = min(r.Length, left)
		left -= r.Length
		cut = append(cut, r)
	}
	return left == 0 && slices.Equal(mergeFSRuns(cut), mergeFSRuns(prefix))
}
