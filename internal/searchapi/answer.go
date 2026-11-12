package searchapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
	"web-crawler/internal/answer"
	"web-crawler/internal/auth"
	"web-crawler/internal/llm"
	"web-crawler/internal/markdown"
	"web-crawler/internal/retrieval"
	"web-crawler/internal/weblookup"

	"go.uber.org/zap"
)

type answerHandler struct {
	logger     *zap.SugaredLogger
	answerer   Answerer
	labs       LabService
	timeout    time.Duration
	maxResults int
	recent     *recentAnswers
	web        WebLookup
	webTimeout time.Duration
}

type WebLookup interface {
	Look(ctx context.Context, userID, question string, events weblookup.Events) ([]string, error)
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

	verify := h.verifies(r)
	out := verifiedAnswer{Result: res, ID: h.keep(r, req, res, !verify)}

	var claims []answer.Claim

	if verify {
		v, err := h.answerer.Verify(ctx, res, answer.VerifyEvents{})
		if err != nil {
			h.logger.Warnw("Verifying an answer failed", "error", err)
		} else {
			out.Verification, claims = &v, v.Claims
		}

		h.recent.check(out.ID, v)
	}

	out.HTML = answerHTML(res.Answer, len(res.Sources), claims)

	writeJSON(w, http.StatusOK, out)
}

type verifiedAnswer struct {
	answer.Result
	// The answer's Markdown rendered: citations linked, claims marked.
	HTML string `json:"html"`
	// What "add to lab" names it by, while it is kept.
	ID           string               `json:"id,omitempty"`
	Verification *answer.Verification `json:"verification,omitempty"`
}

// answerHTML renders an answer for the page, its citations linking to the
// sources listed under it.
func answerHTML(text string, sources int, claims []answer.Claim) string {
	return markdown.Render(text, markdown.Options{CiteHref: "#src-%d", Sources: sources, Claims: claims})
}

// While an answer streams, its rendering is sent this often: Markdown reads
// as it is meant to as it arrives, without a render per token.
const htmlEvery = 150 * time.Millisecond

func (h *answerHandler) keep(r *http.Request, req answer.Request, res answer.Result, checked bool) string {
	user, _ := auth.UserFrom(r.Context())
	return h.recent.add(user.ID, req.Query, res, checked)
}

// verifies: the answer is checked unless the request says verify=0 - the
// check costs a model call per claim.
func (h *answerHandler) verifies(r *http.Request) bool {
	return h.answerer.Verifying() && r.URL.Query().Get("verify") != "0"
}

// serveStream sends server-sent events: the listed results and the sources
// once, the answer as delta events while it is generated - and html events,
// its rendering so far - then done, or error at any point. A checked answer
// goes on: claims, how it splits into claims, with the rendering carrying a
// pending mark for each; a claim event for each verdict; then verified.
func (h *answerHandler) serveStream(w http.ResponseWriter, r *http.Request) {
	h.stream(w, r, h.timeout, nil)
}

// serveWebStream looks on the web for the question first - searched, then
// reading events while the pages picked are crawled, then looked with the
// pages read - and answers from those pages, with the labs ticked, before the
// rest of the index. When the web gives nothing, looked says why and the index
// answers as it would have.
func (h *answerHandler) serveWebStream(w http.ResponseWriter, r *http.Request) {
	if h.web == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: weblookup.ErrUnavailable.Error()})
		return
	}

	h.stream(w, r, h.timeout+h.webTimeout, func(ctx context.Context, req *answer.Request, send func(string, any) error) error {
		user, _ := auth.UserFrom(ctx)

		pages, err := h.web.Look(ctx, user.ID, req.Query, weblookup.Events{
			Searched: func(query string, found []weblookup.Found, failed map[string]string) error {
				return send("searched", map[string]any{"query": query, "results": found, "failed": failed})
			},
			Reading: func(reading []weblookup.Reading) error { return send("reading", reading) },
		})
		if err != nil {
			if errors.Is(err, context.Canceled) {
				return err
			}

			return send("looked", map[string]any{"pages": []string{}, "error": h.webFailure(err)})
		}

		for _, p := range pages {
			if !slices.Contains(req.LabURLs, p) {
				req.LabURLs = append(req.LabURLs, p)
			}
		}

		req.Fresh = len(pages) > 0

		// Before the sources: the page tags these as from the web, not a lab.
		return send("looked", map[string]any{"pages": pages})
	})
}

// webFailure says why looking on the web did not work.
func (h *answerHandler) webFailure(err error) string {
	switch {
	case errors.Is(err, weblookup.ErrUnavailable), errors.Is(err, weblookup.ErrNoEngines), errors.Is(err, weblookup.ErrNothingFound),
		errors.Is(err, weblookup.ErrBusy):
		return err.Error()
	case errors.Is(err, context.DeadlineExceeded):
		return "looking on the web took too long"
	}

	h.logger.Warnw("Looking on the web failed", "error", err)

	return "the web could not be searched"
}

// stream answers over server-sent events; before, when given, runs once the
// stream is open - looking on the web - and may change the request. Its error
// ends the stream: the reader is gone.
func (h *answerHandler) stream(w http.ResponseWriter, r *http.Request, timeout time.Duration,
	before func(ctx context.Context, req *answer.Request, send func(string, any) error) error) {
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

	ctx, cancel := context.WithTimeout(r.Context(), timeout)
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

	if before != nil {
		if err := before(ctx, &req, send); err != nil {
			return
		}
	}

	// A write failing means the reader is gone; returning the error stops the
	// model generating for nobody.
	var (
		written  strings.Builder
		sources  int
		rendered time.Time
	)

	res, err := h.answerer.Answer(ctx, req, answer.Events{
		Results: func(found []answer.Found) error { return send("results", found) },
		Sources: func(list []answer.Source) error {
			sources = len(list)
			return send("sources", list)
		},
		Delta: func(text string) error {
			if err := send("delta", map[string]string{"text": text}); err != nil {
				return err
			}

			written.WriteString(text)

			if time.Since(rendered) < htmlEvery {
				return nil
			}

			rendered = time.Now()

			return send("html", map[string]string{"html": answerHTML(written.String(), sources, nil)})
		},
	})
	if err != nil {
		_, message := h.failure(req, err)
		_ = send("error", errorResponse{Error: message})

		return
	}

	verify := h.verifies(r)
	id := h.keep(r, req, res, !verify)

	if err := send("html", map[string]string{"html": answerHTML(res.Answer, len(res.Sources), nil)}); err != nil {
		return
	}

	if err := send("done", map[string]any{"cited": res.Cited, "scope": res.Scope, "weak": res.Weak, "model": res.Model, "usage": res.Usage,
		"verifying": verify, "id": id, "web": before != nil}); err != nil || !verify {
		return
	}

	v, err := h.answerer.Verify(ctx, res, answer.VerifyEvents{
		Claims: func(parts []answer.Part, claims []answer.Claim) error {
			return send("claims", map[string]any{"parts": parts, "claims": claims, "html": answerHTML(res.Answer, len(res.Sources), claims)})
		},
		Claim:     func(i int, c answer.Claim) error { return send("claim", map[string]any{"i": i, "claim": c}) },
		Conflicts: func(c []answer.Conflict) error { return send("conflicts", c) },
	})
	if err != nil {
		h.logger.Warnw("Verifying an answer failed", "error", err)
		_ = send("error", errorResponse{Error: "the answer could not be checked"})

		return
	}

	h.recent.check(id, v)
	_ = send("verified", map[string]any{"model": v.Model, "seconds": v.Seconds})
}

func (h *answerHandler) failure(req answer.Request, err error) (int, string) {
	switch {
	case errors.Is(err, answer.ErrNoSources):
		return http.StatusNotFound, err.Error()
	case errors.Is(err, llm.ErrNoModel):
		return http.StatusServiceUnavailable, llm.ErrNoModel.Error()
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
