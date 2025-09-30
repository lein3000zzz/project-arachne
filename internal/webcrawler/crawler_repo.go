package webcrawler

import (
	"encoding/json"
	"time"
	"web-crawler/internal/cache"
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
	Cache     cache.CachedStorage
}

func NewCrawlerRepo(logger *zap.SugaredLogger, parser pageparser.PageParser, networker networker.Networker, pageRepo pages.PageDataRepo, cache cache.CachedStorage) *CrawlerRepo {
	return &CrawlerRepo{
		Logger:    logger,
		Parser:    parser,
		Networker: networker,
		PageRepo:  pageRepo,
		Cache:     cache,
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
			cachedPageRaw, errCache := repo.Cache.Get(link)

			var cachedPageData pages.PageData
			errUnmarshal := json.Unmarshal([]byte(cachedPageRaw), &cachedPageData)

			if errCache == nil && errUnmarshal == nil {
				repo.Logger.Infof("using cached page: %s", link)
				newLinks = append(newLinks, cachedPageData.Links...)
				continue
			}
			
			fetchRes, err := repo.Networker.Fetch(link)
			if err != nil {
				repo.Logger.Warnw("Failed to fetch link", "url", link, "depth", currDepth)
				continue
			}

			linksFromThePage := repo.Parser.ParseLinks(fetchRes.Body, link)

			pageData := &pages.PageData{
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
				repo.Logger.Warnw("Failed to save page", "url", link, "depth", currDepth, "err", errCache)
			}

			errCache = repo.Cache.Set(link, pageData, cache.BaseTTL)
			if errCache != nil {
				repo.Logger.Warnw("Failed to cache page", "url", link, "depth", currDepth, "err", errCache)
			}

			newLinks = append(newLinks, linksFromThePage...)
		}
		links = newLinks
		currDepth++
	}

	return nil
}
