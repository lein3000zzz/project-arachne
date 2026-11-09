// Package openalex reads OpenAlex, the open index of scholarly works: search,
// the works a work cites, and the works that cite it. Lookups by ID or DOI are
// free; a list or a filter costs $0.0001 and a search $0.001 of a daily budget
// that a free key raises tenfold.
package openalex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const DefaultBaseURL = "https://api.openalex.org"

var (
	// Refused for the rate or the day's budget, after retrying.
	ErrRefused = errors.New("OpenAlex refused the request: its rate limit or the day's budget is spent")
	ErrFailed  = errors.New("OpenAlex did not answer")
)

// Work is a scholarly work as research needs it: enough to judge it, read it
// and follow its citations.
type Work struct {
	// Short form, W123.
	ID string
	// Bare and lower case, 10.1000/xyz.
	DOI      string
	Title    string
	Year     int
	Type     string
	Venue    string
	CitedBy  int
	Abstract string
	// An open-access PDF, when there is one, and the landing page.
	PDFURL     string
	LandingURL string
	// Short IDs of the works it cites.
	References []string
}

type Client struct {
	base string
	key  string
	http *http.Client
	// Waited between retries of a refused request; per-second refusals cost
	// nothing and pass quickly.
	retryWait time.Duration
}

// New takes an empty key for keyless use, with a tenth of the budget.
func New(base, key string, httpClient *http.Client) *Client {
	if base == "" {
		base = DefaultBaseURL
	}

	if httpClient == nil {
		httpClient = &http.Client{Timeout: 20 * time.Second}
	}

	return &Client{base: strings.TrimRight(base, "/"), key: key, http: httpClient, retryWait: time.Second}
}

const (
	fields = "id,doi,display_name,publication_year,type,cited_by_count,primary_location,best_oa_location,referenced_works,abstract_inverted_index"
	// A filter takes at most 100 values joined by |.
	maxBatch    = 100
	maxPerPage  = 100
	abstractCap = 1500
	retries     = 3
)

// Search finds works matching the query, best first.
func (c *Client) Search(ctx context.Context, query string, n int) ([]Work, error) {
	return c.list(ctx, url.Values{"search": {query}, "per_page": {perPage(n)}})
}

// Citing is the works that cite the work, newest first: later work is what a
// search for the topic misses most.
func (c *Client) Citing(ctx context.Context, id string, n int) ([]Work, error) {
	return c.list(ctx, url.Values{"filter": {"cites:" + short(id)}, "per_page": {perPage(n)}, "sort": {"publication_date:desc"}})
}

// Works looks works up by their OpenAlex IDs, in batches.
func (c *Client) Works(ctx context.Context, ids []string) ([]Work, error) {
	return c.batch(ctx, "openalex", ids, short)
}

// ByDOI looks one work up by a DOI of any of its versions - an arXiv
// preprint's finds the published paper. Lookups like this are free. A DOI
// OpenAlex does not know gives nil.
func (c *Client) ByDOI(ctx context.Context, doi string) (*Work, error) {
	var w work

	found, err := c.get(ctx, "/works/doi:"+url.PathEscape(BareDOI(doi)), url.Values{}, &w)
	if err != nil || !found {
		return nil, err
	}

	out := w.work()

	return &out, nil
}

func (c *Client) batch(ctx context.Context, filter string, values []string, clean func(string) string) ([]Work, error) {
	var out []Work

	for chunk := range slices.Chunk(values, maxBatch) {
		cleaned := make([]string, len(chunk))
		for i, v := range chunk {
			cleaned[i] = clean(v)
		}

		works, err := c.list(ctx, url.Values{"filter": {filter + ":" + strings.Join(cleaned, "|")}, "per_page": {strconv.Itoa(maxPerPage)}})
		if err != nil {
			return out, err
		}

		out = append(out, works...)
	}

	return out, nil
}

func perPage(n int) string {
	return strconv.Itoa(min(max(n, 1), maxPerPage))
}

func (c *Client) list(ctx context.Context, q url.Values) ([]Work, error) {
	var body struct {
		Results []work `json:"results"`
	}

	if _, err := c.get(ctx, "/works", q, &body); err != nil {
		return nil, err
	}

	works := make([]Work, 0, len(body.Results))
	for _, w := range body.Results {
		works = append(works, w.work())
	}

	return works, nil
}

// get reads one answer into into; found is false for a 404.
func (c *Client) get(ctx context.Context, path string, q url.Values, into any) (found bool, _ error) {
	q.Set("select", fields)
	if c.key != "" {
		q.Set("api_key", c.key)
	}

	target := c.base + path + "?" + q.Encode()

	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return false, err
		}

		resp, err := c.http.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return false, ctx.Err()
			}

			return false, fmt.Errorf("%w: %w", ErrFailed, err)
		}

		switch {
		case resp.StatusCode == http.StatusOK:
			err = json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(into)
			_ = resp.Body.Close()

			if err != nil {
				return false, fmt.Errorf("%w: unreadable answer: %w", ErrFailed, err)
			}

			return true, nil
		case resp.StatusCode == http.StatusNotFound:
			_ = resp.Body.Close()
			return false, nil
		case resp.StatusCode == http.StatusTooManyRequests && attempt < retries:
			_ = resp.Body.Close()

			select {
			case <-ctx.Done():
				return false, ctx.Err()
			case <-time.After(c.retryWait * time.Duration(attempt+1)):
			}
		case resp.StatusCode == http.StatusTooManyRequests:
			_ = resp.Body.Close()
			return false, ErrRefused
		default:
			_ = resp.Body.Close()
			return false, fmt.Errorf("%w: status %d", ErrFailed, resp.StatusCode)
		}
	}
}

// short turns https://openalex.org/W123 into W123.
func short(id string) string {
	return id[strings.LastIndexByte(id, '/')+1:]
}

type location struct {
	LandingPageURL string `json:"landing_page_url"`
	PDFURL         string `json:"pdf_url"`
	Source         *struct {
		DisplayName string `json:"display_name"`
	} `json:"source"`
}

type work struct {
	ID              string           `json:"id"`
	DOI             string           `json:"doi"`
	DisplayName     string           `json:"display_name"`
	PublicationYear int              `json:"publication_year"`
	Type            string           `json:"type"`
	CitedByCount    int              `json:"cited_by_count"`
	PrimaryLocation *location        `json:"primary_location"`
	BestOALocation  *location        `json:"best_oa_location"`
	ReferencedWorks []string         `json:"referenced_works"`
	Abstract        map[string][]int `json:"abstract_inverted_index"`
}

func (w work) work() Work {
	out := Work{
		ID: short(w.ID), DOI: BareDOI(w.DOI), Title: strings.Join(strings.Fields(w.DisplayName), " "), Year: w.PublicationYear, Type: w.Type,
		CitedBy: w.CitedByCount, Abstract: abstract(w.Abstract),
	}

	for _, ref := range w.ReferencedWorks {
		out.References = append(out.References, short(ref))
	}

	if l := w.PrimaryLocation; l != nil {
		out.LandingURL = l.LandingPageURL
		if l.Source != nil {
			out.Venue = l.Source.DisplayName
		}
	}

	if l := w.BestOALocation; l != nil {
		out.PDFURL = l.PDFURL
		if out.LandingURL == "" {
			out.LandingURL = l.LandingPageURL
		}
	}

	return out
}

// BareDOI turns https://doi.org/10.1/X into 10.1/x: DOIs ignore case.
func BareDOI(doi string) string {
	doi = strings.TrimSpace(doi)
	for _, prefix := range []string{"https://doi.org/", "http://doi.org/", "https://dx.doi.org/", "http://dx.doi.org/", "doi:"} {
		if len(doi) >= len(prefix) && strings.EqualFold(doi[:len(prefix)], prefix) {
			doi = doi[len(prefix):]
			break
		}
	}

	if !strings.HasPrefix(doi, "10.") {
		return ""
	}

	return strings.ToLower(doi)
}

// abstract rebuilds the text OpenAlex keeps as word positions.
func abstract(index map[string][]int) string {
	type at struct {
		pos  int
		word string
	}

	var words []at

	for word, positions := range index {
		for _, p := range positions {
			words = append(words, at{p, word})
		}
	}

	slices.SortFunc(words, func(a, b at) int { return a.pos - b.pos })

	var b strings.Builder

	for i, w := range words {
		if i > 0 {
			b.WriteByte(' ')
		}

		b.WriteString(w.word)

		if b.Len() > abstractCap*4 {
			break
		}
	}

	text := b.String()
	if utf8.RuneCountInString(text) > abstractCap {
		text = string([]rune(text)[:abstractCap]) + "…"
	}

	return text
}
