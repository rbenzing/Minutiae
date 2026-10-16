package dbrecover

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/records"
	"github.com/rbenzing/minutiae/internal/records/recordstest"
	"github.com/rbenzing/minutiae/internal/sqlitefile"
)

// J34: a name that holds NUL or invalid UTF-8 is percent-encoded byte for byte.
func TestLocatorEncodesUnrepresentableNamesExactly(t *testing.T) {
	for name, want := range map[string]string{
		"a\x00b":   "sqlite:table=a%00b;page=1;cell=0;file=db",
		"a\xffb":   "sqlite:table=a%FFb;page=1;cell=0;file=db",
		"a~b":      "sqlite:table=a~b;page=1;cell=0;file=db",
		"\x00\xff": "sqlite:table=%00%FF;page=1;cell=0;file=db",
	} {
		if got := locatorFor(row(name, 1, 0, sqlitefile.OriginOrphan, sqlitefile.FileDB)); got != want {
			t.Errorf("%q:\n got  %s\n want %s", name, got, want)
		}
	}
	idx := row("", 1, 0, sqlitefile.OriginOrphan, sqlitefile.FileDB)
	idx.Index = "i\x00x"
	if got := locatorFor(idx); got != "sqlite:index=i%00x;page=1;cell=0;file=db" {
		t.Errorf("index: %s", got)
	}
	if got := locatorFor(row("", 1, 0, sqlitefile.OriginOrphan, sqlitefile.FileDB)); got != "sqlite:page=1;cell=0;file=db" {
		t.Errorf("empty name must be omitted: %s", got)
	}
	// a long unrepresentable name is capped by the 200-byte rule, never dropped
	long := strings.Repeat("\xff", 500)
	dec := pctDecode(t, nameOf(locatorFor(row(long, 1, 0, sqlitefile.OriginOrphan, sqlitefile.FileDB))))
	head, tail, ok := strings.Cut(dec, "~")
	if !ok || len(head) != 200 || len(tail) != 8 || head != long[:200] {
		t.Errorf("long invalid name: head %d tail %q", len(head), tail)
	}
}

func TestPctEncodeKeepsTilde(t *testing.T) {
	if got := pctEncode("~._-"); got != "~._-" {
		t.Errorf("unreserved bytes were encoded: %s", got)
	}
}

// J36: exact size against the standard encoder, including the newline rule.
func TestEncodedSizeEqualsStandardEncoding(t *testing.T) {
	for _, m := range []map[string]any{
		{},
		{"a": int64(1)},
		{"value": "line\u2028sep\u2029", "x": "ctl\x01\x1f\"\\"},
		{"value_b64": "AAEC", "html": "<&>"},
	} {
		want, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(m); err != nil {
			t.Fatal(err)
		}
		exact := int64(len(bytes.TrimSuffix(buf.Bytes(), []byte("\n"))))
		if got := mustSize(m); got != exact {
			t.Errorf("%v: encodedSize %d, canonical %d", m, got, exact)
		}
		if _, ok := m["html"]; !ok && mustSize(m) != int64(len(want)) {
			t.Errorf("%v: encodedSize %d, Marshal %d", m, mustSize(m), len(want))
		}
	}
}

func TestMinEncodedBytes(t *testing.T) {
	blob := func(n int) sqlitefile.Value {
		return sqlitefile.Value{Kind: sqlitefile.KindBlob, Bytes: make([]byte, n), Len: int64(n)}
	}
	for n, want := range map[int]int64{0: 0, 1: 4, 2: 4, 3: 4, 4: 8, 30: 40, 31: 44} {
		if got := minEncodedBytes(blob(n)); got != want {
			t.Errorf("blob of %d bytes: %d, want %d", n, got, want)
		}
	}
	if got := minEncodedBytes(textV([]byte("abcd"), sqlitefile.EncUTF8)); got != 2 {
		t.Errorf("text: %d, want 2", got)
	}
}

func degradedRowSize(vals []sqlitefile.Value) int64 {
	var n int64
	for i, v := range vals {
		n += mustSize(degraded(i, v)) + 1
	}
	return n
}

// J35: the budget is a hard cap on the encoded row.
func TestColumnsBudgetIsAHardCap(t *testing.T) {
	blob := bytes.Repeat([]byte{0xab}, 5000)
	vals := make([]sqlitefile.Value, 32767)
	for i := range vals {
		vals[i] = sqlitefile.Value{Kind: sqlitefile.KindBlob, Bytes: blob, Len: int64(len(blob))}
	}
	floor := degradedRowSize(vals)
	for _, budget := range []int64{1000, floor - 1, floor, floor + 10_000} {
		cols := mustColumns(vals, budget)
		var total int64
		for _, c := range cols {
			total += mustSize(c.(map[string]any)) + 1
		}
		if len(cols) != len(vals) {
			t.Fatalf("budget %d: %d columns, want %d", budget, len(cols), len(vals))
		}
		if allow := max(budget, floor); total > allow {
			t.Errorf("budget %d: row encodes to %d, allowance %d", budget, total, allow)
		}
	}
}

func TestColumnsFullValueOnlyWhenItFits(t *testing.T) {
	vals := []sqlitefile.Value{
		{Kind: sqlitefile.KindInt, Int: 1},
		{Kind: sqlitefile.KindBlob, Bytes: make([]byte, 3000), Len: 3000},
		{Kind: sqlitefile.KindInt, Int: 2},
	}
	cols := mustColumns(vals, degradedRowSize(vals)+200)
	if cols[0].(map[string]any)["value"] != int64(1) || cols[2].(map[string]any)["value"] != int64(2) {
		t.Errorf("small values degraded: %v", cols)
	}
	if cols[1].(map[string]any)["omitted"] != "payload-budget" {
		t.Errorf("a blob that cannot fit was encoded in full")
	}
	cols = mustColumns(vals, 1<<20)
	if cols[1].(map[string]any)["value_b64"] == nil {
		t.Errorf("a blob that fits was degraded")
	}
}

func TestWideRowOfLargeValuesIsAcceptedByTheWriter(t *testing.T) {
	c := recordstest.NewCase(t)
	art := recordstest.AddArtifact(t, c, "a.db", []byte("x"))
	blob := bytes.Repeat([]byte{0xab}, 5000)
	vals := make([]sqlitefile.Value, 32767)
	for i := range vals {
		vals[i] = sqlitefile.Value{Kind: sqlitefile.KindBlob, Bytes: blob, Len: int64(len(blob))}
	}
	p := validPayload()
	p["columns"] = mustColumns(vals, 12<<20)
	enc, err := json.Marshal(p)
	if err != nil || len(enc) >= 16<<20 {
		t.Fatalf("payload %d bytes (err %v), want under 16 MiB", len(enc), err)
	}
	res := recordstest.Ingest(t, c, records.Parser{Name: "t", Version: "1"}, []string{art.ID}, []records.Record{{
		Type: TypeName, ArtifactID: art.ID, Summary: "x", Deleted: true, Recovery: "sqlite-freelist", Payload: p,
	}})
	if res.Records != 1 {
		t.Fatalf("stored %d records, want 1", res.Records)
	}
}
