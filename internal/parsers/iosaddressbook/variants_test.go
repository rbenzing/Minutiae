package iosaddressbook_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/parsers/iosaddressbook"
	"github.com/rbenzing/minutiae/internal/parsers/parsertest"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// Every DDL snapshot of the address book family is the schema of at least one fixture (the images snapshot of
// the one that carries an images database), and every variant parses to exactly the records its oracle states
// with no contract violation and no refused record.
func TestParserCoversEveryVariant(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(schemasPath, "addressbook*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no DDL snapshots found: %v", err)
	}
	used := map[string]bool{}
	for _, v := range variants {
		fx := parsertest.LoadFixture(t, "testdata", v)
		used[fx.Expect.DDL] = true
		if _, has := fx.Files["images"]; has {
			used["addressbookimages.sql"] = true
		}
		h := parsertest.NewHarness(t)
		b := bundleOf(h, fx, v)
		p := iosaddressbook.Parser{}
		res := h.RunLenient(p, b.Input(h, p, false))
		if res.Err != nil || len(res.Violations) != 0 || len(res.Records) != len(fx.Expect.Records) || len(res.Records) == 0 {
			t.Errorf("%s: err %v, violations %v, %d records, oracle %d", v, res.Err, res.Violations, len(res.Records), len(fx.Expect.Records))
		}
		if len(res.Warnings) != len(fx.Expect.Warnings) {
			t.Errorf("%s: %d warnings, oracle %d: %+v", v, len(res.Warnings), len(fx.Expect.Warnings), res.Warnings)
		}
	}
	for _, f := range files {
		if base := filepath.Base(f); !used[base] {
			t.Errorf("DDL snapshot %s is used by no fixture", base)
		}
	}
	for ddl := range used {
		if !strings.HasPrefix(ddl, "addressbook") {
			t.Errorf("fixture names DDL %q", ddl)
		}
		if _, err := os.Stat(filepath.Join(schemasPath, ddl)); err != nil {
			t.Error(err)
		}
	}
}

func TestParserIsDeterministic(t *testing.T) {
	for _, v := range variants {
		h := parsertest.NewHarness(t)
		b := bundleOf(h, parsertest.LoadFixture(t, "testdata", v), v)
		p := iosaddressbook.Parser{}
		parsertest.AssertDeterministic(t, func() parsertest.Result {
			res := h.RunLenient(p, b.Input(h, p, false))
			if res.Err != nil || len(res.Violations) != 0 || len(res.Records) == 0 {
				t.Errorf("%s: err %v, violations %v, %d records", v, res.Err, res.Violations, len(res.Records))
			}
			return res
		})
	}
}

func TestParserHonoursCancel(t *testing.T) {
	h := parsertest.NewHarness(t)
	fx := parsertest.LoadFixture(t, "testdata", "current")
	b := bundleOf(h, fx, "current")
	p := iosaddressbook.Parser{}
	res := h.AssertHonoursCancel(p, b.Input(h, p, false), 10*time.Second)
	if len(res.Violations) != 0 {
		t.Errorf("contract violations: %v", res.Violations)
	}
	// a parser that ignored its context would still emit every row of a small database
	if want := len(fx.Expect.Records); len(res.Records) >= want {
		t.Errorf("%d records on an already cancelled context, the database holds %d", len(res.Records), want)
	}
}

// One person with 1200 phones and 1100 e-mail addresses (the huge-contact fixture) is cut at 1000 per list with
// the totals and the truncation flags stated, and the cut keeps the first values in storage order.
func TestHugeContactIsCappedWithCounts(t *testing.T) {
	h := parsertest.NewHarness(t)
	b := bundleOf(h, parsertest.LoadFixture(t, "testdata", "huge-contact"), "huge-contact")
	p := iosaddressbook.Parser{}
	res := h.RunLenient(p, b.Input(h, p, false))
	if res.Err != nil || len(res.Violations) != 0 || len(res.Records) != 1 {
		t.Fatalf("err %v, violations %v, %d records", res.Err, res.Violations, len(res.Records))
	}
	r := res.Records[0]
	raw := rawIn(r)
	phones, emails := asList(r.Payload["phones"]), asList(r.Payload["emails"])
	if len(phones) != 1000 || raw["phones_total"] != int64(1200) || raw["phones_truncated"] != true ||
		len(emails) != 1000 || raw["emails_total"] != int64(1100) || raw["emails_truncated"] != true {
		t.Fatalf("caps: %d phones (total %v, truncated %v), %d e-mails (total %v, truncated %v)",
			len(phones), raw["phones_total"], raw["phones_truncated"], len(emails), raw["emails_total"], raw["emails_truncated"])
	}
	if first := asMap(phones[0])["value"]; first != "+15550000000" {
		t.Errorf("first kept phone %v: the cut must keep the first values", first)
	}
	if last := asMap(phones[999])["value"]; last != "+15550000999" {
		t.Errorf("last kept phone %v", last)
	}
}

// A person with 100,000 multi-value rows (a hostile database): the joined rows are never held beyond the budget.
// With a small budget the run ends with the typed parse.ErrBudget; it never allocates for the declared size and
// never turns the refusal into an undecided join. With the default budget the lists are capped with totals.
func TestHostileFanOutIsBoundedByTheBudget(t *testing.T) {
	const rows = 100000
	b := sqlitetest.New(sqlitetest.Options{PageSize: 4096})
	b.CreateTable("ABPerson", "create table ABPerson(ROWID integer primary key autoincrement, First text, Last text)").
		Insert(1, int64(1), "Hostile", "Fan")
	mv := b.CreateTable("ABMultiValue", "create table ABMultiValue(UID integer primary key, record_id integer, property integer, label integer, value text)")
	// InsertRaw: the table's Insert looks the rowid up linearly, which is quadratic for 100,000 rows. The record is
	// UID (NULL, the rowid alias), record_id 1, property 3 (a phone), label NULL and the 8-byte value text.
	serials := []uint64{0, 9, 1, 0, 13 + 2*8}
	body := append([]byte{3}, "+1555000"...)
	for i := int64(1); i <= rows; i++ {
		mv.InsertRaw(i, serials, body)
	}
	db := b.Bytes()
	run := func(budget int64) parsertest.Result {
		h := parsertest.NewHarness(t)
		bundle := parsertest.Fixture{Name: "fan", Files: map[string][]byte{"db": db}}.BundleIOS(h, remote, logical)
		p := iosaddressbook.Parser{}
		in := bundle.Input(h, p, false)
		if budget > 0 {
			in.Budget = parse.NewBudget(budget).View()
		}
		return h.RunLenient(p, in)
	}
	small := run(2 << 20)
	if !errors.Is(small.Err, parse.ErrBudget) {
		t.Errorf("small budget: err %v (%d records), want parse.ErrBudget (a typed refusal, not an unbounded allocation and not a silent undecided join)", small.Err, len(small.Records))
	}
	big := run(0)
	if big.Err != nil && !errors.Is(big.Err, parse.ErrBudget) {
		t.Fatalf("default budget: err %v", big.Err)
	}
	if big.Err == nil {
		if len(big.Records) != 1 {
			t.Fatalf("default budget: %d records", len(big.Records))
		}
		raw := rawIn(big.Records[0])
		if len(asList(big.Records[0].Payload["phones"])) != 1000 || raw["phones_total"] != int64(rows) || raw["phones_truncated"] != true {
			t.Errorf("default budget: %d phones, raw %v", len(asList(big.Records[0].Payload["phones"])), raw)
		}
	}
	t.Logf("small budget: err %v; default budget: err %v", small.Err, big.Err)
}
