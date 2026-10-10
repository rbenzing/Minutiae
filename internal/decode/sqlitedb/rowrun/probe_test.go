package rowrun_test

import (
	"crypto/sha256"
	"encoding/binary"
	"strings"
	"testing"

	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

func probeDB(t testing.TB) []byte {
	t.Helper()
	b := sqlitetest.New(sqlitetest.Options{PageSize: 1024})
	b.CreateTable("sms", "create table sms(_id integer primary key, address text, date integer, body text)").Insert(1, int64(1), "a", int64(1), "b")
	b.CreateTable("th", "create table th(_id integer, r text)")
	b.CreateTable("pdu", "create table pdu(_id integer primary key, date integer)")
	return b.Bytes()
}

func probeOf(t *testing.T, data []byte, withWAL bool, reqs ...rowrun.Req) (parse.Applicability, error) {
	t.Helper()
	in := dbInput(t, data)
	if withWAL {
		in.Artifacts["wal"] = parse.Artifact{ID: "A-WAL", Size: 4096}
	}
	return rowrun.Probe(t.Context(), in, reqs)
}

func TestProbeApplicable(t *testing.T) {
	got, err := probeOf(t, probeDB(t), false, rowrun.Req{Table: "sms", Need: []string{"address", "date", "body"}, Alias: "_id"})
	if err != nil || got.Status != parse.Applicable {
		t.Errorf("Probe = %+v, %v", got, err)
	}
}

func TestProbeNotSQLiteAndEncrypted(t *testing.T) {
	got, err := probeOf(t, []byte(strings.Repeat("hello world ", 400)), false, rowrun.Req{Table: "sms"})
	if err != nil || got.Status != parse.NotApplicable {
		t.Errorf("text file: %+v, %v; want not-applicable", got, err)
	}
	// 8 KiB of hash output looks like ciphertext: high entropy, no SQLite magic
	var enc []byte
	for i := uint64(0); len(enc) < 8192; i++ {
		var c [8]byte
		binary.LittleEndian.PutUint64(c[:], i)
		sum := sha256.Sum256(c[:])
		enc = append(enc, sum[:]...)
	}
	got, err = probeOf(t, enc, false, rowrun.Req{Table: "sms"})
	if err != nil || got.Status != parse.Encrypted {
		t.Errorf("high-entropy file: %+v, %v; want encrypted", got, err)
	}
}

func TestProbeNotSQLiteWithACompanionIsLoud(t *testing.T) {
	got, err := probeOf(t, []byte(strings.Repeat("x", 4096)), true, rowrun.Req{Table: "sms"})
	if err != nil || got.Status != parse.UnsupportedSchema {
		t.Errorf("Probe = %+v, %v; want unsupported-schema (a -wal exists, the main file is not a database)", got, err)
	}
}

func TestProbeNamesWhatIsMissing(t *testing.T) {
	data := probeDB(t)
	got, _ := probeOf(t, data, false, rowrun.Req{Table: "calls"})
	if got.Status != parse.NotApplicable || !strings.Contains(got.Reason, "calls") {
		t.Errorf("absent table: %+v", got)
	}
	got, _ = probeOf(t, data, false, rowrun.Req{Table: "sms", Need: []string{"address", "nope1", "nope2"}})
	if got.Status != parse.UnsupportedSchema || !strings.Contains(got.Reason, "sms") || !strings.Contains(got.Reason, "nope1, nope2") {
		t.Errorf("missing columns: %+v", got)
	}
	got, _ = probeOf(t, data, false, rowrun.Req{Table: "th", Alias: "_id"})
	if got.Status != parse.UnsupportedSchema || !strings.Contains(got.Reason, "rowid alias") {
		t.Errorf("a join column that is not the rowid alias: %+v", got)
	}
}

func TestProbeOptionalAndWithTables(t *testing.T) {
	data := probeDB(t)
	got, err := probeOf(t, data, false, rowrun.Req{Table: "sms", Need: []string{"body"}}, rowrun.Req{Table: "threads", Optional: true})
	if err != nil || got.Status != parse.Applicable {
		t.Errorf("optional absent table: %+v, %v", got, err)
	}
	// an optional table that exists must still have its needed columns
	got, _ = probeOf(t, data, false, rowrun.Req{Table: "pdu", Need: []string{"msg_box"}, Optional: true})
	if got.Status != parse.UnsupportedSchema {
		t.Errorf("optional table lacking a needed column: %+v", got)
	}
	got, _ = probeOf(t, data, false, rowrun.Req{Table: "pdu", Optional: true, With: []string{"addr", "part"}})
	if got.Status != parse.UnsupportedSchema || !strings.Contains(got.Reason, "addr") {
		t.Errorf("pdu without addr: %+v", got)
	}
}

func TestProbeWithACompanionDefersTheSchemaDecisionToParse(t *testing.T) {
	data := probeDB(t)
	for name, rq := range map[string]rowrun.Req{
		"absent table":   {Table: "calls"},
		"missing column": {Table: "sms", Need: []string{"nope"}},
		"alias":          {Table: "th", Alias: "_id"},
	} {
		got, err := probeOf(t, data, true, rq)
		if err != nil || got.Status != parse.Applicable || !strings.Contains(got.Reason, "parse time") {
			t.Errorf("%s with a -wal: %+v, %v; want applicable, decided at parse time", name, got, err)
		}
	}
}

func TestProbeBudgetFailureIsAnErrorNotAnAnswer(t *testing.T) {
	in := dbInput(t, probeDB(t))
	in.Budget = parse.NewBudget(1).View()
	if got, err := rowrun.Probe(t.Context(), in, []rowrun.Req{{Table: "sms"}}); err == nil {
		t.Errorf("Probe under an exhausted budget answered %+v", got)
	}
}
