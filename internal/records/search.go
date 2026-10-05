package records

import (
	"context"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/rbenzing/minutiae/internal/evidence"
)

// Span is a run of snippet text; Match marks the part that matched the query. Text is the original
// text, unescaped: whoever prints it escapes it.
type Span struct {
	Text  string
	Match bool
}

// Snippet is a window of a text: its spans concatenate to a contiguous substring of the original.
// LeadingCut and TrailingCut say that text before or after the window is not shown.
type Snippet struct {
	Spans                   []Span
	LeadingCut, TrailingCut bool
}

// Hit is one search result. Summary and Body are nil when the field is empty or snippets were not
// requested.
type Hit struct {
	Row           Row
	Summary, Body *Snippet
}

// SearchOptions are the options of Search.
type SearchOptions struct {
	Snippets bool
	// SnippetWidth is the width of a snippet in runes: 0 = 120, 20 to 400.
	SnippetWidth int
}

// SearchResult is one page of hits.
type SearchResult struct {
	Hits       []Hit
	NextCursor string
}

// ErrSearchTimeout is returned (wrapping context.DeadlineExceeded) when the deadline of the context
// passed while a search ran. A cancelled context is not a timeout.
var ErrSearchTimeout = errors.New("search timed out")

// Limits of Search.
const (
	// MaxRankLimit is the most hits a relevance-ordered search returns.
	MaxRankLimit = 1000
	// MaxSnippetLimit is the most hits of a page that carries snippets.
	MaxSnippetLimit = 1000
)

// snippetScanBytes is the most bytes of a body a snippet is built from.
const snippetScanBytes = 256 << 10

// Search returns one page of the records f selects whose text matches f.Text, in the order of List
// (keyset pages, Page.Cursor and Page.Desc work as there) or, when the query was compiled with Rank,
// the best Page.Limit hits by relevance (bm25, summary weighted twice the body, ties by id; at most
// MaxRankLimit, no cursor, NextCursor is always ""). With SearchOptions.Snippets every hit carries
// snippets of its summary and body (at most MaxSnippetLimit hits). It requires a current full-text
// index: ErrIndexNotCurrent (naming records reindex) or, for a case older than schema v3,
// ErrNeedsUpgrade. A deadline that passes while it runs is ErrSearchTimeout.
func (r *Reader) Search(ctx context.Context, f Filter, p Page, so SearchOptions) (SearchResult, error) {
	if f.Text == nil {
		return SearchResult{}, invalidFilter("a search needs a text query")
	}
	limit, err := p.limit()
	if err != nil {
		return SearchResult{}, err
	}
	if so.Snippets && limit > MaxSnippetLimit {
		return SearchResult{}, fmt.Errorf("%w: limit %d is above %d with snippets (ask for none, or fewer)", ErrInvalidPage, limit, MaxSnippetLimit)
	}
	var out SearchResult
	if f.Text.rank {
		out, err = r.searchRank(ctx, f, p, limit, so)
	} else {
		out, err = r.searchPage(ctx, f, p, limit, so)
	}
	if err != nil {
		return SearchResult{}, mapTimeout(err)
	}
	return out, nil
}

func (r *Reader) searchPage(ctx context.Context, f Filter, p Page, limit int, so SearchOptions) (SearchResult, error) {
	var out SearchResult
	res, err := r.page(ctx, f, p, limit, func(h evidence.ReadHandle, rows []Row) error {
		var err error
		out.Hits, err = r.hits(ctx, h, rows, f.Text, so)
		return err
	})
	if err != nil {
		return SearchResult{}, err
	}
	out.NextCursor = res.NextCursor
	return out, nil
}

// buildRank assembles the relevance-ordered statement: the full-text table leads, so the one MATCH is
// in its WHERE clause and bm25 can rank it. The last argument is the LIMIT.
func buildRank(f Filter, have bool) (query, error) {
	if f.Text == nil || !f.Text.rank || f.Text.table != evidence.FTSWordTable || f.Text.match == "" {
		return query{}, invalidFilter("a relevance-ordered search needs a query compiled with Rank for the word index")
	}
	g := f
	g.Text = nil
	w, err := g.compile(have)
	if err != nil {
		return query{}, err
	}
	const t = evidence.FTSWordTable
	sqlText := "SELECT " + rowColumns + ", " + supersededColumn(have) + ", bm25(" + t + ", 2.0, 1.0) AS score FROM " + t +
		" JOIN records r ON r.id = " + t + ".rowid" + joinParsers + joinBatches + " WHERE " + t + " MATCH ?"
	for _, c := range w.conds {
		sqlText += " AND " + c
	}
	sqlText += " ORDER BY score, r.id LIMIT ?"
	args := append([]any{f.Text.match}, w.args...)
	return query{sqlText, append(args, 0)}, nil
}

func (r *Reader) searchRank(ctx context.Context, f Filter, p Page, limit int, so SearchOptions) (SearchResult, error) {
	if p.Cursor != "" {
		return SearchResult{}, fmt.Errorf("%w: a relevance-ordered search has no cursor", ErrInvalidPage)
	}
	if limit > MaxRankLimit {
		return SearchResult{}, fmt.Errorf("%w: limit %d is above %d for a relevance-ordered search", ErrInvalidPage, limit, MaxRankLimit)
	}
	if _, err := f.compile(false); err != nil {
		return SearchResult{}, err
	}
	var out SearchResult
	err := r.readTx(ctx, true, func(h evidence.ReadHandle) error {
		have, err := hasSuperseded(ctx, h)
		if err != nil {
			return err
		}
		q, err := buildRank(f, have)
		if err != nil {
			return err
		}
		q.args[len(q.args)-1] = limit
		r.matching()
		rows, err := h.QueryContext(ctx, q.sql, q.args...)
		if err != nil {
			return fmt.Errorf("records: search: %w", err)
		}
		var found []Row
		for rows.Next() {
			var score float64
			row, err := scanRow(rows, &score)
			if err != nil {
				_ = rows.Close()
				return fmt.Errorf("records: search: %w", err)
			}
			found = append(found, row)
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return fmt.Errorf("records: search: %w", err)
		}
		out.Hits, err = r.hits(ctx, h, found, f.Text, so)
		return err
	})
	if err != nil {
		return SearchResult{}, err
	}
	return out, nil
}

// hits wraps rows in hits, with snippets when asked. A body is read one row at a time, at most
// snippetScanBytes of it (a prefix cut on a rune boundary).
func (r *Reader) hits(ctx context.Context, h evidence.ReadHandle, rows []Row, q *TextQuery, so SearchOptions) ([]Hit, error) {
	out := make([]Hit, len(rows))
	for i, row := range rows {
		out[i].Row = row
		if !so.Snippets {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		out[i].Summary = SnippetFor(row.Summary, q, so.SnippetWidth)
		var body []byte
		if err := h.QueryRowContext(ctx, `SELECT substr(CAST(body AS BLOB), 1, ?) FROM records WHERE id = ?`, snippetScanBytes+1, row.ID).Scan(&body); err != nil {
			return nil, fmt.Errorf("records: snippet of record %d: %w", row.ID, err)
		}
		truncated := len(body) > snippetScanBytes
		if truncated {
			body = body[:snippetScanBytes]
			// cut on a rune boundary
			for k := len(body) - 1; k >= 0 && len(body)-k <= utf8.UTFMax; k-- {
				if utf8.RuneStart(body[k]) {
					if !utf8.FullRune(body[k:]) {
						body = body[:k]
					}
					break
				}
			}
		}
		if s := SnippetFor(string(body), q, so.SnippetWidth); s != nil {
			s.TrailingCut = s.TrailingCut || truncated
			out[i].Body = s
		}
	}
	return out, nil
}
