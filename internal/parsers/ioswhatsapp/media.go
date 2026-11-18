package ioswhatsapp

import (
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
)

// The media row of a message (4I2-P4, 4I2-R6, rulings O5 and PF1). Two directions name it: ZWAMEDIAITEM.ZMESSAGE equal
// to the message's Z_PK (the converse key, sources agree), and ZWAMESSAGE.ZMEDIAITEM naming the row (one source, which
// never links alone).

const tableMediaItem = "ZWAMEDIAITEM"

// mediaKey is ZWAMESSAGE.ZMEDIAITEM as a join key: typed, never a bare cell (PF7). Set is false for NULL, 0 and a cell
// that cannot be read (the reading has stated that in unknown_columns).
type mediaKey struct {
	PK  int64
	Set bool
}

// mediaJoin is the outcome of the media join.
type mediaJoin int

const (
	mediaRow       mediaJoin = iota // exactly one row names the message, and the forward key (if set) agrees
	mediaNone                       // no row names the message, and the forward key names no row
	mediaUndecided                  // the directions disagree, only the forward key names a row, two rows name the message, or a join cannot be decided
)

// joinMedia finds the ZWAMEDIAITEM row of the message msgPK. The row is returned for mediaRow only; in every other
// outcome no media field is taken. A join that cannot be decided (a missing or guarded table, a lossy index) is
// mediaUndecided, never mediaNone.
func joinMedia(m parse.MapContext, msgPK int64, forward mediaKey) (parse.Row, mediaJoin, error) {
	rows, st, err := matchInt(m, tableMediaItem, "ZMESSAGE", msgPK)
	if err != nil {
		return nil, mediaUndecided, err
	}
	if st != jFound || len(rows) >= 2 {
		return nil, mediaUndecided, nil
	}
	if len(rows) == 1 {
		if forward.Set {
			if id, ok := rows[0].Rowid(); !ok || id != forward.PK {
				return nil, mediaUndecided, nil // both directions are set and disagree
			}
		}
		return rows[0], mediaRow, nil
	}
	if !forward.Set {
		return nil, mediaNone, nil
	}
	_, gst, err := getRow(m, tableMediaItem, forward.PK)
	if err != nil {
		return nil, mediaUndecided, err
	}
	if gst == jNoRow {
		return nil, mediaNone, nil // the forward key is dangling: no row names the message
	}
	return nil, mediaUndecided, nil // only the single-source direction names a row (or the lookup is undecided)
}

// mediaKeyOf reads the ZMEDIAITEM cell of a message row.
func mediaKeyOf(r cellReader) mediaKey {
	v, ok := r.int("ZMEDIAITEM")
	return mediaKey{PK: v, Set: ok && v != 0}
}

// mediaColumns are the ZWAMEDIAITEM columns the mapping reads.
func mediaColumns() []string {
	return []string{
		"ZMEDIAURL", "ZTHUMBNAILLOCALPATH", "ZAUTHORNAME", "ZVCARDNAME", "ZFILESIZE", "ZMOVIEDURATION", "ZCLOUDSTATUS",
		"ZMEDIAURLDATE", "ZTITLE", "ZMEDIAKEY", "ZLATITUDE", "ZLONGITUDE", "ZMETADATA", "ZVCARDSTRING", "ZMEDIALOCALPATH",
	}
}

// mediaReader is the cell reader of a joined ZWAMEDIAITEM row.
func mediaReader(m parse.MapContext, row parse.Row, raw, unknown map[string]any) cellReader {
	return cellReader{row: row, cols: rowrun.Resolve(m, tableMediaItem, mediaColumns()...), raw: raw, unknown: unknown, prefix: prefixMedia}
}

// readMediaFields states the media row's columns in raw as stored (4I2-R7 "Media"): flat names, cleaned text, the
// location for type 5 only (for every other type the latitude and longitude of a row are other things, e.g. the
// width and height of an image, and stay under their own names), the length and SHA-256 of ZMETADATA, and whether a
// media key is present, never the key. ZTITLE is raw unless it was taken as the body.
func readMediaFields(ir cellReader, typ int64, titleIsBody bool) {
	raw := ir.raw
	for _, n := range []string{"ZMEDIAURL", "ZTHUMBNAILLOCALPATH", "ZAUTHORNAME", "ZVCARDNAME"} {
		if b, ok := ir.text(n); ok {
			raw[n] = cleanField(raw, n, b)
		}
	}
	for _, n := range []string{"ZFILESIZE", "ZMOVIEDURATION", "ZCLOUDSTATUS"} {
		if v, ok := ir.int(n); ok {
			raw[n] = v
		}
	}
	if v, ok := ir.number("ZMEDIAURLDATE"); ok {
		raw["ZMEDIAURLDATE"] = v.Raw()
	}
	if !titleIsBody {
		if b, ok := ir.text("ZTITLE"); ok && len(b) > 0 {
			raw["ZTITLE"] = cleanField(raw, "ZTITLE", b)
		}
	}
	if _, k := blobCell(ir.row, ir.cols.At("ZMEDIAKEY")); k == rowrun.CellValue {
		raw["media_key_present"] = true
	} else if k == rowrun.CellOther {
		raw["media_key_present"] = false
	} else {
		rowrun.Flag(ir.unknown, ir.row, prefixMedia+"ZMEDIAKEY", ir.cols.At("ZMEDIAKEY"), k)
	}
	lat, latOK := ir.number("ZLATITUDE")
	lon, lonOK := ir.number("ZLONGITUDE")
	if typ == typeLocation {
		loc := map[string]any{}
		if latOK {
			loc["latitude"] = lat.Raw()
		}
		if lonOK {
			loc["longitude"] = lon.Raw()
		}
		if len(loc) > 0 {
			raw["location"] = loc
		}
	} else {
		if latOK {
			raw["ZLATITUDE"] = lat.Raw()
		}
		if lonOK {
			raw["ZLONGITUDE"] = lon.Raw()
		}
	}
	if b, ok := ir.blob("ZMETADATA"); ok {
		raw["ZMETADATA"] = digest(b)
	}
}
