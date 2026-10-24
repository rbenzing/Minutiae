package cli

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/rbenzing/minutiae/internal/evidence"
)

// anchorFlags are the anchor inputs of `case verify`.
type anchorFlags struct {
	file, head, key string
}

func (a *anchorFlags) register(cmd *cobra.Command) {
	cmd.Flags().StringVar(&a.file, "anchor", "", "audit anchor file (from case anchor) to check the audit log against")
	cmd.Flags().StringVar(&a.head, "anchor-head", "", "audit anchor as SEQ:HASH (a head written down on paper) to check the audit log against")
	cmd.Flags().StringVar(&a.key, "anchor-key", "", "trusted Ed25519 public key (PEM): the anchor must be signed by exactly this key")
}

// anchorOpts is the verify input read from the anchor flags, before any case is opened.
type anchorOpts struct{ o evidence.VerifyOptions }

// with returns the options with the progress observer set.
func (a anchorOpts) with(p *evidence.VerifyProgress) evidence.VerifyOptions {
	a.o.Progress = p
	return a.o
}

// options validates the flag combination (a usage error, exit 2) and reads the anchor and key files (a
// malformed one is a plain error, exit 1: it is rejected before any verify runs, never a pass).
func (a *anchorFlags) options() (anchorOpts, error) {
	if a.file != "" && a.head != "" {
		return anchorOpts{}, usageErrorf("--anchor and --anchor-head are exclusive")
	}
	if a.key != "" && a.file == "" && a.head == "" {
		return anchorOpts{}, usageErrorf("--anchor-key needs --anchor or --anchor-head")
	}
	var r anchorOpts
	switch {
	case a.file != "":
		b, err := readLimited(a.file)
		if err != nil {
			return anchorOpts{}, fmt.Errorf("anchor file %s: %w", a.file, err)
		}
		an, err := evidence.ParseAnchor(b)
		if err != nil {
			return anchorOpts{}, fmt.Errorf("anchor file %s: %w", a.file, err)
		}
		r.o.Anchor, r.o.AnchorPath = &an, a.file
	case a.head != "":
		an, err := evidence.ParseAnchorHead(a.head)
		if err != nil {
			return anchorOpts{}, fmt.Errorf("anchor head: %w", err)
		}
		r.o.Anchor = &an
	}
	if a.key != "" {
		b, err := readLimited(a.key)
		if err != nil {
			return anchorOpts{}, fmt.Errorf("anchor key %s: %w", a.key, err)
		}
		pub, err := evidence.ParseAnchorPublicKey(b)
		if err != nil {
			return anchorOpts{}, fmt.Errorf("anchor key %s: %w", a.key, err)
		}
		r.o.TrustedKey = pub
	}
	return r, nil
}

// readLimited reads a regular-size anchor or key file: at most evidence.MaxAnchorFileBytes, else an error.
func readLimited(path string) ([]byte, error) {
	f, err := os.Open(path) //nolint:gosec // the examiner names this file
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, evidence.MaxAnchorFileBytes+1))
	if err != nil {
		return nil, err
	}
	if len(b) > evidence.MaxAnchorFileBytes {
		return nil, fmt.Errorf("larger than %d bytes", evidence.MaxAnchorFileBytes)
	}
	return b, nil
}

// printAnchorLine is the one ANCHOR line of the text report (nothing without an anchor).
func printAnchorLine(w io.Writer, r *evidence.AnchorResult) {
	switch {
	case r == nil:
	case r.OK:
		fmt.Fprintf(w, "ANCHOR: matches up to audit seq %d; %d entries follow it\n", r.Seq, r.After)
	default:
		fmt.Fprintln(w, "ANCHOR: FAILED (see the anchor problems)")
	}
}

// insideDir reports whether p lies inside dir (evidence.PathInside: the one implementation, shared with verify).
func insideDir(dir, p string) (bool, error) { return evidence.PathInside(dir, p) }

// writeNewFile creates path with O_EXCL (an existing file, or a symlink, is an error), writes data and syncs.
func writeNewFile(path string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm) //nolint:gosec // the examiner names this file
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

func newCaseAnchorCmd(d Deps, opts *rootOptions) *cobra.Command {
	var path, out, signKey string
	cmd := &cobra.Command{
		Use:   "anchor",
		Short: "Record an audit entry and print an anchor of the audit log to store outside the case",
		Args:  exactArgs(0),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if path == "" {
				return usageErrorf("--case is required")
			}
			for _, f := range []struct{ flag, p string }{{"--out", out}, {"--sign-key", signKey}} {
				if f.p == "" {
					continue
				}
				in, err := insideDir(path, f.p)
				if err != nil {
					return err
				}
				if in {
					return usageErrorf("%s %s lies inside the case directory: an anchor or key kept there is only as safe as the case; name a path outside it", f.flag, f.p)
				}
			}
			var signer ed25519.PrivateKey
			if signKey != "" {
				b, err := readLimited(signKey)
				if err != nil {
					return fmt.Errorf("sign key %s: %w", signKey, err)
				}
				if signer, err = evidence.ParseAnchorPrivateKey(b); err != nil {
					return fmt.Errorf("sign key %s: %w", signKey, err)
				}
			}
			if out != "" { // fail before the audit entry when the file is already there; O_EXCL below is the real guard
				if _, err := os.Lstat(out); err == nil {
					return fmt.Errorf("anchor file %s: %w", out, fs.ErrExist)
				}
			}
			c, err := openCase(path)
			if err != nil {
				return err
			}
			defer func() { _ = c.Close() }()
			absOut := ""
			if out != "" {
				if absOut, err = filepath.Abs(out); err != nil {
					return err
				}
			}
			an, err := c.Anchor(cmd.Context(), signer, absOut)
			if err != nil {
				return err
			}
			if out != "" {
				if err := writeAnchorFile(out, an.Marshal(), 0o600); err != nil {
					return fmt.Errorf("anchor file %s was not written completely (it may exist partially; it is not removed): the case.anchor audit entry (seq %d) was written, re-anchor to a new path: %w", escapeText(out), an.Seq, err)
				}
			}
			if opts.json {
				_, err := d.Out.Write(an.Marshal())
				return err
			}
			line := fmt.Sprintf("anchor: case %s audit seq %d sha256 %s", escapeText(an.CaseID), an.Seq, an.Hash)
			if an.Signature != nil {
				if raw, err := hex.DecodeString(an.Signature.PublicKey); err == nil {
					line += " signed by " + evidence.KeyFingerprint(ed25519.PublicKey(raw))
				}
			}
			fmt.Fprintln(d.Out, line)
			if out != "" {
				fmt.Fprintf(d.Out, "anchor written to %s\n", escapeText(out))
			} else {
				fmt.Fprintln(d.Out, "store this anchor outside the case (case anchor --out FILE writes it to a file)")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&path, "case", "", "case directory")
	cmd.Flags().StringVar(&out, "out", "", "write the anchor to this new file (outside the case directory)")
	cmd.Flags().StringVar(&signKey, "sign-key", "", "Ed25519 private key (PEM, outside the case directory) to sign the anchor with")
	_ = cmd.MarkFlagRequired("case")
	return cmd
}

func newCaseAnchorKeygenCmd(d Deps, _ *rootOptions) *cobra.Command {
	var out string
	cmd := &cobra.Command{
		Use:   "anchor-keygen",
		Short: "Create an Ed25519 key pair for signing audit anchors (private key unencrypted: keep it off the evidence host)",
		Args:  exactArgs(0),
		RunE: func(_ *cobra.Command, _ []string) error {
			if out == "" {
				return usageErrorf("--out is required")
			}
			pubPath := out + ".pub"
			for _, p := range []string{out, pubPath} {
				if _, err := os.Lstat(p); err == nil {
					return fmt.Errorf("%s: %w", p, fs.ErrExist)
				}
			}
			priv, pub, fp, err := evidence.GenerateAnchorKey(rand.Reader)
			if err != nil {
				return err
			}
			if err := writeNewFile(out, priv, 0o600); err != nil {
				return err
			}
			if err := writeNewFile(pubPath, pub, 0o644); err != nil { //nolint:gosec // a public key
				err = errors.Join(err, os.Remove(out)) // our own new file, written a moment ago
				return err
			}
			fmt.Fprintf(d.Out, "private key: %s (unencrypted; keep it outside the case and off the evidence host)\npublic key: %s\npublic key fingerprint: %s\n",
				escapeText(out), escapeText(pubPath), fp)
			return nil
		},
	}
	cmd.Flags().StringVar(&out, "out", "", "private key file to create (the public key goes to FILE.pub)")
	return cmd
}

// writeAnchorFile writes the anchor file of case anchor (a variable so a test can make the write fail).
var writeAnchorFile = writeNewFile
