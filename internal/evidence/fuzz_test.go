package evidence

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// FuzzCheckReadSQL: the lexical read guard (checkReadSQL) never panics, and a
// statement it accepts (a) starts with SELECT or WITH after blanks and comments
// and (b) compiles, as far as its first statement goes, to bytecode that opens
// no table for writing (EXPLAIN compiles without running, so a hostile recursive
// query cannot stall the fuzzer). The seeds are the refused and allowed lists of
// TestReadHandleRefusesNonReadSQL and a few lexer edge cases.
func FuzzCheckReadSQL(f *testing.F) {
	for _, s := range []string{
		`COMMIT; UPDATE records_meta SET value = '777'`, `PRAGMA query_only=OFF`, `WITH x AS (SELECT 1) DELETE FROM records_meta`,
		`WITH x AS (SELECT 1) REPLACE INTO records_meta VALUES ('k', 'v')`, `SELECT 1; SELECT 2`, `SELECT 1;;`, `ATTACH DATABASE ':memory:' AS evil`,
		`SELECT 1 FROM records_meta WHERE value = 'a' PRAGMA query_only = OFF`, `/* unterminated`, `SELECT 'unterminated`,
		`SELECT 1`, `  -- leading comment` + "\n SELECT 1;", `SELECT 'commit; pragma query_only = off; update x'`,
		`SELECT "update", [delete], ` + "`attach`" + ` FROM (SELECT 1 AS "update", 2 AS [delete], 3 AS ` + "`attach`" + `)`,
		`WITH x AS (SELECT 1 AS n) SELECT n FROM x`, `SELECT CASE WHEN 1 THEN 'a' ELSE 'b' END`, `SELECT replace('abc', 'b', 'x')`,
		"SELECT 1 /* \x00 */", "select\x0b1", "[", "\"", "`", "--", "/*",
	} {
		f.Add(s)
	}
	c := newTestCase(f)
	ctx := context.Background()
	f.Fuzz(func(t *testing.T, q string) {
		if err := checkReadSQL(q); err != nil {
			return
		}
		rest := q
		for {
			rest = strings.TrimLeft(rest, " \t\n\r\f\v")
			switch {
			case strings.HasPrefix(rest, "--"):
				i := strings.IndexByte(rest, '\n')
				if i < 0 {
					rest = ""
				} else {
					rest = rest[i+1:]
				}
				continue
			case strings.HasPrefix(rest, "/*"):
				i := strings.Index(rest[2:], "*/")
				if i < 0 {
					rest = ""
				} else {
					rest = rest[i+4:]
				}
				continue
			}
			break
		}
		if lw := strings.ToLower(rest); !strings.HasPrefix(lw, "select") && !strings.HasPrefix(lw, "with") {
			t.Fatalf("accepted a statement that does not start with SELECT or WITH: %q", q)
		}
		err := c.ReadTx(ctx, func(h ReadHandle) error {
			rows, err := h.tx.QueryContext(ctx, "EXPLAIN "+q)
			if err != nil {
				return nil // does not compile: nothing can run
			}
			defer func() { _ = rows.Close() }()
			for rows.Next() {
				var addr int
				var opcode string
				var p1, p2, p3 int
				var p4 any
				var p5 int
				var comment any
				if err := rows.Scan(&addr, &opcode, &p1, &p2, &p3, &p4, &p5, &comment); err != nil {
					return nil
				}
				switch opcode {
				case "OpenWrite", "Insert", "Delete", "Destroy", "Clear", "CreateBtree", "ParseSchema", "SetCookie":
					t.Fatalf("an accepted statement compiles to a write (%s): %q", opcode, q)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("ReadTx after %q: %v", q, err)
		}
	})
}

// FuzzDecodeDetails: decoding audit details of any shape into every codec type
// never panics, and details that decode re-encode (Details) to something that
// decodes to the same value: the strict decoding (unknown fields refused, integer
// numbers) leaves no input that changes meaning on the way through.
func FuzzDecodeDetails(f *testing.F) {
	for _, s := range []string{
		`{}`, `{"ingest_id":"ing-1","batch_no":1,"first_id":1,"count":2,"digest":"d","created":"c","artifacts":{"a":"s"},"artifact_incomplete":[],"types":{"t":2}}`,
		`{"ingest_id":"x","batch_no":1.5}`, `{"ingest_id":"x","unknown":1}`, `{"first_id":9223372036854775808}`, `{"first_id":-9223372036854775809}`,
		`{"ingest_id":"x","outcome":"complete","batches":1,"records":2,"first_id":1,"last_id":2,"rollup":"r","types":{}}`,
		`{"ingest_id":"x","by_ingest_id":"y","batch_nos":[1,2],"run_missing":true,"reason":"r"}`,
		`{"ingest_id":"x","parser":"p","parser_version":"1","artifacts":["a"],"batch_rows":5}`, `{"ingest_id":null}`, `{"count":"2"}`, `[]`, `null`, `1e400`,
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.UseNumber()
		var d map[string]any
		if err := dec.Decode(&d); err != nil {
			return
		}
		roundTrip(t, d, func(x BatchCommit) map[string]any { return x.Details() })
		roundTrip(t, d, func(x BatchFailure) map[string]any { return x.Details() })
		roundTrip(t, d, func(x IngestStart) map[string]any { return x.Details() })
		roundTrip(t, d, func(x IngestConclusion) map[string]any { return x.Details() })
		roundTrip(t, d, func(x IngestRecover) map[string]any { return x.Details() })
	})
}

func roundTrip[T any](t *testing.T, d map[string]any, details func(T) map[string]any) {
	t.Helper()
	v, err := DecodeDetails[T](d)
	if err != nil {
		return
	}
	again, err := DecodeDetails[T](details(v))
	if err != nil {
		t.Fatalf("details %v decoded as %T but its re-encoding does not: %v", d, v, err)
	}
	if !reflect.DeepEqual(v, again) {
		t.Fatalf("details %v decode to %+v, and after re-encoding to %+v", d, v, again)
	}
}
