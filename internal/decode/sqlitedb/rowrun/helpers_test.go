package rowrun_test

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/sqlitefile/sqlitetest"
)

// fakeMC is a MapContext with no database: Where only needs Input.
type fakeMC struct{ in *parse.Input }

func (f fakeMC) Input() *parse.Input { return f.in }
func (fakeMC) Note(string, string)   {}
func (fakeMC) Column(string, string) int {
	return -1
}
func (fakeMC) Get(string, int64) (parse.Row, bool, error) { return nil, false, nil }
func (fakeMC) Match(string, string, parse.JoinKey) ([]parse.Row, error) {
	return nil, nil
}

var _ parse.MapContext = fakeMC{}

// dbInput is an Input over the bytes of one database in the role "db", with a
// budget no test here can exhaust.
func dbInput(t testing.TB, db []byte) *parse.Input {
	t.Helper()
	art := parse.Artifact{
		ID: "A-DB", Size: int64(len(db)), R: bytes.NewReader(db),
		Platform: parse.PlatformAndroid, Logical: "android:/data/x/t.db",
	}
	return &parse.Input{
		Primary: art, Artifacts: map[string]parse.Artifact{"db": art},
		Limits: parse.DefaultLimits(), Budget: parse.NewBudget(1 << 30).View(),
	}
}

// tinyDB is t(id integer primary key, a integer, s text) with n rows: a = 10*id.
func tinyDB(t testing.TB, n int) []byte {
	t.Helper()
	b := sqlitetest.New(sqlitetest.Options{PageSize: 1024})
	tb := b.CreateTable("t", "create table t(id integer primary key, a integer, s text)")
	for i := 1; i <= n; i++ {
		tb.Insert(int64(i), int64(i), int64(i)*10, fmt.Sprintf("row-%d", i))
	}
	return b.Bytes()
}
