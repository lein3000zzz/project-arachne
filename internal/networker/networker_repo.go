package networker

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

// net/http's own limit, which a custom CheckRedirect has to re-impose.
const maxRedirects = 10

var errTooManyRedirects = errors.New("stopped after 10 redirects")

// A body this large is a file, not a page: a dataset, a video, a book scan.
// The largest documents read on purpose, lab PDFs, fit well within it.
const maxBodyBytes = 32 << 20

var ErrBodyTooLarge = errors.New("response body too large")

type NetworkWorker struct {
	Logger     *zap.SugaredLogger
	httpClient *http.Client
	userAgent  string
}

func NewNetworker(logger *zap.SugaredLogger, userAgent string, timeout time.Duration) *NetworkWorker {
	return &NetworkWorker{
		Logger:    logger,
		userAgent: userAgent,
		httpClient: &http.Client{
			Transport: otelhttp.NewTransport(http.DefaultTransport),
			Timeout:   timeout,
		},
	}
}

func (nw *NetworkWorker) Fetch(url string) (*FetchResult, error) {
	return nw.withSpan("Fetch", func(ctx context.Context, span trace.Span) (*FetchResult, error) {
		span.SetAttributes(
			attribute.String("url", url),
			attribute.String("method", "GET"),
		)

		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			nw.Logger.Errorf("fetch url %s error: %v", url, err)
			return nil, err
		}

		req.Header.Set("User-Agent", nw.userAgent)

		identity, renamed := url, true

		client := *nw.httpClient
		client.CheckRedirect = func(next *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return errTooManyRedirects
			}

			if renamed && isIdentityRedirect(next.Response.StatusCode, via[len(via)-1].URL, next.URL) {
				identity = next.URL.String()
			} else {
				renamed = false
			}

			return nil
		}

		resp, err := client.Do(req)
		nw.Logger.Infof("fetch url %s", url)
		if err != nil {
			nw.Logger.Errorf("fetch url %s error", url)
			return nil, err
		}

		span.SetAttributes(
			attribute.Int("status_code", resp.StatusCode),
			attribute.String("content_type", resp.Header.Get("Content-Type")),
		)

		fetchResult := &FetchResult{}
		defer resp.Body.Close()

		fetchResult.Body, err = io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
		if err != nil {
			nw.Logger.Errorf("read body for %s error: %v", url, err)
			return nil, err
		}

		if len(fetchResult.Body) > maxBodyBytes {
			nw.Logger.Warnf("body of %s is over %d MiB, not read", url, maxBodyBytes>>20)
			return nil, ErrBodyTooLarge
		}

		// Header first: DetectContentType has no JavaScript verdict and reports
		// JS source as text/plain, which routes it away from the JS parsers.
		fetchResult.ContentType = resp.Header.Get("Content-Type")
		if fetchResult.ContentType == "" {
			if len(fetchResult.Body) > 0 {
				fetchResult.ContentType = http.DetectContentType(fetchResult.Body)
			} else {
				fetchResult.ContentType = "application/octet-stream"
			}
		}

		fetchResult.Status = resp.StatusCode
		fetchResult.FinalURL = resp.Request.URL.String()
		fetchResult.IdentityURL = identity
		span.SetStatus(codes.Ok, "Fetch completed successfully")

		return fetchResult, nil
	})
}

func isIdentityRedirect(status int, from, to *url.URL) bool {
	if status == http.StatusMovedPermanently || status == http.StatusPermanentRedirect {
		return true
	}

	return strings.EqualFold(from.Host, to.Host) &&
		from.RawQuery == to.RawQuery &&
		strings.TrimSuffix(from.EscapedPath(), "/") == strings.TrimSuffix(to.EscapedPath(), "/")
}

func (nw *NetworkWorker) withSpan(name string, f func(context.Context, trace.Span) (*FetchResult, error)) (*FetchResult, error) {
	tracer := otel.Tracer("networker")
	ctx, span := tracer.Start(context.Background(), name)
	defer span.End()

	result, err := f(ctx, span)
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, "Operation failed")
	}

	return result, err
}

func (nw *NetworkWorker) Stop() {
	nw.httpClient.CloseIdleConnections()
}
