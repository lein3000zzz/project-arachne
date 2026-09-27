package webcrawler

import (
	"context"
	"errors"
	"time"
	"web-crawler/internal/domain/config"
	"web-crawler/internal/networker"
	"web-crawler/internal/networker/sugaredworker"
)

type nopNetworker struct{}

func (nopNetworker) Stop()                                        {}
func (nopNetworker) Fetch(string) (*networker.FetchResult, error) { return nil, errors.New("unused") }

type nopExtra struct{}

func (nopExtra) RestartBrowserAndLauncher() error { return nil }
func (nopExtra) PerformExtraTask(string, *config.ExtraTaskFlags) *sugaredworker.ExtraTaskRes {
	return nil
}
func (nopExtra) Shutdown(context.Context) error { return nil }

type nopCache struct{}

func (nopCache) Set(string, any, time.Duration) error { return nil }
func (nopCache) Get(string) (string, error)           { return "", errors.New("miss") }
func (nopCache) Stop(context.Context) error           { return nil }
