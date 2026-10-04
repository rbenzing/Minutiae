package parse

import "errors"

// Sentinel errors of the parser contract. The host wraps them with context;
// callers test with errors.Is.
var (
	// ErrBudget: a memory allocation would exceed the host's budget (or the
	// budget is missing, which fails closed).
	ErrBudget = errors.New("parse: memory budget exceeded")
	// ErrSealed: the input was read after the job ended or was abandoned.
	ErrSealed = errors.New("parse: input is sealed")
	// ErrRecordCap: the job emitted more records than Limits.MaxRecords.
	ErrRecordCap = errors.New("parse: record cap reached")
	// ErrRejectedCap: the job had more rejected records than Limits.MaxRejected.
	ErrRejectedCap = errors.New("parse: too many rejected records")
	// ErrProbeLimit: Probe read more than Limits.ProbeBytes.
	ErrProbeLimit = errors.New("parse: probe read limit reached")
	// ErrPayloadVersionMismatch: Meta.Emits declares a payload version other
	// than the one the record type is registered with.
	ErrPayloadVersionMismatch = errors.New("parse: payload version differs from the registered one")
	// ErrNoPayloadContract: a declared record type has no payload validator.
	ErrNoPayloadContract = errors.New("parse: record type has no payload contract")
	// ErrUndeclaredType: a record's type is not in Meta.Emits.
	ErrUndeclaredType = errors.New("parse: record type not declared in Meta.Emits")
)
