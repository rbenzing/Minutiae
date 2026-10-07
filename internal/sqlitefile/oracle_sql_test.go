package sqlitefile_test

// The SQL statements of the oracle generators are written out per table, as
// literals: the single-writer scan (internal/archtest) refuses a statement whose
// table name is a variable, and the fixture tables are known.

import "fmt"

var insertABSQL = map[string]string{
	"t":     "insert into t(rowid,a,b) values(?,?,?)",
	"u":     "insert into u(rowid,a,b) values(?,?,?)",
	"g":     "insert into g(rowid,a,b) values(?,?,?)",
	"gl":    "insert into gl(rowid,a,b) values(?,?,?)",
	"gone3": "insert into gone3(rowid,a,b,c) values(?,?,?,?)",
}

var updateABSQL = map[string]string{
	"t": "update t set a=?, b=? where rowid=?",
}

var deleteSQL = map[string]string{
	"t": "delete from t where rowid=?",
}

var aliasInsertSQL = map[string]string{
	"al":    "insert into al(id) values(null)",
	"dsc":   "insert into dsc(id) values(null)",
	"sq":    "insert into sq(id) values(null)",
	"over":  "insert into over(id) values(null)",
	"overt": "insert into overt(id) values(null)",
}

var aliasProbeSQL = map[string]string{
	"al":    "select id is null from al where rowid = last_insert_rowid()",
	"dsc":   "select id is null from dsc where rowid = last_insert_rowid()",
	"sq":    "select id is null from sq where rowid = last_insert_rowid()",
	"over":  "select id is null from over where rowid = last_insert_rowid()",
	"overt": "select id is null from overt where rowid = last_insert_rowid()",
}

var rowsInsertSQL = map[string]string{
	"a": "with recursive c(i) as (select %d union all select i+1 from c where i<%d) insert into a(v) select printf('row%%06d-', i) || hex(zeroblob(%d)) from c",
	"b": "with recursive c(i) as (select %d union all select i+1 from c where i<%d) insert into b(v) select printf('row%%06d-', i) || hex(zeroblob(%d)) from c",
	"c": "with recursive c(i) as (select %d union all select i+1 from c where i<%d) insert into c(v) select printf('row%%06d-', i) || hex(zeroblob(%d)) from c",
}

// rowsInsert is a statement inserting rows from..to of about width bytes into
// table tb (a, b or c).
func rowsInsert(tb string, from, to, width int) string {
	q, ok := rowsInsertSQL[tb]
	if !ok {
		panic("rowsInsert: unknown table " + tb)
	}
	return fmt.Sprintf(q, from, to, width/2)
}

func mustSQL(m map[string]string, table string) string {
	q, ok := m[table]
	if !ok {
		panic("no statement for table " + table)
	}
	return q
}
