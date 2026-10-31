package uploads

import (
	"net/http"
	"strings"
	"testing"
	"web-crawler/internal/networker"
)

func TestAddresses(t *testing.T) {
	id := strings.Repeat("0a", 20)

	if got, ok := ID(URL(id)); !ok || got != id || !Is(URL(id)) {
		t.Errorf("ID(URL(%s)) = %q, %v", id, got, ok)
	}

	for _, u := range []string{
		"https://uploads.arachne.invalid/robots.txt",
		"https://uploads.arachne.invalid/" + strings.Repeat("0A", 20),
		"https://uploads.arachne.invalid/" + id + "/more",
		"https://example.com/" + id,
	} {
		if _, ok := ID(u); ok {
			t.Errorf("%s names no upload", u)
		}
	}
}

type webFetcher struct{ asked []string }

func (w *webFetcher) Fetch(url string) (*networker.FetchResult, error) {
	w.asked = append(w.asked, url)
	return &networker.FetchResult{Status: http.StatusOK, FinalURL: url}, nil
}

func (w *webFetcher) Stop() {}

func TestTheWebIsFetchedAsBeforeAndUploadsHaveNoRobotsFile(t *testing.T) {
	web := &webFetcher{}
	f := NewFetcher(web, nil)

	if res, err := f.Fetch("https://example.com/a"); err != nil || res.Status != http.StatusOK || len(web.asked) != 1 {
		t.Errorf("web page: %+v, %v, asked %v", res, err, web.asked)
	}

	res, err := f.Fetch("https://uploads.arachne.invalid/robots.txt")
	if err != nil || res.Status != http.StatusNotFound || len(web.asked) != 1 {
		t.Errorf("robots.txt of the uploads host: %+v, %v; the web must not be asked", res, err)
	}
}
