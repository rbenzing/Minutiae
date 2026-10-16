package fat_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The recovery fixtures (testdata/recover/) are FAT volumes written and deleted by the KERNEL vfat driver in the
// QEMU guest (tools/fixtures/fat_recover.sh). Their oracle (testdata/recover/*.expect.json) comes from
// tools/fixtures/fat_recover_oracle.py, an independent reader of the finished image, the source tree the guest was
// fed, mshowfat and fsck.fat: never Minutiae. They live in a subdirectory so the globs and the old-schema oracle of
// fixture_test.go never see them (ruling F15).

type recoverOracle struct {
	Generator struct {
		ImageSHA256 string `json:"image_sha256"`
	} `json:"generator"`
	Type              string         `json:"type"`
	ClusterSize       int64          `json:"cluster_size"`
	DataStart         int64          `json:"data_start"`
	ClusterCount      int64          `json:"cluster_count"`
	Live              []recLive      `json:"live"`
	FreeClusters      [][2]int64     `json:"free_clusters"`
	DeletedEntryCount int            `json:"deleted_entry_count"`
	Deleted           []recDeleted   `json:"deleted"`
	Families          map[string]int `json:"families"`
}

type recLive struct {
	Path   string     `json:"path"`
	Size   int64      `json:"size"`
	SHA256 string     `json:"sha256"`
	Chain  [][2]int64 `json:"chain"`
}

type recExpect struct {
	Outcome     string     `json:"outcome"`
	Skip        string     `json:"skip"`
	Method      string     `json:"method"`
	Range       [][2]int64 `json:"range"`
	Captured    [][2]int64 `json:"captured"`
	Cut         bool       `json:"cut"`
	EqualSource bool       `json:"equal_source"`
	Overlaps    []string   `json:"overlaps"`
	Confidence  int        `json:"confidence"`
	Assumptions []string   `json:"assumptions"`
}

type recDeleted struct {
	Path         string     `json:"path"`
	Family       string     `json:"family"`
	Listed       bool       `json:"listed"`
	Size         int64      `json:"size"`
	SHA256       string     `json:"sha256"`
	TrueChain    [][2]int64 `json:"true_chain"`
	FirstCluster int64      `json:"first_cluster"`
	NameState    string     `json:"name_state"`
	Expect       recExpect  `json:"expect"`
}

// loadRecoverFixture returns the decompressed image of testdata/recover/<name>.img.gz and its oracle, after
// checking the image sha256 against generator.image_sha256 (a stale oracle fails here, first).
func loadRecoverFixture(t testing.TB, name string) ([]byte, recoverOracle) {
	t.Helper()
	dir := filepath.Join("testdata", "recover")
	img := gunzipFixture(t, filepath.Join(dir, name+".img.gz"))
	raw, err := os.ReadFile(filepath.Join(dir, name+".expect.json"))
	if err != nil {
		t.Fatal(err)
	}
	var o recoverOracle
	if err := json.Unmarshal(raw, &o); err != nil {
		t.Fatal(err)
	}
	if sum := sha256.Sum256(img); hex.EncodeToString(sum[:]) != o.Generator.ImageSHA256 {
		t.Fatalf("%s: image sha256 does not match the oracle: regenerate image and oracle together", name)
	}
	return img, o
}

var (
	requiredFamilies = []string{"contiguous", "zero-size", "reuse-all", "reuse-head", "cut-l", "cut-h", "frag-live", "frag-dead", "lfn", "gone"}
	requiredOutcomes = []string{"contiguous-equal", "empty", "cut-prefix", "skip-no-free-bytes", "not-equal-complete", "overlap", "lfn-tail-lost"}
)

func TestRecoverFixturesHoldWhatTheyClaim(t *testing.T) {
	union := map[string]int{}
	for _, name := range []string{"fat12-recover", "fat16-recover", "fat32-recover"} {
		img, o := loadRecoverFixture(t, name)
		seen := map[string]bool{}
		for _, d := range o.Deleted {
			seen[d.Family] = true
			at := func(r [2]int64) []byte {
				return img[o.DataStart+(r[0]-2)*o.ClusterSize : o.DataStart+(r[1]-1)*o.ClusterSize]
			}
			switch d.Family {
			case "contiguous": // the file really is intact on disk: the bytes of its true chain are the source
				var b []byte
				for _, r := range d.TrueChain {
					b = append(b, at(r)...)
				}
				if sum := sha256.Sum256(b[:d.Size]); hex.EncodeToString(sum[:]) != d.SHA256 {
					t.Errorf("%s: %s: the true chain does not hold the source bytes", name, d.Path)
				}
			case "frag-live", "frag-dead":
				if len(d.TrueChain) != 2 {
					t.Errorf("%s: %s: true chain %v, want two fragments", name, d.Path, d.TrueChain)
				}
			case "reuse-all", "reuse-head":
				if d.Expect.Outcome != "skip" || d.Expect.Skip != "no-free-bytes" {
					t.Errorf("%s: %s: outcome %+v, want skip no-free-bytes", name, d.Path, d.Expect)
				}
			}
			if d.Expect.Outcome == "skip" && d.Expect.Skip == "no-free-bytes" {
				for _, r := range d.Expect.Range[:1] { // the first cluster of the range is not free
					for _, f := range o.FreeClusters {
						if r[0] >= f[0] && r[0] <= f[1] {
							t.Errorf("%s: %s: skip no-free-bytes but cluster %d is free", name, d.Path, r[0])
						}
					}
				}
			}
		}
		for _, fam := range requiredFamilies {
			if !seen[fam] {
				t.Errorf("%s: scenario family %q is missing: the self-check cannot go vacuous", name, fam)
			}
		}
		var free, live int64
		for _, f := range o.FreeClusters {
			free += f[1] - f[0] + 1
		}
		for _, l := range o.Live {
			for _, r := range l.Chain {
				live += r[1] - r[0] + 1
			}
		}
		if free+live > o.ClusterCount { // the root of FAT32 and directories are live chains too: the sum may not exceed
			t.Errorf("%s: free %d + live %d clusters exceed %d", name, free, live, o.ClusterCount)
		}
		for k, v := range o.Families {
			union[k] += v
		}
	}
	for _, k := range requiredOutcomes {
		if union[k] == 0 {
			t.Errorf("outcome family %q occurs in none of the three images", k)
		}
	}
}
