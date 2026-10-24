package cli

import (
	"context"
	"fmt"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/spf13/cobra"
)

// recordsVerifyFlag adds --verify ("run the full case verify before reading; refuse to print on failure").
func recordsVerifyFlag(cmd *cobra.Command) *bool {
	return cmd.Flags().Bool("verify", false, "run the full case verify before reading; print nothing and exit 4 if it finds a problem")
}

// recordsPreamble runs before a records read: with verify, the full case verify (runVerify, every composed
// check, one verify.run entry), returning evidence.ErrIntegrity (problems printed on d.Err) or a cancelled
// error; without it, the notice that nothing was verified. It never writes to d.Out. It runs with no progress
// observer, so a long verify prints nothing until it ends.
func recordsPreamble(ctx context.Context, d Deps, c *evidence.Case, verify bool) error {
	if !verify {
		fmt.Fprintln(d.Err, lastVerifyNotice(c.Audit))
		return nil
	}
	rep, err := runVerify(ctx, c, nil)
	if err != nil {
		return err
	}
	for _, n := range rep.Notices {
		fmt.Fprintln(d.Err, "NOTICE:", escapeText(n))
	}
	for _, p := range rep.Problems {
		fmt.Fprintln(d.Err, "PROBLEM:", escapeText(p))
	}
	if rep.CancelledOnly() {
		return fmt.Errorf("verify cancelled: %w", context.Canceled)
	}
	if !rep.OK() {
		return fmt.Errorf("%w: %d problem(s); nothing was read", evidence.ErrIntegrity, len(rep.Problems))
	}
	seq, _ := c.Audit.Head()
	fmt.Fprintf(d.Err, "verified now: %d artifacts, %d records, 0 problems (audit seq %d)\n", rep.ArtifactsChecked, rep.RecordsChecked, seq)
	return nil
}

// lastVerifyNotice is the notice text: history read from the audit log at open, never a verification.
func lastVerifyNotice(a *evidence.AuditLog) string {
	head, _ := a.Head()
	e, ok := a.Last("verify.run")
	if !ok {
		return fmt.Sprintf("NOTICE: not verified: records are read as stored; no case verify is recorded in the audit log (the log ends at seq %d); use --verify or minutiae case verify", head)
	}
	outcome := "outcome unknown"
	if v, isBool := e.Details["ok"].(bool); isBool {
		outcome = "FAILED"
		if v {
			outcome = "ok"
		}
	}
	return fmt.Sprintf("NOTICE: not verified: records are read as stored; the last case verify in the audit log is seq %d (%s, %s); the log ends at seq %d; use --verify or minutiae case verify",
		e.Seq, escapeText(e.Time), outcome, head)
}
