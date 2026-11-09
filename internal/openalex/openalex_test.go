package openalex

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const resnet = `{"results": [{
	"id": "https://openalex.org/W2194775991",
	"doi": "https://doi.org/10.1109/CVPR.2016.90",
	"display_name": "Deep Residual\nLearning for Image Recognition",
	"publication_year": 2016,
	"type": "article",
	"cited_by_count": 200000,
	"primary_location": {"landing_page_url": "https://doi.org/10.1109/cvpr.2016.90", "source": {"display_name": "CVPR"}},
	"best_oa_location": {"pdf_url": "https://arxiv.org/pdf/1512.03385", "landing_page_url": "https://arxiv.org/abs/1512.03385"},
	"referenced_works": ["https://openalex.org/W1", "https://openalex.org/W2"],
	"abstract_inverted_index": {"networks": [1, 4], "Deeper": [0], "are": [2], "harder": [3]}
}]}`

func TestWorksAreReadWithTheirAbstractsAndReferences(t *testing.T) {
	var got []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.URL.RawQuery)
		_, _ = w.Write([]byte(resnet))
	}))
	defer srv.Close()

	c := New(srv.URL, "key-1", nil)

	works, err := c.Search(context.Background(), "residual networks", 25)
	if err != nil || len(works) != 1 {
		t.Fatalf("%+v, %v", works, err)
	}

	w := works[0]
	if w.ID != "W2194775991" || w.DOI != "10.1109/cvpr.2016.90" || w.Title != "Deep Residual Learning for Image Recognition" || w.Year != 2016 ||
		w.Venue != "CVPR" || w.PDFURL != "https://arxiv.org/pdf/1512.03385" || w.Abstract != "Deeper networks are harder networks" ||
		strings.Join(w.References, ",") != "W1,W2" {
		t.Errorf("work %+v", w)
	}

	if !strings.Contains(got[0], "search=residual+networks") || !strings.Contains(got[0], "api_key=key-1") || !strings.Contains(got[0], "select=") {
		t.Errorf("query %s", got[0])
	}

	if _, err := c.Citing(context.Background(), "https://openalex.org/W2194775991", 500); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(got[1], "filter=cites%3AW2194775991") || !strings.Contains(got[1], "per_page=100") || !strings.Contains(got[1], "sort=publication_date") {
		t.Errorf("citing query %s", got[1])
	}
}

func TestLookupsGoInBatchesOfAHundred(t *testing.T) {
	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)

		if n := strings.Count(r.URL.Query().Get("filter"), "|") + 1; n > 100 {
			t.Errorf("a filter of %d values", n)
		}

		_, _ = w.Write([]byte(`{"results": []}`))
	}))
	defer srv.Close()

	ids := make([]string, 250)
	for i := range ids {
		ids[i] = "W1"
	}

	if _, err := New(srv.URL, "", nil).Works(context.Background(), ids); err != nil || calls.Load() != 3 {
		t.Errorf("%d calls, %v", calls.Load(), err)
	}
}

func TestRefusalsAreRetriedThenReported(t *testing.T) {
	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) < 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}

		_, _ = w.Write([]byte(resnet))
	}))
	defer srv.Close()

	c := New(srv.URL, "", nil)
	c.retryWait = time.Millisecond

	if works, err := c.Search(context.Background(), "x", 5); err != nil || len(works) != 1 {
		t.Errorf("after two refusals: %v", err)
	}

	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTooManyRequests) }))
	defer refusing.Close()

	c = New(refusing.URL, "", nil)
	c.retryWait = time.Millisecond

	if _, err := c.Search(context.Background(), "x", 5); !errors.Is(err, ErrRefused) {
		t.Errorf("err = %v", err)
	}
}

func TestALookupByDOIFindsTheWorkOrNothing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/works/doi:10.48550/arxiv.1512.03385" {
			http.NotFound(w, r)
			return
		}

		_, _ = w.Write([]byte(`{"id": "https://openalex.org/W2194775991", "doi": "https://doi.org/10.1109/CVPR.2016.90", "display_name": "Deep Residual Learning"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "", nil)

	w, err := c.ByDOI(context.Background(), "https://doi.org/10.48550/arXiv.1512.03385")
	if err != nil || w == nil || w.ID != "W2194775991" || w.DOI != "10.1109/cvpr.2016.90" {
		t.Errorf("%+v, %v", w, err)
	}

	if w, err := c.ByDOI(context.Background(), "10.1/unknown"); err != nil || w != nil {
		t.Errorf("an unknown DOI: %+v, %v", w, err)
	}
}

func TestBareDOI(t *testing.T) {
	for in, want := range map[string]string{
		"https://doi.org/10.1109/CVPR.2016.90": "10.1109/cvpr.2016.90",
		"doi:10.1000/ABC":                      "10.1000/abc",
		"10.48550/arXiv.1512.03385":            "10.48550/arxiv.1512.03385",
		"https://example.com/10.1/x":           "",
		"":                                     "",
	} {
		if got := BareDOI(in); got != want {
			t.Errorf("BareDOI(%q) = %q, want %q", in, got, want)
		}
	}
}
