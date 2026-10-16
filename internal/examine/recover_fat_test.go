package examine_test

import (
	"bytes"
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys/fat/fattest"
)

// fatID is the entry id of the short entry at loc.
func fatID(loc fattest.EntryLoc) string {
	return fmt.Sprintf("dirent:%d:%d", loc.DirCluster, loc.Index)
}

// artifactOf returns the recovered artifact of the entry with the given id (runs sidecars are not recovered artifacts).
func artifactOf(t *testing.T, sum examine.RecoverSummary, id string) evidence.ManifestRecord {
	t.Helper()
	var got []evidence.ManifestRecord
	for _, a := range sum.Artifacts {
		if a.Source.Kind == evidence.KindRecover && a.Source.Derived != nil && a.Source.Derived.FSID == id {
			got = append(got, a)
		}
	}
	return only(t, got)
}

func recoveryOf(rec evidence.ManifestRecord) *evidence.Recovery { return rec.Source.Derived.Recovery }

func hasAssumptionPrefix(rv *evidence.Recovery, prefix string) bool {
	for _, a := range rv.Assumptions {
		if strings.HasPrefix(a, prefix) {
			return true
		}
	}
	return false
}

func hasAssumption(rv *evidence.Recovery, want string) bool {
	return slices.Contains(rv.Assumptions, want) // F103: exactly, never a prefix
}

func hasBasis(rv *evidence.Recovery, prefix string) bool {
	for _, b := range rv.Basis {
		if strings.HasPrefix(b, prefix) {
			return true
		}
	}
	return false
}

func TestRecoverFATContiguousFile(t *testing.T) {
	data := pattern(5*blk, 1)
	img, _ := fattest.BuildLayout(fattest.Options{Type: 16}, []fattest.File{{Path: "/GONE.BIN", Data: data, Deleted: true}})
	s, disk := imageSession(t, img)
	sum := recoverAll(t, s)
	rec := only(t, sum.Artifacts)
	rv := recoveryOf(rec)
	if sha256hex(readArtifact(t, s.Case, rec)) != sha256hex(data) || rec.Incomplete ||
		rv.Method != "fat-contiguous" || *rv.Confidence != 55 || rv.Class != evidence.ClassDeletedFile {
		t.Fatalf("%+v %+v", rec, rv)
	}
	assertEqualsRuns(t, s.Case, rec, disk)
	verifyOK(t, s.Case)
}

// A cluster of a deleted file that a new file took (its FAT entry is no longer free) cuts the map: only the
// free prefix is written, flagged incomplete, with the cut named and no byte of the new owner.
func TestRecoverFATReusedClusterKeepsFreePrefix(t *testing.T) {
	data := pattern(6*blk, 2)
	img, b := fattest.BuildLayout(fattest.Options{Type: 16}, []fattest.File{{Path: "/GONE.BIN", Data: data, Deleted: true}})
	loc := b.Find("/GONE.BIN", true)
	c4 := loc.Clusters[3]
	live := pattern(blk, 77)
	copy(img[b.Geometry.ClusterOffset(c4):], live) // what the new owner wrote there
	fattest.SetFATEntry(img, b.Geometry, c4, 0xFFFF)
	s, disk := imageSession(t, img)
	sum := recoverAll(t, s)
	rec := artifactOf(t, sum, fatID(loc))
	rv := recoveryOf(rec)
	got := readArtifact(t, s.Case, rec)
	if !bytes.Equal(got, data[:3*blk]) || !rec.Incomplete || *rv.Confidence != 20 {
		t.Fatalf("artifact %d bytes (want the first %d), incomplete %v, confidence %d", len(got), 3*blk, rec.Incomplete, *rv.Confidence)
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
	if cut != 3*blk || len(rv.Excluded) == 0 || rv.Excluded[0].Offset != partStart+b.Geometry.ClusterOffset(c4) {
		t.Errorf("Excluded %+v does not name the 3 cut clusters from cluster %d", rv.Excluded, c4)
	}
	if !strings.Contains(rec.Error, "runs not captured") {
		t.Errorf("Error %q does not say which runs were not captured", rec.Error)
	}
	assertEqualsRuns(t, s.Case, rec, disk)
	verifyOK(t, s.Case)
}

func TestRecoverFATFirstClusterReusedWritesNothing(t *testing.T) {
	img, b := fattest.BuildLayout(fattest.Options{Type: 16}, []fattest.File{{Path: "/GONE.BIN", Data: pattern(3*blk, 3), Deleted: true}})
	fattest.SetFATEntry(img, b.Geometry, b.Find("/GONE.BIN", true).Clusters[0], 0xFFFF)
	s, _ := imageSession(t, img)
	sum := recoverAll(t, s)
	if sum.SkippedBy["no-free-bytes"] != 1 || len(sum.Artifacts) != 0 {
		t.Fatalf("SkippedBy %v, %d artifacts; want one no-free-bytes skip and nothing written", sum.SkippedBy, len(sum.Artifacts))
	}
	verifyOK(t, s.Case)
}

func TestRecoverFATBadAndReservedClusterAreCut(t *testing.T) {
	cases := []struct {
		typ           int
		bad, reserved uint32
	}{{12, 0xFF7, 0xFF0}, {16, 0xFFF7, 0xFFF0}, {32, 0x0FFFFFF7, 0x0FFFFFF0}}
	for _, tc := range cases {
		for name, v := range map[string]uint32{"bad": tc.bad, "reserved": tc.reserved} {
			t.Run(fmt.Sprintf("fat%d-%s", tc.typ, name), func(t *testing.T) {
				data := pattern(6*blk, 4)
				img, b := fattest.BuildLayout(fattest.Options{Type: tc.typ}, []fattest.File{{Path: "/GONE.BIN", Data: data, Deleted: true}})
				loc := b.Find("/GONE.BIN", true)
				fattest.SetFATEntry(img, b.Geometry, loc.Clusters[3], v)
				s, disk := imageSession(t, img)
				sum := recoverAll(t, s)
				rec := artifactOf(t, sum, fatID(loc))
				rv := recoveryOf(rec)
				if got := readArtifact(t, s.Case, rec); !bytes.Equal(got, data[:3*blk]) || !rec.Incomplete || *rv.Confidence != 20 || !hasAssumption(rv, "prefix-only") {
					t.Fatalf("artifact %d bytes (want %d), incomplete %v, %+v", len(got), 3*blk, rec.Incomplete, rv)
				}
				assertEqualsRuns(t, s.Case, rec, disk)
				verifyOK(t, s.Case)
			})
		}
	}
}

// Two deleted files whose first clusters coincide both claim the same bytes: both are written, both capped at 30
// and naming each other, and neither is presented as equal to the source of the other.
func TestRecoverFATCrossLinkedDeletedEntriesBothCapped(t *testing.T) {
	a, bd := pattern(3*blk, 5), pattern(3*blk, 6)
	img, b := fattest.BuildLayout(fattest.Options{Type: 16}, []fattest.File{
		{Path: "/A.BIN", Data: a, Deleted: true}, {Path: "/B.BIN", Data: bd, Deleted: true},
	})
	la, lb := b.Find("/A.BIN", true), b.Find("/B.BIN", true)
	first := la.FirstCluster
	fattest.PatchEntry(img, b.Geometry, lb, &first, nil)
	s, disk := imageSession(t, img)
	sum := recoverAll(t, s)
	if sum.Overlap != 2 || len(sum.Artifacts) < 2 {
		t.Fatalf("Overlap %d, %d artifacts; want 2 and both written", sum.Overlap, len(sum.Artifacts))
	}
	ra, rb := artifactOf(t, sum, fatID(la)), artifactOf(t, sum, fatID(lb))
	for _, x := range []struct {
		rec         evidence.ManifestRecord
		other, mine string
	}{{ra, fatID(lb), "A"}, {rb, fatID(la), "B"}} {
		rv := recoveryOf(x.rec)
		if *rv.Confidence != 30 || !hasAssumption(rv, "overlap="+x.other) {
			t.Errorf("%s: confidence %d, assumptions %v; want 30 and overlap=%s", x.mine, *rv.Confidence, rv.Assumptions, x.other)
		}
		assertEqualsRuns(t, s.Case, x.rec, disk)
	}
	if sha256hex(readArtifact(t, s.Case, rb)) == sha256hex(bd) {
		t.Error("the artifact of B claims B's own bytes although its map points at A's clusters")
	}
	verifyOK(t, s.Case)
}

// A live file whose chain runs through a cluster of the deleted file owns that cluster: the map stops before it.
func TestRecoverFATCrossLinkedWithLiveFileIsCut(t *testing.T) {
	data, liveData := pattern(5*blk, 7), pattern(4*blk, 8)
	img, b := fattest.BuildLayout(fattest.Options{Type: 16}, []fattest.File{
		{Path: "/GONE.BIN", Data: data, Deleted: true}, {Path: "/LIVE.BIN", Data: liveData},
	})
	loc, ll := b.Find("/GONE.BIN", true), b.Find("/LIVE.BIN", false)
	c3 := loc.Clusters[2]
	fattest.SetFATEntry(img, b.Geometry, ll.Clusters[1], c3) // the live chain now passes through c3 ...
	fattest.SetFATEntry(img, b.Geometry, c3, 0xFFFF)         // ... and ends there
	s, disk := imageSession(t, img)
	sum := recoverAll(t, s)
	rec := artifactOf(t, sum, fatID(loc))
	rv := recoveryOf(rec)
	got := readArtifact(t, s.Case, rec)
	if !bytes.Equal(got, data[:2*blk]) || !rec.Incomplete || *rv.Confidence != 20 {
		t.Fatalf("artifact %d bytes (want %d), incomplete %v, confidence %d", len(got), 2*blk, rec.Incomplete, *rv.Confidence)
	}
	for i := 0; i+blk <= len(liveData); i += blk {
		if bytes.Contains(got, liveData[i:i+blk]) {
			t.Errorf("the artifact holds a block of the live file (offset %d)", i)
		}
	}
	assertEqualsRuns(t, s.Case, rec, disk)
	verifyOK(t, s.Case)
}

// A fragmented file is not recoverable from its entry: the contiguous map crosses another file. It is cut at a live
// neighbour and, when the neighbour is deleted too, capped by the overlap. Never claimed equal to the source.
func TestRecoverFATFragmentedFileIsNotClaimedEqual(t *testing.T) {
	frag, nb := pattern(3*blk, 9), pattern(3*blk, 10)
	t.Run("live neighbour", func(t *testing.T) {
		img, b := fattest.BuildLayout(fattest.Options{Type: 16}, []fattest.File{
			{Path: "/FRAG.BIN", Data: frag, Deleted: true, Fragmented: true}, {Path: "/NEXT.BIN", Data: nb},
		})
		loc := b.Find("/FRAG.BIN", true)
		s, disk := imageSession(t, img)
		sum := recoverAll(t, s)
		rec := artifactOf(t, sum, fatID(loc))
		rv := recoveryOf(rec)
		got := readArtifact(t, s.Case, rec)
		if sha256hex(got) == sha256hex(frag) {
			t.Fatal("the artifact claims the bytes of the fragmented source")
		}
		if *rv.Confidence > 20 || !rec.Incomplete || !bytes.Equal(got, frag[:blk]) {
			t.Errorf("confidence %d, incomplete %v, %d bytes; want <= 20, incomplete and the first cluster only", *rv.Confidence, rec.Incomplete, len(got))
		}
		assertEqualsRuns(t, s.Case, rec, disk)
		verifyOK(t, s.Case)
	})
	t.Run("deleted neighbour", func(t *testing.T) {
		img, b := fattest.BuildLayout(fattest.Options{Type: 16}, []fattest.File{
			{Path: "/FRAG.BIN", Data: frag, Deleted: true, Fragmented: true}, {Path: "/NEXT.BIN", Data: nb, Deleted: true},
		})
		loc := b.Find("/FRAG.BIN", true)
		s, disk := imageSession(t, img)
		sum := recoverAll(t, s)
		rec := artifactOf(t, sum, fatID(loc))
		rv := recoveryOf(rec)
		if sha256hex(readArtifact(t, s.Case, rec)) == sha256hex(frag) {
			t.Fatal("the artifact claims the bytes of the fragmented source")
		}
		if *rv.Confidence > 30 || !hasAssumptionPrefix(rv, "overlap=") {
			t.Errorf("confidence %d, assumptions %v; want <= 30 with an overlap", *rv.Confidence, rv.Assumptions)
		}
		assertEqualsRuns(t, s.Case, rec, disk)
		verifyOK(t, s.Case)
	})
}

// warningReason is the reason of the analysis.warning whose id detail is id ("" when there is none).
func warningReason(t *testing.T, s *examine.Session, id string) string {
	t.Helper()
	for _, a := range auditByAction(t, s.Case, "analysis.warning") {
		if got, _ := a.Details["id"].(string); got == id {
			r, _ := a.Details["reason"].(string)
			return r
		}
	}
	return ""
}

func TestRecoverFATCorruptMapIsSkippedWithReason(t *testing.T) {
	// 0xFFF0 lies past the last cluster; 1 is a reserved FAT index below the first data cluster. 0 is "no
	// cluster" on an entry that claims a size.
	for _, first := range []uint32{0xFFF0, 1, 0} {
		t.Run(fmt.Sprintf("first-cluster-%#x", first), func(t *testing.T) {
			img, b := fattest.BuildLayout(fattest.Options{Type: 16}, []fattest.File{{Path: "/GONE.BIN", Data: pattern(3*blk, 11), Deleted: true}})
			loc := b.Find("/GONE.BIN", true)
			size := uint32(3 * blk)
			fattest.PatchEntry(img, b.Geometry, loc, &first, &size)
			s, _ := imageSession(t, img)
			sum := recoverAll(t, s)
			if sum.SkippedBy["corrupt"] != 1 || len(sum.Artifacts) != 0 {
				t.Fatalf("SkippedBy %v, %d artifacts", sum.SkippedBy, len(sum.Artifacts))
			}
			reason := warningReason(t, s, fatID(loc))
			if !strings.HasPrefix(reason, "corrupt") || !strings.Contains(reason, "first cluster") {
				t.Errorf("warning reason for %s is %q; want a corrupt skip naming the first cluster", fatID(loc), reason)
			}
			verifyOK(t, s.Case)
		})
	}
}

// A map that runs past the end of a cut image is skipped, never zero-filled; a file wholly inside it is recovered.
func TestRecoverFATTruncatedImageSkipsRatherThanZeroFills(t *testing.T) {
	inside, cut := pattern(2*blk, 12), pattern(6*blk, 13)
	img, b := fattest.BuildLayout(fattest.Options{Type: 16}, []fattest.File{
		{Path: "/IN.BIN", Data: inside, Deleted: true}, {Path: "/CUT.BIN", Data: cut, Deleted: true},
	})
	li, lc := b.Find("/IN.BIN", true), b.Find("/CUT.BIN", true)
	end := b.Geometry.ClusterOffset(lc.Clusters[2]) + blk // the image ends inside the clusters of CUT.BIN
	s, disk := imageSession(t, img[:end])
	sum := recoverAll(t, s)
	if sum.SkippedBy["corrupt"] != 1 {
		t.Fatalf("SkippedBy %v; want the cut file skipped as corrupt", sum.SkippedBy)
	}
	rec := only(t, sum.Artifacts)
	if rec.Source.Derived.FSID != fatID(li) || !bytes.Equal(readArtifact(t, s.Case, rec), inside) || rec.Incomplete {
		t.Fatalf("the file inside the image: %+v", rec)
	}
	assertEqualsRuns(t, s.Case, rec, disk)
	verifyOK(t, s.Case)
}

func TestRecoverFATUniformFileIsSkipped(t *testing.T) {
	img, b := fattest.BuildLayout(fattest.Options{Type: 16}, []fattest.File{{Path: "/ZERO.BIN", Data: make([]byte, 3*blk), Deleted: true}})
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
	rec := artifactOf(t, kept, fatID(loc))
	if recoveryOf(rec).Content != "uniform" {
		t.Errorf("Content %q, want uniform", recoveryOf(rec).Content)
	}
	assertEqualsRuns(t, s.Case, rec, disk)
	verifyOK(t, s.Case)
}

func TestRecoverFATLongNameMarkersReachTheManifest(t *testing.T) {
	e9 := string(rune(0xe9)) // not an escape in the source: some tools rewrite those
	long := "r" + e9 + "sum" + e9 + " draft: final.txt"
	files := []fattest.File{{Path: "/" + long, Data: pattern(2*blk, 14), Deleted: true, LongName: true}}
	t.Run("slot reuse", func(t *testing.T) {
		img, b := fattest.BuildLayout(fattest.Options{Type: 16}, files)
		loc := b.Find("/"+long, true)
		if len(loc.LFNOffsets) < 2 {
			t.Fatalf("%d long-name entries, need at least 2", len(loc.LFNOffsets))
		}
		// The farthest long-name slot (the one with the terminator) is reused by another, deleted, entry.
		img[loc.LFNOffsets[0]], img[loc.LFNOffsets[0]+11] = 0xE5, 0x08
		s, _ := imageSession(t, img)
		sum := recoverAll(t, s)
		rec := artifactOf(t, sum, fatID(loc))
		rv := recoveryOf(rec)
		if !hasBasis(rv, "lfn=recovered") || !hasBasis(rv, "lfn_tail=unterminated") {
			t.Errorf("Basis %v lacks lfn=recovered and lfn_tail=unterminated", rv.Basis)
		}
		base := rec.Path[strings.LastIndex(rec.Path, "/")+1:]
		if !strings.Contains(rec.Path, "/recovered/p") || !strings.Contains(rec.Path, "-fat16/000001-") {
			t.Errorf("local path %q", rec.Path)
		}
		if safe, err := evidence.SanitizeRelPath(base); err != nil || safe != base || strings.ContainsAny(base, ":\\") {
			t.Errorf("local name %q is not a safe name (%q, %v)", base, safe, err)
		}
		verifyOK(t, s.Case)
	})
	t.Run("orphan set", func(t *testing.T) {
		img, b := fattest.BuildLayout(fattest.Options{Type: 16}, files)
		loc := b.Find("/"+long, true)
		for _, off := range loc.LFNOffsets {
			img[off+13] ^= 0xFF // the checksum no longer matches the short name
		}
		s, _ := imageSession(t, img)
		sum := recoverAll(t, s)
		rv := recoveryOf(artifactOf(t, sum, fatID(loc)))
		if !hasBasis(rv, "name=short-only") || hasBasis(rv, "lfn=") {
			t.Errorf("Basis %v; want name=short-only and no lfn= marker", rv.Basis)
		}
	})
}

// A deleted directory is not recovered, and the files inside it are not listed: documented limit.
func TestRecoverFATDeletedDirectoryIsSkippedAndItsContentIsNotListed(t *testing.T) {
	img, _ := fattest.BuildLayout(fattest.Options{Type: 16}, []fattest.File{
		{Path: "/GONE", Dir: true, Deleted: true}, {Path: "/GONE/IN.TXT", Data: pattern(2*blk, 15)},
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

func TestRecoverFATNeverModifiesSource(t *testing.T) {
	img, _ := fattest.BuildLayout(fattest.Options{Type: 16}, []fattest.File{{Path: "/GONE.BIN", Data: pattern(3*blk, 16), Deleted: true}})
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
func TestRecoverFATListMatchesRun(t *testing.T) {
	img, b := fattest.BuildLayout(fattest.Options{Type: 16}, []fattest.File{
		{Path: "/OK.BIN", Data: pattern(3*blk, 17), Deleted: true},
		{Path: "/REUSED.BIN", Data: pattern(5*blk, 18), Deleted: true},
		{Path: "/A.BIN", Data: pattern(2*blk, 19), Deleted: true},
		{Path: "/B.BIN", Data: pattern(2*blk, 20), Deleted: true},
	})
	fattest.SetFATEntry(img, b.Geometry, b.Find("/REUSED.BIN", true).Clusters[2], 0xFFFF)
	first := b.Find("/A.BIN", true).FirstCluster
	fattest.PatchEntry(img, b.Geometry, b.Find("/B.BIN", true), &first, nil)
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
	if len(planned) != 4 || !reflect.DeepEqual(planned, written) {
		t.Fatalf("planned %+v\nwritten %+v", planned, written)
	}
}

// F103: hasAssumption matches a whole assumption, so the token bad@11 is not satisfied by bad@110.
func TestHasAssumptionMatchesExactly(t *testing.T) {
	rv := &evidence.Recovery{Assumptions: []string{"stale-chain-stop=bad@110", "overlap=/a"}}
	if hasAssumption(rv, "stale-chain-stop=bad@11") {
		t.Error("hasAssumption(bad@11) matched bad@110: a prefix match passes the wrong token")
	}
	if !hasAssumption(rv, "stale-chain-stop=bad@110") || !hasAssumptionPrefix(rv, "overlap=") {
		t.Error("an exact token, or a deliberate prefix, must match")
	}
}
