//go:build realimages

package ewf_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/image/ewf"
)

// TestEWFRealImages opens every *.E01 found in $MINUTIAE_TEST_IMAGES (with its
// E02, E03, ... segments), requires a clean read of the first and the last chunk
// and, when the image stores a hash, that Verify says "match". Manual only:
//
//	MINUTIAE_TEST_IMAGES=/path/to/images go test -tags realimages -run TestEWFRealImages -v ./internal/image/ewf
func TestEWFRealImages(t *testing.T) {
	dir := os.Getenv("MINUTIAE_TEST_IMAGES")
	if dir == "" {
		t.Skip("MINUTIAE_TEST_IMAGES is not set")
	}
	firsts, err := filepath.Glob(filepath.Join(dir, "*.E01"))
	if err != nil {
		t.Fatal(err)
	}
	if len(firsts) == 0 {
		t.Skipf("no *.E01 in %s", dir)
	}
	for _, first := range firsts {
		t.Run(filepath.Base(first), func(t *testing.T) {
			stem := strings.TrimSuffix(first, "E01")
			var segs []ewf.Segment
			for i := 1; ; i++ {
				p := fmt.Sprintf("%sE%02d", stem, i)
				f, err := os.Open(p)
				if err != nil {
					break
				}
				t.Cleanup(func() { _ = f.Close() })
				st, err := f.Stat()
				if err != nil {
					t.Fatal(err)
				}
				segs = append(segs, ewf.Segment{Name: filepath.Base(p), R: f, Size: st.Size()})
			}
			r, err := ewf.Open(segs)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = r.Close() }()
			t.Logf("%d segment(s), %d bytes, %d chunks of %d, warnings %q", len(segs), r.Size(), r.Chunks(), r.ChunkSize(), r.Warnings())
			cs := int64(r.ChunkSize())
			for _, c := range []int64{0, r.Chunks() - 1} {
				p := make([]byte, cs)
				n, err := r.ReadAt(p, c*cs)
				if err != nil && n < int(min(cs, r.Size()-c*cs)) {
					t.Fatalf("chunk %d: %d bytes, %v", c, n, err)
				}
			}
			res, err := r.Verify(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("verify: %s (md5 %s, sha1 %s)", res.Result(), res.MD5.Status, res.SHA1.Status)
			if res.Result() != "match" && res.Result() != "absent" {
				t.Fatalf("Verify: %+v", res)
			}
		})
	}
}
