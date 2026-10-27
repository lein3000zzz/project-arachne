package webcrawler

import (
	"testing"
	"web-crawler/internal/appconfig"
	"web-crawler/internal/networker"

	"go.uber.org/zap"
)

type robotsNetworker struct {
	nopNetworker
	body string
}

func (r robotsNetworker) Fetch(string) (*networker.FetchResult, error) {
	return &networker.FetchResult{Status: 200, Body: []byte(r.body)}, nil
}

func robotsRepo(robots string) *CrawlerRepo {
	settings := appconfig.Default()
	settings.Crawler.UserAgent = "project-arachne/0.1 (+https://example.com/bot)"

	return &CrawlerRepo{
		logger:      zap.NewNop().Sugar(),
		networker:   robotsNetworker{body: robots},
		cacheRobots: nopCache{},
		settings:    settings,
	}
}

func TestRobotsRulesMatchOurProductName(t *testing.T) {
	repo := robotsRepo("User-agent: project-arachne\nDisallow: /private\n\nUser-agent: *\nDisallow: /\n")

	if repo.isAllowedByRobots("https://example.com/private/page") {
		t.Error("a Disallow in our own group must apply")
	}

	if !repo.isAllowedByRobots("https://example.com/public") {
		t.Error("our own group allows /public; the catch-all group must not apply to us")
	}
}

func TestRobotsCatchAllAppliesWhenWeAreNotNamed(t *testing.T) {
	repo := robotsRepo("User-agent: SomeOtherBot\nDisallow: /\n\nUser-agent: *\nDisallow: /admin\n")

	if repo.isAllowedByRobots("https://example.com/admin") {
		t.Error("the catch-all group must apply when no group names us")
	}

	if !repo.isAllowedByRobots("https://example.com/docs") {
		t.Error("another bot's ban must not apply to us")
	}
}
