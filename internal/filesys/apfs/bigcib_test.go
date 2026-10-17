package apfs_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys/apfs"
)

// bigImageEnv names a copy of the sparse container apfs-big.sh makes. The image is
// never committed (it is larger than 16 GiB); without it the test SKIPS with the
// command that makes it, it does not pass silently.
const bigImageEnv = "MINUTIAE_APFS_BIG_IMAGE"

type bigExpect struct {
	orcExpect
	Spaceman struct {
		CibCount uint32 `json:"sm_dev0_cib_count"`
		CabCount uint32 `json:"sm_dev0_cab_count"`
	} `json:"spaceman"`
}

func TestAPFSBigContainerMatchesOracle(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "apfs-bigcib.expect.json"))
	if err != nil {
		t.Fatalf("the oracle of the multi-CIB container is committed: %v", err)
	}
	var exp bigExpect
	if err := json.Unmarshal(raw, &exp); err != nil {
		t.Fatal(err)
	}
	if exp.Spaceman.CibCount < 2 {
		t.Fatalf("the oracle's container has %d CIB(s); this fixture exists to exercise several", exp.Spaceman.CibCount)
	}
	path := os.Getenv(bigImageEnv)
	if path == "" {
		t.Skipf("%s is not set: regenerate the multi-CIB container outside the repository with tools/fixtures/apfs-big.sh <outdir> <scratch>/apfs-bigcib.sparse.img (see tools/fixtures/README.md) and set it to that path", bigImageEnv)
	}
	fh, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fh.Close() }()
	st, err := fh.Stat()
	if err != nil {
		t.Fatal(err)
	}
	h := sha256.New()
	if _, err := io.Copy(h, fh); err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != exp.Generator.ImageSHA256 {
		t.Fatalf("image sha256 %s, oracle says %s: regenerate image and oracle TOGETHER", got, exp.Generator.ImageSHA256)
	}
	if uint64(st.Size()) != exp.Container.BlockCount*uint64(exp.Container.BlockSize) {
		t.Fatalf("image size %d, oracle block count %d x %d", st.Size(), exp.Container.BlockCount, exp.Container.BlockSize)
	}
	t.Run("unallocated", func(t *testing.T) { orcCheckUnallocatedAt(t, fh, st.Size(), &exp.orcExpect) })
	t.Run("spaceman layout is read, not guessed", func(t *testing.T) {
		f, err := apfs.Open(fh, st.Size())
		if err != nil {
			t.Fatal(err)
		}
		if w := f.Info().Warnings; len(w) != 0 {
			t.Errorf("warnings on a real container: %q", w)
		}
		cibs, okCIB := f.SpacemanCIBs()
		cabs, okCAB := f.SpacemanCABs()
		if !okCIB || !okCAB {
			t.Fatal("the space manager did not decode")
		}
		if cibs != uint64(exp.Spaceman.CibCount) || cabs != uint64(exp.Spaceman.CabCount) {
			t.Errorf("reader sees %d CIBs and %d CABs, the oracle %d and %d", cibs, cabs, exp.Spaceman.CibCount, exp.Spaceman.CabCount)
		}
	})
}
