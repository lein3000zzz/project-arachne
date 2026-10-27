package searchapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"
	"web-crawler/internal/answer"
	"web-crawler/internal/retrieval"

	"go.uber.org/zap"
)

type answerHandler struct {
	logger   *zap.SugaredLogger
	answerer Answerer
	timeout  time.Duration
}

func answerRequest(r *http.Request) (answer.Request, error) {
	q := r.URL.Query()

	req := answer.Request{Query: q.Get("q"), Host: q.Get("host"), Mode: retrieval.ModeHybrid}
	if mode := q.Get("mode"); mode != "" {
		req.Mode = retrieval.Mode(mode)
	}

	if req.Query == "" {
		return req, retrieval.ErrEmptyQuery
	}

	return req, nil
}

func (h *answerHandler) context(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), h.timeout)
}

func (h *answerHandler) serveJSON(w http.ResponseWriter, r *http.Request) {
	if h.answerer == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "answering is not configured"})
		return
	}

	req, err := answerRequest(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}

	ctx, cancel := h.context(r)
	defer cancel()

	res, err := h.answerer.Answer(ctx, req, answer.Events{})
	if err != nil {
		status, message := h.failure(req, err)
		writeJSON(w, status, errorResponse{Error: message})

		return
	}

	writeJSON(w, http.StatusOK, res)
}

// serveStream sends server-sent events: sources once, the answer as delta events
// while it is generated, then done - or error at any point.
func (h *answerHandler) serveStream(w http.ResponseWriter, r *http.Request) {
	if h.answerer == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "answering is not configured"})
		return
	}

	req, err := answerRequest(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "streaming unsupported"})
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	send := func(event string, data any) error {
		payload, err := json.Marshal(data)
		if err != nil {
			return err
		}

		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, payload); err != nil {
			return err
		}

		flusher.Flush()

		return nil
	}

	ctx, cancel := h.context(r)
	defer cancel()

	// A write failing means the reader is gone; returning the error stops the
	// model generating for nobody.
	res, err := h.answerer.Answer(ctx, req, answer.Events{
		Sources: func(sources []answer.Source) error { return send("sources", sources) },
		Delta:   func(text string) error { return send("delta", map[string]string{"text": text}) },
	})
	if err != nil {
		_, message := h.failure(req, err)
		_ = send("error", errorResponse{Error: message})

		return
	}

	_ = send("done", map[string]any{"cited": res.Cited, "model": res.Model, "usage": res.Usage})
}

func (h *answerHandler) failure(req answer.Request, err error) (int, string) {
	switch {
	case errors.Is(err, answer.ErrNoSources):
		return http.StatusNotFound, err.Error()
	case errors.Is(err, retrieval.ErrEmptyQuery), errors.Is(err, retrieval.ErrInvalidRequest), errors.Is(err, retrieval.ErrUnknownMode):
		return http.StatusBadRequest, err.Error()
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout, "answering timed out"
	case errors.Is(err, context.Canceled):
		return 499, "request canceled"
	}

	h.logger.Errorw("Answer failed", "query", req.Query, "error", err)

	return http.StatusInternalServerError, "answering failed"
}
