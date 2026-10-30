package chromium

import (
	"encoding/hex"
	"strings"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/recordtypes/web"
)

func downloadSpecs() []parse.ColumnSpec {
	return []parse.ColumnSpec{
		{Name: "id", Required: true, Affinity: "integer"},
		{Name: "target_path", Required: true, Affinity: "text"},
		{Name: "start_time", Required: true, Affinity: "integer"},
		{Name: "current_path", Affinity: "text"},
		{Name: "guid", Affinity: "text"},
		{Name: "received_bytes", Affinity: "integer"},
		{Name: "total_bytes", Affinity: "integer"},
		{Name: "state", Affinity: "integer"},
		{Name: "danger_type", Affinity: "integer"},
		{Name: "interrupt_reason", Affinity: "integer"},
		{Name: "hash", Affinity: "blob"},
		{Name: "end_time", Affinity: "integer"},
		{Name: "opened", Affinity: "integer"},
		{Name: "last_access_time", Affinity: "integer"},
		{Name: "transient", Affinity: "integer"},
		{Name: "referrer", Affinity: "text"},
		{Name: "site_url", Affinity: "text"},
		{Name: "tab_url", Affinity: "text"},
		{Name: "tab_referrer_url", Affinity: "text"},
		{Name: "http_method", Affinity: "text"},
		{Name: "mime_type", Affinity: "text"},
		{Name: "original_mime_type", Affinity: "text"},
	}
}

func downloadsMapping() parse.TableMapping {
	return parse.TableMapping{Table: "downloads", Columns: downloadSpecs(), Map: mapDownload}
}

// optPayloadText reads a text cell that goes into the typed payload (cleaned and capped); an unusable class is stated
// in unknown_columns, NULL and empty text state nothing.
func optPayloadText(row parse.Row, c rowrun.Cols, col, field string, limit int, raw, unknown map[string]any) string {
	b, k := rowrun.TextCell(row, c.At(col))
	switch {
	case k == rowrun.CellValue && len(b) > 0:
		return capped(raw, field, b, limit)
	case k != rowrun.CellValue && k != rowrun.CellNull:
		rowrun.Flag(unknown, row, col, c.At(col), k)
	}
	return ""
}

// enumCell reads an enum column: the stored integer goes to raw and its name (or "unknown") to the payload.
func enumCell(row parse.Row, c rowrun.Cols, col string, name func(int64) string, raw, unknown map[string]any) string {
	v, k := rowrun.IntCell(row, c.At(col))
	if k != rowrun.CellValue {
		flagInt(raw, unknown, row, col, c.At(col), k)
		return ""
	}
	raw[col] = v
	return name(v)
}

// hashCell keeps a non-empty blob as lowercase hex in raw.hash.
func hashCell(row parse.Row, c rowrun.Cols, raw, unknown map[string]any) {
	col := c.At("hash")
	if col < 0 {
		return
	}
	if st := rowrun.StateOf(row, col); !st.Known() {
		unknown["hash"] = st.String()
		return
	}
	if row.IsNull(col) {
		return
	}
	if b, ok := row.Blob(col); ok {
		if len(b) > 0 {
			raw["hash"] = hex.EncodeToString(b)
		}
		return
	}
	if b, ok := row.Text(col); ok && len(b) == 0 {
		return
	}
	unknown["hash"] = "type-mismatch"
}

// downloadBody is the searchable body: the target path, then each chain URL on its own line, each part cut at
// maxBodyPart and the whole at maxBodyTotal (4F-R26); a cut is counted in raw.
func downloadBody(raw map[string]any, parts []string) string {
	cut := false
	cuts := make([]string, len(parts))
	for i, p := range parts {
		var c bool
		cuts[i], c = cutText(p, maxBodyPart)
		cut = cut || c
	}
	body := strings.Join(cuts, "\n")
	if len(body) > maxBodyTotal {
		body, _ = cutText(body, maxBodyTotal)
		cut = true
	}
	if cut {
		raw["body_truncated"] = true
		raw["body_total_bytes"] = int64(len(strings.Join(parts, "\n")))
	}
	return body
}

// mapDownload gives one download per row (4F-R11): the chain follows chain_index, the enum columns are named from the
// cross-check, and a row with neither a usable target_path nor a chain URL is a row error.
func mapDownload(m parse.MapContext, row parse.Row) ([]records.Record, error) {
	place, err := rowrun.Where(m, row)
	if err != nil {
		return nil, err
	}
	c := rowrun.Resolve(m, "downloads", names(downloadSpecs())...)
	raw, unknown := map[string]any{}, map[string]any{}
	browser, profile, pkg := labelOf(m.Input().Primary.Logical)
	d := web.Download{Browser: browser, Raw: raw}

	d.TargetPath = optPayloadText(row, c, "target_path", "target_path", maxPath, raw, unknown)
	d.CurrentPath = optPayloadText(row, c, "current_path", "current_path", maxPath, raw, unknown)

	var ch chain
	if id, k := rowrun.IntCell(row, c.At("id")); k == rowrun.CellValue {
		if ch, err = chainOf(m, id, raw); err != nil {
			return nil, err
		}
	} else {
		flagInt(raw, unknown, row, "id", c.At("id"), k)
		ch.undecided = true
	}
	if ch.undecided {
		raw["join_undecided"] = []any{"downloads_url_chains"}
	}
	if d.TargetPath == "" && ch.final == "" {
		why := ""
		if ch.undecided {
			why = " (its url chain could not be decided)"
		}
		return nil, rowrun.Unmappable("the download has no usable target_path and no chain url%s", why)
	}
	if ch.final != "" {
		d.URL, d.URLChain = ch.final, ch.urls
	}
	d.Referrer = optPayloadText(row, c, "referrer", "referrer", maxURL, raw, unknown)
	d.TabURL = optPayloadText(row, c, "tab_url", "tab_url", maxURL, raw, unknown)
	d.Mime = optPayloadText(row, c, "mime_type", "mime", maxText, raw, unknown)

	if tv, k := rowrun.IntCell(row, c.At("total_bytes")); k == rowrun.CellValue {
		raw["total_bytes"] = tv // 0 is Chromium's "not recorded": stated only here
		switch {
		case tv > 0:
			d.TotalBytes = &tv
		case tv < 0:
			unknown["total_bytes"] = "negative"
		}
	} else {
		flagInt(raw, unknown, row, "total_bytes", c.At("total_bytes"), k)
	}
	if rv, k := rowrun.IntCell(row, c.At("received_bytes")); k == rowrun.CellValue {
		if rv < 0 {
			unknown["received_bytes"] = "negative"
		} else {
			d.ReceivedBytes = &rv
		}
	} else {
		flagInt(raw, unknown, row, "received_bytes", c.At("received_bytes"), k)
	}
	d.State = enumCell(row, c, "state", stateName, raw, unknown)
	d.DangerType = enumCell(row, c, "danger_type", dangerName, raw, unknown)
	d.InterruptReason = enumCell(row, c, "interrupt_reason", interruptName, raw, unknown)
	if ov, k := rowrun.IntCell(row, c.At("opened")); k == rowrun.CellValue && (ov == 0 || ov == 1) {
		b := ov == 1
		d.Opened = &b
	} else if k == rowrun.CellValue {
		unknown["opened"] = "not-boolean"
	} else {
		flagInt(raw, unknown, row, "opened", c.At("opened"), k)
	}

	rec := records.Record{Type: web.DownloadType, ArtifactID: place.ArtifactID, SourcePath: place.SourcePath, Locator: place.Locator, Range: &place.Range}
	if sv, k := rowrun.IntCell(row, c.At("start_time")); k == rowrun.CellValue {
		in := webkit(sv)
		in.Record(raw, "start_time")
		if in.Valid() {
			rec.Time = &records.Time{T: in.T}
		}
	} else {
		flagInt(raw, unknown, row, "start_time", c.At("start_time"), k)
	}
	for _, t := range []struct{ col, kind string }{{"end_time", "ended"}, {"last_access_time", "accessed"}} {
		if v, k := rowrun.IntCell(row, c.At(t.col)); k == rowrun.CellValue {
			in := webkit(v)
			in.Record(raw, t.col)
			if in.Valid() {
				rec.Times = append(rec.Times, records.NamedTime{Kind: t.kind, Time: records.Time{T: in.T}})
			}
		} else {
			flagInt(raw, unknown, row, t.col, c.At(t.col), k)
		}
	}
	for _, col := range []string{"guid", "original_mime_type", "tab_referrer_url", "site_url", "http_method"} {
		optText(row, c, col, maxText, raw, unknown)
	}
	optInt(row, c, "transient", raw, unknown)
	hashCell(row, c, raw, unknown)
	stampLabel(raw, profile, pkg)
	if len(unknown) > 0 {
		raw["unknown_columns"] = unknown
	}
	if f := rowrun.DamageFlags(row); f != nil {
		raw["row_flags"] = f
	}
	parts := make([]string, 0, 1+len(ch.urls))
	if d.TargetPath != "" {
		parts = append(parts, d.TargetPath)
	}
	parts = append(parts, ch.urls...)
	rec.Body = downloadBody(raw, parts)
	rec.Summary = web.DownloadSummary(d)
	rec.Payload = d.Payload()
	return []records.Record{rec}, nil
}
