package web

import (
	"slices"
	"testing"

	"github.com/rbenzing/minutiae/internal/recordtypes/common"
)

// emptyableStrings are the optional texts of this package that may be an empty string. The
// contract says an optional text is absent, never empty (C3); an entry here is a documented
// exception.
var emptyableStrings = []string{}

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

func allSchemas() []*common.Schema {
	return []*common.Schema{&visitSchema, &searchSchema, &downloadSchema}
}
