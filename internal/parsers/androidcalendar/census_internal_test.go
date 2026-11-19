package androidcalendar

import (
	"errors"
	"fmt"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/census"
)

// Task 4 ruling f and coordinator ruling C2: how a failure to open Events at the provider path is read against the
// schema census. A census that saw a table named Events (ASCII case-insensitive, as the engine) makes any failure an
// error that wraps it, never ErrNoEventsTable; with no Events in a schema read whole it is ErrNoEventsTable; with no
// Events in a schema read in part it is ErrSchemaIncomplete. A name that folds to Events only under Unicode rules
// ("Eventſ") is not Events.
func TestEventsAbsenceFollowsTheCensus(t *testing.T) {
	noSuch := fmt.Errorf("%w: no table", sqlitedb.ErrNoSuchTable)
	seen := census.Report{Objects: []census.Object{{Type: "table", Name: "EVENTS"}}}
	if err := eventsAbsence(noSuch, seen); err == nil || errors.Is(err, ErrNoEventsTable) || !errors.Is(err, sqlitedb.ErrNoSuchTable) {
		t.Errorf("census saw EVENTS, db.Table said no such table: %v; want a wrapped error, never ErrNoEventsTable", err)
	}
	if err := eventsAbsence(noSuch, census.Report{}); !errors.Is(err, ErrNoEventsTable) {
		t.Errorf("no Events in a schema read whole: %v", err)
	}
	if err := eventsAbsence(noSuch, census.Report{SchemaSkipped: 1}); !errors.Is(err, ErrSchemaIncomplete) || errors.Is(err, ErrNoEventsTable) {
		t.Errorf("no Events in a schema read in part: %v", err)
	}
	other := errors.New("damage")
	if err := eventsAbsence(other, census.Report{}); !errors.Is(err, other) || errors.Is(err, ErrNoEventsTable) {
		t.Errorf("another failure: %v", err)
	}
	for name, want := range map[string]bool{"Events": true, "eVENTs": true, "Eventſ": false, "EventSİ": false, "Event": false} {
		if got := hasEvents([]census.Object{{Type: "table", Name: name}}); got != want {
			t.Errorf("hasEvents(%q) = %v, want %v", name, got, want)
		}
	}
	if hasEvents([]census.Object{{Type: "view", Name: "Events"}}) {
		t.Error("a view named Events is a table")
	}
}
