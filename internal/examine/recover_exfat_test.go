package examine_test

import (
	"bytes"
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys/exfat/exfattest"
)

// exfatCS is the cluster size of the builder's default geometry.
const exfatCS = 4096

// exfatID is the entry id of the File entry set at loc.
func exfatID(loc exfattest.EntryLoc) string {
	return fmt.Sprintf("dirent:%d:%d", loc.DirCluster, loc.Index)
}

// A file stored without a FAT chain is contiguous by the format itself: method exfat-nofatchain, 75.
func TestRecoverExfatNoFatChainFile(t *testing.T) {
	data := pattern(3*exfatCS-37, 21)
	img, b := exfattest.BuildLayout(exfattest.Options{}, []exfattest.File{{Path: "/GONE.BIN", Data: data, Deleted: true}})
	loc := b.Find("/GONE.BIN", true)
	s, disk := imageSession(t, img)
	sum := recoverAll(t, s)
	rec := artifactOf(t, sum, exfatID(loc))
	rv := recoveryOf(rec)
	if !bytes.Equal(readArtifact(t, s.Case, rec), data) || rec.Incomplete ||
		rv.Method != "exfat-nofatchain" || *rv.Confidence != 75 || rv.Class != evidence.ClassDeletedFile {
		t.Fatalf("%+v %+v", rec, rv)
	}
	assertEqualsRuns(t, s.Case, rec, disk)
	verifyOK(t, s.Case)
}

// ValidDataLength ends the map: the bytes past it are never presented, never zero-filled. Complete, because
// the map is exactly what the method states.
func TestRecoverExfatNeverZeroFillsTail(t *testing.T) {
	data := pattern(1500, 22)
	valid := int64(700)
	img, b := exfattest.BuildLayout(exfattest.Options{}, []exfattest.File{{Path: "/GONE.BIN", Data: data, Deleted: true, ValidLength: &valid}})
	loc := b.Find("/GONE.BIN", true)
	s, disk := imageSession(t, img)
	sum := recoverAll(t, s)
	rec := artifactOf(t, sum, exfatID(loc))
	rv := recoveryOf(rec)
	got := readArtifact(t, s.Case, rec)
	if rec.Size != 700 || !bytes.Equal(got, data[:700]) {
		t.Fatalf("artifact size %d, want the first 700 bytes of the data", rec.Size)
	}
	if !hasAssumption(rv, "tail-beyond-valid-data-length-not-stored") || !hasAssumption(rv, "data-length=1500") {
		t.Errorf("assumptions %v lack the tail note", rv.Assumptions)
	}
	if rec.Incomplete {
		t.Errorf("flagged incomplete: %q (the map is exactly what the method states)", rec.Error)
	}
	assertEqualsRuns(t, s.Case, rec, disk)
	verifyOK(t, s.Case)
}

// A cluster that an allocation took since (its bitmap bit is set) cuts the map; the prefix is kept and capped at
// 20, which beats the 75 of the method.
func TestRecoverExfatReusedClusterKeepsFreePrefix(t *testing.T) {
	data := pattern(5*exfatCS, 23)
	img, b := exfattest.BuildLayout(exfattest.Options{}, []exfattest.File{{Path: "/GONE.BIN", Data: data, Deleted: true}})
	loc := b.Find("/GONE.BIN", true)
	c3 := loc.Clusters[2]
	live := pattern(exfatCS, 78)
	copy(img[b.ClusterOffset(c3):], live) // what the new owner wrote there
	exfattest.SetBitmapBit(img, b, c3, true)
	s, disk := imageSession(t, img)
	sum := recoverAll(t, s)
	rec := artifactOf(t, sum, exfatID(loc))
	rv := recoveryOf(rec)
	got := readArtifact(t, s.Case, rec)
	if !bytes.Equal(got, data[:2*exfatCS]) || !rec.Incomplete || *rv.Confidence != 20 {
		t.Fatalf("artifact %d bytes (want %d), incomplete %v, confidence %d", len(got), 2*exfatCS, rec.Incomplete, *rv.Confidence)
	}
	if !hasAssumption(rv, "prefix-only") || !hasAssumption(rv, "cut-state=allocated") {
		t.Errorf("assumptions %v lack prefix-only and cut-state=allocated", rv.Assumptions)
	}
	if bytes.Contains(got, live) {
		t.Error("the artifact holds the bytes of the new owner")
	}
	var cut int64
	for _, x := range rv.Excluded {
		cut += x.Length
	}
	if cut != 3*exfatCS || len(rv.Excluded) == 0 || rv.Excluded[0].Offset != partStart+b.ClusterOffset(c3) {
		t.Errorf("Excluded %+v does not name the 3 cut clusters from cluster %d", rv.Excluded, c3)
	}
	assertEqualsRuns(t, s.Case, rec, disk)
	verifyOK(t, s.Case)
}

// A bad-cluster mark is never reported free, so the file is cut there although the bitmap says free.
func TestRecoverExfatBadClusterInRangeIsCut(t *testing.T) {
	data := pattern(5*exfatCS, 24)
	img, b := exfattest.BuildLayout(exfattest.Options{}, []exfattest.File{{Path: "/GONE.BIN", Data: data, Deleted: true}})
	loc := b.Find("/GONE.BIN", true)
	exfattest.SetFATEntry(img, b, loc.Clusters[2], 0xFFFFFFF7)
	s, disk := imageSession(t, img)
	sum := recoverAll(t, s)
	rec := artifactOf(t, sum, exfatID(loc))
	rv := recoveryOf(rec)
	got := readArtifact(t, s.Case, rec)
	if !bytes.Equal(got, data[:2*exfatCS]) || !rec.Incomplete || *rv.Confidence != 20 || !hasAssumption(rv, "prefix-only") {
		t.Fatalf("artifact %d bytes (want %d), incomplete %v, %+v", len(got), 2*exfatCS, rec.Incomplete, rv)
	}
	assertEqualsRuns(t, s.Case, rec, disk)
	verifyOK(t, s.Case)
}

// The builder keeps the FAT chain of a deleted FatChain/Fragmented file (the kernel driver does too). Damaging
// it keeps the clusters before the break, capped at 20; the bad cluster and the later ones are never written.
func TestRecoverExfatBrokenStaleChainKeepsPrefixCappedAt20(t *testing.T) {
	for _, tc := range []struct {
		name  string
		entry func(cl []uint32) uint32 // the damaged FAT entry of cl[3]
		stop  func(cl []uint32) string
		keep  int // clusters of the file that are kept
	}{
		{"bad", func([]uint32) uint32 { return 0xFFFFFFF7 }, func(cl []uint32) string { return fmt.Sprintf("stale-chain-stop=bad@%d", cl[3]) }, 3},
		// The cluster whose entry loops back is data itself (the loop is found on its link), so it is kept.
		{"loop", func(cl []uint32) uint32 { return cl[1] }, func(cl []uint32) string { return fmt.Sprintf("stale-chain-stop=loop@%d", cl[3]) }, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := pattern(6*exfatCS, 25)
			img, b := exfattest.BuildLayout(exfattest.Options{}, []exfattest.File{{Path: "/FRAG.BIN", Data: data, Deleted: true, Fragmented: true}})
			loc := b.Find("/FRAG.BIN", true)
			exfattest.SetFATEntry(img, b, loc.Clusters[3], tc.entry(loc.Clusters))
			s, disk := imageSession(t, img)
			sum := recoverAll(t, s)
			rec := artifactOf(t, sum, exfatID(loc))
			rv := recoveryOf(rec)
			got := readArtifact(t, s.Case, rec)
			if !bytes.Equal(got, data[:tc.keep*exfatCS]) || !rec.Incomplete || *rv.Confidence != 20 || rv.Method != "exfat-fatchain" {
				t.Fatalf("artifact %d bytes (want %d), incomplete %v, method %s, confidence %d", len(got), tc.keep*exfatCS, rec.Incomplete, rv.Method, *rv.Confidence)
			}
			if want := tc.stop(loc.Clusters); !hasAssumption(rv, want) { // F99: the assumptions form only
				t.Errorf("assumptions %v do not name %s (Error %q)", rv.Assumptions, want, rec.Error)
			}
			if tc.name == "bad" {
				for i := 3; i < 6; i++ {
					if bytes.Contains(got, data[i*exfatCS:(i+1)*exfatCS]) {
						t.Errorf("the artifact holds the bytes of cluster %d, at or after the break", i+1)
					}
				}
			}
			assertEqualsRuns(t, s.Case, rec, disk)
			verifyOK(t, s.Case)
		})
	}
}

// The untouched stale chain of a fragmented deleted file is followed: exfat-fatchain, 65, the source's bytes.
// One fragment that was taken since cuts the file at the fragment boundary and caps it at 20.
func TestRecoverExfatStaleChainFollowed(t *testing.T) {
	data := pattern(4*exfatCS, 26)
	build := func() ([]byte, *exfattest.Layout, exfattest.EntryLoc) {
		img, b := exfattest.BuildLayout(exfattest.Options{}, []exfattest.File{{Path: "/FRAG.BIN", Data: data, Deleted: true, Fragmented: true}})
		return img, b, b.Find("/FRAG.BIN", true)
	}
	t.Run("untouched", func(t *testing.T) {
		img, b, loc := build()
		s, disk := imageSession(t, img)
		sum := recoverAll(t, s)
		rec := artifactOf(t, sum, exfatID(loc))
		rv := recoveryOf(rec)
		if !bytes.Equal(readArtifact(t, s.Case, rec), data) || rec.Incomplete || rv.Method != "exfat-fatchain" || *rv.Confidence != 65 {
			t.Fatalf("incomplete %v, method %s, confidence %d", rec.Incomplete, rv.Method, *rv.Confidence)
		}
		var want []evidence.Run
		for _, c := range loc.Clusters {
			want = append(want, evidence.Run{Offset: partStart + b.ClusterOffset(c), Length: exfatCS})
		}
		if !reflect.DeepEqual(rec.Source.Derived.Runs, want) {
			t.Errorf("runs %+v, want the chain %+v", rec.Source.Derived.Runs, want)
		}
		assertEqualsRuns(t, s.Case, rec, disk)
		verifyOK(t, s.Case)
	})
	t.Run("one fragment taken", func(t *testing.T) {
		img, b, loc := build()
		exfattest.SetBitmapBit(img, b, loc.Clusters[2], true)
		s, disk := imageSession(t, img)
		sum := recoverAll(t, s)
		rec := artifactOf(t, sum, exfatID(loc))
		rv := recoveryOf(rec)
		got := readArtifact(t, s.Case, rec)
		if !bytes.Equal(got, data[:2*exfatCS]) || !rec.Incomplete || *rv.Confidence != 20 {
			t.Fatalf("artifact %d bytes (want %d), incomplete %v, confidence %d", len(got), 2*exfatCS, rec.Incomplete, *rv.Confidence)
		}
		assertEqualsRuns(t, s.Case, rec, disk)
		verifyOK(t, s.Case)
	})
}

// Two deleted sets with the same first cluster both claim the same bytes: both written, both capped at 30 and
// naming each other, and neither is presented as equal to the other's source.
func TestRecoverExfatCrossLinkedDeletedEntriesBothCapped(t *testing.T) {
	a, bd := pattern(3*exfatCS, 27), pattern(3*exfatCS, 28)
	img, b := exfattest.BuildLayout(exfattest.Options{}, []exfattest.File{
		{Path: "/A.BIN", Data: a, Deleted: true}, {Path: "/B.BIN", Data: bd, Deleted: true},
	})
	la, lb := b.Find("/A.BIN", true), b.Find("/B.BIN", true)
	first := la.FirstCluster
	exfattest.PatchStream(img, lb, exfattest.StreamPatch{First: &first})
	s, disk := imageSession(t, img)
	sum := recoverAll(t, s)
	if sum.Overlap != 2 || len(sum.Artifacts) < 2 {
		t.Fatalf("Overlap %d, %d artifacts; want 2 and both written", sum.Overlap, len(sum.Artifacts))
	}
	ra, rb := artifactOf(t, sum, exfatID(la)), artifactOf(t, sum, exfatID(lb))
	for _, x := range []struct {
		rec         evidence.ManifestRecord
		other, mine string
	}{{ra, exfatID(lb), "A"}, {rb, exfatID(la), "B"}} {
		rv := recoveryOf(x.rec)
		if *rv.Confidence != 30 || !hasAssumption(rv, "overlap="+x.other) {
			t.Errorf("%s: confidence %d, assumptions %v; want 30 and overlap=%s", x.mine, *rv.Confidence, rv.Assumptions, x.other)
		}
		assertEqualsRuns(t, s.Case, x.rec, disk)
	}
	if bytes.Equal(readArtifact(t, s.Case, rb), bd) {
		t.Error("the artifact of B claims B's own bytes although its map points at A's clusters")
	}
	verifyOK(t, s.Case)
}

// A set whose checksum does not match is damaged: skipped as corrupt, never recovered, and audited with a reason.
func TestRecoverExfatBadChecksumIsSkippedCorrupt(t *testing.T) {
	img, b := exfattest.BuildLayout(exfattest.Options{}, []exfattest.File{{Path: "/GONE.BIN", Data: pattern(2*exfatCS, 29), Deleted: true}})
	loc := b.Find("/GONE.BIN", true)
	exfattest.CorruptSetChecksum(img, loc)
	s, _ := imageSession(t, img)
	sum := recoverAll(t, s)
	if sum.SkippedBy["corrupt"] != 1 || len(sum.Artifacts) != 0 {
		t.Fatalf("SkippedBy %v, %d artifacts", sum.SkippedBy, len(sum.Artifacts))
	}
	if reason := warningReason(t, s, exfatID(loc)); !strings.HasPrefix(reason, "corrupt") || !strings.Contains(reason, "checksum") {
		t.Errorf("warning reason %q; want a corrupt skip naming the checksum", reason)
	}
	verifyOK(t, s.Case)
}

// A first cluster outside the heap (or in the reserved range) is corrupt, whatever the size says.
func TestRecoverExfatFirstClusterOutsideHeapIsCorrupt(t *testing.T) {
	for _, first := range []uint32{0xFFFFF0, 1, 0} {
		t.Run(fmt.Sprintf("first-cluster-%#x", first), func(t *testing.T) {
			img, b := exfattest.BuildLayout(exfattest.Options{}, []exfattest.File{{Path: "/GONE.BIN", Data: pattern(2*exfatCS, 30), Deleted: true}})
			loc := b.Find("/GONE.BIN", true)
			exfattest.PatchStream(img, loc, exfattest.StreamPatch{First: &first})
			s, _ := imageSession(t, img)
			sum := recoverAll(t, s)
			if sum.SkippedBy["corrupt"] != 1 || len(sum.Artifacts) != 0 {
				t.Fatalf("SkippedBy %v, %d artifacts", sum.SkippedBy, len(sum.Artifacts))
			}
			if reason := warningReason(t, s, exfatID(loc)); !strings.HasPrefix(reason, "corrupt") || !strings.Contains(reason, "first cluster") {
				t.Errorf("warning reason %q; want a corrupt skip naming the first cluster", reason)
			}
			verifyOK(t, s.Case)
		})
	}
}

func TestRecoverExfatUniformIsSkipped(t *testing.T) {
	img, b := exfattest.BuildLayout(exfattest.Options{}, []exfattest.File{{Path: "/ZERO.BIN", Data: make([]byte, 2*exfatCS), Deleted: true}})
	loc := b.Find("/ZERO.BIN", true)
	s, disk := imageSession(t, img)
	sum := recoverAll(t, s)
	if sum.SkippedBy["uniform"] != 1 || sum.Uniform != 1 || len(sum.Artifacts) != 0 {
		t.Fatalf("SkippedBy %v, Uniform %d, %d artifacts", sum.SkippedBy, sum.Uniform, len(sum.Artifacts))
	}
	kept, err := s.Recover(context.Background(), examine.RecoverOptions{Partition: -1, All: true, KeepUniform: true})
	if err != nil {
		t.Fatal(err)
	}
	rec := artifactOf(t, kept, exfatID(loc))
	if recoveryOf(rec).Content != "uniform" {
		t.Errorf("Content %q, want uniform", recoveryOf(rec).Content)
	}
	assertEqualsRuns(t, s.Case, rec, disk)
	verifyOK(t, s.Case)
}

// A deleted directory is not recovered, and what was inside it is not listed: documented limit.
func TestRecoverExfatDeletedDirectoryIsSkipped(t *testing.T) {
	img, _ := exfattest.BuildLayout(exfattest.Options{}, []exfattest.File{
		{Path: "/GONE", Dir: true, Deleted: true}, {Path: "/GONE/IN.TXT", Data: pattern(2*exfatCS, 31)},
	})
	s, _ := imageSession(t, img)
	plan, err := s.PlanRecovery(context.Background(), examine.RecoverOptions{Partition: -1, All: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Items) != 1 || strings.Contains(plan.Items[0].Path, "IN.TXT") {
		t.Fatalf("plan items %+v; want only the directory", plan.Items)
	}
	sum := recoverAll(t, s)
	if sum.SkippedBy["not-a-file"] != 1 || len(sum.Artifacts) != 0 || sum.Recovered != 0 {
		t.Fatalf("SkippedBy %v, %d artifacts", sum.SkippedBy, len(sum.Artifacts))
	}
}

func TestRecoverExfatNeverModifiesSource(t *testing.T) {
	img, _ := exfattest.BuildLayout(exfattest.Options{}, []exfattest.File{{Path: "/GONE.BIN", Data: pattern(3*exfatCS, 32), Deleted: true}})
	s, _ := imageSession(t, img)
	before := sha256hex(readArtifact(t, s.Case, s.Parent))
	if sum := recoverAll(t, s); sum.Recovered != 1 {
		t.Fatalf("Recovered %d", sum.Recovered)
	}
	if after := sha256hex(readArtifact(t, s.Case, s.Parent)); after != before {
		t.Fatalf("the source artifact changed: %s -> %s", before, after)
	}
	verifyOK(t, s.Case)
}

// The list (PlanRecovery) plans exactly what a run writes: method, confidence, runs and size.
func TestRecoverExfatListMatchesRun(t *testing.T) {
	img, b := exfattest.BuildLayout(exfattest.Options{}, []exfattest.File{
		{Path: "/OK.BIN", Data: pattern(3*exfatCS, 33), Deleted: true},
		{Path: "/REUSED.BIN", Data: pattern(5*exfatCS, 34), Deleted: true},
		{Path: "/A.BIN", Data: pattern(2*exfatCS, 35), Deleted: true},
		{Path: "/B.BIN", Data: pattern(2*exfatCS, 36), Deleted: true},
		{Path: "/FRAG.BIN", Data: pattern(3*exfatCS, 37), Deleted: true, Fragmented: true},
	})
	exfattest.SetBitmapBit(img, b, b.Find("/REUSED.BIN", true).Clusters[2], true)
	first := b.Find("/A.BIN", true).FirstCluster
	exfattest.PatchStream(img, b.Find("/B.BIN", true), exfattest.StreamPatch{First: &first})
	s, _ := imageSession(t, img)
	opts := examine.RecoverOptions{Partition: -1, All: true}
	plan, err := s.PlanRecovery(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	type shape struct {
		Method     string
		Confidence int
		Runs       []evidence.Run
		Size       int64
	}
	planned := map[string]shape{}
	for _, it := range plan.Items {
		for _, c := range it.Candidates {
			if c.Skip == "" {
				planned[it.ID] = shape{c.Method, c.Confidence, c.Runs, c.Size}
			}
		}
	}
	sum, err := s.Recover(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	written := map[string]shape{}
	for _, a := range sum.Artifacts {
		if a.Source.Kind != evidence.KindRecover {
			continue
		}
		rv := recoveryOf(a)
		written[a.Source.Derived.FSID] = shape{rv.Method, *rv.Confidence, a.Source.Derived.Runs, a.Size}
	}
	if len(planned) != 5 || !reflect.DeepEqual(planned, written) {
		t.Fatalf("planned %+v\nwritten %+v", planned, written)
	}
}

// exFAT stores a UTC offset with its times: a recovered file's time carries the zone (UTC), where FAT's local
// time has none. A set without a valid offset field keeps the local, zone-less form.
func TestRecoverExfatTimesAreZoneKnown(t *testing.T) {
	wall := func(h, m, s int) time.Time { return time.Date(2023, 11, 14, h, m, s, 0, time.UTC) }
	img, b := exfattest.BuildLayout(exfattest.Options{}, []exfattest.File{
		{
			Path: "/ZONED.BIN", Data: pattern(exfatCS, 38), Deleted: true,
			Times:   [3]time.Time{wall(10, 20, 30), wall(22, 13, 31), wall(8, 0, 0)},
			Offsets: [3]exfattest.Offset{{Valid: true, Quarters: 4}, {Valid: true, Quarters: -20}, {}},
			Inc10ms: [2]uint8{0, 2},
		},
		{
			Path: "/LOCAL.BIN", Data: pattern(exfatCS, 39), Deleted: true,
			Times: [3]time.Time{wall(1, 2, 4), wall(1, 2, 5), wall(1, 2, 6)},
		},
	})
	s, _ := imageSession(t, img)
	sum := recoverAll(t, s)
	zoned := artifactOf(t, sum, exfatID(b.Find("/ZONED.BIN", true))).Source.Derived.Times["modified"]
	local := artifactOf(t, sum, exfatID(b.Find("/LOCAL.BIN", true))).Source.Derived.Times["modified"]
	// 22:13:31.02 at -05:00 is 03:13:31.02 UTC the next day.
	if zoned != "2023-11-15T03:13:31.02Z" {
		t.Errorf("modified of the file with an offset = %q, want 2023-11-15T03:13:31.02Z", zoned)
	}
	if local != "2023-11-14T01:02:05" {
		t.Errorf("modified of the file without an offset = %q, want the zone-less 2023-11-14T01:02:05", local)
	}
}

// When no FAT chain survived (every entry of the deleted file's chain is zero, what some drivers do), contiguity
// is the assumption: exfat-contiguous, at most 55, and the artifact is not claimed equal to a fragmented source.
func TestRecoverExfatClearedChainIsContiguousAssumption(t *testing.T) {
	data := pattern(4*exfatCS, 40)
	img, b := exfattest.BuildLayout(exfattest.Options{}, []exfattest.File{{Path: "/FRAG.BIN", Data: data, Deleted: true, Fragmented: true}})
	loc := b.Find("/FRAG.BIN", true)
	for _, c := range loc.Clusters {
		exfattest.SetFATEntry(img, b, c, 0)
	}
	s, disk := imageSession(t, img)
	sum := recoverAll(t, s)
	rec := artifactOf(t, sum, exfatID(loc))
	rv := recoveryOf(rec)
	if rv.Method != "exfat-contiguous" || *rv.Confidence > 55 || !hasAssumption(rv, "contiguous-allocation") {
		t.Fatalf("method %s, confidence %d, assumptions %v", rv.Method, *rv.Confidence, rv.Assumptions)
	}
	if bytes.Equal(readArtifact(t, s.Case, rec), data) {
		t.Error("the artifact equals the fragmented source although the chain that said so is gone")
	}
	assertEqualsRuns(t, s.Case, rec, disk)
	verifyOK(t, s.Case)
}
