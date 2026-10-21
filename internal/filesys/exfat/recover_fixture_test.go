package exfat_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The recovery fixture (testdata/recover/) is an exFAT volume written and deleted by the KERNEL exfat driver in
// the QEMU guest (tools/fixtures/exfat_recover.sh). Its oracle (testdata/recover/*.expect.json) comes from
// tools/fixtures/exfat_recover_oracle.py, an independent reader of the finished image, the source tree the guest
// was fed and dump.exfat: never Minutiae. It lives in a subdirectory so the glob and the old-schema oracle of
// fixture_test.go never see it (ruling F15).

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
	Path            string     `json:"path"`
	Family          string     `json:"family"`
	Listed          bool       `json:"listed"`
	Size            int64      `json:"size"`
	SHA256          string     `json:"sha256"`
	TrueChain       [][2]int64 `json:"true_chain"`
	FirstCluster    int64      `json:"first_cluster"`
	NoFatChain      bool       `json:"no_fat_chain"`
	DataLength      int64      `json:"data_length"`
	ValidDataLength int64      `json:"valid_data_length"`
	FatResidue      string     `json:"fat_residue"`
	Expect          recExpect  `json:"expect"`
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
	requiredFamilies = []string{"contiguous", "zero-size", "reuse-head", "cut-h", "chained", "gone"}
	requiredOutcomes = []string{"nofatchain-equal", "empty", "chained", "skip-no-free-bytes", "cut-prefix", "not-equal-complete"}
)

func TestExfatRecoverFixturesHoldWhatTheyClaim(t *testing.T) {
	img, o := loadRecoverFixture(t, "exfat-recover")
	if o.Type != "exfat" || o.ClusterSize != 4096 {
		t.Fatalf("type %q, cluster size %d; want exfat, 4096", o.Type, o.ClusterSize)
	}
	seen := map[string]bool{}
	bytesOf := func(chain [][2]int64) []byte {
		var b []byte
		for _, r := range chain {
			b = append(b, img[o.DataStart+(r[0]-2)*o.ClusterSize:o.DataStart+(r[1]-1)*o.ClusterSize]...)
		}
		return b
	}
	for _, d := range o.Deleted {
		seen[d.Family] = true
		if d.FatResidue != "not-chained" && d.FatResidue != "chain-kept" && d.FatResidue != "chain-cleared" {
			t.Errorf("%s: fat_residue %q", d.Path, d.FatResidue)
		}
		if d.NoFatChain != (d.FatResidue == "not-chained") && d.Size > 0 {
			t.Errorf("%s: no_fat_chain %v but fat_residue %q", d.Path, d.NoFatChain, d.FatResidue)
		}
		if d.DataLength != d.Size || d.ValidDataLength != d.DataLength {
			t.Errorf("%s: size %d, data_length %d, valid_data_length %d", d.Path, d.Size, d.DataLength, d.ValidDataLength)
		}
		// the method follows the oracle's own rule
		if d.Expect.Outcome == "recover" {
			want := "exfat-contiguous"
			switch {
			case d.NoFatChain:
				want = "exfat-nofatchain"
			case d.FatResidue == "chain-kept":
				want = "exfat-fatchain"
			}
			if d.Expect.Method != want {
				t.Errorf("%s: method %q, want %q", d.Path, d.Expect.Method, want)
			}
		}
		switch d.Family {
		case "contiguous": // nofatchain-5: a NoFatChain file, intact on disk
			if !d.NoFatChain || len(d.TrueChain) != 1 {
				t.Errorf("%s: no_fat_chain %v, true chain %v; want one NoFatChain run", d.Path, d.NoFatChain, d.TrueChain)
			}
			if sum := sha256.Sum256(bytesOf(d.TrueChain)[:d.Size]); hex.EncodeToString(sum[:]) != d.SHA256 {
				t.Errorf("%s: the true chain does not hold the source bytes", d.Path)
			}
		case "chained": // the fragmented f.bin has a FAT chain, which the kernel keeps after the delete
			if d.NoFatChain || len(d.TrueChain) != 2 || d.FatResidue != "chain-kept" {
				t.Errorf("%s: no_fat_chain %v, chain %v, residue %q; want two fragments with the chain kept", d.Path, d.NoFatChain, d.TrueChain, d.FatResidue)
			}
		case "reuse-head":
			if d.Expect.Outcome != "skip" || d.Expect.Skip != "no-free-bytes" {
				t.Errorf("%s: outcome %+v, want skip no-free-bytes", d.Path, d.Expect)
			}
		}
		if d.Expect.Outcome == "skip" && d.Expect.Skip == "no-free-bytes" {
			for _, f := range o.FreeClusters {
				if r := d.Expect.Range[0]; r[0] >= f[0] && r[0] <= f[1] {
					t.Errorf("%s: skip no-free-bytes but cluster %d is free", d.Path, r[0])
				}
			}
		}
	}
	for _, fam := range requiredFamilies {
		if !seen[fam] {
			t.Errorf("scenario family %q is missing: the self-check cannot go vacuous", fam)
		}
	}
	for _, k := range requiredOutcomes {
		if o.Families[k] == 0 {
			t.Errorf("outcome family %q does not occur", k)
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
	if free+live > o.ClusterCount {
		t.Errorf("free %d + live %d clusters exceed %d", free, live, o.ClusterCount)
	}
}
