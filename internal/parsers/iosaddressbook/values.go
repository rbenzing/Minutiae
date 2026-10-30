package iosaddressbook

import (
	"errors"
	"slices"
	"strings"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/recordtypes/common"
	"github.com/rbenzing/minutiae/internal/recordtypes/contact"
)

// Bounds. Every list is capped with its total stated; every text is cut with a flag.
const (
	maxText  = 256 << 10 // bytes of any ABPerson text kept
	maxNote  = 64 << 10  // bytes of the Note kept
	maxValue = 16 << 10  // bytes of a multi-value, label, entry or group text kept
	maxList  = 1000      // entries of any list
)

// The ABMultiValue.property values the mapping reads (from memory, 4E-R13).
const (
	propPhone   = 3
	propEmail   = 4
	propAddress = 5
	propURL     = 22
)

// joinOrder is the order raw.join_undecided names the tables in.
var joinOrder = []string{"ABMultiValue", "ABMultiValueLabel", "ABMultiValueEntry", "ABMultiValueEntryKey", "ABStore", "ABGroupMembers", "ABGroup"}

// markText lists field in raw.text_flags (sorted): its text was altered (invalid UTF-8 or NUL became U+FFFD, or
// it was cut). The original bytes stay in the artifact; nothing is altered without a flag.
func markText(raw map[string]any, field string) {
	cur, _ := raw["text_flags"].([]any)
	if slices.Contains(cur, any(field)) {
		return
	}
	cur = append(cur, field)
	slices.SortFunc(cur, func(a, b any) int { return strings.Compare(a.(string), b.(string)) })
	raw["text_flags"] = cur
}

// wrapped returns X for a system label `_$!<X>!$_` with a non-empty X.
func wrapped(label string) (string, bool) {
	const pre, post = "_$!<", ">!$_"
	if len(label) > len(pre)+len(post) && strings.HasPrefix(label, pre) && strings.HasSuffix(label, post) {
		return label[len(pre) : len(label)-len(post)], true
	}
	return "", false
}

// systemLabel is the label shown for a stored one, and the stored text when the system wrapper was removed.
func systemLabel(stored string) (shown, original string) {
	if x, ok := wrapped(stored); ok {
		return x, stored
	}
	return stored, ""
}

func joinSpace(parts ...string) string {
	var kept []string
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, " ")
}

func setCount(raw map[string]any, key string, n int64) {
	if n > 0 {
		raw[key] = n
	}
}

// state is what one ABPerson mapping collects: the raw object, the unknown columns of the mapped row, the cells
// of joined rows that hold no usable value, and the joins the engine could not decide.
type state struct {
	raw           map[string]any
	unknown       map[string]any
	joinedUnknown map[string]any
	und           map[string]bool
}

func newState() *state {
	return &state{raw: map[string]any{}, unknown: map[string]any{}, joinedUnknown: map[string]any{}, und: map[string]bool{}}
}

// undecided lists the tables whose join could not be decided, in joinOrder.
func (s *state) undecided() []any {
	var out []any
	for _, n := range joinOrder {
		if s.und[n] {
			out = append(out, n)
		}
	}
	return out
}

// clean is common.CleanText for a text that has no flag slot of its own: an altered text is listed in
// raw.text_flags under field.
func (s *state) clean(field string, b []byte, limit int) common.Cleaned {
	c := common.CleanText(b, limit)
	if len(c.Flags) > 0 {
		markText(s.raw, field)
	}
	return c
}

// text reads a TEXT cell of the mapped row. A cell of another storage class or an unreadable one is named in
// unknown_columns and read as empty text, never as a value.
func (s *state) text(row parse.Row, c rowrun.Cols, name string, limit int) common.Cleaned {
	b, k := rowrun.TextCell(row, c.At(name))
	if k != rowrun.CellValue {
		rowrun.Flag(s.unknown, row, name, c.At(name), k)
		return common.Cleaned{}
	}
	return s.clean(name, b, limit)
}

// joinedText reads a TEXT cell of a joined row, qualified by table; NULL is empty, any other non-text cell is
// unknown and listed under raw.joined_unknown_columns.
func (s *state) joinedText(row parse.Row, table, name string, col int, limit int) string {
	b, k := rowrun.TextCell(row, col)
	switch k {
	case rowrun.CellValue:
		return s.clean(table+"."+name, b, limit).Text
	case rowrun.CellNull:
		return ""
	}
	s.flagJoined(row, table, name, col, k)
	return ""
}

func (s *state) flagJoined(row parse.Row, table, name string, col int, k rowrun.CellKind) {
	if k == rowrun.CellMissing {
		s.joinedUnknown[table+"."+name] = "column-missing"
		return
	}
	rowrun.Flag(s.joinedUnknown, row, table+"."+name, col, k)
}

// undecidable reports whether a join error means "the engine cannot answer" (the table or its key column is
// missing, the table is damaged beyond reading, the join index is over its limit): the join is then listed as
// undecided. Any other error (the budget, a cancelled context) fails the job.
func undecidable(err error) bool {
	var unsupported *sqlitedb.UnsupportedSchemaError
	return errors.Is(err, sqlitedb.ErrNoSuchTable) || errors.As(err, &unsupported) ||
		errors.Is(err, sqlitedb.ErrCorrupt) || errors.Is(err, sqlitedb.ErrIndexLimit)
}

// matchRows finds the rows of table whose col equals id. undecided is true when the answer is not trusted: the
// table cannot be joined, or the index lost rows (the rows are then a possibly short answer).
func matchRows(m parse.MapContext, table, col string, id int64) (rows []parse.Row, undecided bool, err error) {
	rows, undecided, err = rowrun.Match(m, table, col, id)
	switch {
	case err == nil:
		return rows, undecided, nil
	case undecidable(err):
		return nil, true, nil
	}
	return nil, false, err
}

// getRow finds the row of table with the given rowid: found is false only for a clean miss; undecided when the
// engine cannot say.
func getRow(m parse.MapContext, table string, id int64) (row parse.Row, found, undecided bool, err error) {
	row, found, err = m.Get(table, id)
	switch {
	case err == nil:
		return row, found, false, nil
	case undecidable(err):
		return nil, false, true, nil
	}
	return nil, false, false, err
}

// labelRes is the outcome of resolving one ABMultiValue.label id.
type labelRes struct {
	text     string // the stored label text ("" when none)
	resolved bool   // the id named a row
}

// multi collects the multi-value rows of one person.
type multi struct {
	s       *state
	labels  map[int64]labelRes
	lists   map[string][]contact.Value
	totals  map[string]int64
	addrs   []contact.Address
	addrTot int64
	labelRW []any
	unmap   []any
	others  []any
	otherTt int64
	skipped int64
	unread  int64
	unres   int64
}

func newMulti(s *state) *multi {
	return &multi{s: s, labels: map[int64]labelRes{}, lists: map[string][]contact.Value{}, totals: map[string]int64{}}
}

// label resolves a label id through ABMultiValueLabel (rowid = id), memoised per person.
func (mv *multi) label(m parse.MapContext, id int64) (labelRes, error) {
	if r, ok := mv.labels[id]; ok {
		return r, nil
	}
	var res labelRes
	row, found, und, err := getRow(m, "ABMultiValueLabel", id)
	switch {
	case err != nil:
		return res, err
	case und:
		mv.s.und["ABMultiValueLabel"] = true
	case found:
		res.resolved = true
		res.text = mv.s.joinedText(row, "ABMultiValueLabel", "value", m.Column("ABMultiValueLabel", "value"), maxValue)
	}
	mv.labels[id] = res
	return res, nil
}

// labelOf reads the label of an ABMultiValue row: the stored text ("" for none). A dangling id is counted in
// raw.labels_unresolved; an undecidable table is listed in raw.join_undecided instead.
func (mv *multi) labelOf(m parse.MapContext, row parse.Row, col int) (string, error) {
	id, k := rowrun.IntCell(row, col)
	switch k {
	case rowrun.CellValue:
	case rowrun.CellNull, rowrun.CellMissing:
		return "", nil
	default:
		mv.s.flagJoined(row, "ABMultiValue", "label", col, k)
		return "", nil
	}
	r, err := mv.label(m, id)
	if err != nil {
		return "", err
	}
	if !r.resolved {
		if !mv.s.und["ABMultiValueLabel"] {
			mv.unres++
		}
		return "", nil
	}
	return r.text, nil
}

// collect reads every ABMultiValue row of the person with the given rowid.
func (mv *multi) collect(m parse.MapContext, personID int64) error {
	s := mv.s
	rows, und, err := matchRows(m, "ABMultiValue", "record_id", personID)
	if err != nil {
		return err
	}
	if und {
		s.und["ABMultiValue"] = true
	}
	c := rowrun.Resolve(m, "ABMultiValue", "property", "label", "value")
	for _, r := range rows {
		prop, k := rowrun.IntCell(r, c.At("property"))
		if k != rowrun.CellValue {
			mv.unread++
			s.flagJoined(r, "ABMultiValue", "property", c.At("property"), k)
			continue
		}
		stored, err := mv.labelOf(m, r, c.At("label"))
		if err != nil {
			return err
		}
		value := s.joinedText(r, "ABMultiValue", "value", c.At("value"), maxValue)
		shown, original := systemLabel(stored)
		switch prop {
		case propPhone:
			mv.list("phones", value, shown, original)
		case propEmail:
			mv.list("emails", value, shown, original)
		case propURL:
			mv.list("urls", value, shown, original)
		case propAddress:
			uid, ok := r.Rowid()
			if !ok {
				mv.unread++
				continue
			}
			if err := mv.address(m, uid, value, shown, original); err != nil {
				return err
			}
		default:
			mv.otherTt++
			if len(mv.others) < maxList {
				e := map[string]any{"property": prop}
				if stored != "" {
					e["label"] = stored
				}
				if value != "" {
					e["value"] = value
				}
				mv.others = append(mv.others, e)
			}
		}
	}
	return nil
}

func (mv *multi) list(name, value, shown, original string) {
	if value == "" {
		mv.skipped++
		return
	}
	mv.totals[name]++
	if len(mv.lists[name]) >= maxList {
		return
	}
	if original != "" {
		mv.labelRW = append(mv.labelRW, map[string]any{"list": name, "index": int64(len(mv.lists[name])), "label": original})
	}
	mv.lists[name] = append(mv.lists[name], contact.Value{Value: value, Label: shown})
}

// unmappedEntry is an ABMultiValueEntry the address mapping does not read into a part.
type unmappedEntry struct {
	key   string // the key name, "" when the key id named no row
	keyID int64
	value string
}

// address maps the ABMultiValueEntry rows of one address (parent_id = the ABMultiValue UID). An address is
// kept only with a mapped part or a formatted value.
func (mv *multi) address(m parse.MapContext, uid int64, formatted, shown, original string) error {
	s := mv.s
	a := contact.Address{Formatted: formatted, Label: shown}
	var un []unmappedEntry
	rows, und, err := matchRows(m, "ABMultiValueEntry", "parent_id", uid)
	if err != nil {
		return err
	}
	if und {
		s.und["ABMultiValueEntry"] = true
	}
	c := rowrun.Resolve(m, "ABMultiValueEntry", "key", "value")
	for _, er := range rows {
		keyID, k := rowrun.IntCell(er, c.At("key"))
		if k != rowrun.CellValue {
			s.flagJoined(er, "ABMultiValueEntry", "key", c.At("key"), k)
			continue
		}
		val := s.joinedText(er, "ABMultiValueEntry", "value", c.At("value"), maxValue)
		kr, found, kund, err := getRow(m, "ABMultiValueEntryKey", keyID)
		if err != nil {
			return err
		}
		if kund {
			s.und["ABMultiValueEntryKey"] = true
		}
		key := ""
		if found {
			key = s.joinedText(kr, "ABMultiValueEntryKey", "value", m.Column("ABMultiValueEntryKey", "value"), maxValue)
		}
		var dst *string
		switch key {
		case "Street":
			dst = &a.Street
		case "City":
			dst = &a.City
		case "State":
			dst = &a.Region
		case "ZIP":
			dst = &a.Postcode
		case "Country":
			dst = &a.Country
		}
		switch {
		case dst != nil && val == "":
			// a mapped part without a value says nothing
		case dst != nil && *dst == "":
			*dst = val
		default:
			un = append(un, unmappedEntry{key: key, keyID: keyID, value: val}) // CountryCode, unknown keys, a repeated part, a key id without a row
		}
	}
	emit := a.Formatted != "" || a.Street != "" || a.City != "" || a.Region != "" || a.Postcode != "" || a.Country != ""
	if emit {
		mv.addrTot++
	}
	keep := emit && len(mv.addrs) < maxList
	idx := int64(len(mv.addrs))
	for _, u := range un {
		if len(mv.unmap) >= maxList {
			break
		}
		e := map[string]any{"value": u.value}
		if u.key != "" {
			e["key"] = u.key
		} else {
			e["key_id"] = u.keyID
		}
		if keep {
			e["index"] = idx
		}
		mv.unmap = append(mv.unmap, e)
	}
	if !keep {
		return nil
	}
	if original != "" {
		mv.labelRW = append(mv.labelRW, map[string]any{"list": "addresses", "index": idx, "label": original})
	}
	mv.addrs = append(mv.addrs, a)
	return nil
}

// finish moves what was collected into the contact and the raw object.
func (mv *multi) finish(ct *contact.Contact) {
	raw := mv.s.raw
	for _, l := range []struct {
		name string
		dst  *[]contact.Value
	}{{"phones", &ct.Phones}, {"emails", &ct.Emails}, {"urls", &ct.URLs}} {
		*l.dst = mv.lists[l.name]
		if mv.totals[l.name] > maxList {
			raw[l.name+"_total"], raw[l.name+"_truncated"] = mv.totals[l.name], true
		}
	}
	ct.Addresses = mv.addrs
	if mv.addrTot > int64(len(mv.addrs)) {
		raw["addresses_total"], raw["addresses_truncated"] = mv.addrTot, true
	}
	if len(mv.labelRW) > 0 {
		raw["label_raw"] = mv.labelRW
	}
	if len(mv.unmap) > 0 {
		raw["address_keys_unmapped"] = mv.unmap
	}
	if len(mv.others) > 0 {
		raw["other_values"] = mv.others
	}
	if mv.otherTt > int64(len(mv.others)) {
		raw["other_values_total"], raw["other_values_truncated"] = mv.otherTt, true
	}
	setCount(raw, "labels_unresolved", mv.unres)
	setCount(raw, "values_skipped", mv.skipped)
	setCount(raw, "multi_values_unreadable", mv.unread)
}

// store states the account of the person's StoreID: [{ab-store, Name}], raw.store_state "no-store-row" for a
// dangling id, nothing for NULL or a variant without the column.
func (s *state) store(m parse.MapContext, row parse.Row, c rowrun.Cols, ct *contact.Contact) error {
	id, k := rowrun.IntCell(row, c.At("StoreID"))
	switch k {
	case rowrun.CellValue:
	case rowrun.CellNull, rowrun.CellMissing:
		return nil
	default:
		rowrun.Flag(s.unknown, row, "StoreID", c.At("StoreID"), k)
		return nil
	}
	sr, found, und, err := getRow(m, "ABStore", id)
	switch {
	case err != nil:
		return err
	case und:
		s.und["ABStore"] = true
	case !found:
		s.raw["store_state"] = "no-store-row"
	default:
		a := contact.Account{Type: "ab-store", Name: s.joinedText(sr, "ABStore", "Name", m.Column("ABStore", "Name"), maxValue)}
		ct.Accounts = []contact.Account{a}
	}
	return nil
}

// groups states the sorted names of the groups the person belongs to (member_type 0 and member_id = ROWID), the
// number of other member rows and the group rows that could not be named.
func (s *state) groups(m parse.MapContext, personID int64) error {
	rows, und, err := matchRows(m, "ABGroupMembers", "member_id", personID)
	if err != nil {
		return err
	}
	if und {
		s.und["ABGroupMembers"] = true
	}
	c := rowrun.Resolve(m, "ABGroupMembers", "group_id", "member_type")
	var names []string
	var other, unresolved, total int64
	for _, r := range rows {
		mt, k := rowrun.IntCell(r, c.At("member_type"))
		if k != rowrun.CellValue {
			s.flagJoined(r, "ABGroupMembers", "member_type", c.At("member_type"), k)
			continue
		}
		if mt != 0 {
			other++
			continue
		}
		gid, k := rowrun.IntCell(r, c.At("group_id"))
		if k != rowrun.CellValue {
			s.flagJoined(r, "ABGroupMembers", "group_id", c.At("group_id"), k)
			unresolved++
			continue
		}
		gr, found, gund, err := getRow(m, "ABGroup", gid)
		switch {
		case err != nil:
			return err
		case gund:
			s.und["ABGroup"] = true
		case !found:
			unresolved++
		default:
			name := s.joinedText(gr, "ABGroup", "Name", m.Column("ABGroup", "Name"), maxValue)
			if name == "" {
				unresolved++
				continue
			}
			total++
			if len(names) < maxList {
				names = append(names, name)
			}
		}
	}
	slices.Sort(names)
	if len(names) > 0 {
		list := make([]any, len(names))
		for i, n := range names {
			list[i] = n
		}
		s.raw["groups"] = list
	}
	if total > int64(len(names)) {
		s.raw["groups_total"], s.raw["groups_truncated"] = total, true
	}
	setCount(s.raw, "group_members_other", other)
	setCount(s.raw, "groups_unresolved", unresolved)
	return nil
}
