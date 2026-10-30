package iosaddressbook_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/iosaddressbook"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// peopleDB is n people called P<i>, each with a phone.
func peopleDB(t *testing.T, n int64) []byte {
	k := newBook(t, "addressbook-current.sql", "ABPerson", "ABMultiValue")
	for i := int64(1); i <= n; i++ {
		k.person(i, map[string]any{"First": fmt.Sprintf("P%d", i)})
	}
	return k.bytes()
}

// photoOf reports the photo.present of a record: present, and whether the key exists.
func photoOf(r records.Record) (present, has bool) {
	p := asMap(r.Payload["photo"])
	if p == nil {
		return false, false
	}
	v, _ := p["present"].(bool)
	return v, true
}

func imagesBook(t *testing.T, names ...string) *book {
	return newBook(t, "addressbookimages.sql", names...)
}

func thumb(k *book, id, rec int64) {
	k.row("ABThumbnailImage", id, map[string]any{"record_id": rec, "format": int64(3), "data": []byte("THUMB")})
}

func full(k *book, id, rec int64) {
	k.row("ABFullSizeImage", id, map[string]any{"record_id": rec, "data": []byte("FULL")})
}

// 4E-R25: photo presence comes from the images database only.
func TestPhotoPresenceComesFromTheImagesDatabaseOnly(t *testing.T) {
	db := peopleDB(t, 3)

	t.Run("no images role", func(t *testing.T) {
		res := runWithImages(t, db, nil, nil)
		if res.Err != nil || len(res.Records) != 3 {
			t.Fatalf("err %v, %d records", res.Err, len(res.Records))
		}
		for _, r := range res.Records {
			if _, has := photoOf(r); has {
				t.Errorf("%s: photo without an images database: %v", r.Locator, r.Payload["photo"])
			}
		}
		if res.Notes["images"] != "absent" {
			t.Errorf("note images = %q", res.Notes["images"])
		}
	})

	t.Run("a WAL without its database is no images database", func(t *testing.T) {
		res := runWithImages(t, db, nil, []byte("not a wal"))
		if res.Err != nil || len(res.Records) != 3 || res.Notes["images"] != "absent" {
			t.Errorf("err %v, %d records, note %q", res.Err, len(res.Records), res.Notes["images"])
		}
	})

	t.Run("readable and complete", func(t *testing.T) {
		k := imagesBook(t)
		thumb(k, 1, 1)
		full(k, 1, 2)
		res := runWithImages(t, db, k.bytes(), nil)
		got := byRow(t, res)
		for id, want := range map[int64]bool{1: true, 2: true, 3: false} {
			if present, has := photoOf(got[id]); !has || present != want {
				t.Errorf("person %d: photo present %v (has %v), want %v", id, present, has, want)
			}
		}
		if res.Notes["images"] != "read" {
			t.Errorf("note images = %q", res.Notes["images"])
		}
	})

	t.Run("an images file that is not a database", func(t *testing.T) {
		res := runWithImages(t, db, []byte(strings.Repeat("\x00", 4096)), nil)
		if res.Err != nil || len(res.Records) != 3 {
			t.Fatalf("err %v, %d records", res.Err, len(res.Records))
		}
		for _, r := range res.Records {
			if _, has := photoOf(r); has {
				t.Errorf("%s: photo from an unreadable images database", r.Locator)
			}
		}
		if n := res.Notes["images"]; !strings.Contains(n, "not a database") {
			t.Errorf("note images = %q, want it to name why", n)
		}
	})

	t.Run("a capped set gives no photo", func(t *testing.T) {
		p := iosaddressbook.WithPhotoCap(2)
		k := imagesBook(t)
		thumb(k, 1, 1)
		thumb(k, 2, 2)
		full(k, 1, 3)
		res := runWithImagesP(t, p, db, k.bytes(), nil)
		if res.Err != nil || len(res.Records) != 3 {
			t.Fatalf("err %v, %d records", res.Err, len(res.Records))
		}
		for _, r := range res.Records {
			if _, has := photoOf(r); has {
				t.Errorf("%s: photo from a set that hit its cap", r.Locator)
			}
		}
		if n := res.Notes["images"]; !strings.Contains(n, "cap") {
			t.Errorf("note images = %q, want it to name the cap", n)
		}
	})
}

// damagedFull builds an images database whose ABFullSizeImage has 300 rows (record_id = rowid + 1) with one
// leaf zeroed, and an ABThumbnailImage for person 1 only. lost holds the people whose full-size row was on the
// zeroed leaf.
func damagedFull(t *testing.T) (db []byte, lost map[int64]bool) {
	t.Helper()
	defs := snapshot(t, "addressbookimages.sql")
	b := sqlitetest.New(sqlitetest.Options{PageSize: 512})
	th := b.CreateTable("ABThumbnailImage", defs["ABThumbnailImage"].ddl)
	fu := b.CreateTable("ABFullSizeImage", defs["ABFullSizeImage"].ddl)
	th.Insert(1, int64(1), int64(1), int64(3), nil, []byte("T"))
	for i := int64(1); i <= 300; i++ {
		fu.Insert(i, i, i+1, nil, nil, nil, nil, []byte("F"))
	}
	db = b.Bytes()
	leaves := fu.Leaves()
	if len(leaves) < 3 {
		t.Fatalf("%d leaves", len(leaves))
	}
	lost = map[int64]bool{}
	for i := int64(1); i <= 300; i++ {
		if _, pg, _ := fu.CellBytes(i); pg == leaves[1] {
			lost[i+1] = true
		}
	}
	if len(lost) == 0 {
		t.Fatal("nothing on the damaged leaf")
	}
	page := db[int(leaves[1]-1)*512:][:512]
	page[3], page[4] = 0, 0
	return db, lost
}

// 4E-R41: photo.present is false only when BOTH image tables are decided for the person.
func TestPhotoFalseNeedsBothImageTablesDecided(t *testing.T) {
	db := peopleDB(t, 4)

	t.Run("a database without ABFullSizeImage decides by the thumbnail alone", func(t *testing.T) {
		k := imagesBook(t, "ABThumbnailImage")
		thumb(k, 1, 1)
		got := byRow(t, runWithImages(t, db, k.bytes(), nil))
		for id, want := range map[int64]bool{1: true, 2: false, 3: false, 4: false} {
			if present, has := photoOf(got[id]); !has || present != want {
				t.Errorf("person %d: present %v (has %v), want %v", id, present, has, want)
			}
		}
	})

	t.Run("a database with neither table decides nothing", func(t *testing.T) {
		other := sqlitetest.New(sqlitetest.Options{PageSize: 1024})
		other.CreateTable("unrelated", "create table unrelated(id integer primary key, x text)")
		res := runWithImages(t, db, other.Bytes(), nil)
		for _, r := range res.Records {
			if _, has := photoOf(r); has {
				t.Errorf("%s: photo from a database with no image table", r.Locator)
			}
		}
	})

	t.Run("a damaged ABFullSizeImage", func(t *testing.T) {
		idb, lost := damagedFull(t)
		people := peopleDB(t, 301)
		res := runWithImages(t, people, idb, nil)
		if res.Err != nil || len(res.Records) != 301 {
			t.Fatalf("err %v, %d records", res.Err, len(res.Records))
		}
		got := byRow(t, res)
		for id := int64(1); id <= 301; id++ {
			present, has := photoOf(got[id])
			switch {
			case id == 1: // has a thumbnail
				if !has || !present {
					t.Errorf("person 1 (thumbnail): present %v has %v, want true", present, has)
				}
			case !lost[id]: // full-size row survived
				if !has || !present {
					t.Errorf("person %d (readable full-size row): present %v has %v, want true", id, present, has)
				}
			default: // no thumbnail, and no readable full-size row: undecided, never false
				if has {
					t.Errorf("person %d: photo %v although ABFullSizeImage is undecided for them", id, present)
				}
			}
		}
		if n := res.Notes["images"]; !strings.Contains(n, "ABFullSizeImage") {
			t.Errorf("note images = %q, want it to name ABFullSizeImage", n)
		}
	})

	t.Run("a cap hit gives no photo", func(t *testing.T) {
		p := iosaddressbook.WithPhotoCap(1)
		k := imagesBook(t)
		thumb(k, 1, 1)
		full(k, 1, 2)
		for _, r := range runWithImagesP(t, p, db, k.bytes(), nil).Records {
			if _, has := photoOf(r); has {
				t.Errorf("%s: photo from a set that hit its cap", r.Locator)
			}
		}
	})
}

// The decorator is outside the mapper (spec 12.1): the same database parses to the same records with and
// without the images role, but for payload.photo.
func TestMapperReadsNoStateBesideRowAndContext(t *testing.T) {
	h := parsertest.NewHarness(t)
	fx := parsertest.LoadFixture(t, "testdata", "with-images")
	with := fx.BundleIOSBackup(h, "HomeDomain", relative)
	parsertest.AssertMapperSourceAgnostic(t, h, iosaddressbook.Parser{}, with, 100)

	a := runWithImages(t, fx.Files["db"], fx.Files["images"], nil)
	b := runWithImages(t, fx.Files["db"], nil, nil)
	if len(a.Records) != 3 || len(b.Records) != 3 {
		t.Fatalf("%d and %d records", len(a.Records), len(b.Records))
	}
	for i := range a.Records {
		if _, has := photoOf(a.Records[i]); !has {
			t.Errorf("record %d: no photo with the images role", i)
		}
		if _, has := photoOf(b.Records[i]); has {
			t.Errorf("record %d: photo without the images role", i)
		}
		pa, pb := map[string]any{}, map[string]any{}
		for k, v := range a.Records[i].Payload {
			if k != "photo" {
				pa[k] = v
			}
		}
		for k, v := range b.Records[i].Payload {
			pb[k] = v
		}
		if fmt.Sprint(pa) != fmt.Sprint(pb) || a.Records[i].Body != b.Records[i].Body || a.Records[i].Summary != b.Records[i].Summary {
			t.Errorf("record %d differs beside photo:\n%v\n%v", i, pa, pb)
		}
	}
}
