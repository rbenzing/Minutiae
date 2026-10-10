package dbrecover

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// inputRef is one input of a run (the database or a companion) as the mapper
// needs it.
type inputRef struct {
	Role, ArtifactID, SHA256 string // Role: "db" | "wal" | "journal"
	Size                     int64
	Recovered, Incomplete    bool
	Cap                      *int
	CapUnknown               bool   // recovered, yet no confidence cap can be read from its trail (J116)
	Unused                   string // "" or the reason a discovered companion was not applied (D9)
}

// mapper turns the recovered rows of one run into never-live db_row records.
type mapper struct {
	byFile                     map[sqlitefile.FileKind]inputRef
	cap                        *int // minimum confidence ceiling over the whole input set (D2)
	mixed                      bool
	capUnknown                 bool // an input is recovered with an unknown cap: rows carry the flag (J116)
	incomplete                 bool
	pairingWAL, pairingJournal string
	maxPayload                 int64
	maxBody                    int64
}

// Defaults of the per-row bounds (D12).
const (
	defaultMaxPayload = 8 << 20
	defaultMaxBody    = 256 << 10
	summaryMaxBytes   = 512
	summaryTextRunes  = 40
	payloadSlack      = 512 // room for what is added to the payload after the columns are sized
)

var errNoArtifactForFile = errors.New("dbrecover: no artifact for the file a row lies in")

// assertRecovered is the never-live gate: a record of this package always
// states how it was recovered, is flagged deleted and carries a confidence.
func assertRecovered(r records.Record) error {
	switch {
	case r.Recovery == "":
		return errors.New("dbrecover: a mapped row has no recovery method (it would be live)")
	case !r.Deleted:
		return errors.New("dbrecover: a mapped row is not flagged deleted")
	case r.Confidence == nil:
		return errors.New("dbrecover: a mapped row has no confidence")
	}
	return nil
}

func fileName(k sqlitefile.FileKind) string {
	switch k {
	case sqlitefile.FileWAL:
		return "wal"
	case sqlitefile.FileJournal:
		return "journal"
	}
	return "db"
}

// sanitize replaces NUL and invalid UTF-8 by U+FFFD and, when oneLine is set,
// every other control character by a space.
func sanitize(s string, oneLine bool) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && n <= 1, r == 0:
			b.WriteRune(utf8.RuneError)
		case oneLine && unicode.IsControl(r):
			b.WriteByte(' ')
		default:
			b.WriteString(s[i : i+n])
		}
		i += n
	}
	return b.String()
}

// cutBytes cuts s to at most n bytes on a rune boundary.
func cutBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// summaryValue is the short rendering of one value in the summary.
func summaryValue(v sqlitefile.Value) string {
	switch {
	case v.Unread:
		return "<unread>"
	case v.Omitted:
		return "<omitted>"
	}
	switch v.Kind {
	case sqlitefile.KindInt:
		return strconv.FormatInt(v.Int, 10)
	case sqlitefile.KindFloat:
		return strconv.FormatFloat(v.Float, 'g', -1, 64)
	case sqlitefile.KindText:
		s, ok := v.Text()
		if !ok {
			return "<text " + strconv.FormatInt(v.Len, 10) + ">"
		}
		s = sanitize(s, true)
		if utf8.RuneCountInString(s) > summaryTextRunes {
			s = string([]rune(s)[:summaryTextRunes]) + "..."
		}
		if v.Clipped {
			s += " [clipped]"
		}
		return s
	case sqlitefile.KindBlob:
		return "<blob " + strconv.FormatInt(v.Len, 10) + ">"
	}
	return "NULL"
}

func summaryOf(r sqlitefile.RecoveredRow, unread int, clipped, lenMismatch bool) string {
	var head string
	if r.Index != "" {
		head = "index " + sanitize(r.Index, true) + ":"
	} else {
		label, rowid := "?", "?"
		if r.TableBasis == sqlitefile.BasisSchema && r.Table != "" {
			label = sanitize(r.Table, true)
		}
		if r.Rowid != nil {
			rowid = strconv.FormatInt(*r.Rowid, 10)
		}
		head = label + " #" + rowid + ":"
	}
	var marks string
	if unread > 0 {
		marks += " [unread]"
	}
	if clipped {
		marks += " [clipped]"
	}
	if lenMismatch {
		marks += " [len-mismatch]"
	}
	room := summaryMaxBytes - len(marks)
	var b strings.Builder
	b.WriteString(head)
	for i, v := range r.Values {
		if b.Len() >= room {
			break
		}
		if i > 0 {
			b.WriteString(" |")
		}
		b.WriteString(" " + summaryValue(v))
	}
	return cutBytes(b.String(), room) + marks
}

// bodyOf joins the text columns whose text is exact (valid and without NUL),
// cut at limit bytes.
func bodyOf(vals []sqlitefile.Value, limit int64) (string, bool) {
	var b strings.Builder
	for _, v := range vals {
		s, ok := v.Text()
		if !ok || strings.ContainsRune(s, 0) {
			continue
		}
		// a clipped value is never presented as the whole value (J41): the
		// mark is inline and separate from body_clipped (J42)
		if v.Clipped {
			s += " [clipped]"
		}
		piece := s
		if b.Len() > 0 {
			piece = "\n" + s
		}
		if int64(b.Len()+len(piece)) > limit {
			b.WriteString(cutBytes(piece, int(limit)-b.Len()))
			return b.String(), true
		}
		b.WriteString(piece)
	}
	return b.String(), false
}

// rangeOf is the byte range of the cell in its artifact, or false when the
// range is empty or does not lie inside the artifact.
func rangeOf(loc sqlitefile.Loc, size int64) (records.Range, bool) {
	if loc.Offset < 0 || loc.Length <= 0 || size < loc.Length || loc.Offset > size-loc.Length {
		return records.Range{}, false
	}
	return records.Range{Offset: loc.Offset, Length: loc.Length}, true
}

func pageParts(parts []sqlitefile.PagePart) []any {
	out := make([]any, 0, len(parts))
	for _, p := range parts {
		e := map[string]any{"page": int64(p.Page), "file": fileName(p.At.File), "offset": p.At.Offset}
		switch p.At.File {
		case sqlitefile.FileWAL:
			e["frame"] = int64(p.At.Frame)
		case sqlitefile.FileJournal:
			e["record"] = int64(p.At.Record)
		}
		out = append(out, e)
	}
	return out
}

// record maps one recovered row to a never-live db_row record.
func (m *mapper) record(r sqlitefile.RecoveredRow) (records.Record, error) {
	ref, ok := m.byFile[r.Loc.File]
	if !ok {
		return records.Record{}, fmt.Errorf("%w (%s)", errNoArtifactForFile, fileName(r.Loc.File))
	}
	maxPayload, maxBody := m.maxPayload, m.maxBody
	if maxPayload <= 0 {
		maxPayload = defaultMaxPayload
	}
	if maxBody <= 0 {
		maxBody = defaultMaxBody
	}
	base := sqlitefile.Confidence(r)
	conf := sqlitefile.CapConfidence(base, m.cap)

	notes := slices.Clone(r.Notes)
	region := r.SlackRegion
	if region == "" {
		region = "unknown"
	}
	addNote := func(n string) {
		if !slices.Contains(notes, n) {
			notes = append(notes, n)
		}
	}
	rec := records.Record{
		Type: TypeName, ArtifactID: ref.ArtifactID, Deleted: true, Recovery: r.Method, Confidence: &conf,
		Locator: locatorFor(r),
	}
	if m.capUnknown {
		addNote("recovery-cap-unknown")
	}
	outside := false
	if rg, ok := rangeOf(r.Loc, ref.Size); ok {
		rec.Range = &rg
	} else {
		addNote("range-outside-artifact")
		outside = true
	}

	var unread, clipped int64
	for _, v := range r.Values {
		if v.Unread {
			unread++
		}
		if v.Clipped {
			clipped++
		}
	}
	p := map[string]any{
		"entry_kind": "table", "table_basis": string(r.TableBasis), "relation": string(r.Relation),
		"file": fileName(r.Loc.File), "origin": r.Origin.String(), "page": int64(r.Loc.Page),
		"method_confidence": int64(base),
	}
	if r.Rowid != nil {
		p["rowid"] = *r.Rowid
	} else {
		p["rowid"] = nil
	}
	if r.Index != "" {
		p["entry_kind"] = "index"
		encodeName("index", r.Index, p)
	} else if r.Table != "" {
		encodeName("table", r.Table, p)
	}
	if outside {
		// the claim is provenance even when it cannot be a Range (J39)
		p["loc_offset"], p["loc_length"] = r.Loc.Offset, r.Loc.Length
	}
	if r.Loc.Cell >= 0 {
		p["cell"] = int64(r.Loc.Cell)
	} else {
		p["carved"] = map[string]any{
			"span_kind": r.Origin.String(), "span_offset": r.Loc.Offset - r.Loc.PageOffset, "span_length": r.Loc.Length, "slack_region": region,
		}
	}
	if r.WAL != nil {
		w := map[string]any{
			"frame": int64(r.WAL.Frame), "salt1": int64(r.WAL.Salt1), "salt2": int64(r.WAL.Salt2), "state": r.WAL.State.String(),
			"committed": r.WAL.Committed, "linked": r.WAL.Linked, "generation": int64(r.WAL.Generation),
		}
		if r.WAL.Note != "" {
			w["note"] = r.WAL.Note
		}
		p["wal"] = w
	}
	if r.Journal != nil {
		j := map[string]any{
			"record": int64(r.Journal.Record), "segment": int64(r.Journal.Segment), "checksum_ok": r.Journal.ChecksumOK,
			"hot": r.Journal.Hot, "applied": r.Journal.Applied, "nonce": int64(r.Journal.Nonce),
		}
		if r.Journal.Note != "" {
			j["note"] = r.Journal.Note
		}
		p["journal"] = j
	}
	if r.Loc.OverflowTotal > 0 || len(r.Loc.Overflow) > 0 {
		p["overflow"] = map[string]any{
			"total": int64(r.Loc.OverflowTotal), "mixed": r.Loc.OverflowMixed, "pages": pageParts(r.Loc.Overflow),
		}
	}
	if r.OverflowHead != 0 {
		p["overflow_head"] = int64(r.OverflowHead)
	}
	if r.Truncated {
		p["truncated"] = true
	}
	if m.cap != nil {
		p["confidence_cap"] = int64(*m.cap)
	}
	var siblings, unused []any
	for _, k := range []sqlitefile.FileKind{sqlitefile.FileDB, sqlitefile.FileWAL, sqlitefile.FileJournal} {
		o, has := m.byFile[k]
		if !has || k == r.Loc.File {
			continue
		}
		siblings = append(siblings, o.ArtifactID)
		if o.Unused != "" {
			unused = append(unused, map[string]any{"id": o.ArtifactID, "role": o.Role, "reason": o.Unused})
		}
	}
	if siblings != nil {
		p["sibling_artifacts"] = siblings
	}
	if m.pairingWAL != "" {
		p["sibling_pairing_wal"] = m.pairingWAL
	}
	if m.pairingJournal != "" {
		p["sibling_pairing_journal"] = m.pairingJournal
	}
	if unused != nil {
		p["sibling_unused"] = unused
	}
	if m.mixed {
		p["mixed_inputs"] = true
	}
	if m.incomplete {
		p["input_incomplete"] = true
	}
	if unread > 0 {
		p["values_unread"] = unread
		addNote("value-unread")
	}

	// size the columns against what is left of the payload allowance
	rest := maps0(p)
	rest["notes"] = toAny(notes)
	restSize, err := encodedSize(rest)
	if err != nil {
		return records.Record{}, err
	}
	allowance := maxPayload - restSize - payloadSlack
	cols, err := encodeColumns(r.Values, allowance)
	if err != nil {
		return records.Record{}, err
	}
	var omitted, budget int64
	for _, c := range cols {
		cm := c.(map[string]any)
		if cm["omitted"] != nil {
			omitted++
		}
		if cm["omitted"] == "payload-budget" {
			budget++
		}
	}
	p["columns"] = cols
	if omitted > 0 {
		p["values_omitted"] = omitted
		addNote("value-omitted")
	}
	if clipped > 0 {
		p["values_clipped"] = clipped
		addNote("value-clipped")
	}
	if budget > 0 {
		addNote("payload-budget")
	}
	if len(notes) > 0 {
		p["notes"] = toAny(notes)
	}

	body, bodyClipped := bodyOf(r.Values, maxBody)
	rec.Body = body
	if bodyClipped {
		p["body_clipped"] = true
	}
	rec.Summary = summaryOf(r, int(unread), clipped > 0, slices.Contains(r.Notes, sqlitefile.NoteRecordLengthMismatch))
	rec.Payload = p
	if err := assertRecovered(rec); err != nil {
		return records.Record{}, err
	}
	return rec, nil
}

func maps0(p map[string]any) map[string]any {
	out := make(map[string]any, len(p)+1)
	for k, v := range p {
		out[k] = v
	}
	return out
}

func toAny(s []string) []any {
	out := make([]any, len(s))
	for i, v := range s {
		out[i] = v
	}
	return out
}
