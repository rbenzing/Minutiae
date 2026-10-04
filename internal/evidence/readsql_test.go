package evidence

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestReadHandleRefusesNonReadSQL: the handle refuses, before it runs anything,
// SQL that is not one read statement. The database is unchanged afterwards and
// ReadTx itself reports no modification (the second layer never had to act).
func TestReadHandleRefusesNonReadSQL(t *testing.T) {
	const upd = `UPDATE records_meta SET value = '777' WHERE key = 'next_id'`
	refused := []string{
		`COMMIT; ` + upd,
		`commit; ` + upd,
		`PRAGMA query_only=OFF`,
		`  -- note` + "\n" + `pragma query_only = off`,
		`WITH x AS (SELECT 1) ` + upd,
		`WITH x AS (SELECT 1) DELETE FROM records_meta`,
		`WITH x AS (SELECT 1) INSERT INTO records_meta VALUES ('k', 'v')`,
		`WITH x AS (SELECT 1) REPLACE INTO records_meta VALUES ('k', 'v')`,
		upd,
		`/* SELECT */ ` + upd,
		`SELECT 1; ` + upd,
		`SELECT 1; SELECT 2`,
		`SELECT 1; COMMIT`,
		`SELECT 1;;`,
		`SELECT 1; -- done` + "\n" + `SELECT 2`,
		`ATTACH DATABASE ':memory:' AS evil`,
		`DETACH DATABASE evil`,
		`BEGIN`, `END`, `ROLLBACK`, `SAVEPOINT s`, `RELEASE s`, `VACUUM`,
		`DROP TABLE records_meta`,
		`SELECT 1 FROM records_meta WHERE value = 'a' PRAGMA query_only = OFF`,
		``, `   `, `-- only a comment`, `/* unterminated`,
		`SELECT 'unterminated`,
	}
	allowed := []string{
		`SELECT 1`,
		`select value from records_meta where key = 'next_id'`,
		"  -- leading comment\n SELECT 1;",
		`/* c */ SELECT 1 ; -- trailing` + "\n",
		`SELECT 'commit; pragma query_only = off; update x'`,
		`SELECT 'it''s; COMMIT'`,
		`SELECT "update", [delete], ` + "`attach`" + ` FROM (SELECT 1 AS "update", 2 AS [delete], 3 AS ` + "`attach`" + `)`,
		`WITH x AS (SELECT 1 AS n) SELECT n FROM x`,
		`SELECT CASE WHEN 1 THEN 'a' ELSE 'b' END`,
		`SELECT replace('abc', 'b', 'x')`,
		`SELECT count(*) FROM pragma_database_list`,
	}
	c := newTestCase(t)
	ctx := context.Background()
	before := nextID(t, c)
	for _, q := range refused {
		err := c.ReadTx(ctx, func(h ReadHandle) error {
			if _, err := h.Query(q); err == nil {
				t.Errorf("Query(%q) was accepted", q)
			} else if !errors.Is(err, ErrReadSQLRefused) {
				t.Errorf("Query(%q) err = %v, want ErrReadSQLRefused", q, err)
			}
			if _, err := h.QueryContext(ctx, q); err == nil {
				t.Errorf("QueryContext(%q) was accepted", q)
			}
			var v any
			if err := h.QueryRow(q).Scan(&v); err == nil {
				t.Errorf("QueryRow(%q) was accepted", q)
			}
			if err := h.QueryRowContext(ctx, q).Scan(&v); err == nil {
				t.Errorf("QueryRowContext(%q) was accepted", q)
			}
			return nil
		})
		if err != nil {
			t.Errorf("ReadTx after refusing %q: %v (the second layer must not have been needed)", q, err)
		}
		if got := nextID(t, c); got != before {
			t.Fatalf("next_id = %s after %q, want %s", got, q, before)
		}
		if v := queryOnly(t, c); v != 0 {
			t.Fatalf("query_only = %d after %q", v, q)
		}
	}
	for _, q := range allowed {
		err := c.ReadTx(ctx, func(h ReadHandle) error {
			rows, err := h.Query(q)
			if err != nil {
				return err
			}
			defer func() { _ = rows.Close() }()
			for rows.Next() {
			}
			return rows.Err()
		})
		if err != nil {
			t.Errorf("Query(%q) refused: %v", q, err)
		}
	}
}

// TestReadHandleRefusalNamesTheProblem pins that the refusal says what was wrong.
func TestReadHandleRefusalNamesTheProblem(t *testing.T) {
	c := newTestCase(t)
	_ = c.ReadTx(context.Background(), func(h ReadHandle) error {
		_, err := h.Query(`SELECT 1; SELECT 2`)
		if err == nil || !strings.Contains(err.Error(), "statement") {
			t.Errorf("err = %v, want it to mention the second statement", err)
		}
		_, err = h.Query(`PRAGMA query_only = OFF`)
		if err == nil || !strings.Contains(err.Error(), "SELECT or WITH") {
			t.Errorf("err = %v, want it to say a read statement starts with SELECT or WITH", err)
		}
		return nil
	})
}

// rawExec runs a statement straight on the transaction, around the handle's
// statement filter, to exercise the second layer (detect and roll back).
func rawExec(h ReadHandle, stmt string) error {
	_, err := h.tx.Exec(stmt)
	return err
}
