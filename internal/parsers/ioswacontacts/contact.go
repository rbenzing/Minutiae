package ioswacontacts

import (
	"math"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/decode/wajid"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/recordtypes/common"
	"github.com/rbenzing/minutiae/internal/recordtypes/contact"
)

const (
	maxText = 256 << 10 // bytes of text kept; the rest is flagged (C6)
	maxJID  = 1024      // the longest JID stated: a longer one is too-long, never cut (the message contract's maxAddr)
)

// The table the mapping reads, the columns that decide a row and the prefix under which one of their unreadable cells is listed in
// raw.unknown_columns (4I-R22: "<table>.<column>").
const (
	tableContact  = "ZWAADDRESSBOOKCONTACT"
	colWhatsAppID = "ZWHATSAPPID"
	colFullName   = "ZFULLNAME"
	colPhone      = "ZPHONENUMBER"
	colGiven      = "ZGIVENNAME"
	colHighlight  = "ZHIGHLIGHTEDNAME"
	colLID        = "ZLID"
	prefixContact = tableContact + "."
)

// The reasons of the counted warnings (plan 4I2 "State vocabularies", PF16). They name the table, never device content.
const (
	reasonNotPersonFmt = tableContact + " row is not a person (%s)"
	reasonJIDOnly      = tableContact + " row has no name or number (JID only)"
	reasonJIDUndecided = tableContact + " row has no name or number (JID undecided)"
	reasonNoFullName   = tableContact + " row has no full name or number"
	reasonNameDamaged  = tableContact + " row has no readable name or number (a name column is damaged or not text)"
	reasonNoIdentity   = tableContact + " row has no name, number or JID"
	unparseable        = "unparseable"
)

// env is what Parse hands the mapping: the place of the database and its app label. It is nil in Mappings(), the typed recovery
// pass, which has no Parse context (4I2-P8, PF5): the mapping then derives the label from the Input of its MapContext.
type env struct {
	Place dbPlace
	Label appLabel
}

// contactSpecs is the ZWAADDRESSBOOKCONTACT column list. The Required columns are the ones Probe insists on (spec 6.11
// signature); every other column is absent from some release or image (ZLID on some images, S7), so its absence is handled per
// row: a missing name or number column is unreadable, never absent (PF16).
func contactSpecs() []parse.ColumnSpec {
	return []parse.ColumnSpec{
		{Name: "Z_PK", Required: true, Affinity: "integer"},
		{Name: colWhatsAppID, Required: true, Affinity: "text"},
		{Name: "Z_ENT", Affinity: "integer"},
		{Name: colFullName, Affinity: "text"},
		{Name: colGiven, Affinity: "text"},
		{Name: colHighlight, Affinity: "text"},
		{Name: colPhone, Affinity: "text"},
		{Name: "ZPHONENUMBERLABEL", Affinity: "text"},
		{Name: "ZABOUTTEXT", Affinity: "text"},
		{Name: "ZABOUTTIMESTAMP", Affinity: "real"},
		{Name: "ZIDENTIFIER", Affinity: "text"},
		{Name: colLID, Affinity: "text"},
	}
}

func specNames(specs []parse.ColumnSpec) []string {
	out := make([]string, len(specs))
	for i, s := range specs {
		out[i] = s.Name
	}
	return out
}

// contactMappingWith maps ZWAADDRESSBOOKCONTACT with the environment e (nil: the typed recovery pass): a closure over an immutable
// value, so the mapper stays a pure function of the row and its context.
func contactMappingWith(e *env) parse.TableMapping {
	names := specNames(contactSpecs())
	return parse.TableMapping{Table: tableContact, Columns: contactSpecs(), Map: func(m parse.MapContext, row parse.Row) ([]records.Record, error) {
		return mapContact(m, row, e, names)
	}}
}

// rowLabel is the app label of the rows of this mapping: Parse's when it has one, else derived from the place (PF5): a domain
// names its app; a recovered row of an app group states "unknown" and "not-evaluated" (4I2-P8).
func rowLabel(m parse.MapContext, e *env) appLabel {
	if e != nil {
		return e.Label
	}
	if p := classifyPlace(m.Input().Primary.Logical); p.Place == placeDomain {
		return appLabel{App: p.App, Basis: "backup_domain"}
	}
	return appLabel{App: "unknown", State: "not-evaluated"}
}

// cellReader reads the cells of one row into raw and unknown (payload.raw and payload.raw.unknown_columns); a cell that holds no
// usable value is stated in unknown, never read as empty or absent.
type cellReader struct {
	row     parse.Row
	cols    rowrun.Cols
	raw     map[string]any
	unknown map[string]any
}

// nanCell says a cell is a stored REAL NaN: the reader (as the engine) reads it as NULL but keeps the serial type 7, so it is
// detectable here and is never read as absent (unknown is never absent).
func nanCell(row parse.Row, col int) bool {
	r, ok := row.(sqlitedb.Row)
	return ok && col >= 0 && r.State(col) == sqlitedb.StateNull && r.Value(col).Serial == 7
}

// peekText reads a TEXT cell without stating it. A stored NaN is a value of another class, not a NULL.
func (r cellReader) peekText(name string) ([]byte, rowrun.CellKind) {
	col := r.cols.At(name)
	b, k := rowrun.TextCell(r.row, col)
	if k == rowrun.CellNull && nanCell(r.row, col) {
		k = rowrun.CellOther
	}
	return b, k
}

// text reads a TEXT cell; the bytes may be empty (a stored empty string is a value). A cell of another class or an unreadable one
// is stated in unknown.
func (r cellReader) text(name string) ([]byte, rowrun.CellKind) {
	b, k := r.peekText(name)
	rowrun.Flag(r.unknown, r.row, prefixContact+name, r.cols.At(name), k)
	return b, k
}

// int reads an INTEGER cell.
func (r cellReader) int(name string) (int64, bool) {
	col := r.cols.At(name)
	v, k := rowrun.IntCell(r.row, col)
	if k == rowrun.CellNull && nanCell(r.row, col) {
		k = rowrun.CellOther
	}
	rowrun.Flag(r.unknown, r.row, prefixContact+name, col, k)
	return v, k == rowrun.CellValue
}

// stored returns the stored value of a cell of any storage class for raw: an integer, a cleaned text, a finite float, the string
// "non-finite", or a blob marker; nil for NULL and an absent column. An unreadable cell is stated in unknown.
func (r cellReader) stored(name string) any {
	col := r.cols.At(name)
	if col < 0 {
		return nil
	}
	if nanCell(r.row, col) {
		return "non-finite"
	}
	if !rowrun.StateOf(r.row, col).Known() {
		rowrun.Flag(r.unknown, r.row, prefixContact+name, col, rowrun.CellUnknown)
		return nil
	}
	if r.row.IsNull(col) {
		return nil
	}
	if v, ok := r.row.Int(col); ok {
		return v
	}
	if b, ok := r.row.Text(col); ok {
		return cleanField(r.raw, name, b)
	}
	if f, ok := r.row.Float(col); ok {
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return "non-finite"
		}
		return f
	}
	if b, ok := r.row.Blob(col); ok {
		return blobValue(b)
	}
	return nil
}

// usable says a cell holds text with at least one byte.
func usable(b []byte, k rowrun.CellKind) bool { return k == rowrun.CellValue && len(b) > 0 }

// unreadable says a cell could not be read as text: its column is missing from the table, it holds another class or the reader
// cannot vouch for it. Unknown is not absent (PF16).
func unreadable(k rowrun.CellKind) bool { return k == rowrun.CellMissing || k >= rowrun.CellOther }

// cleanField is common.CleanText for a free-text field; an altered or cut text is listed in raw.text_flags under field.
func cleanField(raw map[string]any, field string, b []byte) string {
	cl := common.CleanText(b, maxText)
	if len(cl.Flags) > 0 {
		markText(raw, field)
	}
	return cl.Text
}

// jidCell is the decision on the ZWHATSAPPID cell: State is "" for a JID that is TEXT, whole and unchanged by cleaning (Text,
// Class), else the plan's state: absent (NULL or empty), altered (cleaning changed it: Text is the cleaned text, never parsed),
// too-long (over the contract's limit, checked first: no text kept) or not-text (another class or an unreadable cell).
type jidCell struct{ State, Text, Class string }

func readJID(r cellReader) jidCell {
	b, k := r.text(colWhatsAppID)
	switch {
	case k == rowrun.CellNull:
		return jidCell{State: "absent"}
	case k != rowrun.CellValue:
		return jidCell{State: "not-text"} // another class, an unreadable cell or (impossible for a required column) a missing one
	case len(b) == 0:
		return jidCell{State: "absent"}
	}
	cl := common.CleanText(b, maxText)
	switch {
	case len(cl.Text) > maxJID || cl.Truncated: // before altered: an over-long JID keeps no text
		return jidCell{State: "too-long"}
	case len(cl.Flags) > 0:
		return jidCell{State: "altered", Text: cl.Text}
	}
	class := unparseable
	if j, err := wajid.Parse(cl.Text); err == nil {
		class = string(j.Class)
	}
	return jidCell{Text: cl.Text, Class: class}
}

func (j jidCell) person() bool {
	return j.Class == string(wajid.ClassUser) || j.Class == string(wajid.ClassLID)
}

// mapContact maps one ZWAADDRESSBOOKCONTACT row. The mapping is total (4I2-R10, spec 6.11): a row is one contact record or one
// counted warning. The JID's class is decided first, on the stored bytes: a decided JID that is no user or LID address makes the
// row a non-person, whatever its names. A row is a record when it then has a full name or a number; otherwise the warning says
// why, and a name or number column that is missing, not text or unreadable is never read as "no name" (PF16). It reads no other
// table, so a recovered row maps exactly as a live one. Two rows with one JID are two records: nothing is merged or looked up.
func mapContact(m parse.MapContext, row parse.Row, e *env, names []string) ([]records.Record, error) {
	place, err := rowrun.Where(m, row)
	if err != nil {
		return nil, err
	}
	cols := rowrun.Resolve(m, tableContact, names...)
	if e != nil && cols.At(colLID) < 0 {
		m.Note(colLID, "absent") // PF16(a): the Android jid_map=absent analogue; no record of this store can state a LID
	}
	raw, unknown := map[string]any{}, map[string]any{}
	r := cellReader{row: row, cols: cols, raw: raw, unknown: unknown}

	jid := readJID(r)
	if jid.State == "" && !jid.person() {
		return nil, rowrun.Unmappable(reasonNotPersonFmt, jid.Class)
	}
	full, fk := r.text(colFullName)
	phone, pk := r.text(colPhone)
	given, gk := r.text(colGiven)
	_, hk := r.peekText(colHighlight)
	hasFull, hasPhone := usable(full, fk), usable(phone, pk)
	if !hasFull && !hasPhone {
		if unreadable(fk) || unreadable(pk) || unreadable(gk) || unreadable(hk) {
			return nil, rowrun.Unmappable(reasonNameDamaged)
		}
		if hb, _ := r.peekText(colHighlight); usable(given, gk) || len(hb) > 0 {
			return nil, rowrun.Unmappable(reasonNoFullName)
		}
		switch jid.State {
		case "absent":
			return nil, rowrun.Unmappable(reasonNoIdentity)
		case "":
			return nil, rowrun.Unmappable(reasonJIDOnly) // a decided person's JID (user or lid)
		}
		return nil, rowrun.Unmappable(reasonJIDUndecided) // altered, too-long or not-text: no class is decided

	}

	rowLabel(m, e).raw(raw)
	ct := contact.Contact{Raw: raw}
	if hasFull {
		ct.DisplayName = cleanField(raw, colFullName, full)
	}
	if usable(given, gk) {
		ct.Names = &contact.Names{Given: cleanField(raw, colGiven, given)}
	}
	labelB, lk := r.text("ZPHONENUMBERLABEL")
	label := ""
	if usable(labelB, lk) {
		label = cleanField(raw, "ZPHONENUMBERLABEL", labelB)
	}
	switch {
	case hasPhone:
		v := contact.Value{Value: cleanField(raw, colPhone, phone), Label: label}
		if x, ok := wrapped(label); ok {
			v.Label = x
			raw["phone_label_stored"] = label
		}
		ct.Phones = []contact.Value{v}
	case label != "":
		raw["ZPHONENUMBERLABEL"] = label // no phone to hold it: kept as stored, never dropped
	}
	if ib, ik := r.text("ZIDENTIFIER"); usable(ib, ik) {
		ct.SourceID = cleanField(raw, "ZIDENTIFIER", ib)
	}
	about := ""
	if ab, ak := r.text("ZABOUTTEXT"); usable(ab, ak) {
		about = cleanField(raw, "ZABOUTTEXT", ab)
	}

	if jid.State == "" {
		ct.IMs = []contact.Value{{Value: jid.Text, Kind: "whatsapp"}}
	} else {
		raw["whatsapp_id_state"] = jid.State
		switch jid.State {
		case "altered":
			raw["whatsapp_id_text"] = jid.Text
			markText(raw, "whatsapp_id_text")
		case "not-text":
			if v := r.stored(colWhatsAppID); v != nil {
				raw[colWhatsAppID] = v // as stored: an integer, a float or a blob marker (unknown is never absent)
			}
		}
	}
	lidState(r, jid)

	if v, ok := r.int("Z_ENT"); ok {
		raw["Z_ENT"] = v
	}
	for _, n := range []string{colHighlight, "ZABOUTTIMESTAMP"} { // raw as stored; ZABOUTTIMESTAMP is no time of the contact
		if v := r.stored(n); v != nil {
			raw[n] = v
		}
	}
	if len(unknown) > 0 {
		raw["unknown_columns"] = unknown
	}
	if f := rowrun.DamageFlags(row); f != nil {
		raw["row_flags"] = f
	}

	// SourcePath is the remote path of the file that holds the cell (ruling T48). Record.Time stays unset: ContactsV2 holds no
	// per-contact modification time the spec establishes (ZABOUTTIMESTAMP stays raw).
	rec := records.Record{Type: contact.Type, ArtifactID: place.ArtifactID, SourcePath: place.SourcePath, Locator: place.Locator, Range: &place.Range}
	rec.Body = contact.Body(ct)
	if about != "" {
		if rec.Body != "" {
			rec.Body += "\n"
		}
		rec.Body += about
	}
	rec.Summary = contact.Summary(ct)
	rec.Payload = ct.Payload()
	return []records.Record{rec}, nil
}

// lidState states what the row's own ZLID says (D6): raw.lid_map when it is clean text and the row's JID is a decided user
// (phone) JID, else, with a non-empty ZLID, raw.lid_state (amendment 4I2-A5): "jid-undecided" for a JID that is altered,
// too-long or not text, "jid-not-a-phone" for a decided JID of another class or an absent one. Whenever no lid_map is written
// and the LID is not too long, raw.ZLID keeps it as stored (cleaned, listed in text_flags when cleaning changed it): its format
// is not established. Nothing is inferred from the JID, and no other row is consulted.
func lidState(r cellReader, jid jidCell) {
	b, k := r.text(colLID)
	switch {
	case k == rowrun.CellNull || k == rowrun.CellMissing || (k == rowrun.CellValue && len(b) == 0):
		return
	case k != rowrun.CellValue:
		r.raw["lid_state"] = "not-text"
		if v := r.stored(colLID); v != nil {
			r.raw[colLID] = v
		}
		return
	}
	cl := common.CleanText(b, maxText)
	switch {
	case len(cl.Text) > maxJID || cl.Truncated:
		r.raw["lid_state"] = "too-long" // no text kept: it would be over the contract's limit
		return
	case len(cl.Flags) > 0:
		r.raw["lid_state"] = "altered"
	case jid.State == "" && jid.Class == string(wajid.ClassUser):
		r.raw["lid_map"] = []any{map[string]any{"lid": cl.Text, "jid": jid.Text, "source": tableContact + "." + colLID}}
		return
	case jid.State == "altered" || jid.State == "too-long" || jid.State == "not-text":
		r.raw["lid_state"] = "jid-undecided"
	default:
		r.raw["lid_state"] = "jid-not-a-phone"
	}
	r.raw[colLID] = cleanField(r.raw, colLID, b)
}

// wrapped returns X for a system label of the form _$!<X>!$_ with a non-empty X (the rule of ios.addressbook, restated).
func wrapped(label string) (string, bool) {
	const pre, post = "_$!<", ">!$_"
	if len(label) > len(pre)+len(post) && label[:len(pre)] == pre && label[len(label)-len(post):] == post {
		return label[len(pre) : len(label)-len(post)], true
	}
	return "", false
}

// markText lists field in raw.text_flags (sorted): its text was altered or cut. The original bytes stay in the artifact.
func markText(raw map[string]any, field string) {
	cur, _ := raw["text_flags"].([]any)
	for _, f := range cur {
		if f == field {
			return
		}
	}
	cur = append(cur, field)
	for i := len(cur) - 1; i > 0 && cur[i].(string) < cur[i-1].(string); i-- {
		cur[i], cur[i-1] = cur[i-1], cur[i]
	}
	raw["text_flags"] = cur
}

// blobValue is a BLOB as raw shows it: the bytes in base64 when short, else only their length.
func blobValue(b []byte) map[string]any {
	if len(b) <= 256 {
		return map[string]any{"blob_b64": base64(b)}
	}
	return map[string]any{"blob_len": int64(len(b))}
}

const b64Alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"

// base64 is standard padded base64 (encoding/base64 is not importable by a parser; a blob marker is its only use).
func base64(b []byte) string {
	out := make([]byte, 0, (len(b)+2)/3*4)
	for i := 0; i < len(b); i += 3 {
		var n uint32
		k := min(3, len(b)-i)
		for j := 0; j < 3; j++ {
			n <<= 8
			if j < k {
				n |= uint32(b[i+j])
			}
		}
		out = append(out, b64Alphabet[n>>18&63], b64Alphabet[n>>12&63])
		if k > 1 {
			out = append(out, b64Alphabet[n>>6&63])
		} else {
			out = append(out, '=')
		}
		if k > 2 {
			out = append(out, b64Alphabet[n&63])
		} else {
			out = append(out, '=')
		}
	}
	return string(out)
}
