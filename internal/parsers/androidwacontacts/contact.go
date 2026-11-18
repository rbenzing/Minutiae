package androidwacontacts

import (
	"math"
	"strings"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/decode/wajid"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/recordtypes/common"
	"github.com/rbenzing/minutiae/internal/recordtypes/contact"
)

const (
	maxText = 256 << 10 // bytes of text kept; the rest is flagged (C6)
	maxJID  = 1024      // the longest JID stated: a longer one is a counted warning, never cut
)

// The reasons of the counted warnings (spec 6.6, R2). They name columns, never device content.
const (
	reasonJIDOnly      = "wa_contacts row has no name or number (JID only)"
	reasonNameless     = "wa_contacts row has no display name, push name or number"
	reasonNameDamaged  = "wa_contacts row has no readable name or number (a name column is damaged or not text)"
	reasonNoJID        = "wa_contacts row has no readable JID"
	reasonJIDTooLong   = "wa_contacts row has a JID over 1024 bytes"
	reasonNotPersonFmt = "wa_contacts row is not a person (%s)"
	unparseable        = "unparseable"
)

// contactSpecs are the columns of wa_contacts the mapping reads (spec 6.6). Only jid is required: every other column
// is absent from some release. The rowid alias _id and the avatar column photo_ts are not read; the note
// unmapped_columns.wa_contacts lists what a variant holds beyond these.
func contactSpecs() []parse.ColumnSpec {
	return []parse.ColumnSpec{
		{Name: "jid", Required: true, Affinity: "text"},
		{Name: "is_whatsapp_user", Affinity: "integer"},
		{Name: "status", Affinity: "text"},
		{Name: "status_timestamp", Affinity: "integer"},
		{Name: "number", Affinity: "text"},
		{Name: "raw_contact_id", Affinity: "integer"},
		{Name: "display_name", Affinity: "text"},
		{Name: "phone_type", Affinity: "integer"},
		{Name: "phone_label", Affinity: "text"},
		{Name: "unseen_msg_count", Affinity: "integer"},
		{Name: "thumb_ts", Affinity: "integer"},
		{Name: "photo_id_timestamp", Affinity: "integer"},
		{Name: "given_name", Affinity: "text"},
		{Name: "family_name", Affinity: "text"},
		{Name: "wa_name", Affinity: "text"},
		{Name: "sort_name", Affinity: "text"},
		{Name: "nickname", Affinity: "text"},
		{Name: "company", Affinity: "text"},
		{Name: "title", Affinity: "text"},
	}
}

// rawAsStored are the columns kept in raw exactly as stored (raw beside interpreted), a NULL cell being absent.
func rawAsStored() []string {
	return []string{
		"is_whatsapp_user", "status", "status_timestamp", "raw_contact_id", "phone_type", "phone_label", "unseen_msg_count",
		"thumb_ts", "photo_id_timestamp", "wa_name", "sort_name", "nickname", "company", "title",
	}
}

func waContactsMapping() parse.TableMapping {
	return parse.TableMapping{Table: "wa_contacts", Columns: contactSpecs(), Map: mapContact}
}

func specNames() []string {
	specs := contactSpecs()
	out := make([]string, len(specs))
	for i, s := range specs {
		out[i] = s.Name
	}
	return out
}

// rowCells reads the cells of one row; a cell that is not usable is stated in unknown, never read as absent.
type rowCells struct {
	row     parse.Row
	cols    rowrun.Cols
	raw     map[string]any
	unknown map[string]any
}

// text reads a TEXT cell. A cell of another storage class or an unreadable one is stated in unknown; kind says which.
func (r rowCells) text(name string) ([]byte, rowrun.CellKind) {
	b, k := rowrun.TextCell(r.row, r.cols.At(name))
	rowrun.Flag(r.unknown, r.row, name, r.cols.At(name), k)
	return b, k
}

// stored returns the stored value of a cell of any storage class for raw: an integer, a cleaned text, a finite float or
// a blob marker; nil for NULL, an absent column and an unreadable cell (which is stated in unknown).
func (r rowCells) stored(name string) any {
	col := r.cols.At(name)
	if col < 0 || r.row.IsNull(col) {
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
	rowrun.Flag(r.unknown, r.row, name, col, rowrun.CellUnknown)
	return nil
}

// cleanField is common.CleanText for a text; an altered or cut text is listed in raw.text_flags under field.
func cleanField(raw map[string]any, field string, b []byte) string {
	cl := common.CleanText(b, maxText)
	if len(cl.Flags) > 0 {
		markText(raw, field)
	}
	return cl.Text
}

// markText lists field in raw.text_flags (sorted).
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

// mapContact maps one wa_contacts row. The mapping is total (spec 6.6, R2): a row is one contact record or one counted
// warning. A row is a record when its JID is a user or LID address and it has a display name, a push name or a number.
// It reads no other table, so a recovered row maps exactly as a live one.
func mapContact(m parse.MapContext, row parse.Row) ([]records.Record, error) {
	place, err := rowrun.Where(m, row)
	if err != nil {
		return nil, err
	}
	rc := rowCells{row: row, cols: rowrun.Resolve(m, "wa_contacts", specNames()...), raw: map[string]any{}, unknown: map[string]any{}}
	raw, unknown := rc.raw, rc.unknown

	// who the row names: a person's JID, never a group, a list, a status, a channel or anything else
	jb, jk := rc.text("jid")
	if jk != rowrun.CellValue {
		return nil, rowrun.Unmappable(reasonNoJID)
	}
	j, perr := wajid.Parse(string(jb))
	switch {
	case perr != nil:
		return nil, rowrun.Unmappable(reasonNotPersonFmt, unparseable)
	case j.Class != wajid.ClassUser && j.Class != wajid.ClassLID:
		return nil, rowrun.Unmappable(reasonNotPersonFmt, string(j.Class))
	case len(jb) > maxJID:
		return nil, rowrun.Unmappable(reasonJIDTooLong)
	}
	jidText := string(jb) // valid UTF-8 without NUL: wajid.Parse refused anything else

	dn, dk := rc.text("display_name")
	wn, wk := rc.text("wa_name")
	num, nk := rc.text("number")
	given, gk := rc.text("given_name")
	family, fk := rc.text("family_name")
	if len(dn) == 0 && len(wn) == 0 && len(num) == 0 {
		// no name to say who this is: a counted warning. Unknown is not absent: a name cell that is damaged or not text
		// is no statement that the row has no name.
		if dk >= rowrun.CellOther || wk >= rowrun.CellOther || nk >= rowrun.CellOther {
			return nil, rowrun.Unmappable(reasonNameDamaged)
		}
		for _, n := range []string{"given_name", "family_name", "nickname", "company", "title", "sort_name"} {
			if b, k := rowrun.TextCell(row, rc.cols.At(n)); k == rowrun.CellValue && len(b) > 0 {
				return nil, rowrun.Unmappable(reasonNameless)
			}
		}
		return nil, rowrun.Unmappable(reasonJIDOnly)
	}

	ct := contact.Contact{Raw: raw}
	switch {
	case len(dn) > 0:
		ct.DisplayName = cleanField(raw, "display_name", dn)
		raw["display_name_source"] = "address_book"
	case len(wn) > 0:
		ct.DisplayName = cleanField(raw, "display_name", wn)
		raw["display_name_source"] = "wa_name_push_name"
	}
	var names contact.Names
	if len(given) > 0 {
		names.Given = cleanField(raw, "given_name", given)
	}
	if len(family) > 0 {
		names.Family = cleanField(raw, "family_name", family)
	}
	if names != (contact.Names{}) {
		ct.Names = &names
	}
	phone := ""
	if len(num) > 0 {
		phone = cleanField(raw, "number", num)
		ct.Phones = []contact.Value{{Value: phone}} // as stored: never derived from the JID, no kind
	}
	ct.IMs = []contact.Value{{Value: jidText, Kind: "whatsapp"}}

	// raw beside interpreted
	isUser, uk := rowrun.IntCell(row, rc.cols.At("is_whatsapp_user"))
	rowrun.Flag(unknown, row, "is_whatsapp_user", rc.cols.At("is_whatsapp_user"), uk)
	for _, n := range rawAsStored() {
		if v := rc.stored(n); v != nil {
			raw[n] = v
		}
	}
	for n, k := range map[string]rowrun.CellKind{"display_name": dk, "number": nk, "given_name": gk, "family_name": fk} {
		if k == rowrun.CellOther {
			raw[n] = rc.stored(n) // the interpreted TEXT column holds another class: kept as stored
		}
	}
	if uk == rowrun.CellValue {
		if l, ok := lookupEnum(colIsWhatsAppUser, isUser); ok {
			raw["label_is_whatsapp_user"] = l
		}
	}
	raw["app"], raw["app_basis"] = classifyDB(m.Input().Primary.Logical).App, "path"
	if len(unknown) > 0 {
		raw["unknown_columns"] = unknown
	}
	if f := rowrun.DamageFlags(row); f != nil {
		raw["row_flags"] = f
	}

	// body: the display name, the number, the JID, the push name when it differs, and the About text
	lines := []string{ct.DisplayName, phone, jidText}
	if len(wn) > 0 {
		if w := common.CleanText(wn, maxText).Text; w != ct.DisplayName {
			lines = append(lines, w)
		}
	}
	if sb, k := rowrun.TextCell(row, rc.cols.At("status")); k == rowrun.CellValue && len(sb) > 0 {
		lines = append(lines, common.CleanText(sb, maxText).Text)
	}
	kept := lines[:0]
	for _, l := range lines {
		if l != "" {
			kept = append(kept, l)
		}
	}
	// SourcePath is the remote path of the file that holds the cell (ruling T48). Record.Time stays unset: wa_contacts
	// holds no per-contact modification time (spec 6.6); the status and avatar times stay raw.
	rec := records.Record{Type: contact.Type, ArtifactID: place.ArtifactID, SourcePath: place.SourcePath, Locator: place.Locator, Range: &place.Range}
	rec.Body = strings.Join(kept, "\n")
	rec.Summary = contact.Summary(ct)
	rec.Payload = ct.Payload()
	return []records.Record{rec}, nil
}
