package utf8fold

import (
	"encoding/hex"
	"testing"
)

// TestKernelVectors replays every row of kernel-vectors.json.gz: the verdict a real kernel gave for "does
// this query resolve to that stored name" must be Equal(query, stored), in both directions (equal is equal
// and different is different). Every committed row is a measured shape, so none may be undecided.
func TestKernelVectors(t *testing.T) {
	v := readVectors(t)
	byCat := map[string][]vectorRow{}
	for _, r := range v.Vectors {
		byCat[r.Category] = append(byCat[r.Category], r)
	}
	for _, cat := range vectorCategories {
		rows := byCat[cat]
		t.Run(cat, func(t *testing.T) {
			if len(rows) == 0 {
				t.Fatal("no rows")
			}
			bad := 0
			for _, r := range rows {
				stored, query := decodeName(t, r.ID, "stored", r.StoredHex), decodeName(t, r.ID, "query", r.QueryHex)
				eq, decided := eqd(query, stored)
				if !decided || eq != r.Ext4 {
					bad++
					if bad <= 10 {
						ks, _ := Key(nil, stored)
						kq, _ := Key(nil, query)
						t.Errorf("row %d: Equal(query, stored) = (%v, decided %v), kernel says %v\n  stored %s key %s\n  query  %s key %s",
							r.ID, eq, decided, r.Ext4, r.StoredHex, hex.EncodeToString(ks), r.QueryHex, hex.EncodeToString(kq))
					}
				}
			}
			if bad > 10 {
				t.Errorf("%d rows in all disagree", bad)
			}
		})
	}
}
