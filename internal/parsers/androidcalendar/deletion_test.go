package androidcalendar_test

import "testing"

// B7: the deletion source is the first match of Events.deleted, Events.lastSynced, Calendars.deleted; the other
// matches are stated in raw.deletion_also, in that order; raw.deleted_via is "calendar" only when the calendar is the
// source.
func TestAndroidCalendarDeletionPrecedence(t *testing.T) {
	cals := []row{{1, nil}, {2, map[string]any{"calendar_displayName": "Old", "deleted": int64(1)}}}
	p := parseBuilt(t, buildCalendarDB(t, []row{
		{1, map[string]any{"calendar_id": int64(2), "deleted": int64(1), "lastSynced": int64(1)}},
		{2, map[string]any{"calendar_id": int64(2), "lastSynced": int64(1)}},
		{3, map[string]any{"calendar_id": int64(1), "deleted": int64(1), "lastSynced": int64(1)}},
		{4, map[string]any{"calendar_id": int64(2)}},
		{5, map[string]any{"calendar_id": int64(1)}},
	}, cals), false)
	for _, c := range []struct {
		id                          int64
		source, relation, also, via string
	}{
		{1, "Events.deleted", "", "Events.lastSynced,Calendars.deleted", ""},
		{2, "Events.lastSynced", "last-synced-copy", "Calendars.deleted", ""},
		{3, "Events.deleted", "", "Events.lastSynced", ""},
		{4, "Calendars.deleted", "", "", "calendar"},
		{5, "", "", "", ""},
	} {
		r := p.rec(t, c.id)
		d := objOf(r, "deleted")
		raw := rawOf(r)
		also, _ := raw["deletion_also"].(string)
		via, _ := raw["deleted_via"].(string)
		src, _ := d["source"].(string)
		rel, _ := d["relation"].(string)
		if r.Deleted != (c.source != "") || src != c.source || rel != c.relation || also != c.also || via != c.via {
			t.Errorf("row %d: Deleted %v, deleted %v, deletion_also %q, deleted_via %q; want %q %q %q %q",
				c.id, r.Deleted, d, also, via, c.source, c.relation, c.also, c.via)
		}
	}
}
