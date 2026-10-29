package iosaddressbook_test

import (
	"encoding/binary"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/parsers/iosaddressbook"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/records"
)

// onePerson runs a database built by fill and returns the record of person 1.
func onePerson(t *testing.T, fill func(k *book), tables ...string) records.Record {
	t.Helper()
	if len(tables) == 0 {
		tables = []string{"ABPerson", "ABMultiValue", "ABMultiValueLabel", "ABMultiValueEntry", "ABMultiValueEntryKey", "ABStore", "ABGroup", "ABGroupMembers"}
	}
	k := newBook(t, "addressbook-current.sql", tables...)
	k.person(1, map[string]any{"First": "Cap", "Last": "Test"})
	fill(k)
	res := runBook(t, k.bytes())
	if res.Err != nil || len(res.Records) != 1 {
		t.Fatalf("err %v, %d records, warnings %+v", res.Err, len(res.Records), res.Warnings)
	}
	return res.Records[0]
}

// The counters and totals of the mapping, each pinned by a database that provokes exactly it.
func TestCountersTotalsAndCaps(t *testing.T) {
	cases := []struct {
		name string
		fill func(k *book)
		want func(t *testing.T, r records.Record)
	}{
		{"groups over the cap state the total", func(k *book) {
			for i := int64(1); i <= 1003; i++ {
				k.row("ABGroup", i, map[string]any{"Name": fmt.Sprintf("G%04d", i)})
				k.row("ABGroupMembers", i, map[string]any{"group_id": i, "member_type": int64(0), "member_id": int64(1)})
			}
		}, func(t *testing.T, r records.Record) {
			raw := rawIn(r)
			if g := asList(raw["groups"]); len(g) != 1000 || g[0] != "G0001" || g[999] != "G1000" {
				t.Errorf("%d groups", len(g))
			}
			if raw["groups_total"] != int64(1003) || raw["groups_truncated"] != true {
				t.Errorf("raw %v", raw["groups_total"])
			}
		}},
		{"dangling and unnamed groups are counted", func(k *book) {
			k.row("ABGroup", 1, map[string]any{"Name": "Named"})
			k.row("ABGroup", 2, map[string]any{"Name": ""})
			for i, gid := range []int64{1, 2, 99} {
				k.row("ABGroupMembers", int64(i+1), map[string]any{"group_id": gid, "member_type": int64(0), "member_id": int64(1)})
			}
		}, func(t *testing.T, r records.Record) {
			raw := rawIn(r)
			if g := asList(raw["groups"]); len(g) != 1 || g[0] != "Named" {
				t.Errorf("groups %v", g)
			}
			if raw["groups_unresolved"] != int64(2) || raw["groups_total"] != nil || raw["join_undecided"] != nil {
				t.Errorf("raw %v", raw)
			}
		}},
		{"other values over the cap state the total", func(k *book) {
			for i := range 1003 {
				k.mv(1, 13, nil, fmt.Sprintf("im%04d", i))
			}
		}, func(t *testing.T, r records.Record) {
			raw := rawIn(r)
			if o := asList(raw["other_values"]); len(o) != 1000 {
				t.Errorf("%d other values", len(o))
			}
			if raw["other_values_total"] != int64(1003) || raw["other_values_truncated"] != true {
				t.Errorf("raw %v", raw)
			}
		}},
		{"empty list values are skipped and counted", func(k *book) {
			k.mv(1, 3, nil, nil)
			k.mv(1, 4, nil, "")
			k.mv(1, 3, nil, "+1 555 0001")
		}, func(t *testing.T, r records.Record) {
			raw := rawIn(r)
			if raw["values_skipped"] != int64(2) || len(asList(r.Payload["phones"])) != 1 || r.Payload["emails"] != nil {
				t.Errorf("raw %v payload %v", raw, r.Payload)
			}
		}},
		{"a multi-value whose property is unreadable is counted", func(k *book) {
			k.mv(1, 3, nil, "+1 555 0001")
			k.uid++
			k.row("ABMultiValue", k.uid, map[string]any{"record_id": int64(1), "property": "x", "value": "lost"})
		}, func(t *testing.T, r records.Record) {
			raw := rawIn(r)
			if raw["multi_values_unreadable"] != int64(1) || asMap(raw["joined_unknown_columns"])["ABMultiValue.property"] != "type-mismatch" {
				t.Errorf("raw %v", raw)
			}
		}},
		{"an entry whose key id names no row keeps its key_id", func(k *book) {
			k.keys("Street")
			a := k.mv(1, 5, nil, nil)
			k.entry(a, 1, "1 Main St")
			k.entry(a, 99, "orphan")
		}, func(t *testing.T, r records.Record) {
			raw := rawIn(r)
			want := []any{map[string]any{"index": int64(0), "key_id": int64(99), "value": "orphan"}}
			if fmt.Sprint(raw["address_keys_unmapped"]) != fmt.Sprint(want) || len(asList(r.Payload["addresses"])) != 1 {
				t.Errorf("raw %v payload %v", raw["address_keys_unmapped"], r.Payload["addresses"])
			}
		}},
		{"addresses over the cap state the total; an address without a part is not one", func(k *book) {
			k.keys("Street", "CountryCode")
			for i := range 1002 {
				k.mv(1, 5, nil, fmt.Sprintf("addr %04d", i))
			}
			a := k.mv(1, 5, nil, nil)
			k.entry(a, 2, "xx")
		}, func(t *testing.T, r records.Record) {
			raw := rawIn(r)
			if n := len(asList(r.Payload["addresses"])); n != 1000 {
				t.Errorf("%d addresses", n)
			}
			if raw["addresses_total"] != int64(1002) || raw["addresses_truncated"] != true {
				t.Errorf("raw %v", raw["addresses_total"])
			}
			u := asList(raw["address_keys_unmapped"])
			if len(u) != 1 || asMap(u[0])["key"] != "CountryCode" || asMap(u[0])["index"] != nil {
				t.Errorf("unmapped %v", u)
			}
		}},
		{"a joined text over 16 KiB is cut and flagged", func(k *book) {
			k.mv(1, 3, nil, strings.Repeat("x", 20000))
		}, func(t *testing.T, r records.Record) {
			ph := asList(r.Payload["phones"])
			if len(ph) != 1 || len(asMap(ph[0])["value"].(string)) != 16<<10 {
				t.Fatalf("phones %d", len(ph))
			}
			if f := asList(rawIn(r)["text_flags"]); len(f) != 1 || f[0] != "ABMultiValue.value" {
				t.Errorf("text_flags %v", f)
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { c.want(t, onePerson(t, c.fill)) })
	}
}

// personWithBirthday stores one ABPerson record whose Birthday (column 11) is the given serial type and body.
func personWithBirthday(t *testing.T, serial uint64, body []byte) records.Record {
	t.Helper()
	k := newBook(t, "addressbook-current.sql", "ABPerson")
	serials := []uint64{0, 15} // ROWID alias, First = one byte
	for range 9 {              // Last .. Kind
		serials = append(serials, 0)
	}
	serials = append(serials, serial)
	k.tables["ABPerson"].InsertRaw(1, serials, append([]byte{'A'}, body...))
	res := runBook(t, k.bytes())
	if res.Err != nil || len(res.Records) != 1 {
		t.Fatalf("err %v, %d records", res.Err, len(res.Records))
	}
	return res.Records[0]
}

// A non-finite REAL is stated by its IEEE bits (ruling P-4-1); an other-class Birthday is unknown.
func TestBirthdayOfOtherStorageClasses(t *testing.T) {
	var inf [8]byte
	binary.BigEndian.PutUint64(inf[:], math.Float64bits(math.Inf(1)))
	raw := rawIn(personWithBirthday(t, 7, inf[:]))
	if raw["birthday_ieee"] != "7ff0000000000000" || raw["birthday_status"] != "invalid" || raw["birthday"] != nil || raw["unknown_columns"] != nil {
		t.Errorf("+Inf birthday: %v", raw)
	}
	// a BLOB of three bytes: serial type 12 + 2*3
	raw = rawIn(personWithBirthday(t, 18, []byte{1, 2, 3}))
	if asMap(raw["unknown_columns"])["Birthday"] != "type-mismatch" || raw["birthday"] != nil || raw["birthday_ieee"] != nil {
		t.Errorf("BLOB birthday: %v", raw)
	}
}

// A run whose memory budget cannot hold the photo ids gives no photo to anyone and says why; the people are
// still parsed.
func TestPhotoSetOverTheBudgetGivesNoPhoto(t *testing.T) {
	db := peopleDB(t, 3)
	k := imagesBook(t, "ABThumbnailImage")
	for i := int64(1); i <= 3000; i++ {
		thumb(k, i, i)
	}
	h := parsertest.NewHarness(t)
	b := parsertest.Fixture{Name: "inline", Files: map[string][]byte{"db": db, "images": k.bytes()}}.BundleIOSBackup(h, "HomeDomain", relative)
	p := iosaddressbook.Parser{}
	in := b.Input(h, p, false)
	// leave about 100 KB: room to open both databases, not for 3000 kept ids
	limit := in.Budget.Limit()
	if err := in.Budget.Alloc(limit - 100<<10); err != nil {
		t.Fatal(err)
	}
	res := h.RunLenient(p, in)
	if res.Err != nil || len(res.Records) != 3 {
		t.Fatalf("err %v, %d records", res.Err, len(res.Records))
	}
	for _, r := range res.Records {
		if _, has := photoOf(r); has {
			t.Errorf("%s: photo from a set the budget could not hold", r.Locator)
		}
	}
	if n := res.Notes["images"]; !strings.Contains(n, "cap") {
		t.Errorf("note images = %q", n)
	}
}

// An Incomplete images artifact gives only true facts: a person with a row has a photo, nobody is told "false".
func TestIncompleteImagesDatabaseNeverSaysFalse(t *testing.T) {
	db := peopleDB(t, 3)
	k := imagesBook(t)
	thumb(k, 1, 1)
	full(k, 1, 2)
	h := parsertest.NewHarness(t)
	b := parsertest.Fixture{Name: "inline", Files: map[string][]byte{"db": db, "images": k.bytes()}}.BundleIOSBackup(h, "HomeDomain", relative)
	p := iosaddressbook.Parser{}
	in := b.Input(h, p, false)
	a := in.Artifacts["images"]
	a.Incomplete = true
	in.Artifacts["images"] = a
	res := h.RunLenient(p, in)
	got := byRow(t, res)
	for id, wantHas := range map[int64]bool{1: true, 2: true, 3: false} {
		present, has := photoOf(got[id])
		if has != wantHas || (has && !present) {
			t.Errorf("person %d: photo present %v has %v", id, present, has)
		}
	}
	if n := res.Notes["images"]; !strings.Contains(n, "incomplete") {
		t.Errorf("note images = %q", n)
	}
}
