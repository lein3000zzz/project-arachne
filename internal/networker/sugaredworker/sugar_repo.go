package sugaredworker

import (
	"context"
	"fmt"
	"net/url"
	"web-crawler/internal/domain/config"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/proto"
	"go.uber.org/zap"
)

type ExtraRodWorker struct {
	Logger           *zap.SugaredLogger
	LauncherInstance *launcher.Launcher
	Browser          *rod.Browser
}

func NewExtraRodParser(logger *zap.SugaredLogger) (*ExtraRodWorker, error) {
	l := launcher.New().
		Bin("/usr/bin/chromium-browser").
		Headless(true).
		Set("no-sandbox").
		Set("disable-gpu").
		Set("disable-dev-shm-usage")

	browserURL, err := l.Launch()
	if err != nil {
		logger.Errorf("failed to launch browser: %v", err)
		return nil, err
	}

	browser := rod.New().ControlURL(browserURL)

	if errConnect := browser.Connect(); errConnect != nil {
		return nil, errConnect
	}

	err = proto.BrowserSetDownloadBehavior{
		Behavior: proto.BrowserSetDownloadBehaviorBehaviorDeny,
	}.Call(browser)
	if err != nil {
		return nil, err
	}

	return &ExtraRodWorker{
		Logger:           logger,
		LauncherInstance: l,
		Browser:          browser,
	}, nil
}

func (p *ExtraRodWorker) RestartBrowserAndLauncher() error {
	if p.LauncherInstance == nil {
		p.LauncherInstance = launcher.New().
			Bin("/usr/bin/chromium-browser").
			Headless(true).
			Set("no-sandbox").
			Set("disable-gpu").
			Set("disable-dev-shm-usage")
	}

	browserURL, err := p.LauncherInstance.Launch()
	if err != nil {
		p.Logger.Errorf("failed to launch browser: %v", err)
		return err
	}

	p.Browser = rod.New().ControlURL(browserURL)

	if errConnect := p.Browser.Connect(); errConnect != nil {
		p.Logger.Warnw("failed to connect to browser", "err", err)
		return errConnect
	}

	return nil
}

func (p *ExtraRodWorker) PerformExtraTask(pageURL string, flags *config.ExtraTaskFlags) *ExtraTaskRes {
	page := p.getPageFromURL(pageURL)
	defer p.recoveryHelper()

	res := new(ExtraTaskRes)

	defer func(page *rod.Page) {
		err := page.Close()
		if err != nil {
			p.Logger.Warnw("failed to close page", "err", err)
		}
	}(page)

	if flags.ShouldScreenshot {
		p.takeScreenshot(pageURL, page)
	}

	if flags.ParseRenderedHTML {
		res.HTMLTask = []byte(p.getRenderedHTML(page))
	}

	return res
}

func (p *ExtraRodWorker) getPageFromURL(pageURL string) *rod.Page {
	defer p.recoveryHelper()
	return p.Browser.MustPage(pageURL)
}

func (p *ExtraRodWorker) takeScreenshot(pageURL string, page *rod.Page) {
	defer p.recoveryHelper()

	safeName := url.QueryEscape(pageURL)
	outPath := fmt.Sprintf("%s/%s.png", defaultOutDir, safeName)

	page.MustWaitLoad()
	page.MustScreenshot(outPath)

	p.Logger.Infof("screenshot saved: %s", outPath)
}

func (p *ExtraRodWorker) getRenderedHTML(page *rod.Page) string {
	defer p.recoveryHelper()

	page = page.MustWaitLoad()

	return page.MustHTML()
}

func (p *ExtraRodWorker) recoveryHelper() {
	if r := recover(); r != nil {
		p.Logger.Warnw("recovered from panic during getting html", "err", r)
		err := p.RestartBrowserAndLauncher()
		if err != nil {
			p.Logger.Warnw("failed to restart launcher and browser", "err", err)
			return
		}
	}
}

func (p *ExtraRodWorker) Shutdown(ctx context.Context) error {
	done := make(chan error, 1)
	go func() {
		done <- p.Browser.Close()
	}()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
