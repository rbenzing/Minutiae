package cli

import (
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
)

// Provenance printing limits.
const (
	maxExtentLinesPerHop = 20
	provIndent           = "    "
	provContinue         = "                   " // provIndent plus the 15-column label
	provChecked          = "manifest links and hashes, audit entries, runs sidecar hash; NOT checked here: file contents, image bytes at the runs, the audit chain (run: minutiae case verify)"
)

// printProvenance prints the checked provenance chain of a record: what the manifest, the audit log
// and the runs sidecars prove, and what is not checked here. The status line comes first, and a
// record on recovered data is introduced as recovered data even when the chain is damaged. Every
// string that came from the case or the device goes through printable.
func printProvenance(w io.Writer, p records.Provenance, row records.Row) {
	top := func(label, format string, a ...any) {
		fmt.Fprintf(w, provIndent+"%-15s"+format+"\n", append([]any{label}, a...)...)
	}
	fmt.Fprintln(w, "  provenance:")
	top("status:", "%s", provStatus(p, row))
	for i, h := range p.Chain {
		printProvHop(w, p, i, h)
	}
	if v := p.Recovery; v != nil {
		top("recovery:", "%s", recoveryText(v))
		if dr := v.DeclaredRuns; dr != nil {
			state := printable(dr.State)
			if dr.Detail != "" {
				state += ": " + printable(dr.Detail)
			}
			top("declared runs:", "sidecar %s (declared by the filesystem, not captured, not used for offsets) [%s]", printable(dr.ArtifactID), state)
		}
	}
	printProvOffset(w, p)
	printProvList(w, "problems:", provProblemLines(p))
	printProvList(w, "notes:", provNoteLines(p))
	top("checked:", "%s", provChecked)
}

func provStatus(p records.Provenance, row records.Row) string {
	if v := p.Recovery; v != nil {
		conf := v.MinConfidence
		if conf == nil {
			conf = v.Recovery.Confidence
		}
		return recoveredStatus(printable(v.Recovery.Class), printable(v.Recovery.Method), conf)
	}
	if row.Recovered {
		return recoveredStatus("unknown", printable(row.Method), row.Confidence)
	}
	return "live"
}

func recoveredStatus(class, method string, conf *int) string {
	c := "none"
	if conf != nil {
		c = fmt.Sprint(*conf)
	}
	return fmt.Sprintf("RECOVERED DATA (class %s, method %s, confidence %s; not live evidence)", class, method, c)
}

func printProvHop(w io.Writer, p records.Provenance, i int, h records.Hop) {
	a := h.Artifact
	line := fmt.Sprintf("    [%d] artifact   %s  %s  %d bytes  sha256 %s", i, printable(a.ID), printable(a.Path), a.Size, printable(a.SHA256))
	if a.Incomplete {
		line += "  [incomplete: " + printable(a.Error) + "]"
	}
	fmt.Fprintln(w, line)
	sub := func(label, format string, args ...any) {
		fmt.Fprintf(w, "        %-11s"+format+"\n", append([]any{label}, args...)...)
	}
	if d := a.Source.Derived; d != nil {
		text := fmt.Sprintf("%s of partition %d (%s) at image offset %d, fs path %s, fs id %s",
			printable(a.Source.Kind), d.Partition, printable(d.FSType), d.PartitionOffset, printable(d.FSPath), printable(d.FSID))
		if d.Snapshot != nil {
			text += fmt.Sprintf(", snapshot \"%s\" (xid %d)", escapeText(d.Snapshot.Name), d.Snapshot.Xid)
		}
		if d.Encrypted {
			text += ", encrypted"
		}
		sub("derived:", "%s", text)
		switch {
		case d.RunsArtifact != "":
			sub("runs:", "sidecar %s", printable(d.RunsArtifact))
		case len(d.Runs) > 0:
			sub("runs:", "%d inline", len(d.Runs))
		default:
			sub("runs:", "none recorded")
		}
		sub("link:", "%s", linkText(p, i, h, d))
	} else {
		root := printable(a.Source.Kind) + " from device " + printable(a.Source.DeviceID)
		if a.Source.OriginalPath != "" {
			root += ", original path " + printable(a.Source.OriginalPath)
		}
		if a.Source.RemotePath != "" {
			root += ", remote path " + printable(a.Source.RemotePath)
		}
		if a.Source.Segment > 0 {
			root += fmt.Sprintf(", segment %d of %d", a.Source.Segment, a.Source.Segments)
		}
		sub("root:", "%s", root)
	}
	if h.SegmentsTotal > 0 {
		fmt.Fprintf(w, "        parent segments: %d listed, %d bad (first %d shown)\n", h.SegmentsTotal, h.SegmentsBad, records.MaxShownSegments)
		for k, s := range h.Segments {
			if s.State != "ok" {
				fmt.Fprintf(w, "          segment %d: %s %s\n", k+1, printable(s.ID), printable(s.State))
			}
		}
	}
	if h.Audit.State == evidence.AuditBound {
		sub("audit:", "bound to audit seq %d", h.Audit.Seq)
	} else {
		text := "NOT BOUND: " + printable(string(h.Audit.State))
		if h.Audit.Detail != "" {
			text += ": " + printable(h.Audit.Detail)
		}
		sub("audit:", "%s", text)
	}
}

func linkText(p records.Provenance, i int, h records.Hop, d *evidence.Derivation) string {
	switch h.Link {
	case records.LinkParentMissing:
		return "PARENT MISSING " + printable(d.ParentID)
	case records.LinkParentAmbiguous:
		return "PARENT AMBIGUOUS"
	case records.LinkTooDeep:
		return fmt.Sprintf("CHAIN TOO DEEP (more than %d links)", evidence.MaxDerivedDepth)
	case records.LinkCycle:
		for k, o := range p.Chain {
			if o.Artifact.ID == d.ParentID {
				return fmt.Sprintf("CYCLE (derives from [%d])", k)
			}
		}
		return "CYCLE"
	}
	if i+1 < len(p.Chain) {
		next := p.Chain[i+1]
		if next.Link == records.LinkHashDiffers {
			return fmt.Sprintf("PARENT HASH DIFFERS (recorded %s, found %s)", printable(d.ParentSHA256), printable(next.Artifact.SHA256))
		}
		return fmt.Sprintf("ok -> [%d]", i+1)
	}
	return "NOT FOLLOWED"
}

func recoveryText(v *records.RecoveryView) string {
	r := v.Recovery
	q := func(ss []string) string {
		out := make([]string, len(ss))
		for i, s := range ss {
			out[i] = printable(s)
		}
		return strings.Join(out, "; ")
	}
	text := fmt.Sprintf("artifact %s (%d hops up), basis %s, assumptions %s", printable(v.ArtifactID), v.Hops, q(r.Basis), q(r.Assumptions))
	if len(r.Params) > 0 {
		keys := make([]string, 0, len(r.Params))
		for k := range r.Params {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		kv := make([]string, len(keys))
		for i, k := range keys {
			kv[i] = printable(k) + "=" + printable(r.Params[k])
		}
		text += ", params " + strings.Join(kv, "; ")
	}
	a := r.Alloc
	text += fmt.Sprintf(", alloc %d/%d/%d, captured %d bytes", a.Free, a.Allocated, a.Unknown, a.Free+a.Allocated+a.Unknown)
	if a.ExcludedRuns > 0 || a.ExcludedBytes > 0 {
		text += fmt.Sprintf("; %d bytes in %d runs named but not captured", a.ExcludedBytes, a.ExcludedRuns)
	}
	return text
}

func extentText(e records.ImageExtent, id string) string {
	left := fmt.Sprintf("bytes %d..%d", e.ArtifactOffset, e.ArtifactOffset+e.Length)
	if id != "" {
		left += " of " + id
	}
	if e.Hole {
		return fmt.Sprintf("%s = hole of %d bytes (no media bytes)", left, e.Length)
	}
	return fmt.Sprintf("%s = media bytes %d..%d", left, e.ImageOffset, e.ImageOffset+e.Length)
}

func printProvOffset(w io.Writer, p records.Provenance) {
	o := p.Offset
	switch o.State {
	case records.OffsetNoRange:
		fmt.Fprintf(w, provIndent+"%-15s%s\n", "image offset:", "(record has no byte range)")
		return
	case records.OffsetUnavailable:
		fmt.Fprintf(w, provIndent+"image offset unavailable: %s\n", printable(o.Reason))
	case records.OffsetTranslated:
		segs := "segments not recorded"
		if o.Segments > 0 {
			segs = fmt.Sprintf("%d segment(s)", o.Segments)
		}
		id := ""
		if len(p.Chain) > 0 {
			id = printable(p.Chain[0].Artifact.ID)
		}
		head := fmt.Sprintf(provIndent+"image offset (logical media offset, %s):  ", segs)
		if len(o.Image) == 0 {
			fmt.Fprintln(w, head+"(no bytes)")
		}
		for k, e := range o.Image {
			if k >= maxExtentLinesPerHop {
				fmt.Fprintf(w, "        ... and %d more extents\n", len(o.Image)-maxExtentLinesPerHop)
				break
			}
			if k == 0 {
				fmt.Fprintln(w, head+extentText(e, id))
			} else {
				fmt.Fprintln(w, "        "+extentText(e, id))
			}
		}
		for _, h := range o.Hops {
			if h.RunsExceedSize {
				fmt.Fprintln(w, "        runs describe more than the bytes held (incomplete)")
				break
			}
		}
	default:
		return
	}
	for k, h := range o.Hops {
		last := k == len(o.Hops)-1
		if !last {
			fmt.Fprintf(w, "        via %s -> %s (%s): %d extents\n", printable(h.From), printable(h.To), printable(h.Runs), h.Total)
			for n, e := range h.Extents {
				if n >= maxExtentLinesPerHop {
					fmt.Fprintf(w, "          ... and %d more extents\n", len(h.Extents)-maxExtentLinesPerHop)
					break
				}
				fmt.Fprintln(w, "          "+extentText(e, ""))
			}
		}
		if h.Truncated {
			pad := "        "
			if !last {
				pad += "  "
			}
			fmt.Fprintf(w, "%s%d extents, first %d listed\n", pad, h.Total, records.MaxExtentsPerHop)
		}
	}
}

func provProblemLines(p records.Provenance) []string {
	var out []string
	for _, pr := range p.Problems {
		out = append(out, fmt.Sprintf("PROBLEM %s: %s", printable(string(pr.Kind)), printable(pr.Detail)))
	}
	if p.ProblemsSuppressed > 0 {
		out = append(out, fmt.Sprintf("%d further problems not listed", p.ProblemsSuppressed))
	}
	return out
}

func provNoteLines(p records.Provenance) []string {
	out := make([]string, len(p.Notes))
	for i, n := range p.Notes {
		out[i] = printable(n)
	}
	return out
}

func printProvList(w io.Writer, label string, lines []string) {
	for i, l := range lines {
		if i == 0 {
			fmt.Fprintf(w, provIndent+"%-15s%s\n", label, l)
		} else {
			fmt.Fprintln(w, provContinue+l)
		}
	}
}
