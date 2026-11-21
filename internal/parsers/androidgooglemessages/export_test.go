package androidgooglemessages

import (
	"github.com/rbenzing/minutiae/internal/decode/sqlitedb/rowrun"
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
)

// IDCost is what the decided message id set charges per id.
const IDCost = idCost

// CountingSteps are the live steps of one job (the joins of Parse, no guard) whose every Get and Match is reported to on
// with the step (the table being mapped), the operation ("get" or "match") and the table it asks. memberBudget and idBudget
// are what the member cache and the id set charge (nil: the job's own, in.Budget).
func CountingSteps(in *parse.Input, memberBudget, idBudget *parse.BudgetView, on func(step, op, table string)) []rowrun.Step {
	if memberBudget == nil {
		memberBudget = in.Budget
	}
	if idBudget == nil {
		idBudget = in.Budget
	}
	cnt := &counts{}
	e := &env{Members: newMemberCache(memberBudget, cnt), IDs: newIDSet(idBudget), Counts: cnt}
	steps := liveSteps(e)
	for i := range steps {
		mp := &steps[i].Mapping
		inner, table := mp.Map, mp.Table
		mp.Map = func(m parse.MapContext, row parse.Row) ([]records.Record, error) {
			return inner(countingCtx{MapContext: m, on: on, step: table}, row)
		}
	}
	return steps
}

type countingCtx struct {
	parse.MapContext
	on   func(step, op, table string)
	step string
}

func (c countingCtx) Match(table, col string, key parse.JoinKey) ([]parse.Row, error) {
	c.on(c.step, "match", table)
	return c.MapContext.Match(table, col, key)
}

func (c countingCtx) Get(table string, rowid int64) (parse.Row, bool, error) {
	c.on(c.step, "get", table)
	return c.MapContext.Get(table, rowid)
}

// AutoincrementID is autoincrementID (spec 8.3 condition 1).
func AutoincrementID(createSQL string) bool { return autoincrementID(createSQL) }
