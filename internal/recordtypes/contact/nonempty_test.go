package contact

import (
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/recordtypes/common"
)

// emptyableStrings are the optional texts of this package that may be an empty string. The
// contract says an optional text is absent, never empty (C3); an entry here is a documented
// exception.
// display_name and the organization texts are covered by the at-least-one rule, which reads an empty
// string as absent (TestContactAtLeastOneIdentifyingField), so they stay empty-able on purpose.
var emptyableStrings = []string{"display_name", "organization.department", "organization.name", "organization.title"}

func TestOptionalTextsAreNeverEmpty(t *testing.T) {
	var got []string
	for _, s := range allSchemas() {
		got = append(got, common.EmptyableStrings(s)...)
	}
	slices.Sort(got)
	if !slices.Equal(got, emptyableStrings) {
		t.Errorf("fields that accept an empty string: %v, want only %v", got, emptyableStrings)
	}
}

func allSchemas() []*common.Schema { return []*common.Schema{&schema} }
