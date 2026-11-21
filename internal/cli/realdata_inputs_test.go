package cli

import (
	"testing"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers"
)

// realCLIInput is the device name of a parser's database: its logical path and the extra roles found by file name.
// The table lives in the default build so TestRealDataCLIInputsCoverTheRegistry keeps it in step with the registry;
// the realdata-tagged tests (realdata_test.go) read it.
type realCLIInput struct {
	logical, file string
	extra         map[string]string
}

var realCLIInputs = map[string]realCLIInput{
	"android.mmssms":   {logical: "android:/data/data/com.android.providers.telephony/databases/mmssms.db", file: "mmssms.db"},
	"android.calllog":  {logical: "android:/data/data/com.android.providers.contacts/databases/calllog.db", file: "calllog.db"},
	"android.contacts": {logical: "android:/data/data/com.android.providers.contacts/databases/contacts2.db", file: "contacts2.db"},
	"ios.sms":          {logical: "ios:HomeDomain/Library/SMS/sms.db", file: "sms.db"},
	"ios.calls":        {logical: "ios:HomeDomain/Library/CallHistoryDB/CallHistory.storedata", file: "CallHistory.storedata"},
	"ios.addressbook": {
		logical: "ios:HomeDomain/Library/AddressBook/AddressBook.sqlitedb", file: "AddressBook.sqlitedb",
		extra: map[string]string{"images": "AddressBookImages.sqlitedb", "images_wal": "AddressBookImages.sqlitedb-wal"},
	},
	"chromium.history":          {logical: "android:/data/data/com.android.chrome/app_chrome/Default/History", file: "History"},
	"safari.history":            {logical: "ios:HomeDomain/Library/Safari/History.db", file: "History.db"},
	"android.whatsapp.msgstore": {logical: "android:/data/data/com.whatsapp/databases/msgstore.db", file: "msgstore.db"},
	"android.whatsapp.contacts": {logical: "android:/data/data/com.whatsapp/databases/wa.db", file: "wa.db"},
	"ios.whatsapp.chats": {
		logical: "ios:AppDomainGroup-group.net.whatsapp.WhatsApp.shared/ChatStorage.sqlite", file: "ChatStorage.sqlite",
		extra: map[string]string{"container_meta": ".com.apple.mobile_container_manager.metadata.plist"},
	},
	"ios.whatsapp.calls": {
		logical: "ios:AppDomainGroup-group.net.whatsapp.WhatsApp.shared/CallHistory.sqlite", file: "CallHistory.sqlite",
		extra: map[string]string{"container_meta": ".com.apple.mobile_container_manager.metadata.plist"},
	},
	"ios.whatsapp.contacts": {
		logical: "ios:AppDomainGroup-group.net.whatsapp.WhatsApp.shared/ContactsV2.sqlite", file: "ContactsV2.sqlite",
		extra: map[string]string{"container_meta": ".com.apple.mobile_container_manager.metadata.plist"},
	},
	"android.calendar": {logical: "android:/data/data/com.android.providers.calendar/databases/calendar.db", file: "calendar.db"},
	"ios.calendar":     {logical: "ios:HomeDomain/Library/Calendar/Calendar.sqlitedb", file: "Calendar.sqlitedb"},
	"android.telegram": {logical: "android:/data/data/org.telegram.messenger/files/cache4.db", file: "cache4.db"},
}

// Every registered parser has a realCLIInputs entry whose logical path its primary glob matches, so a real-data set
// of a new parser is never refused as "names no registered parser" (the CLI twin of the parsers package's
// TestRealDataInputsCoverTheRegistry).
func TestRealDataCLIInputsCoverTheRegistry(t *testing.T) {
	all := parsers.All()
	if len(all) == 0 {
		t.Fatal("the registry is empty, so this test would check nothing")
	}
	for _, p := range all {
		m := p.Meta()
		in, ok := realCLIInputs[m.Name]
		if !ok {
			t.Errorf("%s is registered but has no realCLIInputs entry", m.Name)
			continue
		}
		matched := false
		for _, gl := range m.Inputs[0].Globs {
			g, err := parse.CompileGlob(gl)
			if err != nil {
				t.Fatalf("%s: %v", m.Name, err)
			}
			matched = matched || g.Match(in.logical)
		}
		if !matched {
			t.Errorf("%s: logical path %q matches none of its primary globs %v", m.Name, in.logical, m.Inputs[0].Globs)
		}
	}
	for name := range realCLIInputs {
		found := false
		for _, p := range all {
			found = found || p.Meta().Name == name
		}
		if !found {
			t.Errorf("realCLIInputs names %s, which is not registered", name)
		}
	}
}
