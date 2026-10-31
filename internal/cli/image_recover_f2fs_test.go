package cli

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/device"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs/f2fstest"
	"github.com/rbenzing/minutiae/internal/volume/volumetest"
)

// F2FS through `image recover` (3D Task 8): the real reader, the default driver registry and the shared exit-1
// rule. Images are builder images with two node segments and summaries.

const (
	f2bs    = f2fstest.BlockSize
	f2oAddr = 360 // i_addr in an inode block without an extra attribute header
)

func f2Stamp(n int, seed string) []byte {
	out := make([]byte, n)
	for i := range out {
		rec := fmt.Sprintf("%s:%d", seed, i/f2bs)
		rec += strings.Repeat(" ", 32-len(rec))
		out[i] = rec[i%32]
	}
	return out
}

func f2Blk(img []byte, a uint32) []byte { return img[int(a)*f2bs : (int(a)+1)*f2bs] }

// f2PlantCopy writes a copy of the block at src into free node slot j of the last node segment of a 16-segment
// image with footer version ver and its summary naming nid.
func f2PlantCopy(o f2fstest.Options, img []byte, src, j, nid uint32, ver uint64, mod func(b []byte)) {
	l := f2fstest.Geometry(o)
	a := l.Main + 15*f2fstest.BlocksPerSeg + j
	copy(f2Blk(img, a), f2Blk(img, src))
	if mod != nil {
		mod(f2Blk(img, a))
	}
	f2fstest.SetFooter(img, a, f2fstest.Footer{NID: nid, Ino: nid, CPVer: ver})
	f2fstest.SetSSAEntry(img, o, a, nid)
	f2fstest.ResealInode(o, img, a)
}

// f2Env imports an F2FS volume as the one partition of a disk into a fresh case. The default driver registry is
// used (no injected filesystem).
func f2Env(t *testing.T, vol []byte) *imgEnv {
	t.Helper()
	const firstLBA = 40
	sectors := uint64(len(vol)+511) / 512
	disk := volumetest.GPT(512, firstLBA+sectors+8+40, "11111111-2222-3333-4444-555555555555", []volumetest.Part{{
		StartLBA: firstLBA, Sectors: sectors, TypeGUID: imgLinuxType,
		GUID: "00000000-0000-0000-0000-000000000001", Name: "data",
	}})
	copy(disk[firstLBA*512:], vol)
	e := &imgEnv{d: Deps{}, c: newCLICase(t)}
	code, out := run(t, e.d, "image", "import", "--case", e.c, "--json", imgFile(t, disk))
	if code != 0 {
		t.Fatalf("image import: %d %s", code, out)
	}
	var recs []evidence.ManifestRecord
	if err := json.Unmarshal(jsonPart(out), &recs); err != nil || len(recs) != 1 {
		t.Fatalf("import json %q: %v", out, err)
	}
	e.rec, e.ref = recs[0], recs[0].ID
	return e
}

// recoverErr runs `image recover` and returns the command's error with stdout and stderr.
func (e *imgEnv) recoverErr(args ...string) (stdout, stderr string, err error) {
	var out, errb bytes.Buffer
	d := e.d
	d.Out, d.Err, d.In, d.Registry = &out, &errb, strings.NewReader(""), device.NewRegistry()
	root := newRootCmd(d)
	root.SetArgs(append([]string{"image", "recover", "--case", e.c, e.ref}, args...))
	err = root.Execute()
	return out.String(), errb.String(), err
}

// f2fsTied is an image with two decided unlinked files and one whose newest stale inode has two copies with
// different block maps (a tie the reader cannot decide).
func f2fsTied(t *testing.T) []byte {
	t.Helper()
	o := f2fstest.Options{NodeSegs: 2, SSA: true}
	files := []f2fstest.File{
		{Path: "/d/a.bin", Data: f2Stamp(2*f2bs, "a"), Deleted: true, Unlinked: true},
		{Path: "/d/b.bin", Data: f2Stamp(3*f2bs, "b"), Deleted: true, Unlinked: true},
		{Path: "/d/other.bin", Data: f2Stamp(f2bs, "o")},
		{Path: "/d/tie.bin", Data: f2Stamp(f2bs, "t"), Deleted: true, Unlinked: true},
	}
	img, tr := f2fstest.BuildTree(o, files)
	tie := tr.NID["/d/tie.bin"]
	f2PlantCopy(o, img, tr.Addr[tie], 10, tie, 1, nil)
	f2PlantCopy(o, img, tr.Addr[tie], 11, tie, 1, func(b []byte) {
		binary.LittleEndian.PutUint32(b[f2oAddr:], tr.Data["/d/other.bin"][0])
	})
	return img
}

// firstIndex returns the byte offset of the first occurrence of sub in s, failing the test when absent.
func firstIndex(t *testing.T, s, sub string) int {
	t.Helper()
	i := strings.Index(s, sub)
	if i < 0 {
		t.Fatalf("%q not in:\n%s", sub, s)
	}
	return i
}

func TestImageRecoverF2FSUndecidedExitsOne(t *testing.T) {
	check := func(t *testing.T, err error) {
		t.Helper()
		if ExitCode(err) != 1 || !errors.Is(err, filesys.ErrUndecided) || errors.Is(err, evidence.ErrIntegrity) {
			t.Errorf("err = %v (exit %d); want exit 1 wrapping ErrUndecided", err, ExitCode(err))
		}
		var batch *undecidedBatch
		if !errors.As(err, &batch) || batch.n != 1 {
			t.Errorf("err = %v; want the undecided batch error naming the count 1", err)
		}
	}
	img := f2fsTied(t)

	t.Run("run", func(t *testing.T) {
		e := f2Env(t, img)
		out, _, err := e.recoverErr("--all")
		check(t, err)
		undecided := firstIndex(t, out, "undecided 1")
		if n := strings.Count(out[:undecided], "[recovered:f2fs-node-scan"); n != 2 {
			t.Errorf("%d recovered lines before the undecided line, want both decided artifacts:\n%s", n, out)
		}
		if got := len(recoveredRecords(t, e.c)); got != 2 {
			t.Errorf("%d recovered records, want the 2 decided artifacts kept", got)
		}
		if vc, vout := run(t, e.d, "case", "verify", "--case", e.c); vc != 0 {
			t.Errorf("case verify exit %d:\n%s", vc, vout)
		}
	})
	t.Run("run json", func(t *testing.T) {
		e := f2Env(t, img)
		out, _, err := e.recoverErr("--all", "--json")
		check(t, err)
		var rep struct {
			Recovered int            `json:"recovered"`
			SkippedBy map[string]int `json:"skipped_by"`
		}
		if jerr := json.Unmarshal([]byte(out), &rep); jerr != nil {
			t.Fatalf("json %q: %v", out, jerr)
		}
		if rep.Recovered != 2 || rep.SkippedBy["undecided"] != 1 {
			t.Errorf("recovered %d, skipped_by %v; want 2 and undecided 1", rep.Recovered, rep.SkippedBy)
		}
	})
	t.Run("list", func(t *testing.T) {
		e := f2Env(t, img)
		out, _, err := e.recoverErr("--list")
		check(t, err)
		undecided := firstIndex(t, out, "undecided 1")
		if n := strings.Count(out[:undecided], "[would write]"); n != 2 {
			t.Errorf("%d would-write lines before the undecided line, want 2:\n%s", n, out)
		}
	})
	t.Run("list json", func(t *testing.T) {
		e := f2Env(t, img)
		out, _, err := e.recoverErr("--list", "--json")
		check(t, err)
		var rep struct {
			SkippedBy map[string]int `json:"skipped_by"`
		}
		if jerr := json.Unmarshal([]byte(out), &rep); jerr != nil {
			t.Fatalf("json %q: %v", out, jerr)
		}
		if rep.SkippedBy["undecided"] != 1 {
			t.Errorf("skipped_by %v; want undecided 1", rep.SkippedBy)
		}
	})
	t.Run("Run prints the count note", func(t *testing.T) {
		e := f2Env(t, img)
		code, _, errOut := e.recoverSplit("--all")
		if code != 1 || !strings.Contains(errOut, "1 deleted entries could not be decided") {
			t.Errorf("exit %d, stderr %q; want 1 and the count", code, errOut)
		}
	})
}

// A decided skip does not raise the exit code: an inode number a live file holds is `reused`, a stop at the
// first block is `no-runs` (its detail names the reader's stop). Both runs exit 0.
func TestImageRecoverF2FSDecidedSkipsExitZero(t *testing.T) {
	o := f2fstest.Options{NodeSegs: 2, SSA: true}
	reused, _ := f2fstest.BuildTree(o, []f2fstest.File{
		{Path: "/d/ok.bin", Data: f2Stamp(2*f2bs, "k"), Deleted: true, Unlinked: true},
		{Path: "/d/inuse.bin", Data: f2Stamp(f2bs, "u"), Deleted: true},
	})
	hole, holeTr := f2fstest.BuildTree(o, []f2fstest.File{
		{Path: "/d/ok.bin", Data: f2Stamp(2*f2bs, "k"), Deleted: true, Unlinked: true},
		{Path: "/d/hole.bin", Data: f2Stamp(3*f2bs, "h"), Deleted: true, Unlinked: true},
	})
	// hole.bin: its first data address is 0 in the stale inode (hole@0).
	binary.LittleEndian.PutUint32(f2Blk(hole, holeTr.Addr[holeTr.NID["/d/hole.bin"]])[f2oAddr:], 0)
	for _, tc := range []struct {
		name   string
		img    []byte
		reason string
		detail string
	}{
		{"reused", reused, "reused 1", ""},
		{"stop at the first block", hole, "no-runs", "stale-chain-stop=hole@0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := f2Env(t, tc.img)
			for _, args := range [][]string{{"--list"}, {"--all"}} {
				out, errOut, err := e.recoverErr(args...)
				if err != nil || ExitCode(err) != 0 || strings.Contains(out, "undecided") {
					t.Errorf("%v: err %v, stdout %q, stderr %q; want exit 0 and no undecided line", args, err, out, errOut)
				}
				if !strings.Contains(out, tc.reason) || !strings.Contains(out, tc.detail) {
					t.Errorf("%v: stdout lacks %q and %q:\n%s", args, tc.reason, tc.detail, out)
				}
			}
		})
	}
}
