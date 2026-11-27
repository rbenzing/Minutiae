package androidtelegram

import (
	"strings"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb"
	"github.com/rbenzing/minutiae/internal/decode/tl"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
)

// The test-only door of the external tests to the package's internals (compiled into test builds only).

// DecodeBlob is decodeBlob of column "data" through a table of its own: the raw keys it states and the state. It frees
// what the decode charged before it returns, as a mapping does when its row is done (F8).
func DecodeBlob(b []byte, typ string, bud tl.Budget) (map[string]any, string) {
	raw := map[string]any{}
	v, state := decodeBlob(raw, "data", b, typ, newTable(), bud)
	if bud != nil {
		bud.Free(v.r.Charged())
	}
	return raw, state
}

// The TL types and the fact-bound constants the external tests name.
const (
	TypeMessage      = typeMessage
	TypeUser         = typeUser
	UnitEditDate     = unitEditDate
	UnitBlobTimes    = unitBlobTimes
	UnitCallDuration = unitCallDuration
)

// EncChatIDCast is encChatIDSigned as facts.tsv states the cast: "int32" (signed) or "uint32".
func EncChatIDCast() string {
	if encChatIDSigned {
		return "int32"
	}
	return "uint32"
}

// MappedFields is mappedFields.
func MappedFields() []string { return mappedFields() }

// BlobStates is the blob state vocabulary in order.
func BlobStates() []string { return blobStates() }

// MediaLabel is mediaLabel.
func MediaLabel(class string) string { return mediaLabel(class) }

// MediaClassCount is the number of MessageMedia constructors of the table.
func MediaClassCount() int {
	n := 0
	for _, c := range newTable() {
		if c.Type == typeMedia {
			n++
		}
	}
	return n
}

// TableEncChats and EncChatsColumns are the enc_chats allowlist of secret.go (spec 9, F9).
const TableEncChats = tableEncChats

// EncChatsColumns is encChatsColumns.
func EncChatsColumns() []string { return encChatsColumns() }

// LostRowidUsersMapping is the typed pass's users mapping over rows that present as recovered rows whose rowid is lost
// (parse.Row.Rowid false, no locator), as a recovery host hands them over.
func LostRowidUsersMapping() parse.TableMapping {
	m := userMappingWith(nil)
	inner := m.Map
	m.Map = func(mc parse.MapContext, row parse.Row) ([]records.Record, error) {
		return inner(mc, lostRowid{row.(sqlitedb.Row)})
	}
	return m
}

// lostRowid is a stored row that reports itself recovered with no rowid and no locator.
type lostRowid struct{ sqlitedb.Row }

func (lostRowid) Rowid() (int64, bool)               { return 0, false }
func (lostRowid) Locator() (string, bool)            { return "", false }
func (lostRowid) Recovered() *sqlitedb.RecoveredInfo { return &sqlitedb.RecoveredInfo{} }

// PutReinserts is putReinserts (facts.tsv write_verb.*.put).
const PutReinserts = putReinserts

// Table is the parser's constructor table.
func Table() tl.Table { return newTable() }

// TableEntry is the constructor table's entry of id (the zero Constructor when the table lacks it).
func TableEntry(id uint32) tl.Constructor { return newTable()[id] }

// SeenConstructors is every constructor ID tl.Decode read WHOLE in b as the TL type typ with the parser's table (plan
// 4I4b P10, Task 4 review M1): the top object's when the table knows it for typ, and every nested object's (a field or a
// vector element of kind KObject), in decode order, each only when the decode's stop (if any) is neither at nor inside
// the object's path. An object whose body then stops (a TL_messageMediaPoll can be read no further than its opaque Poll)
// had its ID read but is not whole.
func SeenConstructors(b []byte, typ string) []uint32 {
	bud := parse.NewBudget(1 << 30).View()
	r := tl.Decode(b, typ, newTable(), bud)
	defer bud.Free(r.Charged())
	whole := func(path string) bool {
		if r.Stop == nil {
			return true
		}
		s := r.Stop.Path
		return path != "" && s != path && !strings.HasPrefix(s, path+".") && !strings.HasPrefix(s, path+"[")
	}
	var out []uint32
	if c, ok := newTable()[r.Top]; ok && c.Type == typ && whole("") {
		out = append(out, r.Top)
	}
	for _, n := range r.Fields {
		if n.Kind == tl.KObject && whole(n.Path) {
			out = append(out, n.ID)
		}
	}
	return out
}
