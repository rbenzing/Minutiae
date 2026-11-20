package androidtelegram

import "testing"

// 4I4-P7, F34: the app place is the app's files directory (account 0) or one account<x> directory below it; <x> of 1 to
// 6 ASCII digits is the account as written, anything else undecided; elsewhere the account is unknown.
func TestClassifyPlace(t *testing.T) {
	const files = "android:/data/user/0/org.telegram.messenger/files/"
	cases := []struct {
		logical string
		want    place
		account string
	}{
		{"android:/data/data/org.telegram.messenger/files/cache4.db", placeApp, "0"},
		{files + "cache4.db", placeApp, "0"},
		{files + "account3/cache4.db", placeApp, "3"},
		{files + "account007/cache4.db", placeApp, "007"},
		{files + "account123456/cache4.db", placeApp, "123456"},
		{files + "accountX/cache4.db", placeApp, "undecided"},
		{files + "account1234567/cache4.db", placeApp, "undecided"},
		{files + "account/cache4.db", placeApp, "undecided"},
		{files + "account-1/cache4.db", placeApp, "undecided"},
		{"android:/sdcard/cache4.db", placeOther, "unknown"},
		{files + "account1/sub/cache4.db", placeOther, "unknown"},
		{files + "other/cache4.db", placeOther, "unknown"},
		{files + "cache4.db-wal", placeOther, "unknown"},
		{"android:/data/data/org.telegram.messenger.web/files/cache4.db", placeOther, "unknown"},
		{"android:/data/data/org.telegram.messenger/databases/cache4.db", placeOther, "unknown"},
		{"", placeOther, "unknown"},
	}
	for _, c := range cases {
		got, account := classifyPlace(c.logical)
		if got != c.want || account != c.account {
			t.Errorf("classifyPlace(%q) = %d, %q; want %d, %q", c.logical, got, account, c.want, c.account)
		}
	}
}

// 4I4-P7: the Android user of the database is read from the app's data directory, else unknown.
func TestDatabaseUser(t *testing.T) {
	cases := map[string]string{
		"android:/data/data/org.telegram.messenger/files/cache4.db":            "0",
		"android:/data/user/10/org.telegram.messenger/files/cache4.db":         "10",
		"android:/data/user/0/org.telegram.messenger/files/account2/cache4.db": "0",
		"android:/data/user/x/org.telegram.messenger/files/cache4.db":          "unknown",
		"android:/data/user/1234567/org.telegram.messenger/files/cache4.db":    "unknown",
		"android:/data/data/org.telegram.messenger.web/files/cache4.db":        "unknown",
		"android:/sdcard/cache4.db":                                            "unknown",
		"android:/sdcard/data/data/org.telegram.messenger/files/cache4.db":     "unknown",
		"android:/data/user_de/0/org.telegram.messenger/files/cache4.db":       "unknown",
		"": "unknown",
	}
	for logical, want := range cases {
		if got := databaseUser(logical); got != want {
			t.Errorf("databaseUser(%q) = %q, want %q", logical, got, want)
		}
	}
}
