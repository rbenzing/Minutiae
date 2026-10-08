package parse

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/records"
)

func checkMeta() Meta {
	return Meta{
		Name: "p", Version: "1.0.0", Title: "p",
		Platforms: []string{PlatformAndroid},
		Emits:     []Emit{{Type: "message", PayloadVersion: 1}, {Type: "call", PayloadVersion: 1}},
		Inputs:    []InputSpec{{Role: "primary", Globs: []string{"android:/x"}, Required: true}},
	}
}

func TestCheckRecordType(t *testing.T) {
	m := checkMeta()
	for _, typ := range []string{"message", "call"} {
		if err := CheckRecordType(m, typ); err != nil {
			t.Errorf("declared type %q refused: %v", typ, err)
		}
	}
	for _, typ := range []string{"web_visit", "", "Message", "message "} {
		err := CheckRecordType(m, typ)
		if !errors.Is(err, ErrUndeclaredType) || !strings.Contains(err.Error(), fmt.Sprintf("%q", typ)) {
			t.Errorf("type %q: %v, want ErrUndeclaredType naming it", typ, err)
		}
	}
}

func TestCheckPlatform(t *testing.T) {
	m := checkMeta()
	m.Platforms = []string{PlatformAndroid, PlatformIOS}
	for _, p := range []string{PlatformAndroid, PlatformIOS} {
		if err := CheckPlatform(m, Artifact{ID: "a", Platform: p}); err != nil {
			t.Errorf("platform %q refused: %v", p, err)
		}
	}
	m.Platforms = []string{PlatformIOS}
	err := CheckPlatform(m, Artifact{ID: "art1", Platform: PlatformAndroid})
	if !errors.Is(err, ErrForeignPlatform) || !strings.Contains(err.Error(), "art1") {
		t.Errorf("foreign platform: %v, want ErrForeignPlatform naming the artifact", err)
	}
	if err := CheckPlatform(m, Artifact{ID: "art1"}); !errors.Is(err, ErrForeignPlatform) {
		t.Errorf("an artifact with no platform: %v, want ErrForeignPlatform", err)
	}
}

func TestCheckRecord(t *testing.T) {
	m := checkMeta()
	bundle := []Artifact{{ID: "a1", Platform: PlatformAndroid}, {ID: "a2", Platform: PlatformIOS}}
	if err := CheckRecord(m, bundle, records.Record{Type: "message", ArtifactID: "a1"}); err != nil {
		t.Errorf("a good record refused: %v", err)
	}
	if err := CheckRecord(m, bundle, records.Record{Type: "web_visit", ArtifactID: "a1"}); !errors.Is(err, ErrUndeclaredType) {
		t.Errorf("undeclared type: %v", err)
	}
	if err := CheckRecord(m, bundle, records.Record{Type: "message", ArtifactID: "a2"}); !errors.Is(err, ErrForeignPlatform) {
		t.Errorf("record for an iOS artifact of an Android parser: %v", err)
	}
	// an artifact outside the bundle is the writer's rule, not this one's
	if err := CheckRecord(m, bundle, records.Record{Type: "message", ArtifactID: "zz"}); err != nil {
		t.Errorf("an artifact outside the bundle is the writer's to refuse: %v", err)
	}
}

func TestCheckWarningCount(t *testing.T) {
	if MaxWarnings != 10_000 {
		t.Fatalf("MaxWarnings = %d", MaxWarnings)
	}
	for _, n := range []int{0, 1, MaxWarnings} {
		if err := CheckWarningCount(n); err != nil {
			t.Errorf("%d warnings refused: %v", n, err)
		}
	}
	err := CheckWarningCount(MaxWarnings + 1)
	if !errors.Is(err, ErrWarningCap) || !strings.Contains(err.Error(), "10001") {
		t.Errorf("one warning past the cap: %v", err)
	}
}

func TestCheckRefusals(t *testing.T) {
	if err := CheckRefusals(0); err != nil {
		t.Errorf("no refusals: %v", err)
	}
	err := CheckRefusals(3)
	if !errors.Is(err, ErrRefusedRecords) || !strings.Contains(err.Error(), "3") {
		t.Errorf("3 refusals: %v", err)
	}
}

func TestCheckInputsUnchanged(t *testing.T) {
	want := []Artifact{{ID: "a1", SHA256: "h1"}, {ID: "a2", SHA256: "h2"}}
	var seen []string
	same := func(a Artifact) (string, error) { seen = append(seen, a.ID); return a.SHA256, nil }
	if err := CheckInputsUnchanged(want, same); err != nil {
		t.Fatalf("unchanged inputs refused: %v", err)
	}
	if len(seen) != 2 {
		t.Errorf("rehashed %v, want every input", seen)
	}
	// the LAST input changed: every input is checked, not just the first
	changed := func(a Artifact) (string, error) {
		if a.ID == "a2" {
			return "other", nil
		}
		return a.SHA256, nil
	}
	err := CheckInputsUnchanged(want, changed)
	var ic *InputChangedError
	if !errors.As(err, &ic) || !errors.Is(err, ErrInputChanged) || ic.ArtifactID != "a2" || ic.Want != "h2" || ic.Got != "other" {
		t.Fatalf("changed input: %v", err)
	}
	for _, s := range []string{"a2", "h2", "other"} {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("error %q does not name %q", err, s)
		}
	}
	// a failing rehash is returned, and is not an integrity verdict
	boom := errors.New("boom")
	err = CheckInputsUnchanged(want, func(Artifact) (string, error) { return "", boom })
	if !errors.Is(err, boom) || errors.Is(err, ErrInputChanged) {
		t.Errorf("rehash failure: %v", err)
	}
	if err := CheckInputsUnchanged(nil, same); err != nil {
		t.Errorf("no inputs: %v", err)
	}
}
