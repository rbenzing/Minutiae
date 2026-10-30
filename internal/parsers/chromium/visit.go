package chromium

import (
	"context"
	"errors"
	"strconv"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/recordtypes/web"
)

func urlsSpecs() []parse.ColumnSpec {
	return []parse.ColumnSpec{
		{Name: "url", Required: true, Affinity: "text"},
		{Name: "last_visit_time", Required: true, Affinity: "integer"},
		{Name: "title", Affinity: "text"},
		{Name: "visit_count", Affinity: "integer"},
		{Name: "typed_count", Affinity: "integer"},
		{Name: "hidden", Affinity: "integer"},
	}
}

func visitsSpecs() []parse.ColumnSpec {
	return []parse.ColumnSpec{
		{Name: "url", Required: true, Affinity: "integer"},
		{Name: "visit_time", Required: true, Affinity: "integer"},
		{Name: "transition", Required: true, Affinity: "integer"},
		{Name: "from_visit", Affinity: "integer"},
		{Name: "visit_duration", Affinity: "integer"},
		{Name: "opener_visit", Affinity: "integer"},
		{Name: "originator_cache_guid", Affinity: "text"},
		{Name: "originator_visit_id", Affinity: "integer"},
		{Name: "external_referrer_url", Affinity: "text"},
	}
}

func urlsMapping() parse.TableMapping {
	return parse.TableMapping{Table: "urls", Columns: urlsSpecs(), Map: mapURL}
}

func visitsMapping() parse.TableMapping {
	return parse.TableMapping{Table: "visits", Columns: visitsSpecs(), Map: mapVisit}
}

// fatal says an error from a join ends the job: a budget, a cancelled context or a failed mapping Context. Every
// other join error is an undecided answer for the row (4F-R22 as amended by 4F-R30).
func fatal(err error) bool {
	return errors.Is(err, parse.ErrBudget) || errors.Is(err, rowrun.ErrContext) ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// urlState is the outcome of looking a urls row up by rowid.
type urlState uint8

const (
	urlFound     urlState = iota
	urlAbsent             // a clean miss: no urls row has this id
	urlUndecided          // the table cannot answer (absent, unreadable, id not the rowid, other join failure)
)

// urlRow holds the cells of a urls row with their kinds, read while the row is valid.
type urlRow struct {
	row  parse.Row
	cols rowrun.Cols
	why  string // why the state is urlUndecided
}

func urlCols(m parse.MapContext) rowrun.Cols {
	return rowrun.Resolve(m, "urls", "id", "url", "title", "visit_count", "typed_count", "last_visit_time", "hidden")
}

// idIsRowid checks a urls row found by rowid: its id cell must be that rowid (4F-R30).
func idIsRowid(row parse.Row, c rowrun.Cols, rowid int64) bool {
	v, k := rowrun.IntCell(row, c.At("id"))
	return k == rowrun.CellValue && v == rowid
}

// urlOf finds the urls row with rowid id through MapContext.Get (no index, 4F-R30). Budget, context and ErrContext
// errors are returned; every other failure is urlUndecided with the reason, so a required join turns it into a row
// error and an information join into join_undecided.
func urlOf(m parse.MapContext, id int64) (urlRow, urlState, error) {
	ur := urlRow{cols: urlCols(m)}
	row, found, err := m.Get("urls", id)
	switch {
	case err != nil && fatal(err):
		return ur, urlUndecided, err
	case err != nil:
		ur.why = "the urls table cannot be read: " + err.Error()
		return ur, urlUndecided, nil
	case !found:
		return ur, urlAbsent, nil
	case !idIsRowid(row, ur.cols, id):
		ur.why = "the id column of the urls table is not the rowid"
		return ur, urlUndecided, nil
	}
	ur.row = row
	return ur, urlFound, nil
}

// usableURL is the cleaned url of the row; false when it holds no usable text.
func (u urlRow) usableURL(raw map[string]any) (string, bool) {
	b, k := rowrun.TextCell(u.row, u.cols.At("url"))
	if k != rowrun.CellValue || len(b) == 0 {
		return "", false
	}
	return capped(raw, "url", b, maxURL), true
}

// flagCell records a cell that holds no usable value. A joined cell is qualified by its table, and a column the
// variant lacks is stated as such (the joined row is not the mapped one).
func flagCell(raw, unknown map[string]any, prefix, name string, row parse.Row, col int, k rowrun.CellKind) {
	if prefix == "" {
		flagInt(raw, unknown, row, name, col, k)
		return
	}
	if k == rowrun.CellMissing {
		unknown[prefix+name] = "column-missing"
		return
	}
	flagInt(raw, unknown, row, prefix+name, col, k)
}

func (u urlRow) title(raw, unknown map[string]any, prefix string) string {
	b, k := rowrun.TextCell(u.row, u.cols.At("title"))
	if k != rowrun.CellValue {
		if k != rowrun.CellNull {
			flagCell(nil, unknown, prefix, "title", u.row, u.cols.At("title"), k)
		}
		return ""
	}
	return capped(raw, "title", b, maxText)
}

// count reads a count that must not be negative.
func (u urlRow) count(raw, unknown map[string]any, prefix, name string) *int64 {
	v, k := rowrun.IntCell(u.row, u.cols.At(name))
	switch {
	case k == rowrun.CellValue && v < 0:
		unknown[prefix+name] = "negative"
	case k == rowrun.CellValue:
		return &v
	case k != rowrun.CellNull:
		flagCell(raw, unknown, prefix, name, u.row, u.cols.At(name), k)
	}
	return nil
}

// facts puts the counts and the hidden flag of the urls row into v.
func (u urlRow) facts(v *web.Visit, raw, unknown map[string]any, prefix string) {
	v.URLVisitCount = u.count(raw, unknown, prefix, "visit_count")
	v.TypedCount = u.count(raw, unknown, prefix, "typed_count")
	h, k := rowrun.IntCell(u.row, u.cols.At("hidden"))
	switch {
	case k == rowrun.CellValue && (h == 0 || h == 1):
		b := h == 1
		v.Hidden = &b
	case k == rowrun.CellValue:
		unknown[prefix+"hidden"] = "not-boolean"
	case k != rowrun.CellNull:
		flagCell(raw, unknown, prefix, "hidden", u.row, u.cols.At("hidden"), k)
	}
}

// stamp adds raw.package.
func stamp(raw map[string]any, pkg string) {
	if pkg != "" {
		raw["package"] = pkg
	}
}

// bodyOf is web.VisitBody over the URL and title cut at maxBodyPart; a cut is counted in raw.
func bodyOf(raw map[string]any, url, title string) string {
	u, uc := cutText(url, maxBodyPart)
	t, tc := cutText(title, maxBodyPart)
	if uc || tc {
		raw["body_truncated"] = true
		raw["body_total_bytes"] = int64(len(web.VisitBody(web.Visit{URL: url, Title: title})))
	}
	return web.VisitBody(web.Visit{URL: u, Title: t})
}

// optText keeps a text cell in raw.<name>; NULL and empty text state nothing.
func optText(row parse.Row, c rowrun.Cols, name string, limit int, raw, unknown map[string]any) {
	b, k := rowrun.TextCell(row, c.At(name))
	switch {
	case k == rowrun.CellValue && len(b) > 0:
		raw[name] = capped(raw, name, b, limit)
	case k != rowrun.CellValue && k != rowrun.CellNull:
		rowrun.Flag(unknown, row, name, c.At(name), k)
	}
}

// optInt keeps an integer cell in raw.<name>.
func optInt(row parse.Row, c rowrun.Cols, name string, raw, unknown map[string]any) {
	v, k := rowrun.IntCell(row, c.At(name))
	if k == rowrun.CellValue {
		raw[name] = v
		return
	}
	flagInt(raw, unknown, row, name, c.At(name), k)
}

func mapVisit(m parse.MapContext, row parse.Row) ([]records.Record, error) {
	place, err := rowrun.Where(m, row)
	if err != nil {
		return nil, err
	}
	c := rowrun.Resolve(m, "visits", names(visitsSpecs())...)
	raw, unknown, joined := map[string]any{}, map[string]any{}, map[string]any{}

	uid, k := rowrun.IntCell(row, c.At("url"))
	if k != rowrun.CellValue {
		return nil, rowrun.Unmappable("the visit does not name a urls row: its url column is not an integer")
	}
	ur, state, err := urlOf(m, uid)
	if err != nil {
		return nil, err
	}
	switch state {
	case urlAbsent:
		return nil, rowrun.Unmappable("no urls row with id %d: the visit has no url", uid)
	case urlUndecided:
		return nil, rowrun.Unmappable("the urls row %d cannot be decided: %s", uid, ur.why)
	}
	url, ok := ur.usableURL(raw)
	if !ok {
		return nil, rowrun.Unmappable("the urls row %d holds no usable url", uid)
	}
	browser, profile, pkg := labelOf(m.Input().Primary.Logical)
	v := web.Visit{URL: url, Browser: browser, Profile: profile, Raw: raw}
	v.Title = ur.title(raw, joined, "urls.")
	ur.facts(&v, raw, joined, "urls.")
	if id, ok := row.Rowid(); ok {
		v.VisitID = strconv.FormatInt(id, 10)
	} else {
		unknown["id"] = "rowid-lost"
	}

	if tv, tk := rowrun.IntCell(row, c.At("transition")); tk == rowrun.CellValue {
		raw["transition"] = tv
		if core, quals, left, ok := decodeTransition(tv); ok {
			v.Transition, v.TransitionQualifiers = core, quals
			if left != 0 {
				raw["transition_unknown_bits"] = left
			}
		} else {
			unknown["transition"] = "out-of-range"
		}
	} else {
		flagInt(raw, unknown, row, "transition", c.At("transition"), tk)
	}
	if fv, fk := rowrun.IntCell(row, c.At("from_visit")); fk == rowrun.CellValue {
		raw["from_visit"] = fv
		if fv > 0 {
			v.Referrer = &web.Referrer{VisitID: strconv.FormatInt(fv, 10)}
		}
	} else {
		flagInt(raw, unknown, row, "from_visit", c.At("from_visit"), fk)
	}
	if dv, dk := rowrun.IntCell(row, c.At("visit_duration")); dk == rowrun.CellValue {
		raw["visit_duration"] = dv
		switch {
		case dv > maxExact:
			unknown["visit_duration"] = "precision" // T62: beyond 2^53 an integer is not a float64
		case dv > 0:
			s := float64(dv) / 1e6
			v.DurationS = &s
		case dv < 0:
			unknown["visit_duration"] = "negative"
		}
	} else {
		flagInt(raw, unknown, row, "visit_duration", c.At("visit_duration"), dk)
	}

	rec := records.Record{Type: web.VisitType, ArtifactID: place.ArtifactID, SourcePath: place.SourcePath, Locator: place.Locator, Range: &place.Range}
	if tv, tk := rowrun.IntCell(row, c.At("visit_time")); tk == rowrun.CellValue {
		in := webkit(tv)
		in.Record(raw, "visit_time")
		if in.Valid() {
			rec.Time = &records.Time{T: in.T}
		}
	} else {
		flagInt(raw, unknown, row, "visit_time", c.At("visit_time"), tk)
	}
	raw["url_id"] = uid
	optInt(row, c, "opener_visit", raw, unknown)
	optInt(row, c, "originator_visit_id", raw, unknown)
	optText(row, c, "originator_cache_guid", maxText, raw, unknown)
	optText(row, c, "external_referrer_url", maxURL, raw, unknown)
	stamp(raw, pkg)
	if len(unknown) > 0 {
		raw["unknown_columns"] = unknown
	}
	if len(joined) > 0 {
		raw["joined_unknown_columns"] = joined
	}
	if f := rowrun.DamageFlags(row); f != nil {
		raw["row_flags"] = f
	}
	rec.Body = bodyOf(raw, v.URL, v.Title)
	rec.Summary = web.VisitSummary(v)
	rec.Payload = v.Payload()
	return []records.Record{rec}, nil
}

// visitsOf finds the visits of urls row id: undecided is true when the engine cannot answer (the table or column is
// missing, the index lost rows or is over its cap, the key is undecidable); budget, context and ErrContext errors are
// returned.
func visitsOf(m parse.MapContext, id int64) (hits int, undecided bool, err error) {
	rows, und, err := rowrun.Match(m, "visits", "url", id)
	if err != nil {
		if fatal(err) {
			return 0, false, err
		}
		return 0, true, nil
	}
	return len(rows), und, nil
}

// mapURL emits a urls row only when it is visitless (a decided miss in visits) or when that cannot be decided
// (4F-R9): the record then says so and claims neither (raw.join_undecided, no raw.visitless).
func mapURL(m parse.MapContext, row parse.Row) ([]records.Record, error) {
	place, err := rowrun.Where(m, row)
	if err != nil {
		return nil, err
	}
	id, ok := row.Rowid()
	if !ok {
		return nil, rowrun.Unmappable("the urls row has lost its rowid: its visits cannot be looked up")
	}
	ur := urlRow{row: row, cols: urlCols(m)}
	if !idIsRowid(row, ur.cols, id) {
		return nil, rowrun.Unmappable("the id column of the urls table is not the rowid")
	}
	hits, undecided, err := visitsOf(m, id)
	if err != nil {
		return nil, err
	}
	if hits > 0 {
		return nil, nil // visited: the visits carry the url
	}
	raw, unknown := map[string]any{}, map[string]any{}
	url, ok := ur.usableURL(raw)
	if !ok {
		return nil, rowrun.Unmappable("the urls row holds no usable url")
	}
	browser, profile, pkg := labelOf(m.Input().Primary.Logical)
	v := web.Visit{URL: url, Browser: browser, Profile: profile, Raw: raw}
	v.Title = ur.title(raw, unknown, "")
	ur.facts(&v, raw, unknown, "")
	rec := records.Record{Type: web.VisitType, ArtifactID: place.ArtifactID, SourcePath: place.SourcePath, Locator: place.Locator, Range: &place.Range}
	c := ur.cols
	if lv, lk := rowrun.IntCell(row, c.At("last_visit_time")); lk == rowrun.CellValue {
		in := webkit(lv)
		in.Record(raw, "last_visit_time")
		if in.Valid() {
			rec.Time = &records.Time{T: in.T}
		}
	} else {
		flagInt(raw, unknown, row, "last_visit_time", c.At("last_visit_time"), lk)
	}
	if undecided {
		raw["join_undecided"] = []any{"visits"}
	} else {
		raw["visitless"] = true
	}
	stamp(raw, pkg)
	if len(unknown) > 0 {
		raw["unknown_columns"] = unknown
	}
	if f := rowrun.DamageFlags(row); f != nil {
		raw["row_flags"] = f
	}
	rec.Body = bodyOf(raw, v.URL, v.Title)
	rec.Summary = web.VisitSummary(v)
	rec.Payload = v.Payload()
	return []records.Record{rec}, nil
}
