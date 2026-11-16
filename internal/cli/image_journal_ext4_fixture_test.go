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
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/evidence/evidencetest"
)

// The kernel-written ext4 and ext3 journal images (internal/filesys/ext4/testdata/journal/) through the real command and the
// real driver registry: journal report, block copies of one live and one stale transaction, recover --use-journal stale, then
// case verify (3G Task 14). Which transactions to copy and what must verify come from the independent oracle file; none of it
// is restated here. Skipped under -short like the other fixture tests.

type jxOracle struct {
	Generator struct {
		ImageSHA256 string `json:"image_sha256"`
	} `json:"generator"`
	Journal struct {
		NeedsRecovery bool `json:"needs_recovery"`
	} `json:"journal"`
	Txns []struct {
		Seq       uint32 `json:"seq"`
		Region    string `json:"region"`
		Committed bool   `json:"committed"`
		Tags      []struct {
			Copy    string `json:"copy"`
			Revoked bool   `json:"revoked"`
		} `json:"tags"`
	} `json:"txns"`
}

var jxImages = []string{"ext4-journal-clean", "ext4-journal-crash", "ext3-journal-clean"}

func loadJournalFixtureCLI(t *testing.T, name string) ([]byte, jxOracle) {
	t.Helper()
	base := filepath.Join("..", "filesys", "ext4", "testdata", "journal", name)
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
	var o jxOracle
	if err := json.Unmarshal(raw, &o); err != nil {
		t.Fatal(err)
	}
	if sum := sha256.Sum256(img); hex.EncodeToString(sum[:]) != o.Generator.ImageSHA256 {
		t.Fatalf("%s: image sha256 does not match the oracle: regenerate image and oracle together", name)
	}
	return img, o
}

type jxPick struct {
	seq    uint32
	region string
}

// jxPicks chooses, from the oracle, one live and one stale committed transaction whose sequence number names exactly one
// transaction and which logs a block a copy accepts. A clean image has no live one.
func jxPicks(o jxOracle) []jxPick {
	count := map[uint32]int{}
	for _, x := range o.Txns {
		count[x.Seq]++
	}
	var live, stale []jxPick
	for _, x := range o.Txns {
		if count[x.Seq] != 1 || !x.Committed {
			continue
		}
		ok := false
		for _, g := range x.Tags {
			ok = ok || (g.Copy == "verified" || g.Copy == "unverified") && !g.Revoked
		}
		switch {
		case !ok:
		case x.Region == "live":
			live = append(live, jxPick{x.Seq, x.Region})
		default:
			stale = append(stale, jxPick{x.Seq, x.Region})
		}
	}
	var out []jxPick
	if len(live) > 0 {
		out = append(out, live[0])
	}
	if len(stale) > 0 {
		out = append(out, stale[len(stale)/2])
	}
	return out
}

var (
	jxCopied      = regexp.MustCompile(`(?m)^copied (\d+)$`)
	jxUnconfirmed = regexp.MustCompile(`(?m)^bitmap unconfirmed: (\d+)$`)
)

// jxRun does the report, the block copies and the recovery on a fresh case of the fixture and returns the environment and the
// number of block copies written.
func jxRun(t *testing.T, name string) (e *imgEnv, blocks int) {
	t.Helper()
	img, o := loadJournalFixtureCLI(t, name)
	e = f2Env(t, img)
	if code, out, errOut := e.journal("report", e.ref); code != 0 || strings.Contains(out, "INCOMPLETE") {
		t.Fatalf("journal report: %d\n%s%s", code, out, errOut)
	}
	picks := jxPicks(o)
	if len(picks) == 0 {
		t.Fatal("the oracle has no transaction to copy")
	}
	for _, p := range picks {
		code, out, errOut := e.journal("blocks", e.ref, strconv.Itoa(int(p.seq)), "--region", p.region)
		m := jxCopied.FindStringSubmatch(out)
		if code != 0 || m == nil || m[1] == "0" || strings.Contains(out, "INCOMPLETE") {
			t.Fatalf("journal blocks %d %s: %d\n%s%s", p.seq, p.region, code, out, errOut)
		}
		n, _ := strconv.Atoi(m[1])
		blocks += n
	}
	if code, out, errOut := jcRecover(e, "--all", "--use-journal", "stale"); code != 0 {
		t.Fatalf("recover --use-journal stale: %d\n%s%s", code, out, errOut)
	}
	return e, blocks
}

func jxJournalFiles(t *testing.T, c string) []evidence.ManifestRecord {
	t.Helper()
	var out []evidence.ManifestRecord
	for _, r := range recoveredRecords(t, c) {
		if r.Source.Derived != nil && r.Source.Derived.Recovery != nil && r.Source.Derived.Recovery.Method == "ext4-journal-inode" {
			out = append(out, r)
		}
	}
	return out
}

func TestImageJournalExt4FixtureVerifyClean(t *testing.T) {
	if testing.Short() {
		t.Skip("imports and recovers real-image fixtures")
	}
	for _, name := range jxImages {
		t.Run(name, func(t *testing.T) {
			e, blocks := jxRun(t, name)
			files := jxJournalFiles(t, e.c)
			if len(files) == 0 {
				t.Fatal("no file was recovered from the journal")
			}
			code, out := run(t, Deps{}, "case", "verify", "--case", e.c)
			if code != 0 {
				t.Fatalf("case verify: exit %d, want 0:\n%s", code, out)
			}
			// R13 (journal_rechecked), R6 (recovered_reproduced) and R9 (allocation_rechecked) ran on what was written
			v := lastVerifyRun(t, e.c)
			num := func(k string) int { f, _ := v[k].(float64); return int(f) }
			if want := 1 + blocks + len(files); num("journal_rechecked") != want {
				t.Errorf("journal_rechecked = %v, want %d (report, %d block copies, %d files)", v["journal_rechecked"], want, blocks, len(files))
			}
			if num("recovered_reproduced") < blocks+len(files) {
				t.Errorf("recovered_reproduced = %v, want at least %d", v["recovered_reproduced"], blocks+len(files))
			}
			if num("allocation_rechecked") < len(files) {
				t.Errorf("allocation_rechecked = %v, want at least %d", v["allocation_rechecked"], len(files))
			}
		})
	}
}

func TestImageJournalExt4FixtureTamperExitsFour(t *testing.T) {
	if testing.Short() {
		t.Skip("imports and recovers real-image fixtures")
	}
	for _, name := range jxImages {
		for _, which := range []string{"block-copy", "journal-inode-artifact"} {
			t.Run(name+"/"+which, func(t *testing.T) {
				e, _ := jxRun(t, name)
				if code, out := run(t, Deps{}, "case", "verify", "--case", e.c); code != 0 {
					t.Fatalf("case verify before the tamper: exit %d\n%s", code, out)
				}
				var victim evidence.ManifestRecord
				if which == "journal-inode-artifact" {
					victim = jxJournalFiles(t, e.c)[0]
				} else {
					for _, r := range manifestRecords(t, e.c) {
						if r.Source.Kind == evidence.KindJournal && r.Size > 0 {
							victim = r
							break
						}
					}
				}
				if victim.ID == "" {
					t.Fatalf("no %s to tamper with", which)
				}
				evidencetest.FlipArtifactByte(t, e.c, victim, victim.Size/2)
				if code, out := run(t, Deps{}, "case", "verify", "--case", e.c); code != 4 || !strings.Contains(out, "PROBLEM:") {
					t.Errorf("case verify after a flipped byte in %s: exit %d, want 4 with a PROBLEM:\n%s", which, code, out)
				}
			})
		}
	}
}

// C17 (3G-R17): block copies taken from the crash image, whose filesystem warns (needs_recovery), are counted as copies whose
// bitmap view was unknown, in the text and in the JSON; a clean image's are not.
func TestImageJournalExt4FixtureBitmapUnconfirmed(t *testing.T) {
	if testing.Short() {
		t.Skip("imports and recovers real-image fixtures")
	}
	for _, name := range jxImages {
		t.Run(name, func(t *testing.T) {
			img, o := loadJournalFixtureCLI(t, name)
			picks := jxPicks(o)
			if len(picks) == 0 {
				t.Fatal("the oracle has no transaction to copy")
			}
			p := picks[0]
			for _, asJSON := range []bool{false, true} {
				e := f2Env(t, img) // a fresh case each time: a second copy run would copy the same blocks again
				args := []string{e.ref, strconv.Itoa(int(p.seq)), "--region", p.region}
				if asJSON {
					args = append(slices.Clone(args), "--json")
				}
				code, out, errOut := e.journal("blocks", args...)
				if code != 0 {
					t.Fatalf("journal blocks: %d\n%s%s", code, out, errOut)
				}
				var copied, unconfirmed int
				if asJSON {
					var j struct {
						Copied       int `json:"copied"`
						AllocUnknown int `json:"alloc_unknown"`
					}
					if err := json.Unmarshal(jsonPart(out), &j); err != nil {
						t.Fatalf("json: %v\n%s", err, out)
					}
					copied, unconfirmed = j.Copied, j.AllocUnknown
				} else {
					c, u := jxCopied.FindStringSubmatch(out), jxUnconfirmed.FindStringSubmatch(out)
					if c == nil || u == nil {
						t.Fatalf("no copied / bitmap unconfirmed line:\n%s", out)
					}
					copied, _ = strconv.Atoi(c[1])
					unconfirmed, _ = strconv.Atoi(u[1])
				}
				if copied == 0 {
					t.Fatalf("nothing was copied:\n%s", out)
				}
				want := 0
				if o.Journal.NeedsRecovery {
					want = copied // every copy of a filesystem that warns has an unknown bitmap view
				}
				if unconfirmed != want {
					t.Errorf("json=%v: %d copies, bitmap unconfirmed %d, want %d (the filesystem warns: %v)", asJSON, copied, unconfirmed, want, o.Journal.NeedsRecovery)
				}
			}
		})
	}
}
