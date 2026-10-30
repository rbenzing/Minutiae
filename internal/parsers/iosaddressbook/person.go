package iosaddressbook

import (
	"math"
	"strconv"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/cellx"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/recordtypes/contact"
)

const tablePerson = "ABPerson"

// personSpecs are the columns of ABPerson the mapping reads: ROWID, then the optional ones in the order of the
// DDL snapshot (the schema note lists them in this order). FirstSort, LastSort, CompositeNameFallback,
// ExternalModificationTag and ExternalRepresentation are left unmapped (and said so by the run's notes).
func personSpecs() []parse.ColumnSpec {
	specs := []parse.ColumnSpec{{Name: "ROWID", Required: true, Affinity: "integer"}}
	for _, n := range []string{"First", "Last", "Middle", "FirstPhonetic", "MiddlePhonetic", "LastPhonetic", "Organization", "Department", "Note"} {
		specs = append(specs, parse.ColumnSpec{Name: n, Affinity: "text"})
	}
	specs = append(specs, parse.ColumnSpec{Name: "Kind", Affinity: "integer"}, parse.ColumnSpec{Name: "Birthday", Affinity: "text"})
	for _, n := range []string{"JobTitle", "Nickname", "Prefix", "Suffix"} {
		specs = append(specs, parse.ColumnSpec{Name: n, Affinity: "text"})
	}
	specs = append(specs,
		parse.ColumnSpec{Name: "CreationDate", Affinity: "integer"}, parse.ColumnSpec{Name: "ModificationDate", Affinity: "integer"},
		parse.ColumnSpec{Name: "ExternalIdentifier", Affinity: "text"}, parse.ColumnSpec{Name: "ExternalUUID", Affinity: "text"},
		parse.ColumnSpec{Name: "StoreID", Affinity: "integer"}, parse.ColumnSpec{Name: "DisplayName", Affinity: "text"},
		parse.ColumnSpec{Name: "guid", Affinity: "text"})
	return specs
}

func personMapping() parse.TableMapping {
	return parse.TableMapping{Table: tablePerson, Columns: personSpecs(), Map: mapPerson}
}

func specNames(specs []parse.ColumnSpec) []string {
	out := make([]string, len(specs))
	for i, s := range specs {
		out[i] = s.Name
	}
	return out
}

// identifies mirrors the contact payload's cross rule: the contact must say who it is.
func identifies(ct contact.Contact) bool {
	if ct.DisplayName != "" || len(ct.Phones) > 0 || len(ct.Emails) > 0 {
		return true
	}
	o := ct.Organization
	return o != nil && (o.Name != "" || o.Title != "" || o.Department != "")
}

// birthday states the stored Birthday in raw.birthday as stored: text cleaned, a number as the number (a
// non-finite REAL by its IEEE bits, ruling P-4-1). It is never turned into a date (4E-R23: that needs a time
// zone the source does not hold).
func (s *state) birthday(row parse.Row, c rowrun.Cols) {
	col := c.At("Birthday")
	if b, k := rowrun.TextCell(row, col); k == rowrun.CellValue {
		if t := s.clean("Birthday", b, maxText).Text; t != "" {
			s.raw["birthday"] = t
		}
		return
	}
	n, k := cellx.NumberCell(row, col)
	switch {
	case k == rowrun.CellValue && !n.IsInt && (math.IsNaN(n.F) || math.IsInf(n.F, 0)):
		s.raw["birthday_ieee"] = strconv.FormatUint(math.Float64bits(n.F), 16)
		s.raw["birthday_status"] = "invalid"
	case k == rowrun.CellValue:
		s.raw["birthday"] = n.Raw()
	default:
		rowrun.Flag(s.unknown, row, "Birthday", col, k)
	}
}

// instant reads CreationDate or ModificationDate as Cocoa seconds and states the stored value in raw.
func (s *state) instant(row parse.Row, c rowrun.Cols, col string) (records.Time, bool) {
	n, k := cellx.NumberCell(row, c.At(col))
	if k != rowrun.CellValue {
		rowrun.Flag(s.unknown, row, col, c.At(col), k)
		return records.Time{}, false
	}
	in := cellx.CocoaSeconds(n)
	in.Record(s.raw, col)
	return records.Time{T: in.T}, in.Valid()
}

// mapPerson maps one ABPerson row to a contact. Everything the row joins (multi-values, labels, address
// entries, the store, groups) comes through the MapContext; the photo is not read here (photo.go decorates the
// emitter), so the mapper is a pure function of the row and its context (spec 12.1).
func mapPerson(m parse.MapContext, row parse.Row) ([]records.Record, error) {
	place, err := rowrun.Where(m, row)
	if err != nil {
		return nil, err
	}
	id, ok := row.Rowid()
	if !ok {
		return nil, rowrun.Unmappable("the ABPerson row has no rowid, so its multi-values cannot be joined")
	}
	c := rowrun.Resolve(m, tablePerson, specNames(personSpecs())...)
	s := newState()
	ct := contact.Contact{Raw: s.raw}
	rec := records.Record{Type: contact.Type, ArtifactID: place.ArtifactID, SourcePath: place.SourcePath, Locator: place.Locator, Range: &place.Range}

	first, last, middle := s.text(row, c, "First", maxText).Text, s.text(row, c, "Last", maxText).Text, s.text(row, c, "Middle", maxText).Text
	prefix, suffix, nick := s.text(row, c, "Prefix", maxText).Text, s.text(row, c, "Suffix", maxText).Text, s.text(row, c, "Nickname", maxText).Text
	phonetic := joinSpace(s.text(row, c, "FirstPhonetic", maxText).Text, s.text(row, c, "MiddlePhonetic", maxText).Text, s.text(row, c, "LastPhonetic", maxText).Text)
	names := contact.Names{Given: first, Family: last, Middle: middle, Prefix: prefix, Suffix: suffix, Nickname: nick, Phonetic: phonetic}
	if names != (contact.Names{}) {
		ct.Names = &names
	}
	// 4E-R24: the stored DisplayName when non-empty, else the composed name (no nickname), else none
	if stored := s.text(row, c, "DisplayName", maxText).Text; stored != "" {
		ct.DisplayName = stored
		s.raw["display_name_source"] = "stored"
	} else if composed := joinSpace(prefix, first, middle, last, suffix); composed != "" {
		ct.DisplayName = composed
		s.raw["display_name_source"] = "composed"
	}
	org := contact.Organization{Name: s.text(row, c, "Organization", maxText).Text, Department: s.text(row, c, "Department", maxText).Text, Title: s.text(row, c, "JobTitle", maxText).Text}
	if org != (contact.Organization{}) {
		ct.Organization = &org
	}

	if v, k := rowrun.IntCell(row, c.At("Kind")); k == rowrun.CellValue {
		s.raw["Kind"] = v
	} else {
		rowrun.Flag(s.unknown, row, "Kind", c.At("Kind"), k)
	}
	if note := s.text(row, c, "Note", maxNote); note.Text != "" {
		s.raw["Note"] = note.Text
		if note.Truncated {
			s.raw["note_truncated"] = true
			s.raw["note_total_bytes"] = int64(note.TotalBytes)
		}
	}
	s.birthday(row, c)

	created, haveCreated := s.instant(row, c, "CreationDate")
	modified, haveModified := s.instant(row, c, "ModificationDate")
	if haveCreated {
		rec.Times = append(rec.Times, records.NamedTime{Kind: "created", Time: created})
	}
	if haveModified {
		rec.Times = append(rec.Times, records.NamedTime{Kind: "modified", Time: modified})
	}
	switch {
	case haveModified:
		rec.Time = &modified
	case haveCreated:
		rec.Time = &created
	}

	ct.SourceID = strconv.FormatInt(id, 10)
	for _, name := range []string{"guid", "ExternalIdentifier", "ExternalUUID"} {
		if v := s.text(row, c, name, maxText).Text; v != "" {
			ct.SourceIDs = append(ct.SourceIDs, v)
		}
	}

	if err := s.store(m, row, c, &ct); err != nil {
		return nil, err
	}
	mv := newMulti(s)
	if err := mv.collect(m, id); err != nil {
		return nil, err
	}
	mv.finish(&ct)
	if err := s.groups(m, id); err != nil {
		return nil, err
	}

	if u := s.undecided(); len(u) > 0 {
		s.raw["join_undecided"] = u
	}
	if len(s.joinedUnknown) > 0 {
		s.raw["joined_unknown_columns"] = s.joinedUnknown
	}
	if len(s.unknown) > 0 {
		s.raw["unknown_columns"] = s.unknown
	}
	if f := rowrun.DamageFlags(row); f != nil {
		s.raw["row_flags"] = f
	}
	if len(s.raw) == 0 {
		ct.Raw = nil
	}
	if !identifies(ct) {
		why := ""
		if len(s.und) > 0 {
			why = " (a join is undecidable)"
		}
		return nil, rowrun.Unmappable("the ABPerson row identifies nobody: no display name, phone, e-mail or organization%s", why)
	}
	rec.Body, rec.Summary = contact.Body(ct), contact.Summary(ct)
	rec.Payload = ct.Payload()
	return []records.Record{rec}, nil
}
