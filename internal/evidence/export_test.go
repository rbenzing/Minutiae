package evidence

// V1Statements exposes the schema v1 DDL to the external test package, which
// may import internal/records/recordstest (an internal test cannot: import cycle).
var V1Statements = v1Statements
