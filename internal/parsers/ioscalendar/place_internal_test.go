package ioscalendar

import "testing"

// 4I3-P5, B4: the system place is exactly the system calendar path of HomeDomain (backup and extraction alike), by
// the logical path alone, byte-exact.
func TestClassifyPlace(t *testing.T) {
	cases := []struct {
		logical string
		want    place
	}{
		{"ios:HomeDomain/Library/Calendar/Calendar.sqlitedb", placeSystem},
		{"ios:MediaDomain/Library/Calendar/Calendar.sqlitedb", placeOther},
		{"ios:AppDomain-x/Library/Calendar/Calendar.sqlitedb", placeOther},
		{"ios:HomeDomain/Library/Calendar/Calendar.sqlitedb-wal", placeOther},
		{"ios:HomeDomain/Library/Calendar/Calendar.sqlitedb-journal", placeOther},
		{"ios:HomeDomain/Library/Calendar/Extras.db", placeOther},
		{"ios:HomeDomain/Library/Calendar/calendar.sqlitedb", placeOther},
		{"ios:HomeDomain/Library/Calendar/sub/Calendar.sqlitedb", placeOther},
		{"ios:homedomain/Library/Calendar/Calendar.sqlitedb", placeOther},
		{"xios:HomeDomain/Library/Calendar/Calendar.sqlitedb", placeOther},
		{"", placeOther},
	}
	for _, c := range cases {
		if got := classifyPlace(c.logical); got != c.want {
			t.Errorf("classifyPlace(%q) = %d, want %d", c.logical, got, c.want)
		}
	}
}
