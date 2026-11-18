package ioswacontacts

import "testing"

func TestClassifyPlace(t *testing.T) { // 4I2-R2, 4I2-P2
	grp := "ios:HomeDomain/Containers/Shared/AppGroup/5A0B4C3D-0000-4000-8000-00000000A001"
	for _, c := range []struct {
		logical string
		want    dbPlace
	}{
		{"ios:" + domainConsumer + "/ContactsV2.sqlite", dbPlace{placeDomain, "whatsapp", "ios:" + domainConsumer}},
		{"ios:" + domainBusiness + "/ContactsV2.sqlite", dbPlace{placeDomain, "whatsapp_business", "ios:" + domainBusiness}},
		{grp + "/ContactsV2.sqlite", dbPlace{placeAppGroup, "", grp}},
		{"ios:HomeDomain/Library/ContactsV2.sqlite", dbPlace{}},
		{"ios:" + domainConsumer + "/ContactsV2.sqlite-wal", dbPlace{}},
		{grp + "/ContactsV2.sqlite-journal", dbPlace{}},
		{grp + "/x/ContactsV2.sqlite", dbPlace{}},
		{"ios:HomeDomain/Containers/Shared/AppGroup//ContactsV2.sqlite", dbPlace{}},
		{"ios:" + domainConsumer + "/sub/ContactsV2.sqlite", dbPlace{}},
		{"ios:AppDomain-net.whatsapp.WhatsApp/Documents/ContactsV2.sqlite", dbPlace{}},
		{"ios:" + domainConsumer + "/ChatStorage.sqlite", dbPlace{}}, // another WhatsApp store is no contacts store
		{"/data/ContactsV2.sqlite", dbPlace{}},
		{"", dbPlace{}},
	} {
		if got := classifyPlace(c.logical); got != c.want {
			t.Errorf("classifyPlace(%q) = %+v, want %+v", c.logical, got, c.want)
		}
	}
	if (dbPlace{}).Place != placeOther {
		t.Error("the zero place is not placeOther: an unclassified path would be trusted")
	}
}
