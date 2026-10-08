package sqlitedb

import "github.com/rbenzing/minutiae/internal/sqlitefile"

// Budget accounts for the memory the decoder holds. *parse.BudgetView
// satisfies it.
type Budget = sqlitefile.Budget
