package apfs_test

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// layoutOracle is the part of testdata/*.expect.json this smoke test reads. The
// oracle comes from tools/fixtures/apfs_oracle.py, an independent decoder of
// the mkapfs images, never from Minutiae.
type layoutOracle struct {
	Generator struct {
		ImageSHA256 string `json:"image_sha256"`
	} `json:"generator"`
	Container struct {
		BlockSize  uint32 `json:"block_size"`
		BlockCount uint64 `json:"block_count"`
		UUID       string `json:"uuid"`
	} `json:"container"`
	Checkpoint struct {
		Ring []struct {
			Block uint64 `json:"block"`
			Kind  string `json:"kind"`
		} `json:"ring"`
	} `json:"checkpoint"`
	Volume struct {
		Block uint64 `json:"block"`
		Name  string `json:"name"`
		UUID  string `json:"uuid"`
	} `json:"volume"`
}

// TestFixtureLayoutMatchesOracle is a smoke check that the committed bytes are
// the ones the oracle describes: the image hash first, then the raw facts the
// reader relies on at fixed offsets (the "NXSB" magic at 32, block size, block
// count, a superblock in the ring, and the "APSB" volume superblock at the
// address the oracle gives). The full comparison is TestAPFSMatchesOracle.
func TestFixtureLayoutMatchesOracle(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("testdata", "*.img.gz"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no fixtures in testdata (glob err %v)", err)
	}
	for _, path := range paths {
		name := strings.TrimSuffix(filepath.Base(path), ".img.gz")
		t.Run(name, func(t *testing.T) {
			if testing.Short() && strings.Contains(name, "multichunk") {
				t.Skip("multi-chunk fixture skipped under -short")
			}
			raw, err := os.ReadFile(strings.TrimSuffix(path, ".img.gz") + ".expect.json")
			if err != nil {
				t.Fatal(err)
			}
			var want layoutOracle
			if err := json.Unmarshal(raw, &want); err != nil {
				t.Fatal(err)
			}
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
			sum := sha256.Sum256(img)
			if got := hex.EncodeToString(sum[:]); got != want.Generator.ImageSHA256 {
				t.Fatalf("image sha256 %s != oracle generator.image_sha256 %s: the oracle is stale, regenerate the fixtures", got, want.Generator.ImageSHA256)
			}

			if string(img[32:36]) != "NXSB" {
				t.Fatalf("magic at 32 = %q, want NXSB", img[32:36])
			}
			bs := binary.LittleEndian.Uint32(img[36:])
			if bs != want.Container.BlockSize {
				t.Fatalf("block size %d, oracle %d", bs, want.Container.BlockSize)
			}
			if got := binary.LittleEndian.Uint64(img[40:]); got != want.Container.BlockCount {
				t.Fatalf("block count %d, oracle %d", got, want.Container.BlockCount)
			}
			if uint64(len(img)) != want.Container.BlockCount*uint64(bs) {
				t.Fatalf("image is %d bytes, want block_count*block_size = %d", len(img), want.Container.BlockCount*uint64(bs))
			}
			if got := layoutUUID(img[72:88]); got != want.Container.UUID {
				t.Fatalf("container uuid %s, oracle %s", got, want.Container.UUID)
			}

			// Block 0 is a copy of the ring's superblock (mkapfs writes a single
			// checkpoint); the oracle names the ring block that holds it.
			var ringSB uint64
			for _, r := range want.Checkpoint.Ring {
				if r.Kind == "superblock" {
					ringSB = r.Block
				}
			}
			if ringSB == 0 {
				t.Fatal("oracle ring has no superblock")
			}
			block := func(n uint64) []byte {
				t.Helper()
				off := n * uint64(bs)
				if off+uint64(bs) > uint64(len(img)) {
					t.Fatalf("oracle block %d is outside the image", n)
				}
				return img[off : off+uint64(bs)]
			}
			if !bytes.Equal(block(ringSB), img[:bs]) {
				t.Errorf("ring block %d is not a copy of block 0", ringSB)
			}

			vsb := block(want.Volume.Block)
			if string(vsb[32:36]) != "APSB" {
				t.Fatalf("volume superblock magic %q at block %d, want APSB", vsb[32:36], want.Volume.Block)
			}
			if got := layoutUUID(vsb[240:256]); got != want.Volume.UUID {
				t.Fatalf("volume uuid %s, oracle %s", got, want.Volume.UUID)
			}
			nameField := vsb[704:960]
			if end := bytes.IndexByte(nameField, 0); end < 0 || string(nameField[:end]) != want.Volume.Name {
				t.Fatalf("volume name %q, oracle %q", bytes.TrimRight(nameField, "\x00"), want.Volume.Name)
			}
		})
	}
}

func layoutUUID(b []byte) string {
	h := hex.EncodeToString(b)
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
