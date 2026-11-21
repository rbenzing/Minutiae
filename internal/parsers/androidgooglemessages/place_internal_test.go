package androidgooglemessages

import "testing"

// Spec 5.2: the place and the app label come from the package directory of the logical path alone.
func TestClassifyPlace(t *testing.T) {
	for _, c := range []struct {
		logical string
		want    place
		app     string
	}{
		{"android:/data/data/com.google.android.apps.messaging/databases/bugle_db", placeApp, "google_messages"},
		{"/data/data/com.google.android.apps.messaging/databases/bugle_db", placeApp, "google_messages"},
		{"android:/data/data/com.android.messaging/databases/bugle_db", placeApp, "aosp_messaging"},
		{"android:/data/user/10/com.google.android.apps.messaging/databases/bugle_db", placeApp, "google_messages"},
		{"android:/sdcard/bugle_db", placeOther, "unknown"},
		{"android:/data/data/com.google.android.apps.messaging/files/bugle_db", placeOther, "unknown"},
		{"android:/data/data/com.google.android.apps.messagingx/databases/bugle_db", placeOther, "unknown"},
		{"android:/data/data/com.google.android.apps.messaging/databases/bugle_db-wal", placeOther, "unknown"},
	} {
		got, app := classifyPlace(c.logical)
		if got != c.want || app != c.app {
			t.Errorf("classifyPlace(%q) = %v, %q; want %v, %q", c.logical, got, app, c.want, c.app)
		}
	}
}

// Spec 5.2: the Android user is read from the path, 1 to 6 ASCII digits as written, else undecided.
func TestDatabaseUser(t *testing.T) {
	const pkg = "com.google.android.apps.messaging"
	for logical, want := range map[string]string{
		"android:/data/data/" + pkg + "/databases/bugle_db":           "0",
		"android:/data/data/com.android.messaging/databases/bugle_db": "0",
		"android:/data/user/10/" + pkg + "/databases/bugle_db":        "10",
		"android:/data/user/007/" + pkg + "/databases/bugle_db":       "007",
		"android:/data/user/x/" + pkg + "/databases/bugle_db":         "undecided",
		"android:/data/user/1234567/" + pkg + "/databases/bugle_db":   "undecided",
		"android:/data/user/10/com.other.app/databases/bugle_db":      "undecided",
		"android:/sdcard/bugle_db":                                    "undecided",
	} {
		if got := databaseUser(logical); got != want {
			t.Errorf("databaseUser(%q) = %q, want %q", logical, got, want)
		}
	}
}
