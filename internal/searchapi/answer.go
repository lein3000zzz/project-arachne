package searchapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"
	"web-crawler/internal/answer"
	"web-crawler/internal/retrieval"

	"go.uber.org/zap"
)

type answerHandler struct {
	logger     *zap.SugaredLogger
	answerer   Answerer
	labs       LabService
	timeout    time.Duration
	maxResults int
}

func (h *answerHandler) request(r *http.Request) (answer.Request, error) {
	req, err := answerRequest(r)
	if err != nil {
		return req, err
	}

	if raw := r.URL.Query().Get("results"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 || n > h.maxResults {
			return req, fmt.Errorf("results must be an integer in [0, %d]", h.maxResults)
		}

		req.Results = n
	}

	return req, nil
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

	var err error

	req.PageRankWeight, req.SiteRankWeight, req.Rerank, err = rankingOptions(q)

	return req, err
}

func (h *answerHandler) context(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), h.timeout)
}

func (h *answerHandler) serveJSON(w http.ResponseWriter, r *http.Request) {
	if h.answerer == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "answering is not configured"})
		return
	}

	req, err := h.request(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}

	ctx, cancel := h.context(r)
	defer cancel()

	if req.LabURLs, err = labURLs(ctx, h.labs, r); err != nil {
		status, message := labFailure(h.logger, err)
		writeJSON(w, status, errorResponse{Error: message})

		return
	}

	res, err := h.answerer.Answer(ctx, req, answer.Events{})
	if err != nil {
		status, message := h.failure(req, err)
		writeJSON(w, status, errorResponse{Error: message})

		return
	}

	out := verifiedAnswer{Result: res}

	if h.verifies(r) {
		v, err := h.answerer.Verify(ctx, res, answer.VerifyEvents{})
		if err != nil {
			h.logger.Warnw("Verifying an answer failed", "error", err)
		} else {
			out.Verification = &v
		}
	}

	writeJSON(w, http.StatusOK, out)
}

type verifiedAnswer struct {
	answer.Result
	Verification *answer.Verification `json:"verification,omitempty"`
}

// verifies: the answer is checked unless the request says verify=0 - the
// check costs a model call per claim.
func (h *answerHandler) verifies(r *http.Request) bool {
	return h.answerer.Verifying() && r.URL.Query().Get("verify") != "0"
}

// serveStream sends server-sent events: the listed results and the sources
// once, the answer as delta events while it is generated, then done - or error
// at any point. A checked answer goes on: claims, how it splits into claims,
// a claim event for each verdict, then verified.
func (h *answerHandler) serveStream(w http.ResponseWriter, r *http.Request) {
	if h.answerer == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "answering is not configured"})
		return
	}

	req, err := h.request(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "streaming unsupported"})
		return
	}

	ctx, cancel := h.context(r)
	defer cancel()

	if req.LabURLs, err = labURLs(ctx, h.labs, r); err != nil {
		status, message := labFailure(h.logger, err)
		writeJSON(w, status, errorResponse{Error: message})

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

	// A write failing means the reader is gone; returning the error stops the
	// model generating for nobody.
	res, err := h.answerer.Answer(ctx, req, answer.Events{
		Results: func(found []answer.Found) error { return send("results", found) },
		Sources: func(sources []answer.Source) error { return send("sources", sources) },
		Delta:   func(text string) error { return send("delta", map[string]string{"text": text}) },
	})
	if err != nil {
		_, message := h.failure(req, err)
		_ = send("error", errorResponse{Error: message})

		return
	}

	verify := h.verifies(r)

	if err := send("done", map[string]any{"cited": res.Cited, "scope": res.Scope, "weak": res.Weak, "model": res.Model, "usage": res.Usage,
		"verifying": verify}); err != nil || !verify {
		return
	}

	v, err := h.answerer.Verify(ctx, res, answer.VerifyEvents{
		Claims: func(parts []answer.Part, claims []answer.Claim) error {
			return send("claims", map[string]any{"parts": parts, "claims": claims})
		},
		Claim: func(i int, c answer.Claim) error { return send("claim", map[string]any{"i": i, "claim": c}) },
	})
	if err != nil {
		h.logger.Warnw("Verifying an answer failed", "error", err)
		_ = send("error", errorResponse{Error: "the answer could not be checked"})

		return
	}

	_ = send("verified", map[string]any{"model": v.Model, "seconds": v.Seconds})
}

func (h *answerHandler) failure(req answer.Request, err error) (int, string) {
	switch {
	case errors.Is(err, answer.ErrNoSources):
		return http.StatusNotFound, err.Error()
	case errors.Is(err, retrieval.ErrEmptyQuery), errors.Is(err, retrieval.ErrInvalidRequest), errors.Is(err, retrieval.ErrUnknownMode),
		errors.Is(err, retrieval.ErrNoReranker):
		return http.StatusBadRequest, err.Error()
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout, "answering timed out"
	case errors.Is(err, context.Canceled):
		return 499, "request canceled"
	}

	h.logger.Errorw("Answer failed", "query", req.Query, "error", err)

	return http.StatusInternalServerError, "answering failed"
}
