package examine

import (
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/volume"
)

func TestImageRunsAreBoundedByTheImage(t *testing.T) {
	// A partition that extends past a truncated image: a run inside the
	// partition but beyond the image end must not be recorded.
	part := volume.Partition{Index: 1, Start: 1000, Length: 5000}
	raw := []filesys.Run{{Offset: 0, Length: 1000}, {Offset: 1500, Length: 500}} // image-relative 1000..2000, 2500..3000
	got, err := imageRuns(raw, 1500, part, 3000)
	if err != nil || len(got) != 2 || got[0].Offset != 1000 || got[1].Offset != 2500 {
		t.Fatalf("run ending exactly at the image end: %v, %v", got, err)
	}
	if _, err := imageRuns(raw, 1500, part, 2999); err == nil || !strings.Contains(err.Error(), "beyond") {
		t.Errorf("run past the image end accepted: %v", err)
	}
	// Holes need no image bytes.
	if got, err := imageRuns([]filesys.Run{{Offset: -1, Length: 500}}, 500, part, 10); err != nil || len(got) != 1 || got[0].Offset != -1 {
		t.Errorf("hole: %v, %v", got, err)
	}
}

func TestImageRunsAcceptsOnlyAValidPrefix(t *testing.T) {
	part := volume.Partition{Index: 1, Start: 1000, Length: 5000}
	// A strict prefix of the file (truncated allocation) is converted.
	got, err := imageRuns([]filesys.Run{{Offset: 0, Length: 512}}, 4096, part, 10000)
	if err != nil || len(got) != 1 || got[0].Offset != 1000 || got[0].Length != 512 {
		t.Fatalf("prefix runs: %v, %v", got, err)
	}
	// More bytes than the file has, a run outside the partition, or a prefix
	// beyond the image is still an error (no runs recorded).
	for name, c := range map[string]struct {
		raw  []filesys.Run
		size int64
	}{
		"longer than size":  {[]filesys.Run{{Offset: 0, Length: 512}}, 100},
		"outside partition": {[]filesys.Run{{Offset: 4900, Length: 200}}, 4096},
	} {
		if got, err := imageRuns(c.raw, c.size, part, 10000); err == nil {
			t.Errorf("%s accepted: %v", name, got)
		}
	}
	if _, err := imageRuns([]filesys.Run{{Offset: 0, Length: 512}}, 4096, part, 1200); err == nil {
		t.Error("prefix beyond the image end accepted")
	}
}
