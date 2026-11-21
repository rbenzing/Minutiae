package androidtelegram

import "github.com/rbenzing/minutiae/internal/decode/tl"

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
