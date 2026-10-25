package chromium

import (
	"strconv"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
)

func metaSpecs() []parse.ColumnSpec {
	return []parse.ColumnSpec{
		{Name: "key", Required: true, Affinity: "text"},
		{Name: "value", Affinity: "text"},
	}
}

func metaMapping() parse.TableMapping {
	return parse.TableMapping{Table: "meta", Columns: metaSpecs(), Map: mapMeta}
}

// mapMeta emits no record: it notes meta.version and meta.last_compatible_version when the value is an integer or
// ASCII decimal text of at most 16 bytes, else "non-numeric" ("unknown" for a cell the decoder cannot vouch for)
// (4F-R21). An absent value gives no note.
func mapMeta(m parse.MapContext, row parse.Row) ([]records.Record, error) {
	c := rowrun.Resolve(m, "meta", "key", "value")
	kb, k := rowrun.TextCell(row, c.At("key"))
	if k != rowrun.CellValue {
		return nil, nil
	}
	var note string
	switch string(kb) {
	case "version":
		note = "meta.version"
	case "last_compatible_version":
		note = "meta.last_compatible_version"
	default:
		return nil, nil
	}
	val := c.At("value")
	if n, nk := rowrun.IntCell(row, val); nk == rowrun.CellValue {
		m.Note(note, strconv.FormatInt(n, 10))
		return nil, nil
	} else if nk == rowrun.CellNull || nk == rowrun.CellMissing {
		return nil, nil
	}
	tb, tk := rowrun.TextCell(row, val)
	switch {
	case tk == rowrun.CellUnknown:
		m.Note(note, "unknown")
	case tk == rowrun.CellValue && decimal(tb):
		m.Note(note, string(tb))
	default:
		m.Note(note, "non-numeric")
	}
	return nil, nil
}

// decimal reports ASCII digits only, 1 to 16 bytes.
func decimal(b []byte) bool {
	if len(b) == 0 || len(b) > 16 {
		return false
	}
	for _, c := range b {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
