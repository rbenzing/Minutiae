package examine_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/evidence/evidencetest"
	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/image/ewf/ewftest"
)

// r6Verify runs case verify with the composed reproduce check.
func r6Verify(t *testing.T, c *evidence.Case) evidence.VerifyReport {
	t.Helper()
	rep, err := c.VerifyWith(context.Background(), examine.RecoveredCheck())
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func r6NoReproduceProblem(t *testing.T, rep evidence.VerifyReport) {
	t.Helper()
	var got []string
	for _, p := range rep.Problems {
		if strings.Contains(p, "reproduce: ") {
			got = append(got, p)
		}
	}
	if len(got) != 0 {
		t.Fatalf("unexpected reproduce problems:\n%s", strings.Join(got, "\n"))
	}
}

// r6Image is a raw parent of n pattern bytes imported as one artifact.
func r6Image(t *testing.T, n int) (*evidence.Case, evidence.ManifestRecord, []byte) {
	t.Helper()
	c := evidencetest.NewCase(t)
	data := pat(7, n)
	return c, evidencetest.AddImage(t, c, data), data
}

func segRefs(recs []evidence.ManifestRecord) []evidence.SegmentRef {
	refs := make([]evidence.SegmentRef, len(recs))
	for i, r := range recs {
		refs[i] = evidence.SegmentRef{ID: r.ID, SHA256: r.SHA256}
	}
	return refs
}

var r6Runs = []evidence.Run{{Offset: 4096, Length: 8192}, {Offset: 100000, Length: 5000}}

func TestVerifyRecoveredReproduces(t *testing.T) {
	const size = 8192 + 5000
	t.Run("good artifact is clean and counted", func(t *testing.T) {
		c, parent, img := r6Image(t, 256<<10)
		evidencetest.AddRecovered(t, c, parent, img, evidencetest.RecoveredSpec{Runs: r6Runs})
		rep := r6Verify(t, c)
		evidencetest.RequireClean(t, rep)
		if rep.RecoveredReproduced != 1 || rep.RecoveredArtifacts != 1 {
			t.Errorf("reproduced %d of %d, want 1 of 1", rep.RecoveredReproduced, rep.RecoveredArtifacts)
		}
	})
	// a flipped byte at the first, a middle (second run) and the last position
	for _, tc := range []struct{ pos, imgOff int }{{0, 4096}, {9000, 100000 + 9000 - 8192}, {size - 1, 100000 + 4999}} {
		t.Run(fmt.Sprintf("byte %d differs", tc.pos), func(t *testing.T) {
			c, parent, img := r6Image(t, 256<<10)
			data := slices.Concat(img[4096:4096+8192], img[100000:105000])
			data[tc.pos] ^= 0x01
			evidencetest.AddRecovered(t, c, parent, img, evidencetest.RecoveredSpec{Runs: r6Runs, Data: data})
			rep := r6Verify(t, c)
			evidencetest.RequireProblem(t, rep, fmt.Sprintf("reproduce: byte %d of the artifact differs from image offset %d", tc.pos, tc.imgOff))
			if rep.RecoveredReproduced != 0 {
				t.Errorf("reproduced = %d, want 0", rep.RecoveredReproduced)
			}
		})
	}
	t.Run("another file's bytes", func(t *testing.T) {
		c, parent, img := r6Image(t, 256<<10)
		evidencetest.AddRecovered(t, c, parent, img, evidencetest.RecoveredSpec{Runs: r6Runs, Data: img[50000 : 50000+size]})
		evidencetest.RequireProblem(t, r6Verify(t, c), "reproduce: byte 0 of the artifact differs")
	})
	t.Run("artifact longer than its runs is R4, not R6", func(t *testing.T) {
		c, parent, img := r6Image(t, 256<<10)
		data := slices.Concat(img[4096:4096+8192], img[100000:105000], []byte("extra bytes"))
		evidencetest.AddRecovered(t, c, parent, img, evidencetest.RecoveredSpec{Runs: r6Runs, Data: data})
		rep := r6Verify(t, c)
		evidencetest.RequireProblem(t, rep, "runs cover")
		r6NoReproduceProblem(t, rep)
	})
	t.Run("split parent of three segments", func(t *testing.T) {
		c := newCase(t)
		img := pat(8, 300<<10)
		recs, _ := importFiles(t, c, []string{"d.001", "d.002", "d.003"}, [][]byte{img[:100<<10], img[100<<10 : 200<<10], img[200<<10:]})
		// the first run crosses the first segment boundary, the second the second
		runs := []evidence.Run{{Offset: 100<<10 - 1000, Length: 3000}, {Offset: 200<<10 - 10, Length: 50}}
		evidencetest.AddRecovered(t, c, recs[0], img, evidencetest.RecoveredSpec{Runs: runs, ParentSegments: segRefs(recs)})
		rep := r6Verify(t, c)
		evidencetest.RequireClean(t, rep)
		if rep.RecoveredReproduced != 1 {
			t.Errorf("reproduced = %d, want 1", rep.RecoveredReproduced)
		}
	})
	t.Run("non-zero partition offset", func(t *testing.T) {
		c, parent, img := r6Image(t, 256<<10)
		evidencetest.AddRecovered(t, c, parent, img, evidencetest.RecoveredSpec{Runs: r6Runs, PartitionOffset: 65536})
		rep := r6Verify(t, c)
		evidencetest.RequireClean(t, rep)
		if rep.RecoveredReproduced != 1 {
			t.Errorf("reproduced = %d, want 1", rep.RecoveredReproduced)
		}
	})
	t.Run("runs sidecar", func(t *testing.T) {
		c, parent, img := r6Image(t, 256<<10)
		evidencetest.AddRecovered(t, c, parent, img, evidencetest.RecoveredSpec{Runs: r6Runs, Sidecar: true})
		rep := r6Verify(t, c)
		evidencetest.RequireClean(t, rep)
		if rep.RecoveredReproduced != 1 {
			t.Errorf("reproduced = %d, want 1", rep.RecoveredReproduced)
		}
	})
}

// C36: once R6 ran, the built-in "not reproduced byte for byte" notice for recover and carve goes away.
func TestVerifyRecoveredMarksReproduceRan(t *testing.T) {
	c, parent, img := r6Image(t, 256<<10)
	evidencetest.AddRecovered(t, c, parent, img, evidencetest.RecoveredSpec{Runs: r6Runs})
	plain, err := c.Verify()
	if err != nil {
		t.Fatal(err)
	}
	evidencetest.RequireNotice(t, plain, "not reproduced byte for byte")
	rep := r6Verify(t, c)
	for _, n := range rep.Notices {
		if strings.Contains(n, "not reproduced byte for byte") {
			t.Errorf("notice survives R6: %s", n)
		}
	}
}

func TestVerifyRecoveredRunsOutsideImage(t *testing.T) {
	const imgSize = 64 << 10
	for _, tc := range []struct {
		name string
		run  evidence.Run
		want string
	}{
		{"ends one byte past", evidence.Run{Offset: imgSize - 10, Length: 11}, fmt.Sprintf("reproduce: run 0 (%d+11) lies outside", imgSize-10)},
		{"starts past the end", evidence.Run{Offset: imgSize, Length: 5}, fmt.Sprintf("reproduce: run 0 (%d+5) lies outside", imgSize)},
		{"valid for the partition, not the image", evidence.Run{Offset: 40000, Length: 70000}, "reproduce: run 0 (40000+70000) lies outside"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, parent, img := r6Image(t, imgSize)
			data := make([]byte, tc.run.Length)
			evidencetest.AddRecovered(t, c, parent, img, evidencetest.RecoveredSpec{Runs: []evidence.Run{tc.run}, Data: data, PartitionOffset: 32 << 10})
			rep := r6Verify(t, c)
			evidencetest.RequireProblem(t, rep, tc.want)
			if rep.RecoveredReproduced != 0 {
				t.Errorf("reproduced = %d, want 0", rep.RecoveredReproduced)
			}
		})
	}
}

func TestVerifyRecoveredParentMissing(t *testing.T) {
	t.Run("not in the manifest", func(t *testing.T) {
		c, parent, img := r6Image(t, 64<<10)
		ghost := parent
		ghost.ID = "ghost-parent"
		evidencetest.AddRecovered(t, c, ghost, img, evidencetest.RecoveredSpec{Runs: r6Runs[:1]})
		rep := r6Verify(t, c)
		evidencetest.RequireProblem(t, rep, "reproduce: parent")
		if rep.RecoveredReproduced != 0 {
			t.Errorf("reproduced = %d, want 0", rep.RecoveredReproduced)
		}
	})
	t.Run("file gone from disk", func(t *testing.T) {
		c, parent, img := r6Image(t, 64<<10)
		evidencetest.AddRecovered(t, c, parent, img, evidencetest.RecoveredSpec{Runs: r6Runs[:1]})
		if err := os.Remove(filepath.Join(c.Dir, filepath.FromSlash(parent.Path))); err != nil {
			t.Fatal(err)
		}
		evidencetest.RequireProblem(t, r6Verify(t, c), "reproduce: parent")
	})
}

func TestVerifyRecoveredParentSegmentsMustMatch(t *testing.T) {
	t.Run("a split parent recorded as one file", func(t *testing.T) {
		c := newCase(t)
		img := pat(8, 200<<10)
		recs, _ := importFiles(t, c, []string{"d.001", "d.002"}, [][]byte{img[:100<<10], img[100<<10:]})
		evidencetest.AddRecovered(t, c, recs[0], img, evidencetest.RecoveredSpec{Runs: r6Runs[:1]})
		evidencetest.RequireProblem(t, r6Verify(t, c), "reproduce: parent segments differ")
	})
	t.Run("segments recorded for a single parent", func(t *testing.T) {
		c, parent, img := r6Image(t, 64<<10)
		refs := []evidence.SegmentRef{{ID: parent.ID, SHA256: parent.SHA256}, {ID: "x", SHA256: parent.SHA256}}
		evidencetest.AddRecovered(t, c, parent, img, evidencetest.RecoveredSpec{Runs: r6Runs[:1], ParentSegments: refs})
		evidencetest.RequireProblem(t, r6Verify(t, c), "reproduce: parent segments differ")
	})
}

func TestVerifyRecoveredDoesNotNeedPartitionTable(t *testing.T) {
	damaged := bytes.Clone(e01Media())
	for i := range 1024 {
		damaged[i] = 0xAA // the protective MBR and the primary GPT header
	}
	for i := len(damaged) - 17*1024; i < len(damaged); i++ {
		damaged[i] = 0xAA // the backup GPT
	}
	c := evidencetest.NewCase(t)
	parent := evidencetest.AddImage(t, c, damaged)
	evidencetest.AddRecovered(t, c, parent, damaged, evidencetest.RecoveredSpec{Runs: []evidence.Run{{Offset: 0, Length: 1024}, {Offset: 40960, Length: 4096}}})
	rep := r6Verify(t, c)
	evidencetest.RequireClean(t, rep)
	if rep.RecoveredReproduced != 1 {
		t.Errorf("reproduced = %d, want 1", rep.RecoveredReproduced)
	}
}

func TestVerifyRecoveredEWFParent(t *testing.T) {
	media := e01Media()
	o := ewftest.Options{SectorsPerChunk: 8, ChunksPerSegment: 3, Compress: ewftest.CompressMixed}
	t.Run("reproduces at logical offsets", func(t *testing.T) {
		c := evidencetest.NewCase(t)
		recs := importE01(t, c, o, media)
		runs := []evidence.Run{{Offset: 5000, Length: 6000}, {Offset: int64(len(media)) - 100, Length: 100}}
		evidencetest.AddRecovered(t, c, recs[0], media, evidencetest.RecoveredSpec{Runs: runs, ParentSegments: segRefs(recs)})
		rep := r6Verify(t, c)
		evidencetest.RequireClean(t, rep)
		if rep.RecoveredReproduced != 1 {
			t.Errorf("reproduced = %d, want 1", rep.RecoveredReproduced)
		}
	})
	t.Run("a run beyond the logical size", func(t *testing.T) {
		c := evidencetest.NewCase(t)
		recs := importE01(t, c, o, media)
		runs := []evidence.Run{{Offset: int64(len(media)) - 10, Length: 11}}
		evidencetest.AddRecovered(t, c, recs[0], media, evidencetest.RecoveredSpec{Runs: runs, ParentSegments: segRefs(recs), Data: make([]byte, 11)})
		evidencetest.RequireProblem(t, r6Verify(t, c), "reproduce: run 0")
	})
}

func TestVerifyRecoveredBadChunkIsProblem(t *testing.T) {
	media := e01Media()
	c := evidencetest.NewCase(t)
	bad := bytes.Repeat([]byte{0x55}, e01Chunk+4) // the right length, a wrong Adler-32
	recs := importE01(t, c, ewftest.Options{
		SectorsPerChunk: 8, ChunksPerSegment: 3, Compress: ewftest.CompressMixed,
		Override: map[int]ewftest.RawChunk{3: {Data: bad}},
	}, media)
	// the run starts in chunk 2 and runs into the unreadable chunk 3
	runs := []evidence.Run{{Offset: 2*e01Chunk + 100, Length: e01Chunk}}
	evidencetest.AddRecovered(t, c, recs[0], media, evidencetest.RecoveredSpec{Runs: runs, ParentSegments: segRefs(recs)})
	rep := r6Verify(t, c)
	evidencetest.RequireProblem(t, rep, fmt.Sprintf("reproduce: parent image unreadable at offset %d", 3*e01Chunk))
	if rep.RecoveredReproduced != 0 {
		t.Errorf("reproduced = %d, want 0", rep.RecoveredReproduced)
	}
}

func TestVerifyRecoveredCarveArtifactScope(t *testing.T) {
	// a plain artifact that starts like an E01 segment: only a raw read reproduces it
	data := append([]byte("EVF\x09\x0d\x0a\xff\x00"), pat(3, 64<<10)...)
	plain := func(t *testing.T) (*evidence.Case, evidence.ManifestRecord) {
		c := evidencetest.NewCase(t)
		parent, err := c.Capture("dev1", "pull1", "blob.bin", evidence.Source{Kind: "pull", DeviceID: "dev1"}, func(w io.Writer) error {
			_, err := w.Write(data)
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		return c, parent
	}
	spec := evidencetest.RecoveredSpec{Kind: evidence.KindCarve, Class: evidence.ClassCarved, Method: "carve-signature", Scope: "artifact", Path: "carved/p1-mtfs/000001-a.bin"}
	c, parent := plain(t)
	spec.Runs = []evidence.Run{{Offset: 100, Length: 3000}, {Offset: 20000, Length: 500}}
	evidencetest.AddRecovered(t, c, parent, data, spec)
	rep := r6Verify(t, c)
	r6NoReproduceProblem(t, rep)
	if rep.RecoveredReproduced != 1 {
		t.Errorf("reproduced = %d, want 1 (problems %q)", rep.RecoveredReproduced, rep.Problems)
	}
	// a run past the artifact is outside it
	c2, parent2 := plain(t)
	spec.Runs, spec.Data = []evidence.Run{{Offset: int64(len(data)) - 5, Length: 6}}, make([]byte, 6)
	evidencetest.AddRecovered(t, c2, parent2, data, spec)
	evidencetest.RequireProblem(t, r6Verify(t, c2), "reproduce: run 0")
}

func TestVerifyRecoveredCancelled(t *testing.T) {
	c, parent, img := r6Image(t, 256<<10)
	evidencetest.AddRecovered(t, c, parent, img, evidencetest.RecoveredSpec{Runs: r6Runs})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rep, err := c.VerifyWith(ctx, examine.RecoveredCheck())
	if err != nil {
		t.Fatal(err)
	}
	evidencetest.RequireProblem(t, rep, "reproduce: verification cancelled")
	if rep.OK() || rep.RecoveredReproduced != 0 {
		t.Errorf("a cancelled verify must never be clean: ok=%v reproduced=%d", rep.OK(), rep.RecoveredReproduced)
	}
	// the check called directly stops at once and counts nothing
	recs, err := c.Manifest()
	if err != nil {
		t.Fatal(err)
	}
	var direct evidence.VerifyReport
	examine.VerifyRecovered(ctx, c, recs, &direct)
	if direct.RecoveredReproduced != 0 {
		t.Errorf("a cancelled check reproduced %d artifacts", direct.RecoveredReproduced)
	}
}

func TestVerifyRecoveredArtifactTamperedAfterwards(t *testing.T) {
	c, parent, img := r6Image(t, 256<<10)
	rec := evidencetest.AddRecovered(t, c, parent, img, evidencetest.RecoveredSpec{Runs: r6Runs})
	p := filepath.Join(c.Dir, filepath.FromSlash(rec.Path))
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	b[1234] ^= 0xFF
	if err := os.WriteFile(p, b, 0o600); err != nil { //nolint:gosec // p is inside the test case directory
		t.Fatal(err)
	}
	rep := r6Verify(t, c)
	evidencetest.RequireProblem(t, rep, "hash mismatch")
	evidencetest.RequireProblem(t, rep, "reproduce: byte 1234 of the artifact differs from image offset 5330")
}

func TestVerifyRecoveredLeavesCaseUntouched(t *testing.T) {
	c, parent, img := r6Image(t, 256<<10)
	evidencetest.AddRecovered(t, c, parent, img, evidencetest.RecoveredSpec{Runs: r6Runs, Sidecar: true})
	before := hashTree(t, c.Dir)
	n := len(auditEntries(t, c))
	r6Verify(t, c)
	after := hashTree(t, c.Dir)
	for name, h := range before {
		if name != "audit.jsonl" && after[name] != h {
			t.Errorf("%s changed during verify", name)
		}
	}
	for name := range after {
		if _, ok := before[name]; !ok {
			t.Errorf("%s was created during verify", name)
		}
	}
	var added []string
	for _, e := range auditEntries(t, c)[n:] {
		added = append(added, e.Action)
	}
	if !slices.Equal(added, []string{"verify.run"}) {
		t.Errorf("verify appended %v, want only verify.run", added)
	}
}

// hashTree maps every file under dir (slash paths) to its sha256, ignoring the lock file.
func hashTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || d.Name() == "case.lock" {
			return err
		}
		b, err := os.ReadFile(p) //nolint:gosec // a test reads its own case directory
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		h := sha256.Sum256(b)
		out[filepath.ToSlash(rel)] = hex.EncodeToString(h[:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestVerifyRecoveredStreamsInChunks(t *testing.T) {
	const size = 40 << 20
	c := evidencetest.NewCase(t)
	img := bytes.Repeat(pat(5, 4096), size/4096+1)
	parent := evidencetest.AddImage(t, c, img)
	evidencetest.AddRecovered(t, c, parent, img, evidencetest.RecoveredSpec{Runs: []evidence.Run{{Offset: 2048, Length: size}}})
	var reads, total int64
	var biggest int
	defer examine.SetReproduceReadObserver(func(n int) {
		reads++
		total += int64(n)
		biggest = max(biggest, n)
	})()
	rep := r6Verify(t, c)
	evidencetest.RequireClean(t, rep)
	if rep.RecoveredReproduced != 1 {
		t.Fatalf("reproduced = %d, want 1", rep.RecoveredReproduced)
	}
	if biggest > 1<<20 || reads < 2*size/(1<<20) || total != 2*size {
		t.Errorf("%d reads, the largest %d bytes, %d bytes in all; want chunks of at most 1 MiB covering 2 x %d", reads, biggest, total, size)
	}
}

func TestVerifyAfterRecoverIsClean(t *testing.T) {
	a, b := pat(1, 3*bs), pat(2, 2*bs)
	e := newRecEnv(t, delNode("/a-del.bin", a), delNode("/b-del.bin", b))
	sum := e.recover(t, examine.RecoverOptions{All: true})
	if sum.Recovered != 2 {
		t.Fatalf("recovered %d, want 2", sum.Recovered)
	}
	rep := r6Verify(t, e.c)
	evidencetest.RequireClean(t, rep)
	if rep.RecoveredArtifacts != 2 || rep.RecoveredReproduced != 2 {
		t.Errorf("reproduced %d of %d, want 2 of 2", rep.RecoveredReproduced, rep.RecoveredArtifacts)
	}
	evidencetest.RequireNotice(t, rep, "recovered data is not live evidence")
	for _, n := range rep.Notices {
		if strings.Contains(n, "not reproduced byte for byte") {
			t.Errorf("notice after R6: %s", n)
		}
	}
}
