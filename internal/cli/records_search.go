package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/rbenzing/minutiae/internal/evidence"
	"github.com/rbenzing/minutiae/internal/records"
)

// Search output limits.
const (
	// maxSearchLimit is the most hits a plain search page holds (the limit of records list).
	maxSearchLimit = records.MaxLimit
	// maxEchoedQueryBytes cuts the query an error echoes, on a rune boundary.
	maxEchoedQueryBytes = 300
)

// searchContext gives a search its deadline. It is a variable only so a test can hand the command a
// deadline that has certainly passed: a timeout of 1 ns is shorter than the resolution of the
// Windows clock, so whether such a deadline has passed when the first statement starts is not
// defined (the clock may not have ticked), and a test of "an expired deadline" cannot rely on it.
var searchContext = func(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, timeout)
}

// echoQuery is the query as an error message shows it: escaped first (printable), then cut to at most
// maxEchoedQueryBytes of the escaped text, on a rune boundary and never inside an escape sequence,
// with "..." when it was cut.
func echoQuery(q string) string {
	s := printable(q)
	if len(s) <= maxEchoedQueryBytes {
		return s
	}
	i := 0
	for i < len(s) {
		n := escapedTokenLen(s[i:])
		if i+n > maxEchoedQueryBytes {
			break
		}
		i += n
	}
	return s[:i] + "..."
}

// escapedTokenLen is the length of the first token of s: a whole backslash escape (\n, \x1b, \u202e,
// \U0001f600 ...) or one rune.
func escapedTokenLen(s string) int {
	if s[0] != '\\' || len(s) < 2 {
		_, n := utf8.DecodeRuneInString(s)
		return n
	}
	n := 2
	switch s[1] {
	case 'x':
		n = 4
	case 'u':
		n = 6
	case 'U':
		n = 10
	}
	return min(n, len(s))
}

// snippetJSON is a span of a snippet in --json output: the text as stored.
type snippetJSON struct {
	Text  string `json:"text"`
	Match bool   `json:"match"`
}

type snippetsJSON struct {
	Summary []snippetJSON `json:"summary,omitempty"`
	Body    []snippetJSON `json:"body,omitempty"`
}

type hitJSON struct {
	rowJSON
	Snippets *snippetsJSON `json:"snippets,omitempty"`
}

func spansJSON(s *records.Snippet) []snippetJSON {
	if s == nil {
		return nil
	}
	out := make([]snippetJSON, len(s.Spans))
	for i, sp := range s.Spans {
		out[i] = snippetJSON{Text: sp.Text, Match: sp.Match}
	}
	return out
}

func newRecordsSearchCmd(d Deps, opts *rootOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "search [flags] QUERY",
		Short: "Search the text of the records (summary and body)",
		Long: "Search the summary and body text of the records the filters select. The query is words, \"phrases\",\n" +
			"prefix* terms and -negations: words are ANDed (AND is optional), OR (upper case) separates alternatives.\n" +
			"Nothing else is syntax: a star inside a word, a colon, parentheses or NEAR are plain text. Text is\n" +
			"matched without case, accents, width or compatibility differences. A query that starts with - needs\n" +
			"-- before it: minutiae records search --case C -- -x. --substring searches the whole input as one\n" +
			"literal substring of at least 3 characters (no grammar). --in limits the search to the summary or\n" +
			"the body. Hits come in the order of records list, with snippets (the match between U+27E6 and\n" +
			"U+27E7, ... where text is cut); --no-snippets omits them, --rank returns the best hits first (at most\n" +
			"1000, no cursor). A case whose full-text index is not current needs: minutiae records reindex.",
		Args: exactArgs(1),
	}
	casePath := caseFlag(cmd)
	filter := recordFilterFlags(cmd)
	fl := cmd.Flags()
	substring := fl.Bool("substring", false, "search the whole query as one literal substring (3 or more characters)")
	in := fl.String("in", "any", "search only the summary or the body: summary or body")
	rank := fl.Bool("rank", false, "best matches first (at most 1000 hits; no --cursor, no --substring)")
	limit := fl.Int("limit", records.DefaultLimit, fmt.Sprintf("hits per page (1-%d; with snippets 1-%d; with --rank 1-%d)", maxSearchLimit, records.MaxSnippetLimit, records.MaxRankLimit))
	cursor := fl.String("cursor", "", "continue after the page that ended with this cursor")
	noSnippets := fl.Bool("no-snippets", false, "do not show text around the matches")
	timeout := fl.Duration("timeout", 30*time.Second, "give up after this long (0 = no limit)")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		query := args[0]
		f, err := filter()
		if err != nil {
			return err
		}
		to := records.TextOptions{Substring: *substring, Rank: *rank}
		switch *in {
		case "any":
		case "summary":
			to.Column = records.ColSummary
		case "body":
			to.Column = records.ColBody
		default:
			return usageErrorf("--in must be summary or body, got %s", printable(*in))
		}
		switch {
		case *limit < 1 || *limit > maxSearchLimit:
			return usageErrorf("--limit must be between 1 and %d, got %d", maxSearchLimit, *limit)
		case *rank && *limit > records.MaxRankLimit:
			return usageErrorf("--limit above %d is not available with --rank, got %d", records.MaxRankLimit, *limit)
		case !*noSnippets && *limit > records.MaxSnippetLimit:
			return usageErrorf("--limit above %d needs --no-snippets, got %d", records.MaxSnippetLimit, *limit)
		case *rank && *cursor != "":
			return usageErrorf("--rank has no pages: it cannot be combined with --cursor")
		case *rank && *substring:
			return usageErrorf("--rank cannot be combined with --substring")
		case *timeout < 0:
			return usageErrorf("--timeout must not be negative, got %s", *timeout)
		}
		if f.Text, err = records.CompileQuery(query, to); err != nil {
			return fmt.Errorf("query %s: %w", echoQuery(query), err)
		}
		c, r, err := openRecords(*casePath)
		if err != nil {
			return err
		}
		defer func() { _ = c.Close() }()
		ctx := cmd.Context()
		if *timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = searchContext(ctx, *timeout)
			defer cancel()
		}
		res, err := r.Search(ctx, f, records.Page{Limit: *limit, Cursor: *cursor}, records.SearchOptions{Snippets: !*noSnippets})
		if err != nil {
			return recordsError(err, *cursor)
		}
		if opts.json {
			out := struct {
				Records    []hitJSON `json:"records"`
				NextCursor string    `json:"next_cursor"`
			}{Records: make([]hitJSON, 0, len(res.Hits)), NextCursor: res.NextCursor}
			for _, h := range res.Hits {
				hj := hitJSON{rowJSON: recRowJSON(h.Row)}
				if s, b := spansJSON(h.Summary), spansJSON(h.Body); s != nil || b != nil {
					hj.Snippets = &snippetsJSON{Summary: s, Body: b}
				}
				out.Records = append(out.Records, hj)
			}
			return writeJSON(d.Out, out)
		}
		incomplete, err := incompleteArtifacts(c)
		if err != nil {
			return err
		}
		printSearchText(d.Out, res, incomplete)
		return nil
	}
	return cmd
}

// printSearchText prints the hits of a search page, then the cursor line.
func printSearchText(w io.Writer, res records.SearchResult, incomplete map[string]bool) {
	for _, h := range res.Hits {
		printHit(w, h, incomplete[h.Row.ArtifactID])
	}
	if res.NextCursor != "" {
		fmt.Fprintf(w, "# more records: --cursor %s\n", printable(res.NextCursor))
	}
}

// printHit prints the line records list prints, then the snippet lines. The summary of the line
// goes through escapeMarkers as well, so a marker character of the stored text is never taken for a
// match.
func printHit(w io.Writer, h records.Hit, artifactIncomplete bool) {
	row := h.Row
	parts := []string{fmt.Sprint(row.ID), rowTime(row.TS), escapeMarkers(printable(row.Type))}
	if m := markers(row, artifactIncomplete); m != "" {
		parts = append(parts, m)
	}
	parts = append(parts, escapeMarkers(printable(row.Summary)))
	fmt.Fprintln(w, strings.Join(parts, "  "))
	if h.Summary != nil {
		fmt.Fprintf(w, "  summary: %s\n", escapeSnippet(h.Summary))
	}
	if h.Body != nil {
		fmt.Fprintf(w, "  body: %s\n", escapeSnippet(h.Body))
	}
}

func newRecordsReindexCmd(d Deps, opts *rootOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "reindex",
		Short: "Rebuild the full-text index from the records (audited)",
		Long: "Rebuild both full-text indexes of the case from its records. The records are only read. The rebuild\n" +
			"is audited (records.reindex, then records.reindex.done) and needs the case to itself, like every\n" +
			"command; a case in use or with an ingest running is refused. Run it after case upgrade, or when\n" +
			"records search or an ingest says the index is not current.",
		Args: exactArgs(0),
	}
	casePath := caseFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		c, err := openCase(*casePath)
		if err != nil {
			return err
		}
		defer func() { _ = c.Close() }()
		res, err := c.ReindexText(cmd.Context(), evidence.ReindexOptions{})
		if err != nil {
			return err
		}
		if opts.json {
			return writeJSON(d.Out, res)
		}
		printReindex(d.Out, res)
		return nil
	}
	return cmd
}

// printReindex prints the result of records reindex; the versions are database text and are escaped.
func printReindex(w io.Writer, res evidence.ReindexResult) {
	from := printable(res.FromNormVersion)
	if res.FromNormVersion == "" {
		from = "(none)"
	}
	fmt.Fprintf(w, "reindexed %d records (%d indexed): norm version %s -> %s\n", res.Records, res.Indexed, from, printable(res.NormVersion))
}
