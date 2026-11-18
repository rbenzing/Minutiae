package ioswhatsapp

import "testing"

func TestClassifyPlace(t *testing.T) { // 4I2-R2, 4I2-P2
	grp := "ios:HomeDomain/Containers/Shared/AppGroup/5A0B4C3D-0000-4000-8000-00000000A001"
	for _, c := range []struct {
		logical string
		want    dbPlace
	}{
		{"ios:" + domainConsumer + "/ChatStorage.sqlite", dbPlace{placeDomain, "whatsapp", "ios:" + domainConsumer}},
		{"ios:" + domainBusiness + "/ChatStorage.sqlite", dbPlace{placeDomain, "whatsapp_business", "ios:" + domainBusiness}},
		{grp + "/ChatStorage.sqlite", dbPlace{placeAppGroup, "", grp}},
		{"ios:HomeDomain/Library/ChatStorage.sqlite", dbPlace{}},
		{"ios:" + domainConsumer + "/ChatStorage.sqlite-wal", dbPlace{}},
		{grp + "/ChatStorage.sqlite-journal", dbPlace{}},
		{grp + "/x/ChatStorage.sqlite", dbPlace{}},
		{"ios:HomeDomain/Containers/Shared/AppGroup//ChatStorage.sqlite", dbPlace{}},
		{"ios:" + domainConsumer + "/sub/ChatStorage.sqlite", dbPlace{}},
		{"ios:AppDomain-net.whatsapp.WhatsApp/Documents/ChatStorage.sqlite", dbPlace{}},
		{"/data/ChatStorage.sqlite", dbPlace{}},
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
