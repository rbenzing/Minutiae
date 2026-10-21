package cli

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/device"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/filesys/ext4/ext4test"
	"github.com/rbenzing/minutiae/internal/filesys/f2fs/f2fstest"
)

// cutMarker is the name of the one directory record the tests below damage. Nothing else in an image holds it
// (file contents are different bytes), so its offset is found by searching, independently of the readers.
const cutMarker = "bravo-entry-marker"

// ext4CutImage is an ext4 image whose directory /d has two blocks, the second starting with the record
// cutMarker; the record's rec_len is zeroed, so the rest of that block is never read.
func ext4CutImage(t *testing.T) []byte {
	t.Helper()
	img := ext4test.Build(ext4test.Options{Extents: true}, []ext4test.File{
		{Path: "/d", Dir: true},
		{Path: "/d/alpha", Data: []byte("first file")},
		{Path: "/d/" + cutMarker, Data: []byte("second file"), NewBlock: true},
	})
	i := bytes.Index(img, []byte(cutMarker))
	if i < 8 || bytes.Contains(img[i+1:], []byte(cutMarker)) {
		t.Fatalf("the directory record is not found exactly once (offset %d)", i)
	}
	binary.LittleEndian.PutUint16(img[i-8+4:], 0) // rec_len of the first record of the directory's second block
	return img
}

// f2fsCutImage is an F2FS image whose directory /d holds a record named cutMarker with name_len 0, a damaged
// dentry slot: the scan of /d cannot read the whole directory.
func f2fsCutImage(t *testing.T) []byte {
	t.Helper()
	img, _ := f2fstest.BuildTree(f2fstest.Options{Segments: 2}, []f2fstest.File{
		{Path: "/d", Dir: true},
		{Path: "/d/alpha", Data: []byte("first file"), Inline: true},
		{Path: "/d/" + cutMarker, Data: []byte("second file"), Inline: true},
	})
	// struct f2fs_dentry_block: bitmap 27 + reserved 3, 214 dentries of 11 bytes, then 214 name slots of 8. The
	// name also lies in the file's inode (i_name), so the occurrence in a dentry block is the one whose slot
	// holds a dentry with name_len equal to the name's length.
	const dentriesOff, filenamesOff, blockSize = 30, 30 + 214*11, 4096
	found := 0
	for from := 0; ; {
		j := bytes.Index(img[from:], []byte(cutMarker))
		if j < 0 {
			break
		}
		i := from + j
		from = i + 1
		start := i &^ (blockSize - 1)
		rel := i - start - filenamesOff
		if rel < 0 || rel%8 != 0 || rel/8 >= 214 {
			continue
		}
		nl := start + dentriesOff + rel/8*11 + 8
		if int(binary.LittleEndian.Uint16(img[nl:])) != len(cutMarker) {
			continue
		}
		binary.LittleEndian.PutUint16(img[nl:], 0) // name_len
		found++
	}
	if found != 1 {
		t.Fatalf("found %d dentry records named %q, want 1", found, cutMarker)
	}
	return img
}

// Ruling Z57: a real image whose directory scan was cut, driven through the command line. `image ls` and `image
// extract` of a name that is not there exit non-zero with the incomplete-scan message (never "not found", never
// 0), and the extract writes the audited "incomplete" reason before it returns. Both readers.
func TestImageCutDirectoryExitsNonZeroAndIsAudited(t *testing.T) {
	for _, c := range []struct {
		fs  string
		img func(*testing.T) []byte
	}{
		{"ext4", ext4CutImage},
		{"f2fs", f2fsCutImage},
	} {
		t.Run(c.fs, func(t *testing.T) {
			d := Deps{}
			cdir := newCLICase(t)
			code, out := run(t, d, "image", "import", "--case", cdir, "--json", imgFile(t, c.img(t)))
			if code != 0 {
				t.Fatalf("import: %d %s", code, out)
			}
			var recs []evidence.ManifestRecord
			if err := json.Unmarshal(jsonPart(out), &recs); err != nil || len(recs) != 1 {
				t.Fatalf("import json %q: %v", out, err)
			}
			ref := recs[0].ID

			do := func(args ...string) (int, string, string) {
				var stdout, stderr bytes.Buffer
				dd := Deps{Out: &stdout, Err: &stderr, In: strings.NewReader(""), Registry: device.NewRegistry()}
				full := append([]string{"image", args[0], "--case", cdir}, args[1:]...)
				return Run(full, dd), stdout.String(), stderr.String()
			}

			// the readable part of the damaged directory still lists: the baseline that the case is not just broken
			if code, stdout, stderr := do("ls", ref, "/d/alpha"); code != 0 {
				t.Fatalf("ls of a readable name: exit %d\nstdout %q\nstderr %q", code, stdout, stderr)
			}
			for _, args := range [][]string{
				{"ls", ref, "/d/missing"},
				{"extract", "-r", ref, "/d/missing"},
			} {
				code, stdout, stderr := do(args...)
				if code != ExitError {
					t.Errorf("%v: exit %d, want %d\nstdout %q\nstderr %q", args, code, ExitError, stdout, stderr)
				}
				if !strings.Contains(stderr, "cannot be decided") {
					t.Errorf("%v: stderr does not say the lookup cannot be decided:\n%s", args, stderr)
				}
				if strings.Contains(stderr, "not found") {
					t.Errorf("%v: the lookup in a cut directory is reported as not found:\n%s", args, stderr)
				}
			}

			entries, err := evidence.ReadAuditEntries(filepath.Join(cdir, "audit.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			n := 0
			for _, e := range entries {
				if e.Action != "analysis.error" {
					continue
				}
				n++
				if got, _ := e.Details["reason"].(string); got != "incomplete" {
					t.Errorf("analysis.error reason = %q, want \"incomplete\" (%v)", got, e.Details)
				}
				if got, _ := e.Details["path"].(string); got != "/d/missing" {
					t.Errorf("analysis.error path = %q, want /d/missing", got)
				}
			}
			if n != 1 {
				t.Errorf("%d analysis.error entries, want 1 (the extract; ls writes none)", n)
			}
		})
	}
}
