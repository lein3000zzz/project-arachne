package networker

import (
	"io"
	"net/http"

	"go.uber.org/zap"
)

type NetworkWorker struct {
	Logger *zap.SugaredLogger
}

func NewNetworker(logger *zap.SugaredLogger) *NetworkWorker {
	return &NetworkWorker{
		Logger: logger,
	}
}

func (repo *NetworkWorker) Fetch(url string) (*FetchResult, error) {
	resp, err := http.Get(url)
	repo.Logger.Infof("fetch url %s", url)
	if err != nil {
		repo.Logger.Errorf("fetch url %s error", url)
		return nil, err
	}

	fetchResult := new(FetchResult)

	defer resp.Body.Close()

	fetchResult.Body, err = io.ReadAll(resp.Body)
	if err != nil {
		repo.Logger.Errorf("read body for %s error: %v", url, err)
		return nil, err
	}

	if len(fetchResult.Body) > 0 {
		fetchResult.ContentType = http.DetectContentType(fetchResult.Body)
	} else {
		fetchResult.ContentType = "application/octet-stream"
	}

	fetchResult.Status = resp.StatusCode

	return fetchResult, err
}
