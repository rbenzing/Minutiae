package androidcalendar

import "testing"

// 4I3-P5, B4: the provider place is the provider's own databases directory, by the logical path alone.
func TestClassifyPlace(t *testing.T) {
	cases := []struct {
		logical string
		want    place
	}{
		{"android:/data/data/com.android.providers.calendar/databases/calendar.db", placeProvider},
		{"android:/data/user/10/com.android.providers.calendar/databases/calendar.db", placeProvider},
		{"android:/data/user_de/0/com.android.providers.calendar/databases/calendar.db", placeProvider},
		{"android:/sdcard/x/calendar.db", placeOther},
		{"android:/data/data/com.android.providers.contacts/databases/calendar.db", placeOther},
		{"android:/data/data/com.android.providers.calendar/databases/sub/calendar.db", placeOther},
		{"android:/data/data/com.android.providers.calendar/databases/calendar.db-wal", placeOther},
		{"android:/data/data/xcom.android.providers.calendar/databases/calendar.db", placeOther},
		{"android:/data/data/com.android.providers.calendar/databases/Calendar.db", placeOther},
		{"", placeOther},
	}
	for _, c := range cases {
		if got := classifyPlace(c.logical); got != c.want {
			t.Errorf("classifyPlace(%q) = %d, want %d", c.logical, got, c.want)
		}
	}
}
