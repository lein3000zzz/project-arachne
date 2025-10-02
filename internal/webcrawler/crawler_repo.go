package webcrawler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
	"web-crawler/internal/networker"
	"web-crawler/internal/pageparser"
	"web-crawler/internal/pages"
	"web-crawler/internal/utils"
	"web-crawler/internal/webcrawler/cache"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/jimsmart/grobotstxt"
	"go.uber.org/zap"
)

type CrawlerRepo struct {
	Logger      *zap.SugaredLogger
	Parser      pageparser.PageParser
	Networker   networker.Networker
	PageRepo    pages.PageDataRepo
	CachePages  cache.CachedStorage
	CacheRobots cache.CachedStorage
}

func NewCrawlerRepo(logger *zap.SugaredLogger, parser pageparser.PageParser, networker networker.Networker, pageRepo pages.PageDataRepo, cachePages cache.CachedStorage, cacheRobots cache.CachedStorage) *CrawlerRepo {
	return &CrawlerRepo{
		Logger:      logger,
		Parser:      parser,
		Networker:   networker,
		PageRepo:    pageRepo,
		CachePages:  cachePages,
		CacheRobots: cacheRobots,
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
			canParse := repo.isAllowedRobots(link)
			repo.Logger.Infow("link %")
			if !canParse {
				repo.Logger.Warnw("Skipping link because of robots", "url", link)
				continue
			}

			cachedPageRaw, errCache := repo.CachePages.Get(link)

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

			//repo.TakeScreenshot(link)

			errCache = repo.CachePages.Set(link, pageData, cache.BaseTTL)
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

func (repo *CrawlerRepo) isAllowedRobots(urlToCheck string) bool {
	baseURL, err := utils.GetBaseURL(urlToCheck)
	if err != nil {
		repo.Logger.Errorw("Failed to get robots URL", "url", urlToCheck, "err", err)
		return false
	}

	var robots string
	// TODO
	cachedString, errRobotsCache := repo.CacheRobots.Get(baseURL)
	if errRobotsCache == nil {
		repo.Logger.Warnw("Robots cache hit", "url", urlToCheck)
		robots = cachedString
	}

	repo.Logger.Warnw("cache miss or some other redis error", "errCache", errRobotsCache, "errParseBool", errParseBool)

	robotsURL := baseURL + "/robots.txt"
	responseData, errFetch := repo.Networker.Fetch(robotsURL)
	if errFetch != nil {
		repo.Logger.Errorw("failed to fetch robots", "url", robotsURL, "err", errFetch)
		return false
	}

	if responseData.Status == http.StatusNotFound {
		repo.Logger.Warnw("robots URL not found", "url", robotsURL)
		return true
	}

	isAllowed := grobotstxt.AgentAllowed(string(responseData.Body), "project-arachne", urlToCheck)

	errSaveCache := repo.CacheRobots.Set(urlToCheck, strconv.FormatBool(isAllowed), cache.BaseTTL)
	if errSaveCache != nil {
		repo.Logger.Warnw("failed to save cache", "url", baseURL, "err", errSaveCache)
	}

	return isAllowed
}

func (repo *CrawlerRepo) TakeScreenshot(pageURL string) {
	outDir := "output/screenshots"

	safeName := url.QueryEscape(pageURL)
	outPath := fmt.Sprintf("%s/%s.png", outDir, safeName)

	l := launcher.New().Headless(true)

	browserURL, err := l.Launch()
	if err != nil {
		repo.Logger.Warnw("failed to launch browser", "err", err)
		return
	}

	browser := rod.New().ControlURL(browserURL)
	if err := browser.Connect(); err != nil {
		repo.Logger.Warnw("failed to connect to browser", "err", err)
		return
	}
	defer func() {
		_ = browser.Close()
	}()

	page := browser.MustPage(pageURL)
	page.MustWaitLoad()
	page.MustScreenshot(outPath)

	repo.Logger.Infof("screenshot saved: %s", outPath)
}
