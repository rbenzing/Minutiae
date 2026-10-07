package sqlitefile_test

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/sqlitefile"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

func histWarnCount(h *sqlitefile.Hist, code string) (n int, msg string) {
	for _, w := range h.Warnings() {
		if w.Code == code {
			n++
			msg = w.Msg
		}
	}
	return n, msg
}

// TestHistWarnsOnceWithCountForUnattributedPages: pages Layout marks
// unattributed (the schema was read incompletely) are listed as history, never
// silently: Hist raises one warning that carries their count, however often the
// history is walked. An intact schema raises none.
func TestHistWarnsOnceWithCountForUnattributedPages(t *testing.T) {
	b, _ := freeScenario(sqlitetest.Options{PageSize: 512}, 3)
	data := b.Bytes()
	data[108], data[109] = 0xff, 0xff
	l, _ := layoutOf(t, data, sqlitefile.Options{})
	want := 0
	for pg := uint32(1); pg <= l.Addressable; pg++ {
		if l.Class[pg] == sqlitefile.ClassUnattributed {
			want++
		}
	}
	if want == 0 {
		t.Fatal("scenario has no unattributed page")
	}
	d, err := sqlitefile.Open(bytes.NewReader(data), int64(len(data)), sqlitefile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	h := d.History()
	t.Cleanup(h.Release)
	for range 2 {
		if _, err := h.Summary(context.Background()); err != nil {
			t.Fatal(err)
		}
		collectPages(t, h)
	}
	n, msg := histWarnCount(h, sqlitefile.WarnPagesUnattributed)
	if n != 1 {
		t.Fatalf("%d pages-unattributed warnings, want 1", n)
	}
	if !strings.Contains(msg, fmt.Sprint(want)) {
		t.Errorf("warning %q does not carry the count %d", msg, want)
	}

	clean, _ := freeScenario(sqlitetest.Options{PageSize: 512}, 3)
	cd := clean.Bytes()
	d2, err := sqlitefile.Open(bytes.NewReader(cd), int64(len(cd)), sqlitefile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	h2 := d2.History()
	t.Cleanup(h2.Release)
	collectPages(t, h2)
	if n, _ := histWarnCount(h2, sqlitefile.WarnPagesUnattributed); n != 0 {
		t.Errorf("intact schema: %d warnings", n)
	}
}
