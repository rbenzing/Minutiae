package androidgooglemessages

import (
	"github.com/rbenzing/minutiae/internal/parse"
	"github.com/rbenzing/minutiae/internal/records"
)

// CountingMapping is the live messages mapping of one job (the joins of Parse, no guard) whose every Match is reported to
// onMatch with the table it asks. budget is the budget the member cache charges (nil: the job's own, in.Budget).
func CountingMapping(in *parse.Input, budget *parse.BudgetView, onMatch func(table string)) parse.TableMapping {
	if budget == nil {
		budget = in.Budget
	}
	cnt := &counts{}
	mp := messagesMapping(&env{Members: newMemberCache(budget, cnt), Counts: cnt})
	inner := mp.Map
	mp.Map = func(m parse.MapContext, row parse.Row) ([]records.Record, error) {
		return inner(countingCtx{MapContext: m, on: onMatch}, row)
	}
	return mp
}

type countingCtx struct {
	parse.MapContext
	on func(table string)
}

func (c countingCtx) Match(table, col string, key parse.JoinKey) ([]parse.Row, error) {
	c.on(table)
	return c.MapContext.Match(table, col, key)
}
