package cli

import (
	"cmp"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/examine"
	"github.com/rbenzing/minutiae/internal/filesys"
	"github.com/rbenzing/minutiae/internal/image"
)

func newImageCmd(d Deps, opts *rootOptions) *cobra.Command {
	cmd := newGroupCmd("image", "Import disk images into a case and examine their partitions and filesystems")
	cmd.AddCommand(
		newImageImportCmd(d, opts), newImageInfoCmd(d, opts), newImageLsCmd(d, opts),
		newImageStatCmd(d, opts), newImageExtractCmd(d, opts), newImageUnallocCmd(d, opts),
	)
	return cmd
}

// printable returns s unchanged when it is made only of printable characters;
// otherwise it returns the strconv.Quote form, so filesystem-supplied names
// and text can never inject control characters, escape sequences or bidi
// overrides into the examiner's terminal. JSON output does not need this.
func printable(s string) string {
	if utf8.ValidString(s) && !strings.ContainsFunc(s, func(r rune) bool { return r != ' ' && !unicode.IsPrint(r) }) {
		return s
	}
	return strconv.Quote(s)
}

// partitionFlag registers -p/--partition (default -1: the only partition with
// a recognized filesystem) and returns a getter that validates it.
func partitionFlag(cmd *cobra.Command, usage string) func() (int, error) {
	p := cmd.Flags().IntP("partition", "p", -1, usage)
	return func() (int, error) {
		if cmd.Flags().Changed("partition") && *p < 0 {
			return 0, usageErrorf("--partition must be 0 or greater, got %d", *p)
		}
		return *p, nil
	}
}

func caseFlag(cmd *cobra.Command) *string {
	s := cmd.Flags().String("case", "", "case directory")
	_ = cmd.MarkFlagRequired("case")
	return s
}

// openImageSession opens the case and the image artifact ref. The returned
// function closes both. A parent flagged incomplete is reported on stderr.
func openImageSession(d Deps, casePath, ref string) (*examine.Session, func(), error) {
	s, closeAll, err := openImageContainer(d, casePath, ref)
	if err != nil {
		return nil, nil, err
	}
	if err := s.ReadPartitions(); err != nil {
		closeAll()
		return nil, nil, err
	}
	warnIncomplete(d, s)
	return s, closeAll, nil
}

// openImageContainer is openImageSession without reading the partition table
// (examine.OpenContainer): what only needs the container, such as verifying
// it, must not depend on the partition table being readable.
func openImageContainer(d Deps, casePath, ref string) (*examine.Session, func(), error) {
	c, err := openCase(casePath)
	if err != nil {
		return nil, nil, err
	}
	s, err := examine.OpenContainer(c, ref, examine.Options{Drivers: d.FSDrivers})
	if err != nil {
		_ = c.Close()
		return nil, nil, err
	}
	return s, func() { _ = s.Close(); _ = c.Close() }, nil
}

// warnIncomplete reports on stderr a parent flagged incomplete.
func warnIncomplete(d Deps, s *examine.Session) {
	for _, seg := range s.Segments {
		if seg.Incomplete {
			fmt.Fprintln(d.Err, "warning: parent image is flagged incomplete; results may be partial")
			return
		}
	}
}

func newImageImportCmd(d Deps, opts *rootOptions) *cobra.Command {
	var deviceID string
	cmd := &cobra.Command{
		Use:   "import <file>...",
		Short: "Import the segment files of one image, in order",
		Long: "Import the segment files of one image, in order, into the case as hashed artifacts.\n" +
			"The files are the segments 1..N of a single image (the order given is the segment\n" +
			"order; nothing is sorted). To import several independent images, run the command once per image.",
		Args: minArgs(1),
	}
	casePath := caseFlag(cmd)
	cmd.Flags().StringVar(&deviceID, "device", "import", "label of the source the image came from")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if strings.TrimSpace(deviceID) == "" {
			return usageErrorf("--device must not be blank")
		}
		c, err := openCase(*casePath)
		if err != nil {
			return err
		}
		defer func() { _ = c.Close() }()
		recs, err := examine.Import(cmd.Context(), c, deviceID, args, newProgress(d.Err, "import"))
		fmt.Fprintln(d.Err)
		if opts.json {
			if len(recs) > 0 {
				if jerr := writeJSON(d.Out, recs); jerr != nil && err == nil {
					err = jerr
				}
			}
			return err
		}
		for _, r := range recs {
			status := ""
			if r.Incomplete {
				status = "  INCOMPLETE: " + escapeText(r.Error)
			}
			fmt.Fprintf(d.Out, "%s  %d  %s%s\n", printable(r.Path), r.Size, r.SHA256, status)
		}
		return err
	}
	return cmd
}

func newImageInfoCmd(d Deps, opts *rootOptions) *cobra.Command {
	var verify bool
	cmd := &cobra.Command{
		Use:   "info <ref>",
		Short: "Show an image's container, partition table and filesystems",
		Args:  exactArgs(1),
	}
	casePath := caseFlag(cmd)
	cmd.Flags().BoolVar(&verify, "verify", false, "verify the container's stored hashes (raw images have none)")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		open := openImageSession
		if verify {
			// Verification needs the container only: it is exactly when the
			// partition table cannot be read that it matters. The partition
			// table is read after the verification has been audited.
			open = openImageContainer
		}
		s, closeAll, err := open(d, *casePath, args[0])
		if err != nil {
			return err
		}
		defer closeAll()
		var (
			cv       *examine.ContainerVerification
			verr     error
			perr     error // the partition table could not be read (only with --verify)
			noHashes bool
		)
		if verify {
			var res examine.ContainerVerification
			wrote := false
			base := newProgress(d.Err, "verify")
			res, verr = s.VerifyContainer(cmd.Context(), func(done, total int64) { wrote = true; base(done, total) })
			if wrote {
				fmt.Fprintln(d.Err) // end the progress line
			}
			switch {
			case errors.Is(verr, examine.ErrNoStoredHashes):
				// Raw images store no hashes: nothing to verify, nothing audited.
				noHashes, verr = true, nil
			default:
				cv = &res
			}
			if perr = s.ReadPartitions(); perr == nil {
				warnIncomplete(d, s)
			}
		}
		info := s.Info()
		if !opts.json {
			printImageInfo(d.Out, info)
		}
		if noHashes {
			const note = "container has no stored hashes (raw images carry none)"
			if opts.json {
				fmt.Fprintln(d.Err, note)
			} else {
				fmt.Fprintln(d.Out, note)
			}
		}
		if opts.json {
			out := newJSONImageInfo(info)
			if cv != nil {
				out.Verify = newJSONVerify(*cv, verr)
			}
			if err := writeJSON(d.Out, out); err != nil {
				return err
			}
		} else if cv != nil {
			printVerification(d.Out, *cv)
		}
		return errors.Join(verifyOutcome(cv, verr), perr)
	}
	return cmd
}

// verifyOutcome turns a verification into the command's error: nil for a match
// or a container storing no hashes; ErrIntegrity (exit 4) for a mismatch; any
// other outcome (unreadable chunk, cancelled, parse failure, failed audit
// append) is a plain error, exit 1. The result has already been printed.
func verifyOutcome(cv *examine.ContainerVerification, verr error) error {
	if cv == nil {
		return nil
	}
	if verr != nil {
		err := fmt.Errorf("container verification: %w", verr)
		if image.HashStatus(cv.Result) == image.HashMismatch {
			// A finding of the verification is never masked by a failure to record it.
			return errors.Join(mismatchError(cv), err)
		}
		return err
	}
	switch image.HashStatus(cv.Result) {
	case image.HashMatch, image.HashAbsent:
		return nil
	case image.HashMismatch:
		return mismatchError(cv)
	}
	return fmt.Errorf("container could not be verified: %s", escapeText(unverifiedReason(*cv)))
}

// mismatchError is the integrity error (exit 4) of a mismatching hash.
func mismatchError(cv *examine.ContainerVerification) error {
	var bad []string
	if cv.MD5.Status == image.HashMismatch {
		bad = append(bad, "MD5")
	}
	if cv.SHA1.Status == image.HashMismatch {
		bad = append(bad, "SHA-1")
	}
	return fmt.Errorf("%w: the container's stored %s differs from the hash of the image data", evidence.ErrIntegrity, strings.Join(bad, " and "))
}

func unverifiedReason(cv examine.ContainerVerification) string {
	if cv.BadChunk >= 0 {
		return fmt.Sprintf("first unreadable chunk %d: %s", cv.BadChunk, cv.BadChunkError)
	}
	return fmt.Sprintf("%d of %d bytes hashed", cv.BytesHashed, cv.Size)
}

// printVerification prints the stored-versus-computed comparison. Every value
// comes from the container, so all of it goes through printable/escapeText.
func printVerification(w io.Writer, cv examine.ContainerVerification) {
	field := func(name, format string, a ...any) { fmt.Fprintf(w, "%-13s %s\n", name+":", fmt.Sprintf(format, a...)) }
	hash := func(name string, width int, h image.HashCheck) {
		stored := string(image.HashAbsent)
		switch {
		case h.Stored != "":
			stored = printable(h.Stored)
		case h.Status == image.HashUnverified:
			// The run never got to read the container's stored hash (a parser
			// panic): that is not the same as the container storing none.
			stored = "unknown"
		}
		computed := strings.Repeat(" ", len("computed ")+width)
		if h.Computed != "" {
			computed = fmt.Sprintf("computed %-*s", width, printable(h.Computed))
		}
		field("Verify", "%-7s stored %-*s  %s  %s", name, width, stored, computed, printable(string(h.Status)))
	}
	hash("MD5", 32, cv.MD5)
	hash("SHA-1", 40, cv.SHA1)
	if cv.BadChunk >= 0 {
		field("Verify", "first unreadable chunk %d: %s", cv.BadChunk, escapeText(cv.BadChunkError))
	}
	hashed := fmt.Sprintf("%d bytes hashed", cv.BytesHashed)
	if cv.BytesHashed != cv.Size {
		hashed = fmt.Sprintf("%d of %d bytes hashed", cv.BytesHashed, cv.Size)
	}
	field("Verify", "result  %s (%s)", printable(cv.Result), hashed)
}

func printImageInfo(w io.Writer, info examine.ImageInfo) {
	field := func(name, format string, a ...any) { fmt.Fprintf(w, "%-13s %s\n", name+":", fmt.Sprintf(format, a...)) }
	field("Image", "%s (artifact %s)", printable(info.Path), info.ParentID)
	field("SHA-256", "%s", info.SHA256)
	if info.Incomplete {
		field("Incomplete", "yes")
	}
	field("Format", "%s", printable(info.Format))
	field("Size", "%s (%d bytes)", humanBytes(info.Size), info.Size)
	field("Sector size", "%d", info.SectorSize)
	for _, kv := range info.Metadata {
		field(printable(kv.Key), "%s", printable(kv.Value))
	}
	if info.PartitionError != "" {
		field("Partitions", "unreadable: %s", escapeText(info.PartitionError))
		for _, warn := range info.Warnings {
			fmt.Fprintf(w, "Warning: %s\n", escapeText(warn))
		}
		return
	}
	scheme := printable(info.Scheme)
	if info.DiskGUID != "" {
		scheme += " (disk " + printable(info.DiskGUID) + ")"
	}
	field("Scheme", "%s", scheme)

	fmt.Fprintln(w, "Partitions:")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  Index\tStart\tLength\tType\tName\tFS")
	for _, pi := range info.Partitions {
		p := pi.Partition
		fs := "-"
		if pi.FSType != "" {
			fs = printable(pi.FSType)
		}
		fmt.Fprintf(tw, "  %d\t%d\t%d\t%s\t%s\t%s\n", p.Index, p.Start, p.Length, printable(typeLabel(p.TypeName, p.Type)), dash(printable(p.Name)), fs)
	}
	_ = tw.Flush()

	for _, pi := range info.Partitions {
		switch fi := pi.FSInfo; {
		case fi != nil:
			fmt.Fprintf(w, "Partition %d: %s filesystem\n", pi.Partition.Index, printable(fi.Type))
			fmt.Fprintf(w, "  Label:      %s\n", dash(printable(fi.Label)))
			fmt.Fprintf(w, "  UUID:       %s\n", dash(printable(fi.UUID)))
			fmt.Fprintf(w, "  Block size: %d\n", fi.BlockSize)
			fmt.Fprintf(w, "  Size:       %s (%d bytes)\n", humanBytes(fi.Size), fi.Size)
			fmt.Fprintf(w, "  Features:   %s\n", dash(joinPrintable(fi.Features, ", ")))
			fmt.Fprintf(w, "  Encrypted:  %s\n", yesNo(fi.Encrypted))
			if len(fi.Volumes) > 0 {
				fmt.Fprintf(w, "  Volumes:    %s\n", joinPrintable(fi.Volumes, ", "))
			}
			for _, warn := range fi.Warnings {
				fmt.Fprintf(w, "  Warning:    %s\n", escapeText(warn))
			}
		case pi.Error != "":
			fmt.Fprintf(w, "Partition %d: %s\n", pi.Partition.Index, escapeText(pi.Error))
		}
	}

	var total int64
	for _, r := range info.Unallocated {
		total += r.Length // each run lies inside the image, so the sum cannot overflow
	}
	fmt.Fprintf(w, "Unallocated (outside partitions): %s (%d bytes) in %d range(s)\n", humanBytes(total), total, len(info.Unallocated))
	for _, warn := range info.Warnings {
		fmt.Fprintf(w, "Warning: %s\n", escapeText(warn))
	}
}

func typeLabel(name, id string) string {
	switch {
	case name != "" && id != "":
		return name + " (" + id + ")"
	case name != "":
		return name
	}
	return dash(id)
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func joinPrintable(ss []string, sep string) string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = printable(s)
	}
	return strings.Join(out, sep)
}

func typeChar(t filesys.EntryType) byte {
	switch t {
	case filesys.TypeDir:
		return 'd'
	case filesys.TypeFile:
		return '-'
	case filesys.TypeSymlink:
		return 'l'
	}
	return '?'
}

// formatTime renders a timestamp: UTC RFC 3339, "YYYY-MM-DD HH:MM:SS (local)"
// when the zone is unknown, "-" when absent.
func formatTime(ts filesys.Timestamp) string {
	switch {
	case ts.T.IsZero():
		return "-"
	case ts.ZoneKnown:
		return ts.T.UTC().Format(time.RFC3339)
	}
	return ts.T.Format("2006-01-02 15:04:05") + " (local)"
}

// lsLine formats one listing line; shown is the name or path to display.
func lsLine(shown string, e filesys.Entry) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%c %04o %12d %s %s", typeChar(e.Type), e.Mode&0o7777, e.Size, formatTime(e.Times.Modified), printable(shown))
	if e.Type == filesys.TypeDir {
		b.WriteByte('/')
	}
	if e.Deleted {
		b.WriteString(" [deleted]")
	}
	if e.Encrypted {
		b.WriteString(" [encrypted]")
	}
	if e.LinkTarget != "" {
		b.WriteString(" -> " + printable(e.LinkTarget))
	}
	return b.String()
}

func newImageLsCmd(d Deps, opts *rootOptions) *cobra.Command {
	var recursive, deleted bool
	cmd := &cobra.Command{
		Use:   "ls <ref> [<path>]",
		Short: "List a directory of an image's filesystem",
		Args: func(cmd *cobra.Command, args []string) error {
			if err := minArgs(1)(cmd, args); err != nil {
				return err
			}
			if len(args) > 2 {
				return usageErrorf("%s: expected at most 2 arguments, got %d", cmd.CommandPath(), len(args))
			}
			return nil
		},
	}
	casePath := caseFlag(cmd)
	partition := partitionFlag(cmd, "partition index (default: the only partition with a recognized filesystem)")
	cmd.Flags().BoolVarP(&recursive, "recursive", "r", false, "list the whole subtree with full paths")
	cmd.Flags().BoolVar(&deleted, "deleted", false, "also show deleted directory entries")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		pidx, err := partition()
		if err != nil {
			return err
		}
		dirRef := "/"
		if len(args) == 2 {
			dirRef = args[1]
		}
		s, closeAll, err := openImageSession(d, *casePath, args[0])
		if err != nil {
			return err
		}
		defer closeAll()
		fsys, _, err := s.FS(pidx)
		if err != nil {
			return err
		}
		known := len(fsys.Info().Warnings) // those from opening are in `image info`
		dir, dirPath, err := s.Lookup(fsys, dirRef)
		if err != nil {
			return err
		}

		// --json streams the array, so a huge subtree is never held in memory. It is
		// closed only on success: a failed listing leaves invalid JSON behind.
		var (
			wrote   int
			jsonErr error
		)
		if opts.json {
			_, jsonErr = io.WriteString(d.Out, "[")
		}
		emit := func(p string, e filesys.Entry) {
			if e.Deleted && !deleted {
				return
			}
			switch {
			case opts.json:
				if jsonErr != nil {
					return
				}
				b, merr := json.Marshal(newPathEntry(p, e))
				if merr == nil {
					sep := "\n  "
					if wrote > 0 {
						sep = ",\n  "
					}
					_, merr = d.Out.Write(append([]byte(sep), b...))
				}
				jsonErr = merr
				wrote++
			case recursive:
				fmt.Fprintln(d.Out, lsLine(p, e))
			default:
				fmt.Fprintln(d.Out, lsLine(e.Name, e))
			}
		}
		switch {
		case dir.Deleted && !deleted:
			fmt.Fprintln(d.Err, "note: entry is deleted; use --deleted")
		case dir.Type != filesys.TypeDir:
			emit(dirPath, dir)
		case recursive:
			err = filesys.Walk(fsys, dir, dirPath, func(p string, e filesys.Entry, werr error) error {
				if cerr := cmd.Context().Err(); cerr != nil {
					return cerr
				}
				if werr != nil {
					fmt.Fprintf(d.Err, "warning: %s: %s\n", escapeText(p), escapeText(werr.Error()))
					return nil
				}
				emit(p, e)
				return jsonErr
			})
		default:
			var kids []filesys.Entry
			if kids, err = fsys.ReadDir(dir); err == nil {
				slices.SortStableFunc(kids, func(a, b filesys.Entry) int {
					return cmp.Or(strings.Compare(a.Name, b.Name), strings.Compare(a.ID, b.ID))
				})
				base := strings.TrimSuffix(dirPath, "/") + "/"
				for _, k := range kids {
					emit(base+k.Name, k)
				}
			}
		}
		if err == nil {
			err = jsonErr
		}
		if opts.json && err == nil {
			closer := "]\n"
			if wrote > 0 {
				closer = "\n]\n"
			}
			_, err = io.WriteString(d.Out, closer)
		}
		printNewFSWarnings(d.Err, newFSWarnings(fsys, known))
		return err
	}
	return cmd
}

func newImageStatCmd(d Deps, opts *rootOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "stat <ref> <path | id:<fs id>>",
		Short: "Show every recorded attribute of one filesystem entry",
		Args:  exactArgs(2),
	}
	casePath := caseFlag(cmd)
	partition := partitionFlag(cmd, "partition index (default: the only partition with a recognized filesystem)")
	cmd.RunE = func(_ *cobra.Command, args []string) error {
		pidx, err := partition()
		if err != nil {
			return err
		}
		s, closeAll, err := openImageSession(d, *casePath, args[0])
		if err != nil {
			return err
		}
		defer closeAll()
		fsys, _, err := s.FS(pidx)
		if err != nil {
			return err
		}
		known := len(fsys.Info().Warnings)
		e, p, err := s.Lookup(fsys, args[1])
		if err != nil {
			printNewFSWarnings(d.Err, newFSWarnings(fsys, known))
			return err
		}
		warnings := newFSWarnings(fsys, known)
		if opts.json {
			return writeJSON(d.Out, jsonStat{pathEntry: newPathEntry(p, e), Warnings: warnings})
		}
		printEntry(d.Out, p, e)
		printNewFSWarnings(d.Err, warnings)
		return nil
	}
	return cmd
}

func printEntry(w io.Writer, p string, e filesys.Entry) {
	field := func(name, value string) { fmt.Fprintf(w, "%-12s %s\n", name+":", value) }
	field("Path", printable(p))
	field("Name", printable(e.Name))
	if len(e.RawName) > 0 {
		field("Raw name", hex.EncodeToString(e.RawName))
	}
	field("ID", printable(e.ID))
	field("Type", e.Type.String())
	field("Size", strconv.FormatInt(e.Size, 10))
	field("Mode", fmt.Sprintf("%04o", e.Mode&0o7777))
	field("UID", strconv.FormatUint(uint64(e.UID), 10))
	field("GID", strconv.FormatUint(uint64(e.GID), 10))
	field("Deleted", yesNo(e.Deleted))
	field("Encrypted", yesNo(e.Encrypted))
	if e.LinkTarget != "" {
		field("Link target", printable(e.LinkTarget))
	}
	field("Modified", formatTime(e.Times.Modified))
	field("Accessed", formatTime(e.Times.Accessed))
	field("Changed", formatTime(e.Times.Changed))
	field("Created", formatTime(e.Times.Created))
	field("Deleted at", formatTime(e.Times.Deleted))
	for _, kv := range e.Attrs {
		field("Attr", printable(kv.Key)+" = "+printable(kv.Value))
	}
}

// maxNewFSWarnings is how many new filesystem warnings ls and stat print.
const maxNewFSWarnings = 20

// newFSWarnings returns the filesystem warnings that appeared after the first
// known ones (the warnings only grow, in order).
func newFSWarnings(fsys filesys.FileSystem, known int) []string {
	w := fsys.Info().Warnings
	if len(w) <= known {
		return nil
	}
	return w[known:]
}

// printNewFSWarnings writes the warnings a read of the filesystem raised, one
// escaped line each ("warning: <text>"; the text comes from the image), at most
// maxNewFSWarnings of them and then the count of the rest.
func printNewFSWarnings(w io.Writer, warnings []string) {
	for i, text := range warnings {
		if i == maxNewFSWarnings {
			fmt.Fprintf(w, "… %d more\n", len(warnings)-i)
			return
		}
		fmt.Fprintf(w, "warning: %s\n", escapeText(text))
	}
}

// printIncomplete lists the incomplete artifacts of an extraction. It is used
// even when the analysis failed, so the examiner can see which partial
// artifacts were kept.
func printIncomplete(w io.Writer, sum examine.Summary) {
	for _, a := range sum.Artifacts {
		if a.Incomplete {
			fmt.Fprintf(w, "incomplete: %s (%d bytes kept)\n", printable(a.Path), a.Size)
		}
	}
}

// maxSkipReasons is how many skip reasons the human summary prints.
const maxSkipReasons = 5

// printSkipReasons lists the first skipped paths with their reasons (escaped:
// both come from the image) and points at the audit log for the rest.
func printSkipReasons(w io.Writer, sum examine.Summary) {
	for i, wn := range sum.Warnings {
		if i == maxSkipReasons {
			break
		}
		fmt.Fprintf(w, "  skipped %s: %s\n", escapeText(wn.Path), escapeText(wn.Reason))
	}
	if sum.Skipped > maxSkipReasons {
		fmt.Fprintln(w, "  ... see analysis.warning entries in audit.jsonl")
	}
}

// printFSWarnings says how many warnings the filesystem raised during the
// analysis (their text is in the audit log, not printed: it comes from the image).
func printFSWarnings(w io.Writer, sum examine.Summary) {
	if sum.FSWarnings > 0 {
		fmt.Fprintf(w, "filesystem warnings: %d (see analysis.warning entries in audit.jsonl)\n", sum.FSWarnings)
	}
}

func newImageExtractCmd(d Deps, opts *rootOptions) *cobra.Command {
	var recursive, includeEncrypted bool
	cmd := &cobra.Command{
		Use:   "extract <ref> <path>...",
		Short: "Copy files out of an image's filesystem into the case",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) < 2 {
				return usageErrorf("%s: expected an image and at least one path, got %d argument(s)", cmd.CommandPath(), len(args))
			}
			return nil
		},
	}
	casePath := caseFlag(cmd)
	partition := partitionFlag(cmd, "partition index (default: the only partition with a recognized filesystem)")
	cmd.Flags().BoolVarP(&recursive, "recursive", "r", false, "extract the live subtree of directories")
	cmd.Flags().BoolVar(&includeEncrypted, "include-encrypted", false, "accepted for clarity: encrypted files are always extracted as ciphertext")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		pidx, err := partition()
		if err != nil {
			return err
		}
		s, closeAll, err := openImageSession(d, *casePath, args[0])
		if err != nil {
			return err
		}
		defer closeAll()
		sum, err := s.Extract(cmd.Context(), examine.ExtractOptions{
			Partition: pidx, Paths: args[1:], Recursive: recursive, Progress: newProgress(d.Err, "extract"),
		})
		fmt.Fprintln(d.Err)
		if sum.AnalysisID == "" {
			return err // nothing ran
		}
		var encrypted int
		for _, a := range sum.Artifacts {
			if a.Source.Kind == "extract" && a.Source.Derived != nil && a.Source.Derived.Encrypted {
				encrypted++
			}
		}
		note := ""
		if encrypted > 0 && !includeEncrypted {
			note = fmt.Sprintf("note: %d encrypted files were extracted as ciphertext (decryption is roadmap sub-project 10)", encrypted)
		}
		if opts.json {
			if note != "" {
				fmt.Fprintln(d.Err, note)
			}
			if jerr := writeJSON(d.Out, newJSONSummary(sum)); jerr != nil && err == nil {
				err = jerr
			}
			return err
		}
		fmt.Fprintf(d.Out, "extracted %d files (%s), skipped %d\n", sum.Files, humanBytes(sum.Bytes), sum.Skipped)
		printSkipReasons(d.Out, sum)
		printFSWarnings(d.Out, sum)
		printIncomplete(d.Out, sum)
		if note != "" {
			fmt.Fprintln(d.Out, note)
		}
		return err
	}
	return cmd
}

func newImageUnallocCmd(d Deps, opts *rootOptions) *cobra.Command {
	var volumeMode bool
	cmd := &cobra.Command{
		Use:   "unalloc <ref> (-p N | --volume)",
		Short: "Export unallocated space (a filesystem's free space or the gaps between partitions)",
		Args:  exactArgs(1),
	}
	casePath := caseFlag(cmd)
	partition := partitionFlag(cmd, "export the free space of this partition's filesystem")
	cmd.Flags().BoolVar(&volumeMode, "volume", false, "export the volume-level gaps outside every partition")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		pidx, err := partition()
		if err != nil {
			return err
		}
		switch hasP := cmd.Flags().Changed("partition"); {
		case hasP && volumeMode:
			return usageErrorf("--partition and --volume are mutually exclusive")
		case !hasP && !volumeMode:
			return usageErrorf("choose what to export: --partition N or --volume")
		}
		s, closeAll, err := openImageSession(d, *casePath, args[0])
		if err != nil {
			return err
		}
		defer closeAll()
		sum, err := s.ExportUnallocated(cmd.Context(), examine.UnallocOptions{
			Partition: pidx, Volume: volumeMode, Progress: newProgress(d.Err, "unalloc"),
		})
		fmt.Fprintln(d.Err)
		if sum.AnalysisID == "" {
			return err
		}
		if opts.json {
			if jerr := writeJSON(d.Out, newJSONSummary(sum)); jerr != nil && err == nil {
				err = jerr
			}
			return err
		}
		fmt.Fprintf(d.Out, "exported unallocated space: %d file(s) (%s), skipped %d\n", sum.Files, humanBytes(sum.Bytes), sum.Skipped)
		printFSWarnings(d.Out, sum)
		for _, a := range sum.Artifacts {
			status := ""
			if a.Incomplete {
				status = "  INCOMPLETE"
			}
			fmt.Fprintf(d.Out, "%s  %d  %s%s\n", printable(a.Path), a.Size, a.SHA256, status)
		}
		return err
	}
	return cmd
}
