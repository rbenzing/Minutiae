package androidtelegram

import (
	"strconv"
	"strings"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/decode/tl"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/recordtypes/contact"
)

// The fixed values of the contact mapping (spec 8.9).
const (
	kindTelegram         = "telegram"
	kindTelegramUsername = "telegram_username"
	sourceComposed       = "composed"
	colStatus            = "status"
	colAbout             = "about"
	colMutual            = "mutual"
	stateNoAboutRow      = "no-users_data-row"
)

// userFlags are the User flags kept in raw when set, with the raw key each states: deleted is a server account state
// (raw.account_deleted), never Record.Deleted (spec 8.9).
func userFlags() [][2]string {
	return [][2]string{
		{"self", "self"},
		{"contact", "contact"},
		{"mutual_contact", "mutual_contact"},
		{"deleted", "account_deleted"},
		{"bot", "bot"},
		{"min", "min"},
		{"verified", "verified"},
		{"scam", "scam"},
		{"fake", "fake"},
		{"premium", "premium"},
	}
}

// statusLabel names the User status values of spec 8.9: -100/-101/-102 recently, last_week, last_month and
// -1000/-1001/-1002 their "by me" forms; "" for any other value.
func statusLabel(s int64) string {
	switch s {
	case -100:
		return "recently"
	case -101:
		return "last_week"
	case -102:
		return "last_month"
	case -1000:
		return "recently_by_me"
	case -1001:
		return "last_week_by_me"
	case -1002:
		return "last_month_by_me"
	}
	return ""
}

// userColumns are the columns the users mapping reads: data is required (the signature), status optional.
func userColumns() []parse.ColumnSpec {
	return []parse.ColumnSpec{{Name: colData, Required: true, Affinity: "blob"}, {Name: colStatus, Affinity: "integer"}}
}

// userMappingWith is the mapping of users: one contact per row (spec 8.9, T4). A nil e is the typed recovery pass
// (4I4-P6): the row's own columns and blob are read, no join runs and the join-derived fields state
// "not-joined-recovered". It builds its own constructor table; Parse, which runs three mappings, builds one for all
// through userMapping (C2).
func userMappingWith(e *env) parse.TableMapping { return userMapping(e, newTable()) }

// userMapping is userMappingWith over the constructor table tab.
func userMapping(e *env, tab tl.Table) parse.TableMapping {
	cols := userColumns()
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = c.Name
	}
	return parse.TableMapping{Table: tableUsers, Columns: cols, Map: func(m parse.MapContext, row parse.Row) ([]records.Record, error) {
		if e != nil && len(e.Guard.bad) > 0 {
			m = guardedCtx{MapContext: m, g: &e.Guard}
		}
		return mapUser(m, row, e, tab, rowrun.Resolve(m, tableUsers, names...))
	}}
}

// userRow holds what the mapping of one users row reads before it builds the record.
type userRow struct {
	raw, unknown map[string]any
}

// readStatus states the status column (spec 8.9, 10): raw only, with its label or its expiry; never a time. A stored
// NULL is a fact about the row (C1); a cell of another class or an unreadable one is listed in unknown_columns.
func (r *userRow) readStatus(row parse.Row, c rowrun.Cols) {
	col := c.At(colStatus)
	v, k := intCellOf(row, col)
	switch k {
	case rowrun.CellValue:
		r.raw[colStatus] = v
		if l := statusLabel(v); l != "" {
			r.raw["status_label"] = l
		} else if v > 0 {
			r.raw["status_expires"] = v
		}
	case rowrun.CellNull:
		r.raw[colStatus] = nil
	case rowrun.CellOther:
		r.unknown[tableUsers+"."+colStatus] = unknownTypeMismatch
	case rowrun.CellUnknown:
		r.unknown[tableUsers+"."+colStatus] = rowrun.StateOf(row, col).String()
	}
}

// identText is an identifier (a phone or a username) as stored, cleaned and never cut: "" when empty or over its
// limit (then raw states it, ident). A text the cleaning altered is named in raw.text_flags.
func identText(raw map[string]any, key string, b []byte) string {
	s, ok := ident(raw, key, b, maxIdent)
	if !ok || s == "" {
		return ""
	}
	if s != string(b) {
		markText(raw, key)
	}
	return s
}

// userFields reads the contact's own fields from the decoded User: the flags, access_hash_present, the cleaned name
// parts (an empty part is absent), the phone and the username (spec 8.9).
func (r *userRow) userFields(v view) (names contact.Names, phone, username string) {
	for _, f := range userFlags() {
		if v.True(f[0]) {
			r.raw[f[1]] = true
		}
	}
	if v.Has("access_hash") {
		r.raw["access_hash_present"] = true
	}
	part := func(path, key string) string {
		b, ok := v.Str(path)
		if !ok {
			return ""
		}
		return cleanInto(map[string]any{}, r.raw, key, "names."+key, b, maxName)
	}
	names.Given, names.Family = part("first_name", "given"), part("last_name", "family")
	if b, ok := v.Str("phone"); ok {
		phone = identText(r.raw, "phone", b)
	}
	if b, ok := v.Str("username"); ok {
		username = identText(r.raw, "username", b)
	}
	return names, phone, username
}

// joinContact states the contacts row of the user (spec 8.9): its presence and mutual flag, or why the join is not
// decided. A missing row is contact_row false, never an empty field.
func joinContact(m parse.MapContext, e *env, uid int64, uidOK bool, raw, unknown map[string]any) error {
	if e == nil {
		raw["contact_row_state"] = joinRecovered
		return nil
	}
	if !uidOK {
		raw["contact_row_state"] = stateUndecided
		return nil
	}
	crow, st, err := joinGet(m, tableContacts, uid, raw)
	switch {
	case err != nil:
		return err
	case crow != nil:
		raw["contact_row"] = true
		col := m.Column(tableContacts, colMutual)
		v, k := intCellOf(crow, col)
		switch k {
		case rowrun.CellValue:
			raw[colMutual] = v
		case rowrun.CellNull:
			raw[colMutual] = nil
		case rowrun.CellOther:
			unknown[tableContacts+"."+colMutual] = unknownTypeMismatch
		case rowrun.CellUnknown:
			unknown[tableContacts+"."+colMutual] = rowrun.StateOf(crow, col).String()
		}
	case st == stateUndecided:
		raw["contact_row_state"] = stateUndecided
	case st == joinZeroKey:
		raw["contact_row"], raw["contact_row_state"] = false, joinZeroKey
	default:
		raw["contact_row"] = false
	}
	return nil
}

// joinAbout reads users_data.about of the user (spec 8.9): the cleaned text, or in raw.about_state why there is none:
// no row, a zero key, an undecided join. A row whose about is NULL states nothing; a cell of another class or an
// unreadable one is listed in unknown_columns.
func joinAbout(m parse.MapContext, e *env, uid int64, uidOK bool, raw, unknown map[string]any) (string, error) {
	if e == nil {
		raw["about_state"] = joinRecovered
		return "", nil
	}
	if !uidOK {
		raw["about_state"] = stateUndecided
		return "", nil
	}
	drow, st, err := joinGet(m, tableUsersData, uid, raw)
	switch {
	case err != nil:
		return "", err
	case drow == nil && st == stateUndecided, drow == nil && st == joinZeroKey:
		raw["about_state"] = st
		return "", nil
	case drow == nil:
		raw["about_state"] = stateNoAboutRow
		return "", nil
	}
	col := m.Column(tableUsersData, colAbout)
	if nanCell(drow, col) {
		unknown[tableUsersData+"."+colAbout] = unknownTypeMismatch
		return "", nil
	}
	b, k := rowrun.TextCell(drow, col)
	switch k {
	case rowrun.CellValue:
		return cleanInto(map[string]any{}, raw, colAbout, colAbout, b, maxBody), nil
	case rowrun.CellOther:
		unknown[tableUsersData+"."+colAbout] = unknownTypeMismatch
	case rowrun.CellUnknown:
		unknown[tableUsersData+"."+colAbout] = rowrun.StateOf(drow, col).String()
	}
	return "", nil
}

// mapUser turns one users row into its contact record (spec 8.9). The decode of the data blob charges the job budget
// and is freed before the row's mapping returns (F8).
func mapUser(m parse.MapContext, row parse.Row, e *env, tab tl.Table, c rowrun.Cols) ([]records.Record, error) {
	place, err := rowrun.Where(m, row)
	if err != nil {
		return nil, err
	}
	r := &userRow{raw: map[string]any{}, unknown: map[string]any{}}
	raw := r.raw
	if fl := rowrun.DamageFlags(row); fl != nil {
		raw["row_flags"] = fl
	}
	if e != nil && e.Account != valueUnknown {
		raw["account"] = e.Account
	}
	uid, uidOK := row.Rowid()
	r.readStatus(row, c)

	var names contact.Names
	var phone, username string
	bud := budgetOf(m)
	b, k := blobCell(row, c.At(colData))
	switch k {
	case rowrun.CellValue:
		v, state := decodeBlob(raw, colData, b, typeUser, tab, bud)
		defer func() {
			if bud != nil {
				bud.Free(v.r.Charged())
			}
		}()
		if _, ok := topOf(v, tab, typeUser, state); ok {
			names, phone, username = r.userFields(v)
		}
	case rowrun.CellNull:
		raw[colData+"_state"] = stateNull
	case rowrun.CellOther:
		r.unknown[tableUsers+"."+colData] = unknownTypeMismatch
	case rowrun.CellUnknown:
		r.unknown[tableUsers+"."+colData] = rowrun.StateOf(row, c.At(colData)).String()
	}

	cn := contact.Contact{Raw: raw}
	cn.DisplayName = strings.Join(nonEmpty(names.Given, names.Family), " ")
	if cn.DisplayName != "" {
		raw["display_name_source"] = sourceComposed
	}
	if names != (contact.Names{}) {
		cn.Names = &names
	}
	if phone != "" {
		cn.Phones = []contact.Value{{Value: phone}}
	}
	if uidOK {
		id := strconv.FormatInt(uid, 10)
		cn.SourceID = id
		cn.IMs = []contact.Value{{Value: string(classUser) + ":" + id, Kind: kindTelegram}}
	}
	if username != "" {
		cn.IMs = append(cn.IMs, contact.Value{Value: username, Kind: kindTelegramUsername})
	}
	if err := joinContact(m, e, uid, uidOK, raw, r.unknown); err != nil {
		return nil, err
	}
	about, err := joinAbout(m, e, uid, uidOK, raw, r.unknown)
	if err != nil {
		return nil, err
	}
	if len(r.unknown) > 0 {
		raw["unknown_columns"] = r.unknown
	}
	return []records.Record{{
		Type: contact.Type, ArtifactID: place.ArtifactID, SourcePath: place.SourcePath, Locator: place.Locator,
		Range: &place.Range, Body: strings.Join(nonEmpty(cn.DisplayName, phone, username, about), "\n"),
		Summary: contact.Summary(cn), Payload: cn.Payload(),
	}}, nil
}

// nonEmpty are the non-empty strings of its arguments, in order.
func nonEmpty(parts ...string) []string {
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
