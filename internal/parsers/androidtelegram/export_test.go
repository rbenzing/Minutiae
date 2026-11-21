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
	TypeMessage   = typeMessage
	TypeUser      = typeUser
	UnitEditDate  = unitEditDate
	UnitBlobTimes = unitBlobTimes
)

// MappedFields is mappedFields.
func MappedFields() []string { return mappedFields() }

// BlobStates is the blob state vocabulary in order.
func BlobStates() []string { return blobStates() }
