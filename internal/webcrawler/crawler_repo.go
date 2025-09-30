package webcrawler

import (
	"time"
	"web-crawler/internal/networker"
	"web-crawler/internal/pageparser"
	"web-crawler/internal/pages"

	"go.uber.org/zap"
)

type CrawlerRepo struct {
	Logger    *zap.SugaredLogger
	Parser    pageparser.PageParser
	Networker networker.Networker
	PageRepo  pages.PageDataRepo
}

func NewCrawlerRepo(logger *zap.SugaredLogger, parser pageparser.PageParser, networker networker.Networker, pageRepo pages.PageDataRepo) *CrawlerRepo {
	return &CrawlerRepo{
		Logger:    logger,
		Parser:    parser,
		Networker: networker,
		PageRepo:  pageRepo,
	}
}

func (repo *CrawlerRepo) StartCrawler(url string, depth int) error {
	err := repo.PageRepo.EnsureConnectivity()
	if err != nil {
		repo.Logger.Errorf("Error ensuring connectivity: %v", err)
		return err
	}

	currDepth := 0
	links := []string{url}
	for currDepth < depth {
		var newLinks []string
		for _, link := range links {
			fetchRes, err := repo.Networker.Fetch(link)
			if err != nil {
				repo.Logger.Warnw("Failed to fetch link", "url", link, "depth", currDepth)
				continue
			}

			linksFromThePage := repo.Parser.ParseLinks(fetchRes.Body, link)

			pageData := pages.PageData{
				URL:           link,
				Status:        fetchRes.Status,
				Links:         linksFromThePage,
				LastRunID:     "to_be_implemented",
				LastUpdatedAt: time.Now(),
				FoundAt:       time.Now(),
				ContentType:   fetchRes.ContentType,
			}

			errSaving := repo.PageRepo.SavePage(pageData)

			if errSaving != nil {
				repo.Logger.Warnw("Failed to save page", "url", link, "depth", currDepth)
			}

			newLinks = append(newLinks, linksFromThePage...)
		}
		links = newLinks
		currDepth++
	}

	return nil
}
