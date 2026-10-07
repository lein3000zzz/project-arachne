package webcrawler

import (
	"net/http"
	"strings"
	"testing"
	"web-crawler/internal/appconfig"
	"web-crawler/internal/domain/config"
	"web-crawler/internal/networker"
	"web-crawler/internal/parser"

	"go.uber.org/zap"
)

type redirectingNetworker struct {
	nopNetworker
	page *networker.FetchResult
}

func (r redirectingNetworker) Fetch(url string) (*networker.FetchResult, error) {
	if strings.HasSuffix(url, "/robots.txt") {
		return &networker.FetchResult{Status: http.StatusNotFound}, nil
	}

	page := *r.page

	return &page, nil
}

func redirectRepo(t *testing.T, page *networker.FetchResult) (*CrawlerRepo, *recordingSink) {
	t.Helper()

	katana, err := parser.NewKatanaParser(zap.NewNop().Sugar())
	if err != nil {
		t.Fatal(err)
	}

	sink := &recordingSink{}

	return &CrawlerRepo{
		logger:      zap.NewNop().Sugar(),
		parser:      katana,
		networker:   redirectingNetworker{page: page},
		cacheRobots: nopCache{},
		documents:   sink,
		settings:    appconfig.Default(),
	}, sink
}

const redirectBody = `<html><head><title>Guide</title></head><body><main>
<p>Enough text here to count as the content of a real page about the guide.</p>
<a href="intro">Intro</a></main></body></html>`

func TestScrapStoresARenamedPageUnderItsFinalURL(t *testing.T) {
	repo, sink := redirectRepo(t, &networker.FetchResult{
		FinalURL:    "https://Example.com/docs/",
		IdentityURL: "https://Example.com/docs/",
		Status:      http.StatusOK,
		ContentType: "text/html",
		Body:        []byte(redirectBody),
	})

	pd, err := repo.scrap(&config.Task{URL: "http://example.com/docs", Run: &config.Run{ID: "run"}})
	if err != nil {
		t.Fatal(err)
	}

	if pd.URL != "https://example.com/docs/" {
		t.Errorf("stored under %q, want the canonical final URL", pd.URL)
	}

	if len(pd.Aliases) != 1 || pd.Aliases[0] != "http://example.com/docs" {
		t.Errorf("aliases = %v, want the requested URL", pd.Aliases)
	}

	if len(pd.PageLinks) != 1 || pd.PageLinks[0] != "https://example.com/docs/intro" {
		t.Errorf("links = %v: relative links must resolve against where the page really is", pd.PageLinks)
	}

	indexed := map[string]bool{}
	for _, doc := range sink.docs {
		indexed[doc.URL] = doc.Content != ""
	}

	if !indexed["https://example.com/docs/"] {
		t.Error("the page must be indexed under its final URL")
	}

	if hasContent, sent := indexed["http://example.com/docs"]; !sent || hasContent {
		t.Error("the alias must be sent as a removal, so nothing stays indexed under it")
	}
}

func TestScrapKeepsTheRequestedURLAfterATemporaryRedirect(t *testing.T) {
	repo, _ := redirectRepo(t, &networker.FetchResult{
		FinalURL:    "https://example.com/login/",
		IdentityURL: "https://example.com/account",
		Status:      http.StatusOK,
		ContentType: "text/html",
		Body:        []byte(redirectBody),
	})

	pd, err := repo.scrap(&config.Task{URL: "https://example.com/account", Run: &config.Run{ID: "run"}})
	if err != nil {
		t.Fatal(err)
	}

	if pd.URL != "https://example.com/account" || len(pd.Aliases) != 0 {
		t.Errorf("stored under %q with aliases %v: a temporary redirect says nothing about identity", pd.URL, pd.Aliases)
	}

	if len(pd.PageLinks) != 1 || pd.PageLinks[0] != "https://example.com/login/intro" {
		t.Errorf("links = %v: they still resolve against the document's real location", pd.PageLinks)
	}
}
