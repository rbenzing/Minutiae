package evidence

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"testing"
)

func TestNewFTSDoc(t *testing.T) {
	str := func(s string) *string { return &s }
	for _, c := range []struct {
		name    string
		summary string
		body    *string
		want    FTSDoc
		ok      bool
	}{
		{"summary only", "Hello  World", nil, FTSDoc{ID: 7, Summary: "hello world"}, true},
		{"body only", "", str("Only BODY"), FTSDoc{ID: 7, Body: "only body"}, true},
		{"both", "A", str("B"), FTSDoc{ID: 7, Summary: "a", Body: "b"}, true},
		{"nothing", "", nil, FTSDoc{ID: 7}, false},
		{"empty body", "", str(""), FTSDoc{ID: 7}, false},
		{"only text that normalizes away", " \t\n", str("\x00"), FTSDoc{ID: 7}, false},
		{"fullwidth and NFD", string([]rune{0xff21, 0xff22}) + " cafe" + string(rune(0x301)), nil, FTSDoc{ID: 7, Summary: "ab caf" + string(rune(0xe9))}, true},
	} {
		got, ok := NewFTSDoc(7, c.summary, c.body)
		if got != c.want || ok != c.ok {
			t.Errorf("%s: NewFTSDoc = %+v, %v; want %+v, %v", c.name, got, ok, c.want, c.ok)
		}
	}
}

func TestInsertFTSDocsRequiresCurrentIndex(t *testing.T) {
	ctx := context.Background()
	c, err := Create(filepath.Join(t.TempDir(), "FTSDOC"), CreateOptions{ID: "FTSDOC", Examiner: "E"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	docs := []FTSDoc{{ID: 1, Summary: "alpha"}, {ID: 2, Body: "beta"}}
	indexed := func(table string) []int64 {
		var ids []int64
		err := c.ReadTx(ctx, func(h ReadHandle) error {
			rows, err := h.Query(`SELECT id FROM ` + table + `_docsize ORDER BY id`)
			if err != nil {
				return err
			}
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var id int64
				if err := rows.Scan(&id); err != nil {
					return err
				}
				ids = append(ids, id)
			}
			return rows.Err()
		})
		if err != nil {
			t.Fatal(err)
		}
		return ids
	}

	// current: both tables get both documents
	if err := c.StoreTx(ctx, func(tx *sql.Tx) error { return InsertFTSDocs(ctx, tx, docs) }); err != nil {
		t.Fatal(err)
	}
	for _, table := range FTSTables() {
		if got := indexed(table); !slices.Equal(got, []int64{1, 2}) {
			t.Errorf("%s holds %v, want [1 2]", table, got)
		}
	}

	// not current: InsertFTSDocs refuses and writes nothing; the unexported insert (reindex, verify) does not check
	err = c.StoreTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE records_meta SET value = 'building' WHERE key = ?`, MetaFTSNormVersion); err != nil {
			return err
		}
		if err := InsertFTSDocs(ctx, tx, []FTSDoc{{ID: 3, Summary: "gamma"}}); !errors.Is(err, ErrIndexNotCurrent) {
			t.Errorf("InsertFTSDocs on a building index = %v, want ErrIndexNotCurrent", err)
		}
		return insertFTSDocs(ctx, tx, []FTSDoc{{ID: 4, Summary: "delta"}})
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range FTSTables() {
		if got := indexed(table); !slices.Equal(got, []int64{1, 2, 4}) {
			t.Errorf("%s holds %v, want [1 2 4] (3 was refused, 4 was inserted unchecked)", table, got)
		}
	}
}
