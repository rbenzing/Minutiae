package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
)

func newRecordsCmd(d Deps, opts *rootOptions) *cobra.Command {
	cmd := newGroupCmd("records", "List, show and summarise the records parsers stored in a case database")
	cmd.AddCommand(newRecordsListCmd(d, opts), newRecordsShowCmd(d, opts), newRecordsStatsCmd(d, opts), newRecordsSearchCmd(d, opts), newRecordsReindexCmd(d, opts))
	return cmd
}

// Every string a record carries (summary, source path, locator, parser name and
// version, ingest id, body, payload text) came from an evidence file and is
// attacker-controlled: it reaches the terminal only through printable (or, for
// the payload, escapeText), never raw. --json output keeps the strings as given.

// recordFilterFlags registers the flags shared by `records list` and `records
// stats` and returns the function that turns them into a filter.
func recordFilterFlags(cmd *cobra.Command) func() (records.Filter, error) {
	fl := cmd.Flags()
	types := fl.StringArray("type", nil, "only this record type (repeatable)")
	from := fl.String("from", "", "only records at or after this time (RFC 3339)")
	to := fl.String("to", "", "only records before this time (RFC 3339)")
	untimed := fl.Bool("include-untimed", false, "with --from/--to, also select records that have no time")
	artifacts := fl.StringArray("artifact", nil, "only records of this artifact id (repeatable)")
	prefix := fl.String("path-prefix", "", "only records whose source path starts with this text (case-sensitive, literal)")
	deleted := fl.String("deleted", "any", "deleted records: any, only or none")
	recovered := fl.String("recovered", "any", "recovered records: any, only or none")
	parsers := fl.StringArray("parser", nil, "only records of this parser, NAME or NAME@VERSION (repeatable)")
	minConf := fl.Int("min-confidence", 0, "only records with a confidence of at least N (0-100); records without one never match")
	ingest := fl.String("ingest", "", "only the records of this ingest run (shows that run even if superseded)")
	allRuns := fl.Bool("all-runs", false, "include the records of superseded runs")
	return func() (records.Filter, error) {
		f := records.Filter{
			Types: *types, ArtifactIDs: *artifacts, PathPrefix: *prefix,
			IncludeUntimed: *untimed, IngestID: *ingest, IncludeSuperseded: *allRuns,
		}
		var err error
		if f.From, err = parseTimeFlag("from", *from); err != nil {
			return f, err
		}
		if f.To, err = parseTimeFlag("to", *to); err != nil {
			return f, err
		}
		if f.Deleted, err = parseTri("deleted", *deleted); err != nil {
			return f, err
		}
		if f.Recovered, err = parseTri("recovered", *recovered); err != nil {
			return f, err
		}
		for _, p := range *parsers {
			name, version, _ := strings.Cut(p, "@")
			if name == "" || (strings.Contains(p, "@") && version == "") {
				return f, usageErrorf("--parser %s: want NAME or NAME@VERSION", printable(p))
			}
			f.Parsers = append(f.Parsers, records.ParserRef{Name: name, Version: version})
		}
		if fl.Changed("min-confidence") {
			if *minConf < 0 || *minConf > 100 {
				return f, usageErrorf("--min-confidence must be between 0 and 100, got %d", *minConf)
			}
			f.MinConfidence = minConf
		}
		return f, nil
	}
}

func parseTimeFlag(name, s string) (*time.Time, error) {
	if s == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil, usageErrorf("--%s %s is not an RFC 3339 time (for example 2026-10-04T09:12:33Z)", name, printable(s))
	}
	return &t, nil
}

func parseTri(name, s string) (records.Tri, error) {
	switch s {
	case "any":
		return records.Any, nil
	case "only":
		return records.Only, nil
	case "none":
		return records.None, nil
	}
	return records.Any, usageErrorf("--%s must be any, only or none, got %s", name, printable(s))
}

// openRecords opens the case and its reader. A case older than schema v2 is a
// usage error naming `case upgrade` (evidence.ErrNeedsUpgrade). Opening appends
// the usual case.open entry to the audit log; the reads append nothing.
func openRecords(casePath string) (*evidence.Case, *records.Reader, error) {
	c, err := openCase(casePath)
	if err != nil {
		return nil, nil, err
	}
	r, err := records.NewReader(c)
	if err != nil {
		_ = c.Close()
		return nil, nil, err
	}
	return c, r, nil
}

// timeLayout has no zone: the suffix says what is known about it.
const timeLayout = "2006-01-02T15:04:05.999999"

// recordTime renders a record time. A UTC time ends in Z; a local time whose
// zone is unknown (T is the wall clock) ends in ~local; a local time with a known
// offset is shown as that wall clock and ends in ~local+HH:MM.
func recordTime(t time.Time, basis string, offsetMin int) string {
	switch basis {
	case "local-unknown":
		return t.UTC().Format(timeLayout) + "~local"
	case "local-offset":
		sign, o := "+", offsetMin
		if o < 0 {
			sign, o = "-", -o
		}
		return t.In(time.FixedZone("", offsetMin*60)).Format(timeLayout) + fmt.Sprintf("~local%s%02d:%02d", sign, o/60, o%60)
	}
	return t.UTC().Format(timeLayout) + "Z"
}

func rowTime(t *records.Time) string {
	if t == nil {
		return "-"
	}
	return recordTime(t.T, string(t.Basis), t.OffsetMin)
}

func micros(us int64) string { return recordTime(time.UnixMicro(us), "utc", 0) }

func markers(r records.Row, artifactIncomplete bool) string {
	var m []string
	if r.Deleted {
		m = append(m, "[deleted]")
	}
	if r.Recovered {
		m = append(m, "[recovered:"+printable(r.Method)+"]")
	}
	if r.Confidence != nil {
		m = append(m, fmt.Sprintf("[conf %d]", *r.Confidence))
	}
	if artifactIncomplete {
		m = append(m, "[artifact incomplete]")
	}
	if r.Superseded {
		m = append(m, "[superseded]")
	}
	return strings.Join(m, " ")
}

// recordsError wraps a reader error for the user. A bad cursor echoes the
// cursor, escaped and cut short, so the examiner sees what was rejected.
func recordsError(err error, cursor string) error {
	if errors.Is(err, records.ErrBadCursor) && cursor != "" {
		shown := cursor
		if len(shown) > 80 {
			shown = shown[:80] + "..."
		}
		return fmt.Errorf("--cursor %s: %w", printable(shown), err)
	}
	return err
}

// JSON forms.

type timeJSON struct {
	UnixMicro int64  `json:"unix_us"`
	Basis     string `json:"basis"`
	OffsetMin *int   `json:"offset_min,omitempty"`
	Display   string `json:"display"`
}

func recTimeJSON(t *records.Time) *timeJSON {
	if t == nil {
		return nil
	}
	j := &timeJSON{UnixMicro: t.T.UnixMicro(), Basis: string(t.Basis), Display: rowTime(t)}
	if t.Basis == records.BasisLocalOffset {
		o := t.OffsetMin
		j.OffsetMin = &o
	}
	return j
}

type rangeJSON struct {
	Offset int64 `json:"offset"`
	Length int64 `json:"length"`
}

type parserJSON struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	Hash    string `json:"hash,omitempty"`
}

type rowJSON struct {
	ID             int64      `json:"id"`
	Type           string     `json:"type"`
	PayloadV       int        `json:"payload_v"`
	ArtifactID     string     `json:"artifact_id"`
	SourcePath     string     `json:"source_path,omitempty"`
	Locator        string     `json:"locator,omitempty"`
	Range          *rangeJSON `json:"range,omitempty"`
	TS             *timeJSON  `json:"ts"`
	TSEnd          *timeJSON  `json:"ts_end"`
	Deleted        bool       `json:"deleted"`
	Recovered      bool       `json:"recovered"`
	RecoveryMethod string     `json:"recovery_method,omitempty"`
	Confidence     *int       `json:"confidence"`
	Parser         parserJSON `json:"parser"`
	Summary        string     `json:"summary"`
	IngestID       string     `json:"ingest_id"`
	Superseded     bool       `json:"superseded"`
}

func recRowJSON(r records.Row) rowJSON {
	j := rowJSON{
		ID: r.ID, Type: r.Type, PayloadV: r.PayloadV, ArtifactID: r.ArtifactID, SourcePath: r.SourcePath, Locator: r.Locator,
		TS: recTimeJSON(r.TS), TSEnd: recTimeJSON(r.TSEnd), Deleted: r.Deleted, Recovered: r.Recovered, RecoveryMethod: r.Method,
		Confidence: r.Confidence, Parser: parserJSON(r.Parser), Summary: r.Summary, IngestID: r.IngestID, Superseded: r.Superseded,
	}
	if r.Range != nil {
		j.Range = &rangeJSON{Offset: r.Range.Offset, Length: r.Range.Length}
	}
	return j
}

// list

func newRecordsListCmd(d Deps, opts *rootOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List records, oldest first (records without a time last)",
		Long: "List the records of a case, oldest first with records that have no time last, or newest first\n" +
			"with --sort -ts. Times are shown as 2026-10-04T09:12:33.5Z (UTC), 2026-10-04T09:12:33.5~local (a wall\n" +
			"clock whose zone is unknown) or 2026-10-04T11:12:33.5~local+02:00 (a wall clock with a known offset);\n" +
			"- means no time. By default the records of a run that a newer complete run of the same parser has\n" +
			"superseded are hidden; --all-runs shows them, marked [superseded]. A page of --limit records ends\n" +
			"with the --cursor that continues it; a cursor works only with the filter and --sort that produced it.",
		Args: exactArgs(0),
	}
	casePath := caseFlag(cmd)
	filter := recordFilterFlags(cmd)
	sort := cmd.Flags().String("sort", "ts", "order: ts (oldest first) or -ts (newest first)")
	limit := cmd.Flags().Int("limit", records.DefaultLimit, fmt.Sprintf("records per page (1-%d)", records.MaxLimit))
	cursor := cmd.Flags().String("cursor", "", "continue after the page that ended with this cursor")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		f, err := filter()
		if err != nil {
			return err
		}
		if *sort != "ts" && *sort != "-ts" {
			return usageErrorf("--sort must be ts or -ts, got %s", printable(*sort))
		}
		if *limit < 1 || *limit > records.MaxLimit {
			return usageErrorf("--limit must be between 1 and %d, got %d", records.MaxLimit, *limit)
		}
		c, r, err := openRecords(*casePath)
		if err != nil {
			return err
		}
		defer func() { _ = c.Close() }()
		res, err := r.List(cmd.Context(), f, records.Page{Limit: *limit, Cursor: *cursor, Desc: *sort == "-ts"})
		if err != nil {
			return recordsError(err, *cursor)
		}
		if opts.json {
			out := struct {
				Records    []rowJSON `json:"records"`
				NextCursor string    `json:"next_cursor"`
			}{Records: make([]rowJSON, 0, len(res.Rows)), NextCursor: res.NextCursor}
			for _, row := range res.Rows {
				out.Records = append(out.Records, recRowJSON(row))
			}
			return writeJSON(d.Out, out)
		}
		incomplete, err := incompleteArtifacts(c)
		if err != nil {
			return err
		}
		for _, row := range res.Rows {
			parts := []string{strconv.FormatInt(row.ID, 10), rowTime(row.TS), printable(row.Type)}
			if m := markers(row, incomplete[row.ArtifactID]); m != "" {
				parts = append(parts, m)
			}
			parts = append(parts, printable(row.Summary))
			fmt.Fprintln(d.Out, strings.Join(parts, "  "))
		}
		if res.NextCursor != "" {
			fmt.Fprintf(d.Out, "# more records: --cursor %s\n", printable(res.NextCursor))
		}
		return nil
	}
	return cmd
}

// incompleteArtifacts is the set of artifact ids the manifest flags incomplete.
func incompleteArtifacts(c *evidence.Case) (map[string]bool, error) {
	recs, err := c.Manifest()
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, m := range recs {
		if m.Incomplete {
			out[m.ID] = true
		}
	}
	return out, nil
}

// show

type showJSON struct {
	rowJSON
	Body               string                  `json:"body,omitempty"`
	Payload            json.RawMessage         `json:"payload,omitempty"`
	Times              []namedTimeJSON         `json:"times"`
	Artifact           evidence.ManifestRecord `json:"artifact"`
	ArtifactIncomplete bool                    `json:"artifact_incomplete"`
	Batch              batchJSON               `json:"batch"`
	Run                *runJSON                `json:"run"`
	SupersededBy       string                  `json:"superseded_by,omitempty"`
}

type namedTimeJSON struct {
	Kind      string `json:"kind"`
	UnixMicro int64  `json:"unix_us"`
	Basis     string `json:"basis"`
	OffsetMin *int64 `json:"offset_min,omitempty"`
	Display   string `json:"display"`
}

type batchJSON struct {
	IngestID string `json:"ingest_id"`
	BatchNo  int    `json:"batch_no"`
	Digest   string `json:"digest"`
	FirstID  int64  `json:"first_id"`
	Count    int    `json:"count"`
	Created  string `json:"created"`
	AuditSeq int64  `json:"audit_seq"`
}

type runJSON struct {
	IngestID      string `json:"ingest_id"`
	AnalysisID    string `json:"analysis_id,omitempty"`
	Parser        string `json:"parser"`
	ParserVersion string `json:"parser_version"`
	ParserHash    string `json:"parser_hash,omitempty"`
	Outcome       string `json:"outcome"`
	Batches       int    `json:"batches"`
	Records       int64  `json:"records"`
	FirstID       int64  `json:"first_id"`
	LastID        int64  `json:"last_id"`
	Ended         string `json:"ended"`
	AuditSeq      int64  `json:"audit_seq"`
}

func namedTimes(ts []evidence.RecordTime) []namedTimeJSON {
	out := make([]namedTimeJSON, 0, len(ts))
	for _, t := range ts {
		off := 0
		if t.TZOffsetMin != nil {
			off = int(*t.TZOffsetMin)
		}
		out = append(out, namedTimeJSON{
			Kind: t.Kind, UnixMicro: t.TS, Basis: t.Basis, OffsetMin: t.TZOffsetMin,
			Display: recordTime(time.UnixMicro(t.TS), t.Basis, off),
		})
	}
	return out
}

func newRecordsShowCmd(d Deps, opts *rootOptions) *cobra.Command {
	var payload bool
	cmd := &cobra.Command{
		Use:   "show <id>",
		Short: "Show one record with its provenance",
		Long: "Show one record: its columns, the artifact it points at (id, path, SHA-256 and source), the parser,\n" +
			"the batch (digest and audit sequence) and run it was written in, the run that superseded it, if any,\n" +
			"and its body. --payload also prints the structured payload.",
		Args: exactArgs(1),
	}
	casePath := caseFlag(cmd)
	cmd.Flags().BoolVar(&payload, "payload", false, "also print the payload")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		id, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil || id < 1 {
			return usageErrorf("record id must be a positive integer, got %s", printable(args[0]))
		}
		c, r, err := openRecords(*casePath)
		if err != nil {
			return err
		}
		defer func() { _ = c.Close() }()
		full, err := r.Get(cmd.Context(), id)
		if err != nil {
			return err
		}
		if opts.json {
			j := showJSON{
				rowJSON: recRowJSON(full.Row), Body: full.Body, Times: namedTimes(full.Times),
				Artifact: full.Artifact, ArtifactIncomplete: full.ArtifactIncomplete,
				Batch:        batchJSON(full.Batch),
				SupersededBy: full.SupersededBy,
			}
			if payload {
				j.Payload = full.Payload
			}
			if full.Run != nil {
				run := runJSON{
					IngestID: full.Run.IngestID, AnalysisID: full.Run.AnalysisID, Parser: full.Run.Parser, ParserVersion: full.Run.ParserVersion,
					ParserHash: full.Run.ParserHash, Outcome: full.Run.Outcome, Batches: full.Run.Batches, Records: full.Run.Records,
					FirstID: full.Run.FirstID, LastID: full.Run.LastID, Ended: full.Run.Ended, AuditSeq: full.Run.AuditSeq,
				}
				j.Run = &run
			}
			return writeJSON(d.Out, j)
		}
		return printFull(d.Out, full, payload)
	}
	return cmd
}

func printFull(w io.Writer, f records.Full, payload bool) error {
	p := func(label, format string, a ...any) {
		fmt.Fprintf(w, "  %-14s"+format+"\n", append([]any{label + ":"}, a...)...)
	}
	fmt.Fprintf(w, "record %d\n", f.ID)
	p("type", "%s (schema v%d)", printable(f.Type), f.PayloadV)
	p("time", "%s", rowTime(f.TS))
	if f.TSEnd != nil {
		p("time end", "%s", rowTime(f.TSEnd))
	}
	p("summary", "%s", printable(f.Summary))
	if m := markers(f.Row, f.ArtifactIncomplete); m != "" {
		p("flags", "%s", m)
	}
	if f.SourcePath != "" {
		p("source path", "%s", printable(f.SourcePath))
	}
	if f.Locator != "" {
		p("locator", "%s", printable(f.Locator))
	}
	if f.Range != nil {
		p("range", "offset %d, length %d", f.Range.Offset, f.Range.Length)
	}
	a := f.Artifact
	p("artifact", "%s  %s  %d bytes", printable(a.ID), printable(a.Path), a.Size)
	p("sha256", "%s", printable(a.SHA256))
	src := "kind " + printable(a.Source.Kind) + ", device " + printable(a.Source.DeviceID)
	if a.Source.OriginalPath != "" {
		src += ", original path " + printable(a.Source.OriginalPath)
	}
	if a.Source.RemotePath != "" {
		src += ", remote path " + printable(a.Source.RemotePath)
	}
	p("source", "%s", src)
	if f.ArtifactIncomplete {
		p("artifact state", "incomplete: %s", printable(a.Error))
	}
	parser := printable(f.Parser.Name) + " " + printable(f.Parser.Version)
	if f.Parser.Hash != "" {
		parser += " (hash " + printable(f.Parser.Hash) + ")"
	}
	p("parser", "%s", parser)
	b := f.Batch
	p("batch", "ingest %s, batch %d (%d records from id %d), digest %s, audit seq %d, created %s",
		printable(b.IngestID), b.BatchNo, b.Count, b.FirstID, printable(b.Digest), b.AuditSeq, printable(b.Created))
	if f.Run != nil {
		ru := f.Run
		p("run", "%s, %d batches, %d records (ids %d-%d), ended %s, audit seq %d",
			printable(ru.Outcome), ru.Batches, ru.Records, ru.FirstID, ru.LastID, printable(ru.Ended), ru.AuditSeq)
	}
	if f.SupersededBy != "" {
		p("superseded by", "%s", printable(f.SupersededBy))
	}
	if len(f.Times) > 0 {
		fmt.Fprintln(w, "  times:")
		for _, t := range namedTimes(f.Times) {
			fmt.Fprintf(w, "    %-14s%s\n", printable(t.Kind), t.Display)
		}
	}
	if f.Body != "" {
		fmt.Fprintln(w, "  body:")
		for _, line := range strings.Split(f.Body, "\n") {
			fmt.Fprintf(w, "    | %s\n", printable(line))
		}
	}
	if payload {
		text, err := prettyPayload(f.Payload)
		if err != nil {
			text = printable(string(f.Payload))
		}
		fmt.Fprintln(w, "  payload:")
		for _, line := range strings.Split(text, "\n") {
			fmt.Fprintf(w, "    %s\n", line)
		}
	}
	return nil
}

// prettyPayload prints a JSON payload indented, one value per line, in the
// order stored. Every string, key and value, is JSON-quoted and then has its
// non-printable runes escaped, so nothing in the payload reaches the terminal as
// a control or format character. It streams tokens rather than decoding into a
// map so two keys that escape alike are both shown.
func prettyPayload(raw []byte) (string, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var b strings.Builder
	if err := prettyValue(&b, dec, 0); err != nil {
		return "", err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return "", errors.New("trailing data after the payload")
	}
	return b.String(), nil
}

const maxPayloadPrintDepth = 128

func quoteJSONString(s string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return escapeText(strings.TrimSuffix(buf.String(), "\n"))
}

func prettyValue(b *strings.Builder, dec *json.Decoder, depth int) error {
	if depth > maxPayloadPrintDepth {
		return errors.New("payload nested too deeply")
	}
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	pad := strings.Repeat("  ", depth)
	switch v := tok.(type) {
	case json.Delim:
		closer := "}"
		if v == '[' {
			closer = "]"
		}
		b.WriteRune(rune(v))
		first := true
		for dec.More() {
			if first {
				b.WriteString("\n")
				first = false
			} else {
				b.WriteString(",\n")
			}
			b.WriteString(pad + "  ")
			if v == '{' {
				key, err := dec.Token()
				if err != nil {
					return err
				}
				ks, ok := key.(string)
				if !ok {
					return errors.New("object key is not a string")
				}
				b.WriteString(quoteJSONString(ks) + ": ")
			}
			if err := prettyValue(b, dec, depth+1); err != nil {
				return err
			}
		}
		if _, err := dec.Token(); err != nil { // the closing delimiter
			return err
		}
		if !first {
			b.WriteString("\n" + pad)
		}
		b.WriteString(closer)
	case string:
		b.WriteString(quoteJSONString(v))
	case json.Number:
		b.WriteString(escapeText(v.String()))
	case bool:
		b.WriteString(strconv.FormatBool(v))
	case nil:
		b.WriteString("null")
	default:
		return fmt.Errorf("unexpected JSON token %T", tok)
	}
	return nil
}

// stats

var statsKeys = []string{"type", "parser", "artifact", "deleted", "run"}

type overviewJSON struct {
	Records           int64            `json:"records"`
	Deleted           int64            `json:"deleted"`
	Recovered         int64            `json:"recovered"`
	Untimed           int64            `json:"untimed"`
	SupersededRecords int64            `json:"superseded_records"`
	TSMin             *string          `json:"ts_min"`
	TSMax             *string          `json:"ts_max"`
	TSMinMicro        *int64           `json:"ts_min_us"`
	TSMaxMicro        *int64           `json:"ts_max_us"`
	Runs              map[string]int64 `json:"runs"`
}

type indexJSON struct {
	State         string `json:"state"`
	Value         string `json:"value"`
	Current       string `json:"current"`
	WordDocs      int64  `json:"word_docs"`
	SubstringDocs int64  `json:"substring_docs"`
}

type statRowJSON struct {
	Key        string  `json:"key"`
	Count      int64   `json:"count"`
	TSMin      *string `json:"ts_min"`
	TSMax      *string `json:"ts_max"`
	TSMinMicro *int64  `json:"ts_min_us"`
	TSMaxMicro *int64  `json:"ts_max_us"`
}

func microsPtr(us *int64) *string {
	if us == nil {
		return nil
	}
	s := micros(*us)
	return &s
}

func newRecordsStatsCmd(d Deps, opts *rootOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "stats",
		Short: "Count records, overall and grouped by type, parser, artifact, deleted state or run",
		Long: "Count the records the filters select: an overview (records, deleted, recovered, untimed, the time\n" +
			"range, and every run of the case by outcome, incomplete runs included), then the records grouped\n" +
			"by --by. The overview's deleted count includes recovered records; --by deleted puts recovered\n" +
			"records in their own group. Time ranges are the stored instants (a local time of unknown zone is\n" +
			"shown as if it were UTC).",
		Args: exactArgs(0),
	}
	casePath := caseFlag(cmd)
	filter := recordFilterFlags(cmd)
	by := cmd.Flags().String("by", "type", "group by: "+strings.Join(statsKeys, ", "))
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		f, err := filter()
		if err != nil {
			return err
		}
		if !slices.Contains(statsKeys, *by) {
			return usageErrorf("--by must be one of %s, got %s", strings.Join(statsKeys, ", "), printable(*by))
		}
		c, r, err := openRecords(*casePath)
		if err != nil {
			return err
		}
		defer func() { _ = c.Close() }()
		ov, err := r.Overview(cmd.Context(), f)
		if err != nil {
			return err
		}
		rows, err := r.Stats(cmd.Context(), f, *by)
		if err != nil {
			return err
		}
		ix, err := r.IndexStatus(cmd.Context())
		if err != nil {
			return err
		}
		if opts.json {
			out := struct {
				Overview overviewJSON  `json:"overview"`
				Index    indexJSON     `json:"index"`
				By       string        `json:"by"`
				Rows     []statRowJSON `json:"rows"`
			}{
				Overview: overviewJSON{
					Records: ov.Records, Deleted: ov.Deleted, Recovered: ov.Recovered, Untimed: ov.Untimed,
					SupersededRecords: ov.SupersededRecords, TSMin: microsPtr(ov.TSMin), TSMax: microsPtr(ov.TSMax),
					TSMinMicro: ov.TSMin, TSMaxMicro: ov.TSMax, Runs: ov.Runs,
				},
				Index: indexJSON{State: ix.Kind, Value: ix.Value, Current: ix.Current, WordDocs: ix.WordDocs, SubstringDocs: ix.SubstringDocs},
				By:    *by, Rows: make([]statRowJSON, 0, len(rows)),
			}
			if out.Overview.Runs == nil {
				out.Overview.Runs = map[string]int64{}
			}
			for _, s := range rows {
				out.Rows = append(out.Rows, statRowJSON{
					Key: s.Key, Count: s.Count, TSMin: microsPtr(s.TSMin), TSMax: microsPtr(s.TSMax), TSMinMicro: s.TSMin, TSMaxMicro: s.TSMax,
				})
			}
			return writeJSON(d.Out, out)
		}
		printStats(d.Out, ov, ix, *by, rows)
		return nil
	}
	return cmd
}

func printStats(w io.Writer, ov records.Overview, ix records.IndexStatus, by string, rows []records.StatRow) {
	fmt.Fprintf(w, "records:      %d (deleted %d, recovered %d, untimed %d)\n", ov.Records, ov.Deleted, ov.Recovered, ov.Untimed)
	if ov.SupersededRecords > 0 {
		fmt.Fprintf(w, "superseded:   %d records belong to superseded runs\n", ov.SupersededRecords)
	}
	if ov.TSMin != nil && ov.TSMax != nil {
		fmt.Fprintf(w, "time range:   %s .. %s\n", micros(*ov.TSMin), micros(*ov.TSMax))
	}
	var runs []string
	for _, outcome := range []string{"complete", "incomplete", "interrupted"} {
		if n := ov.Runs[outcome]; n > 0 {
			runs = append(runs, fmt.Sprintf("%s %d", outcome, n))
		}
	}
	if len(runs) == 0 {
		runs = []string{"none"}
	}
	fmt.Fprintf(w, "runs:         %s\n", strings.Join(runs, ", "))
	if ix.Kind == "unavailable" {
		fmt.Fprintln(w, "index:        unavailable (the case schema is older than v3; run: minutiae case upgrade)")
	} else {
		fmt.Fprintf(w, "index:        %s (%s; %d word documents, %d substring documents)\n", printable(ix.Kind), printable(ix.Value), ix.WordDocs, ix.SubstringDocs)
	}
	fmt.Fprintln(w)
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "%s\tcount\tfirst\tlast\n", strings.ToUpper(by))
	for _, s := range rows {
		first, last := "-", "-"
		if s.TSMin != nil && s.TSMax != nil {
			first, last = micros(*s.TSMin), micros(*s.TSMax)
		}
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\n", printable(s.Key), s.Count, first, last)
	}
	_ = tw.Flush()
}
