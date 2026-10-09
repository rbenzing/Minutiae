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
	for _, want := range []string{"Acq. errors:", "1 range(s) declared", "declared unreadable by the acquirer", "4096 bytes in the media", "offset 4096"} {
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

type gapDoc struct {
	Gaps *struct {
		State     string `json:"state"`
		Ranges    uint64 `json:"ranges"`
		Bytes     int64  `json:"bytes"`
		Outside   int    `json:"outside"`
		Empty     int    `json:"empty"`
		Truncated bool   `json:"truncated"`
		Kept      []struct {
			Offset int64 `json:"offset"`
			Length int64 `json:"length"`
		} `json:"kept"`
	} `json:"acquisition_gaps"`
}

func gapInfo(t *testing.T, o ewftest.Options) (text, js string, doc gapDoc) {
	t.Helper()
	e := newEWFEnv(t, o)
	code, text, errs := e.info(t)
	if code != 0 {
		t.Fatalf("info: %d\n%s\n%s", code, text, errs)
	}
	code, js, errs = e.info(t, "--json")
	if code != 0 {
		t.Fatalf("info --json: %d\n%s\n%s", code, js, errs)
	}
	if err := json.Unmarshal(jsonPart(js), &doc); err != nil {
		t.Fatal(err)
	}
	return text, js, doc
}

// The three states are distinct in text and JSON: no section, an empty section,
// a damaged section (gaps unknown).
func TestImageInfoGapStatesAreDistinct(t *testing.T) {
	text, _, doc := gapInfo(t, ewftest.Options{})
	if doc.Gaps == nil || doc.Gaps.State != "absent" || !strings.Contains(text, "Acq. errors:") || !strings.Contains(text, "no error2 section") {
		t.Errorf("absent: %+v\n%s", doc.Gaps, text)
	}
	text, _, doc = gapInfo(t, ewftest.Options{ErrorRanges: [][2]uint32{}})
	if doc.Gaps == nil || doc.Gaps.State != "empty" || !strings.Contains(text, "error2 section is empty") || strings.Contains(text, "UNKNOWN") {
		t.Errorf("empty: %+v\n%s", doc.Gaps, text)
	}
	e := newEWFEnvMutated(t, ewftest.Options{ErrorRanges: [][2]uint32{{8, 8}}}, func(segs [][]byte) {
		for _, s := range segs {
			for _, sec := range ewftest.Sections(s) {
				if sec.Type == "error2" {
					s[sec.Offset+76+3] ^= 0xFF // header checksum fails
				}
			}
		}
	})
	code, text, errs := e.info(t)
	if code != 0 {
		t.Fatalf("info: %d\n%s\n%s", code, text, errs)
	}
	var js string
	code, js, errs = e.info(t, "--json")
	if code != 0 {
		t.Fatalf("info --json: %d\n%s\n%s", code, js, errs)
	}
	var d gapDoc
	if err := json.Unmarshal(jsonPart(js), &d); err != nil {
		t.Fatal(err)
	}
	if d.Gaps == nil || d.Gaps.State != "damaged" || !strings.Contains(text, "UNKNOWN") {
		t.Errorf("damaged: %+v\n%s", d.Gaps, text)
	}
}

func TestImageInfoGapOutsideAndTruncatedWording(t *testing.T) {
	// one range wholly beyond the media, one empty range, one inside
	text, js, doc := gapInfo(t, ewftest.Options{ErrorRanges: [][2]uint32{{1000, 4}, {8, 0}, {2, 1}}})
	if strings.Contains(text, "list truncated") || strings.Contains(text, "read as zeros") {
		t.Errorf("wording claims what is not true:\n%s", text)
	}
	for _, want := range []string{"declared unreadable by the acquirer", "1 beyond the media", "1 empty"} {
		if !strings.Contains(text, want) {
			t.Errorf("text lacks %q:\n%s", want, text)
		}
	}
	if doc.Gaps == nil || doc.Gaps.Outside != 1 || doc.Gaps.Empty != 1 || len(doc.Gaps.Kept) != 1 {
		t.Errorf("gaps = %+v\n%s", doc.Gaps, js)
	}

	// every range beyond the media: kept is [], never null
	_, js, doc = gapInfo(t, ewftest.Options{ErrorRanges: [][2]uint32{{1000, 4}}})
	if doc.Gaps == nil || doc.Gaps.Outside != 1 || !strings.Contains(js, `"kept": []`) && !strings.Contains(js, `"kept":[]`) {
		t.Errorf("all-outside JSON: %+v\n%s", doc.Gaps, js)
	}

	// more ranges than are kept: truncated is stated
	n := 65536 + 2 // the parser keeps 65536
	r := make([][2]uint32, n)
	for i := range r {
		r[i] = [2]uint32{uint32(i % 4), 1}
	}
	text, _, doc = gapInfo(t, ewftest.Options{ErrorRanges: r})
	if doc.Gaps == nil || !doc.Gaps.Truncated || !strings.Contains(text, "list truncated") {
		t.Errorf("truncated: %+v\n%s", doc.Gaps, text)
	}
}
