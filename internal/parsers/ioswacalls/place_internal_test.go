package ioswacalls

import "testing"

func TestClassifyPlace(t *testing.T) { // 4I2-R2, 4I2-P2
	grp := "ios:HomeDomain/Containers/Shared/AppGroup/5A0B4C3D-0000-4000-8000-00000000A001"
	for _, c := range []struct {
		logical string
		want    dbPlace
	}{
		{"ios:" + domainConsumer + "/CallHistory.sqlite", dbPlace{placeDomain, "whatsapp", "ios:" + domainConsumer}},
		{"ios:" + domainBusiness + "/CallHistory.sqlite", dbPlace{placeDomain, "whatsapp_business", "ios:" + domainBusiness}},
		{grp + "/CallHistory.sqlite", dbPlace{placeAppGroup, "", grp}},
		{"ios:HomeDomain/Library/CallHistory.sqlite", dbPlace{}},
		{"ios:" + domainConsumer + "/CallHistory.sqlite-wal", dbPlace{}},
		{grp + "/CallHistory.sqlite-journal", dbPlace{}},
		{grp + "/x/CallHistory.sqlite", dbPlace{}},
		{"ios:HomeDomain/Containers/Shared/AppGroup//CallHistory.sqlite", dbPlace{}},
		{"ios:" + domainConsumer + "/sub/CallHistory.sqlite", dbPlace{}},
		{"ios:AppDomain-net.whatsapp.WhatsApp/Documents/CallHistory.sqlite", dbPlace{}},
		{"ios:" + domainConsumer + "/ChatStorage.sqlite", dbPlace{}}, // another WhatsApp store is no call history
		{"/data/CallHistory.sqlite", dbPlace{}},
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
