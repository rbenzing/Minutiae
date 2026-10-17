package androidcontacts

import (
	"strconv"
	"strings"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/recordtypes/common"
	"github.com/rbenzing/minutiae/internal/recordtypes/contact"
)

// rawContactSpecs are the columns of raw_contacts the mapping reads. account_name and account_type are the
// inline columns of older schemas (honoured only when present); the account of the pinned android-14 schema
// comes through account_id, and the time through contact_id (see joins.go). The pinned schema has no
// per-raw-contact time column.
func rawContactSpecs() []parse.ColumnSpec {
	return []parse.ColumnSpec{
		{Name: "display_name", Required: true, Affinity: "text"},
		{Name: "account_id", Affinity: "integer"},
		{Name: "account_name", Affinity: "text"},
		{Name: "account_type", Affinity: "text"},
		{Name: "contact_id", Affinity: "integer"},
		{Name: "starred", Affinity: "integer"},
		{Name: "deleted", Affinity: "integer"},
		{Name: "x_times_contacted", Affinity: "integer"},
		{Name: "sourceid", Affinity: "text"},
	}
}

func rawContactsMapping() parse.TableMapping {
	return parse.TableMapping{Table: "raw_contacts", Columns: rawContactSpecs(), Map: mapRawContact}
}

func specNames(specs []parse.ColumnSpec) []string {
	out := make([]string, len(specs))
	for i, s := range specs {
		out[i] = s.Name
	}
	return out
}

func textOf(row parse.Row, c rowrun.Cols, name string, raw, unknown map[string]any) string {
	b, k := rowrun.TextCell(row, c.At(name))
	if k != rowrun.CellValue {
		rowrun.Flag(unknown, row, name, c.At(name), k)
		return ""
	}
	return cleanField(raw, name, b)
}

// identifies mirrors the contact payload's cross rule: the contact must say who it is.
func identifies(ct contact.Contact) bool {
	if ct.Deleted != nil {
		for _, s := range ct.SourceIDs {
			if s != "" {
				return true
			}
		}
	}
	if ct.DisplayName != "" || len(ct.Phones) > 0 || len(ct.Emails) > 0 {
		return true
	}
	o := ct.Organization
	return o != nil && (o.Name != "" || o.Title != "" || o.Department != "")
}

func mapRawContact(m parse.MapContext, row parse.Row) ([]records.Record, error) {
	place, err := rowrun.Where(m, row)
	if err != nil {
		return nil, err
	}
	id, ok := row.Rowid()
	if !ok {
		return nil, rowrun.Unmappable("the raw_contacts row has no rowid, so its data rows cannot be joined")
	}
	c := rowrun.Resolve(m, "raw_contacts", specNames(rawContactSpecs())...)
	raw := map[string]any{}
	unknown := map[string]any{}
	joinedUnknown := map[string]any{}
	var und joined
	ct := contact.Contact{Raw: raw}
	// SourcePath is the remote path of the file that holds the cell (ruling T48). Record.Time stays unset: the
	// only time this source holds is the aggregate contact's, which is stated under contact.AggregateLastUpdated.
	rec := records.Record{Type: contact.Type, ArtifactID: place.ArtifactID, SourcePath: place.SourcePath, Locator: place.Locator, Range: &place.Range}

	ct.DisplayName = textOf(row, c, "display_name", raw, unknown)
	acct, err := accountOf(m, row, c, raw, unknown, joinedUnknown, &und)
	if err != nil {
		return nil, err
	}
	if acct != nil {
		ct.Accounts = []contact.Account{*acct}
	}
	if v, k := rowrun.IntCell(row, c.At("starred")); k == rowrun.CellValue {
		raw["starred"] = v
		if v == 0 || v == 1 {
			b := v == 1
			ct.Starred = &b
		}
	} else {
		rowrun.Flag(unknown, row, "starred", c.At("starred"), k)
	}
	if v, k := rowrun.IntCell(row, c.At("deleted")); k == rowrun.CellValue {
		raw["deleted"] = v
		if v != 0 { // the application's own marker, not a recovery (C8)
			rec.Deleted = true
			ct.Deleted = map[string]any{"source": "raw_contacts.deleted"}
		}
	} else {
		rowrun.Flag(unknown, row, "deleted", c.At("deleted"), k)
	}
	if v, k := rowrun.IntCell(row, c.At("x_times_contacted")); k == rowrun.CellValue {
		raw["x_times_contacted"] = v
	} else {
		rowrun.Flag(unknown, row, "x_times_contacted", c.At("x_times_contacted"), k)
	}
	in, stated, err := aggregateTime(m, row, c, raw, unknown, joinedUnknown, &und)
	if err != nil {
		return nil, err
	}
	if stated && in.Valid() {
		rec.Times = append(rec.Times, records.NamedTime{Kind: contact.AggregateLastUpdated, Time: records.Time{T: in.T}})
	}
	ct.SourceID = strconv.FormatInt(id, 10)
	if s := textOf(row, c, "sourceid", raw, unknown); s != "" {
		ct.SourceIDs = []string{s}
	}

	// the typed data rows
	drows, du, err := rowrun.Match(m, "data", "raw_contact_id", id)
	if err != nil {
		return nil, err
	}
	und.data = du
	if !du || len(drows) > 0 {
		raw["data_rows"] = int64(len(drows))
	}
	dc := rowrun.Resolve(m, "data", dataNames()...)
	mimeCol := m.Column("mimetypes", "mimetype")
	mimes := map[int64]string{}
	var (
		gotName, photo, photoUnknown bool
		nick                         string
		names                        contact.Names
		org                          *contact.Organization
		phones, emails, urls, ims    []contact.Value
		addrs                        []contact.Address
		notes                        []any
		unmapped, other, skipped     int64
		unk, extraNames, extraOrg    int64
		notesTotal                   int64
	)
	for _, dr := range drows {
		d := dataRow{row: dr, c: dc, raw: raw}
		mid, mok := numCell(dr, dc.At("mimetype_id"))
		mt := ""
		if mok {
			var seen bool
			if mt, seen = mimes[mid]; !seen {
				mr, found, err := m.Get("mimetypes", mid)
				if err != nil {
					return nil, err
				}
				if found {
					if b, k := rowrun.TextCell(mr, mimeCol); k == rowrun.CellValue {
						mt = common.CleanText(b, maxText).Text
					}
				}
				mimes[mid] = mt
			}
		}
		if mt == "" {
			unmapped++
			continue
		}
		primary := d.primary()
		value := func(name func(int64) string, kind bool) (contact.Value, bool) {
			v := d.text(1)
			if v == "" {
				skipped++
				return contact.Value{}, false
			}
			t, tok := d.num(2)
			lb, u := labelOf(t, tok, d.text(3), name)
			if u {
				unk++
			}
			e := contact.Value{Value: v, Label: lb, Primary: primary}
			if kind {
				e.Kind = common.AddressKind(v)
			}
			return e, true
		}
		switch strings.TrimPrefix(mt, mimePrefix) {
		case "name":
			if gotName {
				extraNames++
				continue
			}
			gotName = true
			names = contact.Names{
				Given: d.text(2), Family: d.text(3), Prefix: d.text(4), Middle: d.text(5), Suffix: d.text(6),
				Phonetic: joinSpace(d.text(7), d.text(8), d.text(9)),
			}
		case "nickname":
			if nick == "" {
				nick = d.text(1)
			}
		case "phone_v2":
			if e, ok := value(phoneName, true); ok {
				phones = append(phones, e)
			}
		case "email_v2":
			if e, ok := value(emailName, true); ok {
				emails = append(emails, e)
			}
		case "website":
			if e, ok := value(urlName, false); ok {
				urls = append(urls, e)
			}
		case "im":
			v := d.text(1)
			if v == "" {
				skipped++
				continue
			}
			proto, pok := d.num(5)
			var lb string
			if pok && proto == -1 {
				lb = d.text(6)
			} else {
				var u bool
				if lb, u = imLabel(proto, pok); u {
					unk++
				}
			}
			ims = append(ims, contact.Value{Value: v, Label: lb, Primary: primary})
		case "organization":
			if org != nil {
				extraOrg++
				continue
			}
			org = &contact.Organization{Name: d.text(1), Title: d.text(4), Department: d.text(5)}
		case "postal-address_v2":
			t, tok := d.num(2)
			lb, u := labelOf(t, tok, d.text(3), postalName)
			if u {
				unk++
			}
			a := contact.Address{Formatted: d.text(1), Street: d.text(4), City: d.text(7), Region: d.text(8), Postcode: d.text(9), Country: d.text(10), Label: lb}
			if a.Formatted != "" || a.Street != "" || a.City != "" || a.Region != "" || a.Postcode != "" || a.Country != "" {
				addrs = append(addrs, a)
			}
		case "note":
			if s := d.text(1); s != "" {
				notesTotal++
				if len(notes) < maxNotes {
					notes = append(notes, s)
				}
			}
		case "photo":
			col := dc.At("data15")
			if b, ok := dr.Blob(col); ok && len(b) > 0 {
				photo = true
			} else if st := rowrun.StateOf(dr, col); col >= 0 && !st.Known() {
				photoUnknown = true
			}
		default:
			other++
		}
	}
	if nick != "" {
		names.Nickname = nick
	}
	if names != (contact.Names{}) {
		ct.Names = &names
	}
	if org != nil && *org != (contact.Organization{}) {
		ct.Organization = org
	}
	ct.Phones, ct.Emails = capValues(phones, "phones", raw), capValues(emails, "emails", raw)
	ct.URLs, ct.IMs = capValues(urls, "urls", raw), capValues(ims, "ims", raw)
	if len(addrs) > maxList {
		raw["addresses_total"], raw["addresses_truncated"] = int64(len(addrs)), true
		addrs = addrs[:maxList]
	}
	ct.Addresses = addrs
	if photo {
		t := true
		ct.PhotoPresent = &t
	}
	if photoUnknown {
		raw["photo_state"] = "unknown"
	}
	setCount(raw, "data_unmapped", unmapped)
	setCount(raw, "data_other", other)
	setCount(raw, "values_skipped", skipped)
	setCount(raw, "labels_unknown", unk)
	setCount(raw, "name_rows_extra", extraNames)
	setCount(raw, "organization_rows_extra", extraOrg)
	if len(notes) > 0 {
		raw["notes"] = notes
	}
	if notesTotal > int64(len(notes)) {
		raw["notes_total"], raw["notes_truncated"] = notesTotal, true
	}
	undecided := und.list()
	if len(undecided) > 0 {
		raw["join_undecided"] = undecided
	}
	if len(joinedUnknown) > 0 {
		raw["joined_unknown_columns"] = joinedUnknown
	}
	if len(unknown) > 0 {
		raw["unknown_columns"] = unknown
	}
	if f := rowrun.DamageFlags(row); f != nil {
		raw["row_flags"] = f
	}
	if !identifies(ct) {
		why := ""
		if und.data {
			why = " (its data join is undecidable)"
		}
		return nil, rowrun.Unmappable("the raw_contacts row identifies nobody: no display name, phone, e-mail or organization%s", why)
	}
	rec.Body, rec.Summary = contact.Body(ct), contact.Summary(ct)
	rec.Payload = ct.Payload()
	return []records.Record{rec}, nil
}

func setCount(raw map[string]any, key string, n int64) {
	if n > 0 {
		raw[key] = n
	}
}

func deletedContactSpecs() []parse.ColumnSpec {
	return []parse.ColumnSpec{{Name: "contact_id", Required: true, Affinity: "integer"}, {Name: "contact_deleted_timestamp", Affinity: "integer"}}
}

func deletedContactsMapping() parse.TableMapping {
	return parse.TableMapping{Table: "deleted_contacts", Columns: deletedContactSpecs(), Map: mapDeletedContact}
}

// mapDeletedContact turns a tombstone (the id and the time of a deleted contact) into a
// minimal contact record: Deleted from the application's own table, nothing invented.
func mapDeletedContact(m parse.MapContext, row parse.Row) ([]records.Record, error) {
	place, err := rowrun.Where(m, row)
	if err != nil {
		return nil, err
	}
	c := rowrun.Resolve(m, "deleted_contacts", specNames(deletedContactSpecs())...)
	raw := map[string]any{}
	unknown := map[string]any{}
	cid, k := rowrun.IntCell(row, c.At("contact_id"))
	if k != rowrun.CellValue {
		return nil, rowrun.Unmappable("the deleted_contacts row has no readable contact_id")
	}
	ct := contact.Contact{SourceIDs: []string{strconv.FormatInt(cid, 10)}, Deleted: map[string]any{"source": "deleted_contacts"}, Raw: raw}
	rec := records.Record{Type: contact.Type, ArtifactID: place.ArtifactID, SourcePath: place.SourcePath, Locator: place.Locator, Range: &place.Range, Deleted: true}
	if v, tk := rowrun.IntCell(row, c.At("contact_deleted_timestamp")); tk == rowrun.CellValue {
		in := rowrun.Millis(v)
		in.Record(raw, "contact_deleted_timestamp")
		if in.Valid() {
			rec.Time = &records.Time{T: in.T}
			rec.Times = append(rec.Times, records.NamedTime{Kind: "deleted", Time: records.Time{T: in.T}})
		}
	} else {
		rowrun.Flag(unknown, row, "contact_deleted_timestamp", c.At("contact_deleted_timestamp"), tk)
	}
	if len(unknown) > 0 {
		raw["unknown_columns"] = unknown
	}
	rec.Body, rec.Summary = contact.Body(ct), contact.Summary(ct)
	rec.Payload = ct.Payload()
	return []records.Record{rec}, nil
}
