package networker

import (
	"context"
	"io"
	"net/http"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

type NetworkWorker struct {
	Logger     *zap.SugaredLogger
	httpClient *http.Client
}

func NewNetworker(logger *zap.SugaredLogger) *NetworkWorker {
	return &NetworkWorker{
		Logger: logger,
		httpClient: &http.Client{
			Transport: otelhttp.NewTransport(http.DefaultTransport),
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

		resp, err := nw.httpClient.Do(req)
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

		fetchResult.Body, err = io.ReadAll(resp.Body)
		if err != nil {
			nw.Logger.Errorf("read body for %s error: %v", url, err)
			return nil, err
		}

		if len(fetchResult.Body) > 0 {
			fetchResult.ContentType = http.DetectContentType(fetchResult.Body)
		} else {
			fetchResult.ContentType = "application/octet-stream"
		}

		fetchResult.Status = resp.StatusCode
		span.SetStatus(codes.Ok, "Fetch completed successfully")

		return fetchResult, nil
	})
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
