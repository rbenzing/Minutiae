package cli

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/volume/volumetest"
)

// The kernel-written recovery fixtures (internal/filesys/{fat,exfat}/testdata/recover/) through the real command
// and the real driver registry, with the independent oracle's totals.
type recoverFixtureOracle struct {
	Generator struct {
		ImageSHA256 string `json:"image_sha256"`
	} `json:"generator"`
	// Totals, Incomplete and CandidateMethods are the ext4 schema (the FAT and exFAT oracles have none of them).
	Totals struct {
		SkippedBy map[string]int `json:"skipped_by"`
		Undecided int            `json:"undecided"`
	} `json:"totals"`
	Deleted []struct {
		Path   string `json:"path"`
		Listed bool   `json:"listed"`
		Expect struct {
			Outcome          string   `json:"outcome"`
			Skip             string   `json:"skip"`
			Cut              bool     `json:"cut"`
			Incomplete       bool     `json:"incomplete"`
			CandidateMethods []string `json:"candidate_methods"`
			Overlaps         []string `json:"overlaps"`
		} `json:"expect"`
	} `json:"deleted"`
}

func loadRecoverFixtureImage(t *testing.T, dir, name string) ([]byte, recoverFixtureOracle) {
	t.Helper()
	base := filepath.Join("..", "filesys", dir, "testdata", "recover", name)
	gz, err := os.ReadFile(base + ".img.gz")
	if err != nil {
		t.Fatal(err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		t.Fatal(err)
	}
	img, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(base + ".expect.json")
	if err != nil {
		t.Fatal(err)
	}
	var o recoverFixtureOracle
	if err := json.Unmarshal(raw, &o); err != nil {
		t.Fatal(err)
	}
	if sum := sha256.Sum256(img); hex.EncodeToString(sum[:]) != o.Generator.ImageSHA256 {
		t.Fatalf("%s: image sha256 does not match the oracle: regenerate image and oracle together", name)
	}
	return img, o
}

// gptWhole puts a filesystem image into the only partition of a GPT disk.
func gptWhole(fs []byte) []byte {
	const first = 40
	sectors := uint64(len(fs)+511) / 512
	img := volumetest.GPT(512, first+sectors+8+40, "11111111-2222-3333-4444-555555555555", []volumetest.Part{{
		StartLBA: first, Sectors: sectors, TypeGUID: imgLinuxType,
		GUID: "00000000-0000-0000-0000-000000000001", Name: "data",
	}})
	copy(img[first*512:], fs)
	return img
}

func TestImageRecoverFixturesVerifyClean(t *testing.T) {
	if testing.Short() {
		t.Skip("imports and recovers real-image fixtures")
	}
	for _, tc := range []struct{ dir, name string }{
		{"fat", "fat12-recover"},
		{"fat", "fat16-recover"},
		{"fat", "fat32-recover"},
		{"exfat", "exfat-recover"},
		{"ext4", "ext4-recover"},
		{"ext4", "ext3-recover"},
		{"ext4", "ext4-recover-sameboot"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			img, o := loadRecoverFixtureImage(t, tc.dir, tc.name)
			isExt4 := tc.dir == "ext4"
			var recovered, partial, overlap, leafOnly int
			skipped := map[string]int{}
			for _, d := range o.Deleted {
				switch {
				case !d.Listed && !isExt4:
				case d.Expect.Outcome == "skip":
					skipped[d.Expect.Skip]++
				default:
					recovered++
					// ext4: Partial counts the artifacts flagged incomplete, which the oracle states in its own field
					// (a cut, a file end the reader cannot know, a short map), not only the cuts
					if d.Expect.Cut && !isExt4 || d.Expect.Incomplete {
						partial++
					}
					if slices.Equal(d.Expect.CandidateMethods, []string{"ext4-extent-leaf"}) {
						leafOnly++
					}
					if len(d.Expect.Overlaps) > 0 {
						overlap++
					}
				}
			}
			if isExt4 {
				skipped = o.Totals.SkippedBy // the oracle's totals also count the candidates a better one displaced (not-selected)
			}
			// 3C-R21: an undecided entry makes the batch exit 1 (--all keeps and writes its decided work); 3C-R25: --list
			// does not search for leaves, so an entry whose only source is a leaf is undecided there as well
			wantAll, wantList := 0, 0
			if o.Totals.Undecided > 0 {
				wantAll, wantList = 1, 1
			}
			if leafOnly > 0 {
				wantList = 1
			}
			c := newCLICase(t)
			d := Deps{} // the real driver registry
			ref := importRaw(t, c, gptWhole(img))

			before := auditActions(t, c)
			code, out := run(t, d, "image", "recover", "--case", c, ref, "--list")
			if code != wantList {
				t.Fatalf("--list: exit %d, want %d (undecided %d, leaf-only %d):\n%s", code, wantList, o.Totals.Undecided, leafOnly, out)
			}
			if acts := auditActions(t, c)[len(before):]; !slices.Equal(acts, []string{"case.open"}) {
				t.Errorf("--list appended %v, want only case.open", acts)
			}
			if n := len(recoveredRecords(t, c)); n != 0 {
				t.Fatalf("%d recovered artifacts after --list", n)
			}

			code, out = run(t, d, "image", "recover", "--case", c, ref, "--all", "--json")
			if code != wantAll {
				t.Fatalf("--all: exit %d, want %d (undecided %d):\n%s", code, wantAll, o.Totals.Undecided, out)
			}
			if wantAll != 0 && !strings.Contains(out, strconv.Itoa(o.Totals.Undecided)+" deleted entries could not be decided") {
				t.Errorf("--all: the exit-1 note does not name the count %d:\n%s", o.Totals.Undecided, out)
			}
			var js jsonRecover
			// an exit-1 batch prints its error and note after the JSON document: decode the first value only
			if err := json.NewDecoder(bytes.NewReader(jsonPart(out))).Decode(&js); err != nil {
				t.Fatalf("json %q: %v", out, err)
			}
			if js.Recovered != recovered || js.Partial != partial || js.Overlap != overlap {
				t.Errorf("recovered/partial/overlap = %d/%d/%d, the oracle says %d/%d/%d", js.Recovered, js.Partial, js.Overlap, recovered, partial, overlap)
			}
			for reason, n := range skipped {
				if js.SkippedBy[reason] != n {
					t.Errorf("skipped_by[%s] = %d, the oracle says %d", reason, js.SkippedBy[reason], n)
				}
			}
			recs := recoveredRecords(t, c)
			if len(recs) != recovered {
				t.Fatalf("%d recovered artifacts in the manifest, the oracle says %d", len(recs), recovered)
			}

			want := strconv.Itoa(recovered) + " recovered (" + strconv.Itoa(recovered) + " reproduced)"
			code, out = run(t, d, "case", "verify", "--case", c)
			if code != 0 || !strings.Contains(out, "NOTICE:") || !strings.Contains(out, "recovered artifact(s)") || !strings.Contains(out, want) {
				t.Fatalf("case verify: exit %d, want 0 with the recovered notice and %q:\n%s", code, want, out)
			}
			if got := lastVerifyRun(t, c)["allocation_rechecked"]; got != float64(recovered) {
				t.Errorf("allocation_rechecked = %v, want the %d deleted-file recoveries (R9 reads the real filesystem)", got, recovered)
			}

			// flip one byte of one recovered artifact: verify recomputes the bytes from the image and exits 4
			var victim string
			for _, r := range recs {
				if r.Size > 0 {
					victim = filepath.Join(c, filepath.FromSlash(r.Path))
					break
				}
			}
			b, err := os.ReadFile(victim) //nolint:gosec // a test reads its own case directory
			if err != nil {
				t.Fatal(err)
			}
			b[len(b)/2] ^= 0xFF
			if err := os.WriteFile(victim, b, 0o600); err != nil { //nolint:gosec // idem
				t.Fatal(err)
			}
			code, out = run(t, d, "case", "verify", "--case", c)
			if code != 4 || !strings.Contains(out, "PROBLEM:") || !strings.Contains(out, "reproduce: byte") {
				t.Errorf("case verify after the flipped byte: exit %d, want 4 with a reproduce: byte problem:\n%s", code, out)
			}
		})
	}
}

// Ruling F2: the content of a deleted directory is out of 3B. The deleted directory is listed, its file is not,
// and recover skips the directory as not-a-file.
func TestRecoverFixtureDeletedDirectoryContentIsNotListed(t *testing.T) {
	if testing.Short() {
		t.Skip("imports real-image fixtures")
	}
	for _, tc := range []struct{ dir, name string }{{"fat", "fat12-recover"}, {"exfat", "exfat-recover"}} {
		t.Run(tc.name, func(t *testing.T) {
			img, _ := loadRecoverFixtureImage(t, tc.dir, tc.name)
			c := newCLICase(t)
			d := Deps{}
			ref := importRaw(t, c, gptWhole(img))
			code, out, errs := runSplit(t.Context(), t, d, "image", "ls", "--case", c, "-r", "--deleted", "--json", ref)
			if code != 0 {
				t.Fatalf("ls: %d %s %s", code, out, errs)
			}
			var items []struct {
				Path string `json:"path"`
			}
			if err := json.Unmarshal(jsonPart(out), &items); err != nil {
				t.Fatal(err)
			}
			var gone, inner bool
			for _, it := range items {
				gone = gone || it.Path == "/gone"
				inner = inner || strings.HasPrefix(it.Path, "/gone/") || strings.HasPrefix(it.Path, "/cut-h-hid/")
			}
			if !gone || inner {
				t.Errorf("deleted directory listed: %v, its content listed: %v; want true, false", gone, inner)
			}
			code, out = run(t, d, "image", "recover", "--case", c, ref, "--all", "--json")
			var js jsonRecover
			if err := json.Unmarshal(jsonPart(out), &js); err != nil || code != 0 {
				t.Fatalf("recover: %d %q: %v", code, out, err)
			}
			if js.SkippedBy["not-a-file"] != 2 {
				t.Errorf("skipped_by[not-a-file] = %d, want 2 (the deleted directories)", js.SkippedBy["not-a-file"])
			}
		})
	}
}
