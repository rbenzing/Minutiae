package chromium

import (
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/decode/ts"
)

// webkit reads microseconds since 1601-01-01 UTC through decode/ts. The result is Valid only with a real time; a
// sentinel or an out-of-range value keeps its status and the stored value (4F-R2: kept local to this package).
func webkit(raw int64) rowrun.Instant {
	st := ts.Classify(ts.KindWebKitMicros, raw)
	i := rowrun.Instant{Status: st, Raw: raw}
	if st != ts.StatusValid {
		return i
	}
	t, ok := ts.WebKitMicros(raw)
	if !ok {
		return rowrun.Instant{Status: ts.StatusInvalid, Raw: raw}
	}
	i.T = t
	return i
}
