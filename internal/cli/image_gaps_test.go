package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/image/ewf/ewftest"
)

func TestImageInfoShowsAcquisitionGaps(t *testing.T) {
	e := newEWFEnv(t, ewftest.Options{ErrorRanges: [][2]uint32{{8, 8}}})
	code, out, errs := e.info(t)
	if code != 0 {
		t.Fatalf("info: %d\n%s\n%s", code, out, errs)
	}
	for _, want := range []string{"Acq. errors:", "1 range(s) declared", "4096 bytes read as zeros", "offset 4096"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}

	code, out, errs = e.info(t, "--json")
	if code != 0 {
		t.Fatalf("info --json: %d\n%s\n%s", code, out, errs)
	}
	var doc struct {
		Gaps *struct {
			Ranges    uint64 `json:"ranges"`
			Bytes     int64  `json:"bytes"`
			Outside   int    `json:"outside"`
			Truncated bool   `json:"truncated"`
			Kept      []struct {
				Offset int64 `json:"offset"`
				Length int64 `json:"length"`
			} `json:"kept"`
		} `json:"acquisition_gaps"`
	}
	if err := json.Unmarshal(jsonPart(out), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Gaps == nil || doc.Gaps.Ranges != 1 || doc.Gaps.Bytes != 4096 || len(doc.Gaps.Kept) != 1 || doc.Gaps.Kept[0].Offset != 4096 {
		t.Fatalf("acquisition_gaps = %+v", doc.Gaps)
	}
}

func TestImageInfoWithoutGapsOmitsTheField(t *testing.T) {
	e := newEWFEnv(t, ewftest.Options{})
	_, out, _ := e.info(t)
	if strings.Contains(out, "Acq. errors") {
		t.Fatalf("clean image prints acquisition errors:\n%s", out)
	}
	_, out, _ = e.info(t, "--json")
	if strings.Contains(out, "acquisition_gaps") {
		t.Fatalf("clean image carries acquisition_gaps:\n%s", out)
	}
}
