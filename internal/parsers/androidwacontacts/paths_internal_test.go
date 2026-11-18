package androidwacontacts

import "testing"

// 4I-R3: which paths are the app's live databases directory, which a backup, which neither.
func TestClassifyDBPaths(t *testing.T) {
	cases := []struct {
		logical string
		want    dbPath
	}{
		{"android:/data/data/com.whatsapp/databases/wa.db", dbPath{Place: placeLive, Package: "com.whatsapp", App: "whatsapp", User: 0, UserKnown: true}},
		{"android:/data/user/10/com.whatsapp/databases/wa.db", dbPath{Place: placeLive, Package: "com.whatsapp", App: "whatsapp", User: 10, UserKnown: true}},
		{"android:/data/com.whatsapp.w4b/databases/wa.db", dbPath{Place: placeLive, Package: "com.whatsapp.w4b", App: "whatsapp_business", User: 0, UserKnown: true}},
		{"android:/user/7/com.whatsapp.w4b/databases/wa.db", dbPath{Place: placeLive, Package: "com.whatsapp.w4b", App: "whatsapp_business", User: 7, UserKnown: true}},
		{"/data/user/999999/com.whatsapp/databases/wa.db", dbPath{Place: placeLive, Package: "com.whatsapp", App: "whatsapp", User: 999999, UserKnown: true}},
		{"/storage/emulated/0/Android/data/com.whatsapp/databases/wa.db", dbPath{Place: placeOther, Package: "com.whatsapp", App: "whatsapp"}},
		{"/sdcard/Android/media/com.whatsapp/databases/wa.db", dbPath{Place: placeOther, Package: "com.whatsapp", App: "whatsapp"}},
		{"/data/user/x/com.whatsapp/databases/wa.db", dbPath{Place: placeOther, Package: "com.whatsapp", App: "whatsapp"}},
		{"/data/user/1234567/com.whatsapp/databases/wa.db", dbPath{Place: placeOther, Package: "com.whatsapp", App: "whatsapp"}},
		{"/data/user/+5/com.whatsapp/databases/wa.db", dbPath{Place: placeOther, Package: "com.whatsapp", App: "whatsapp"}},
		{"/data/user/0/com.whatsapp.w4b/files/databases/wa.db", dbPath{Place: placeOther, App: "unknown"}},
		{"/data/data/com.whatsapp/databases/sub/wa.db", dbPath{Place: placeOther, App: "unknown"}},
		{"/sdcard/WhatsApp/Databases/wa.db.crypt14", dbPath{Place: placeBackup, App: "whatsapp"}},
		{"/storage/emulated/0/Android/media/com.whatsapp.w4b/WhatsApp Business/Databases/wa.db", dbPath{Place: placeBackup, App: "whatsapp_business"}},
		{"android:/sdcard/WhatsApp Business/Databases/msgstore-2026-11-01.1.db.crypt15", dbPath{Place: placeBackup, App: "whatsapp_business"}},
	}
	for _, c := range cases {
		if got := classifyDB(c.logical); got != c.want {
			t.Errorf("classifyDB(%q) = %+v, want %+v", c.logical, got, c.want)
		}
	}
}
