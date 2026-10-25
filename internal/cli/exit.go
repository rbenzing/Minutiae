package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/rbenzing/minutiae/internal/artparse"
	"github.com/rbenzing/minutiae/internal/dbrecover"
	"github.com/rbenzing/minutiae/internal/device"
	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/image"
	"github.com/rbenzing/minutiae/internal/records"
)

// Process exit codes. Documented in the spec §8; scripts depend on them.
const (
	ExitOK        = 0
	ExitError     = 1
	ExitUsage     = 2
	ExitDevice    = 3
	ExitIntegrity = 4
)

type usageError struct{ err error }

func (e usageError) Error() string { return e.err.Error() }
func (e usageError) Unwrap() error { return e.err }

func usageErrorf(format string, a ...any) error {
	return usageError{fmt.Errorf(format, a...)}
}

func exactArgs(n int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) != n {
			return usageErrorf("%s: expected %d argument(s), got %d", cmd.CommandPath(), n, len(args))
		}
		return nil
	}
}

func minArgs(n int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) < n {
			return usageErrorf("%s: expected at least %d argument(s), got %d", cmd.CommandPath(), n, len(args))
		}
		return nil
	}
}

// cobra reports these as plain errors; they are usage problems.
var cobraUsagePrefixes = []string{
	"unknown command", "unknown flag", "unknown shorthand flag",
	"required flag", "invalid argument", "flag needs an argument",
}

func isCobraUsageError(err error) bool {
	msg := err.Error()
	for _, p := range cobraUsagePrefixes {
		if strings.HasPrefix(msg, p) {
			return true
		}
	}
	return false
}

// imageErrors are problems with the content of an image (corrupt, unsupported
// or encrypted structures, unknown artifacts, a lookup that cannot be decided):
// exit 1 with a message naming the structure, not an integrity failure of the
// case itself. filesys.ErrUndecided is never exit 0 and never "not found".
var imageErrors = []error{
	filesys.ErrCorrupt, filesys.ErrUnsupported, filesys.ErrEncrypted, filesys.ErrDeleted,
	filesys.ErrNotDeleted, filesys.ErrNoRecovery, filesys.ErrUndecided, filesys.ErrIncomplete, filesys.ErrLimit, examine.ErrInsufficientSpace,
	image.ErrUnsupportedContainer, image.ErrCorruptContainer, image.ErrChunkCorrupt, evidence.ErrUnknownArtifact,
}

var deviceErrors = []error{
	device.ErrNotFound, device.ErrUnauthorized, device.ErrNotRooted,
	device.ErrUnsupported, device.ErrDeviceWriteNotAllowed,
}

// ExitCode maps an error returned by a command to a process exit code.
func ExitCode(err error) int {
	var ue usageError
	switch {
	case err == nil:
		return ExitOK
	case errors.Is(err, evidence.ErrIntegrity):
		// first: an integrity failure joined with any other error is still exit 4 (J96)
		return ExitIntegrity
	case errors.As(err, &ue):
		return ExitUsage
	case errors.Is(err, evidence.ErrNeedsUpgrade), errors.Is(err, records.ErrBadCursor),
		errors.Is(err, records.ErrInvalidFilter), errors.Is(err, records.ErrInvalidPage),
		errors.Is(err, artparse.ErrSelection), errors.Is(err, artparse.ErrNotParserInput),
		errors.Is(err, records.ErrInvalidQuery), errors.Is(err, records.ErrIndexNotCurrent), errors.Is(err, examine.ErrInvalidLimit):
		return ExitUsage
	case isDBRecoverUsage(err):
		return ExitUsage
	}
	for _, ie := range imageErrors {
		if errors.Is(err, ie) {
			return ExitError
		}
	}
	for _, de := range deviceErrors {
		if errors.Is(err, de) {
			return ExitDevice
		}
	}
	return ExitError
}

// isDBRecoverUsage says whether err is a bad option or an ambiguous companion of sqlite recover: the
// examiner fixes the command line, so it is a usage error. The other refusals (not-sqlite, live-unavailable,
// ineligible-kind, sibling-unusable) are exit 1.
func isDBRecoverUsage(err error) bool {
	if errors.Is(err, dbrecover.ErrAmbiguousSibling) {
		return true
	}
	var re *dbrecover.RefusalError
	return errors.As(err, &re) && (re.Reason == dbrecover.ReasonBadOption || re.Reason == dbrecover.ReasonAmbiguousSibling)
}

// undecidedNote follows the message of a filesys.ErrUndecided error: the answer is neither "found" nor
// "absent", and nothing was skipped or recorded as missing.
const undecidedNote = "note: the lookup is neither found nor absent: a casefold directory holds a name of a shape the kernel's comparison was not measured for, so it cannot be shown that the name does not exist. Nothing was skipped; list the directory (image ls) to see the names."

// undecidedBatchNote follows the message of a batch recovery in which some entries could not be decided: those
// entries were not recovered and are not shown absent, while the decided ones were written.
const undecidedBatchNote = "note: the entries that could not be decided were skipped (neither recovered nor shown absent); the others were handled and any artifacts written. The skipped lines and the analysis.warning entries in audit.jsonl name each cause."
