// Package parse is the contract between the artifact-parser host and the
// parsers. A parser is a pure function of the bytes of verified artifacts: it
// receives a deep copy of its Input (read-only io.ReaderAt, plain values, a
// read-only Lookuper and a BudgetView) and hands records back only through an
// Emitter, which the host implements and which forwards to the case writer.
// Nothing here can write to the case, a file or the network.
package parse

import "context"

// Parser extracts records from the artifacts of one bundle.
type Parser interface {
	// Meta describes the parser. It is called once, at registration, before
	// any input exists.
	Meta() Meta
	// Probe decides from a bounded read of the primary input whether the
	// parser applies. It emits nothing.
	Probe(ctx context.Context, in *Input) (Applicability, error)
	// Parse emits the records of the bundle through out.
	Parse(ctx context.Context, in *Input, out Emitter) error
}

// Probe outcomes (Applicability.Status).
const (
	Applicable        = "applicable"
	NotApplicable     = "not-applicable"
	UnsupportedSchema = "unsupported-schema"
	Encrypted         = "encrypted"
)

// Applicability is the answer of Probe. Reason is short and holds no device
// content beyond table and column names.
type Applicability struct{ Status, Reason string }

// Identity names one parser build: its name, version and source hash.
type Identity struct{ Name, Version, Hash string }
