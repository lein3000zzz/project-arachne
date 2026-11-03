package searchapi

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
	"web-crawler/internal/weblookup"
	"web-crawler/internal/websearch"
)

type fakeLookup struct {
	pages []string
	err   error
	user  int64
}

func (f *fakeLookup) Look(_ context.Context, user int64, question string, ev weblookup.Events) ([]string, error) {
	f.user = user

	if f.err != nil {
		return nil, f.err
	}

	_ = ev.Searched(question, []weblookup.Found{{Result: websearch.Result{URL: "https://ziglang.org/"}, Read: true}}, nil)
	_ = ev.Reading([]weblookup.Reading{{URL: "https://ziglang.org/", State: "finished", Indexed: 1}})

	return f.pages, nil
}

func serveWeb(services Services, target string) *httptest.ResponseRecorder {
	services.Searcher = &fakeSearcher{}
	handler := newTestHandler(services, Config{DefaultLimit: 10, MaxLimit: 50, RequestTimeout: time.Second, AnswerTimeout: time.Second,
		WebLookupTimeout: time.Second})

	return do(handler, signedIn(httptest.NewRequest(http.MethodGet, target, nil)))
}

func TestTheWebIsReadBeforeTheAnswer(t *testing.T) {
	answerer, look := &fakeAnswerer{}, &fakeLookup{pages: []string{"https://ziglang.org/", "https://doc.rust-lang.org/book/"}}

	rec := serveWeb(Services{Answerer: answerer, WebLookup: look}, "/answer/web/stream?q=zig+vs+rust")
	if got := eventNames(rec.Body.String()); !strings.HasPrefix(got, "searched,reading,looked,sources,") || !strings.HasSuffix(got, ",done") {
		t.Errorf("events = %s\n%s", got, rec.Body)
	}

	if !slices.Equal(answerer.got.LabURLs, look.pages) || !answerer.got.Fresh || look.user != testUser.ID || !strings.Contains(rec.Body.String(), `"web":true`) {
		t.Errorf("the pages read come first, seen fresh: %+v; user %d", answerer.got, look.user)
	}

	// Read nothing: the index answers as it would have, and is not waited on.
	answerer = &fakeAnswerer{}
	if serveWeb(Services{Answerer: answerer, WebLookup: &fakeLookup{}}, "/answer/web/stream?q=zig"); answerer.got.LabURLs != nil || answerer.got.Fresh {
		t.Errorf("nothing read from the web: %+v", answerer.got)
	}

	// The web giving nothing is said, and the index answers anyway.
	rec = serveWeb(Services{Answerer: &fakeAnswerer{}, WebLookup: &fakeLookup{err: weblookup.ErrNothingFound}}, "/answer/web/stream?q=zzz")
	if got := eventNames(rec.Body.String()); !strings.HasPrefix(got, "looked,sources,") || !strings.HasSuffix(got, ",done") ||
		!strings.Contains(rec.Body.String(), `"error":"the web search found nothing"`) {
		t.Errorf("nothing found: %s\n%s", got, rec.Body)
	}

	rec = serveWeb(Services{Answerer: &fakeAnswerer{}, WebLookup: &fakeLookup{err: errors.New("searxng: 502")}}, "/answer/web/stream?q=zzz")
	if body := rec.Body.String(); !strings.Contains(body, `"error":"the web could not be searched"`) || strings.Contains(body, "502") {
		t.Errorf("an unknown failure is logged, not shown:\n%s", body)
	}

	if rec := serveWeb(Services{Answerer: &fakeAnswerer{}}, "/answer/web/stream?q=zig"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("without look-ups: %d", rec.Code)
	}

	if !strings.Contains(serveWeb(Services{Answerer: &fakeAnswerer{}}, "/answer/stream?q=zig").Body.String(), `"web":false`) {
		t.Error("a plain answer says it did not look on the web")
	}
}
