package calendar_test

import (
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// Parent C7 amendment (plan 4I3): an exception row of a recurring series states the start of the occurrence it
// replaces under the Record.Times key "original_start" (Android originalInstanceTime). The package documents the key,
// so a reader of a stored record and a later parser agree on its meaning. Parent C8 amendment (ruling B3): the doc also
// states that a non-live copy kept by the store's own structure is deleted with a relation ("last-synced-copy").
func TestPackageDocDefinesTheOriginalStartTimeKey(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "calendar.go", nil, parser.ParseComments|parser.PackageClauseOnly)
	if err != nil {
		t.Fatal(err)
	}
	doc := f.Doc.Text()
	for _, want := range []string{"original_start", "Record.Times", "originalInstanceTime", "exclusive", "local-unknown", "last-synced-copy", "relation"} {
		if !strings.Contains(doc, want) {
			t.Errorf("the package documentation does not mention %q", want)
		}
	}
}
