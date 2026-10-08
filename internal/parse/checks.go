package parse

import (
	"fmt"

	"github.com/rbenzing/minutiae/internal/records"
)

// This file holds the contract rules that can be decided in one run, as pure
// functions over plain values. The host (internal/artparse) and the strict test
// harness (internal/parsers/parsertest) both call them, so the two cannot
// disagree about what a parser may do. Hashing is injected (CheckInputsUnchanged)
// because this package stays free of crypto and I/O.

// MaxWarnings is the most warnings one job may report. It equals the cap the
// records writer applies per ingest, so a parser over it is spamming the audit
// log, not just being verbose.
const MaxWarnings = 10_000

// InputChangedError names the artifact whose bytes no longer hash to what the
// manifest recorded.
type InputChangedError struct{ ArtifactID, Want, Got string }

func (e *InputChangedError) Error() string {
	return fmt.Sprintf("parse: input %s changed during the run: manifest sha256 %s, now %s", e.ArtifactID, e.Want, e.Got)
}

// Unwrap returns ErrInputChanged.
func (e *InputChangedError) Unwrap() error { return ErrInputChanged }

// CheckRecordType requires recordType to be one of m.Emits.
func CheckRecordType(m Meta, recordType string) error {
	for _, e := range m.Emits {
		if e.Type == recordType {
			return nil
		}
	}
	return fmt.Errorf("%w: %q (parser %s)", ErrUndeclaredType, recordType, m.Name)
}

// CheckPlatform requires the platform of artifact a to be one of m.Platforms.
func CheckPlatform(m Meta, a Artifact) error {
	for _, p := range m.Platforms {
		if p == a.Platform && p != "" {
			return nil
		}
	}
	return fmt.Errorf("%w: artifact %s is %q, parser %s declares %v", ErrForeignPlatform, a.ID, a.Platform, m.Name, m.Platforms)
}

// CheckRecord applies CheckRecordType and, when the record's artifact is in the
// bundle, CheckPlatform. An artifact outside the bundle is the records writer's
// to refuse (StartOptions.Artifacts), not this rule's.
func CheckRecord(m Meta, bundle []Artifact, r records.Record) error {
	if err := CheckRecordType(m, r.Type); err != nil {
		return err
	}
	for _, a := range bundle {
		if a.ID == r.ArtifactID {
			return CheckPlatform(m, a)
		}
	}
	return nil
}

// CheckWarningCount fails when n exceeds MaxWarnings.
func CheckWarningCount(n int) error {
	if n > MaxWarnings {
		return fmt.Errorf("%w: %d (cap %d)", ErrWarningCap, n, MaxWarnings)
	}
	return nil
}

// CheckRefusals fails when the writer refused n > 0 records: a refusal is
// counted and warned by the host, but a parser that provokes one has a bug.
func CheckRefusals(n int) error {
	if n > 0 {
		return fmt.Errorf("%w: %d", ErrRefusedRecords, n)
	}
	return nil
}

// CheckInputsUnchanged re-hashes EVERY artifact in want with rehash and
// requires the result to equal the artifact's SHA256. A rehash error is
// returned as it is (it is not an integrity verdict).
func CheckInputsUnchanged(want []Artifact, rehash func(Artifact) (string, error)) error {
	for _, a := range want {
		got, err := rehash(a)
		if err != nil {
			return err
		}
		if got != a.SHA256 {
			return &InputChangedError{ArtifactID: a.ID, Want: a.SHA256, Got: got}
		}
	}
	return nil
}
