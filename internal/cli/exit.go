package cli

import (
	"errors"
	"fmt"
	"strings"

	"github.com/rbenzing/minutiae/internal/artparse"

	"github.com/spf13/cobra"

	"github.com/rbenzing/minutiae/internal/device"
	"github.com/rbenzing/minutiae/internal/evidence"
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
// or encrypted structures, unknown artifacts): exit 1 with a message naming
// the structure, not an integrity failure of the case itself.
var imageErrors = []error{
	filesys.ErrCorrupt, filesys.ErrUnsupported, filesys.ErrEncrypted, filesys.ErrDeleted,
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
	case errors.As(err, &ue):
		return ExitUsage
	case errors.Is(err, evidence.ErrNeedsUpgrade), errors.Is(err, records.ErrBadCursor),
		errors.Is(err, records.ErrInvalidFilter), errors.Is(err, records.ErrInvalidPage),
		errors.Is(err, artparse.ErrSelection), errors.Is(err, artparse.ErrNotParserInput),
		errors.Is(err, records.ErrInvalidQuery), errors.Is(err, records.ErrIndexNotCurrent):
		return ExitUsage
	case errors.Is(err, evidence.ErrIntegrity):
		return ExitIntegrity
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
