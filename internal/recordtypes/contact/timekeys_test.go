package contact_test

import (
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// T73: the contacts provider keeps no modification time per raw contact; the only time it holds is the aggregate
// contact's last update. A parser states that under the time key "aggregate_last_updated" and leaves Record.Time
// unset. The package documents the key, so a reader of a stored payload and a later parser agree on its meaning.
func TestPackageDocDefinesTheAggregateTimeKey(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "contact.go", nil, parser.ParseComments|parser.PackageClauseOnly)
	if err != nil {
		t.Fatal(err)
	}
	doc := f.Doc.Text()
	for _, want := range []string{"aggregate_last_updated", "Record.Time", "contacts.contact_last_updated_timestamp"} {
		if !strings.Contains(doc, want) {
			t.Errorf("the package documentation does not mention %q", want)
		}
	}
}
