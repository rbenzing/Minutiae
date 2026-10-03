package volume_test

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/volume"
)

// oracle is the shape of testdata/*.expect.json. It is produced by the
// partitioning tools (tools/fixtures), never by Minutiae.
type oracle struct {
	Scheme     string `json:"scheme"`
	SectorSize int    `json:"sector_size"`
	DiskGUID   string `json:"disk_guid"`
	Partitions []struct {
		Index  int    `json:"index"`
		Start  int64  `json:"start"`
		Length int64  `json:"length"`
		Type   string `json:"type"`
		Name   string `json:"name"`
		GUID   string `json:"guid"`
	} `json:"partitions"`
}

func fixtureImages(t testing.TB) []string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("testdata", "*.img.gz"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no fixtures in testdata (glob err %v)", err)
	}
	return paths
}

// gunzipFixture returns the decompressed image bytes of a *.img.gz fixture.
func gunzipFixture(t testing.TB, path string) []byte {
	t.Helper()
	gz, err := os.ReadFile(path)
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
	return img
}

func TestVolumeFixturesMatchOracle(t *testing.T) {
	for _, path := range fixtureImages(t) {
		name := strings.TrimSuffix(filepath.Base(path), ".img.gz")
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(strings.TrimSuffix(path, ".img.gz") + ".expect.json")
			if err != nil {
				t.Fatal(err)
			}
			var want oracle
			if err := json.Unmarshal(raw, &want); err != nil {
				t.Fatal(err)
			}
			if len(want.Partitions) == 0 {
				t.Fatal("oracle lists no partitions")
			}

			img := gunzipFixture(t, path)
			tab, err := volume.Read(bytes.NewReader(img), int64(len(img)), 0)
			if err != nil {
				t.Fatal(err)
			}
			if tab.Scheme != want.Scheme {
				t.Errorf("scheme %q, oracle %q", tab.Scheme, want.Scheme)
			}
			if tab.SectorSize != want.SectorSize {
				t.Errorf("sector size %d, oracle %d", tab.SectorSize, want.SectorSize)
			}
			if tab.DiskGUID != want.DiskGUID {
				t.Errorf("disk guid %q, oracle %q", tab.DiskGUID, want.DiskGUID)
			}
			for _, w := range tab.Warnings {
				// Probing (sector size 0) always assumes 512 for an MBR.
				if want.Scheme == "mbr" && w == "MBR sector size assumed to be 512 bytes" {
					continue
				}
				t.Errorf("unexpected warning on a clean fixture: %q", w)
			}
			if len(tab.Partitions) != len(want.Partitions) {
				t.Fatalf("got %d partitions %+v, oracle %d", len(tab.Partitions), tab.Partitions, len(want.Partitions))
			}
			for i, w := range want.Partitions {
				g := tab.Partitions[i]
				if g.Index != w.Index || g.Start != w.Start || g.Length != w.Length ||
					g.Type != w.Type || g.Name != w.Name || g.GUID != w.GUID {
					t.Errorf("partition %d:\n got  %+v\n want %+v", i, g, w)
				}
			}
		})
	}
}
