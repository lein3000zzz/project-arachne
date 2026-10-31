// Package searchapi serves retrieval over HTTP.
package searchapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"time"
	"web-crawler/internal/answer"
	"web-crawler/internal/retrieval"

	"go.uber.org/zap"
)

type Searcher interface {
	Search(ctx context.Context, req retrieval.Request) ([]retrieval.Result, error)
}

// Answerer is optional: without a chat model the answer endpoints report 503.
type Answerer interface {
	Answer(ctx context.Context, req answer.Request, events answer.Events) (answer.Result, error)
	Verify(ctx context.Context, res answer.Result, events answer.VerifyEvents) (answer.Verification, error)
	Verifying() bool
}

type Config struct {
	DefaultLimit   int
	MaxLimit       int
	RequestTimeout time.Duration
	AnswerTimeout  time.Duration
}

type Response struct {
	Query   string             `json:"query"`
	Mode    retrieval.Mode     `json:"mode"`
	Host    string             `json:"host,omitempty"`
	TookMS  int64              `json:"took_ms"`
	Results []retrieval.Result `json:"results"`
}

type errorResponse struct {
	Error string `json:"error"`
}

// Services are what the API serves; all but Auth and Searcher may be nil.
type Services struct {
	Auth      Authenticator
	Searcher  Searcher
	Answerer  Answerer
	Runs      RunService
	Labs      LabService
	Gatherer  Gatherer
	Uploads   UploadStore
	Artifacts ArtifactService
}

func NewHandler(logger *zap.SugaredLogger, services Services, cfg Config) http.Handler {
	mux := http.NewServeMux()
	searcher := services.Searcher

	signIn := &authHandler{logger: logger, auth: services.Auth}
	mux.HandleFunc("GET /login", signIn.loginPage)
	mux.HandleFunc("POST /api/login", signIn.login)
	mux.HandleFunc("POST /api/logout", signIn.logout)
	mux.Handle("GET /api/me", signIn.api(signIn.me))
	mux.Handle("POST /api/me/password", signIn.api(signIn.changePassword))

	// Answers are kept a while, by id, for "add to lab".
	recent := newRecentAnswers(recentAnswersKept, recentAnswersFor)

	answers := &answerHandler{logger: logger, answerer: services.Answerer, labs: services.Labs, timeout: cfg.AnswerTimeout, maxResults: cfg.MaxLimit,
		recent: recent}
	mux.Handle("GET /answer", signIn.api(answers.serveJSON))
	mux.Handle("GET /answer/stream", signIn.api(answers.serveStream))

	runsAPI := &runsHandler{logger: logger, service: services.Runs}
	mux.Handle("POST /api/runs", signIn.api(runsAPI.submit))
	mux.Handle("GET /api/runs", signIn.api(runsAPI.list))
	mux.Handle("GET /api/runs/{id}", signIn.api(runsAPI.get))

	labsAPI := &labsHandler{logger: logger, service: services.Labs, planTimeout: cfg.AnswerTimeout}
	mux.Handle("GET /api/labs", signIn.api(labsAPI.list))
	mux.Handle("POST /api/labs", signIn.api(labsAPI.create))
	mux.Handle("POST /api/labs/plan", signIn.api(labsAPI.plan))
	mux.Handle("GET /api/labs/{id}", signIn.api(labsAPI.get))
	mux.Handle("DELETE /api/labs/{id}", signIn.api(labsAPI.delete))
	mux.Handle("PUT /api/labs/{id}/engines", signIn.api(labsAPI.setEngines))

	gatherAPI := &gatherHandler{logger: logger, service: services.Gatherer, labs: labsAPI}
	mux.Handle("GET /api/engines", signIn.api(gatherAPI.engines))
	mux.Handle("POST /api/labs/{id}/gather", signIn.api(gatherAPI.start))
	mux.Handle("GET /api/labs/{id}/gathering", signIn.api(gatherAPI.latest))
	mux.Handle("GET /api/labs/{id}/pages", signIn.api(labsAPI.pages))
	mux.Handle("POST /api/labs/{id}/sources", signIn.api(labsAPI.addSources))
	mux.Handle("GET /api/labs/{id}/references", signIn.api(labsAPI.references))
	mux.Handle("POST /api/labs/{id}/references", signIn.api(labsAPI.addReferences))
	mux.Handle("POST /api/labs/{id}/uploads", signIn.api(labsAPI.addUploads))

	artifactsAPI := &artifactsHandler{logger: logger, service: services.Artifacts, recent: recent, draftTimeout: cfg.AnswerTimeout}
	mux.Handle("POST /api/labs/{id}/artifacts/outline", signIn.api(artifactsAPI.draft))
	mux.Handle("POST /api/labs/{id}/artifacts", signIn.api(artifactsAPI.build))
	mux.Handle("GET /api/labs/{id}/artifacts", signIn.api(artifactsAPI.list))
	mux.Handle("POST /api/labs/{id}/artifacts/answers", signIn.api(artifactsAPI.saveAnswer))
	mux.Handle("GET /api/artifacts/{id}", signIn.api(artifactsAPI.get))
	mux.Handle("GET /api/artifacts/{id}/markdown", signIn.api(artifactsAPI.markdown))
	mux.Handle("POST /api/artifacts/{id}/rebuild", signIn.api(artifactsAPI.rebuild))
	mux.Handle("DELETE /api/artifacts/{id}", signIn.api(artifactsAPI.delete))

	uploadsAPI := &uploadsHandler{logger: logger, files: services.Uploads}
	mux.Handle("GET /uploads/{id}", signIn.page(uploadsAPI.serve))
	mux.Handle("POST /api/labs/{id}/sources/{source}/refresh", signIn.api(labsAPI.refresh))
	mux.Handle("DELETE /api/labs/{id}/sources/{source}", signIn.api(labsAPI.removeSource))

	mux.Handle("GET /search", signIn.api(func(w http.ResponseWriter, r *http.Request) {
		req, err := parseRequest(r, cfg)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
			return
		}

		ctx, cancel := context.WithTimeout(r.Context(), cfg.RequestTimeout)
		defer cancel()

		if req.URLs, err = labURLs(ctx, services.Labs, r); err != nil {
			status, message := labsAPI.failure(err)
			writeJSON(w, status, errorResponse{Error: message})

			return
		}

		started := time.Now()

		results, err := searcher.Search(ctx, req)

		switch {
		case err == nil:
		case errors.Is(err, retrieval.ErrEmptyQuery), errors.Is(err, retrieval.ErrInvalidRequest), errors.Is(err, retrieval.ErrUnknownMode),
			errors.Is(err, retrieval.ErrNoReranker):
			writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
			return
		case errors.Is(err, context.DeadlineExceeded):
			writeJSON(w, http.StatusGatewayTimeout, errorResponse{Error: "search timed out"})
			return
		default:
			logger.Errorw("Search failed", "query", req.Query, "mode", req.Mode, "error", err)
			writeJSON(w, http.StatusInternalServerError, errorResponse{Error: "search failed"})
			return
		}

		writeJSON(w, http.StatusOK, Response{
			Query:   req.Query,
			Mode:    req.Mode,
			Host:    req.Host,
			TookMS:  time.Since(started).Milliseconds(),
			Results: results,
		})
	}))

	mux.Handle("GET /{$}", signIn.page(serveIndex))
	mux.Handle("GET /labs/{id}", signIn.page(serveLab))
	mux.Handle("GET /labs/{id}/artifacts/{artifact}", signIn.page(serveArtifact))
	mux.HandleFunc("GET /favicon.svg", serveFavicon)
	mux.Handle("GET /static/", staticHandler())

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Refuses state-changing requests a browser makes on another site's behalf,
	// a forged login included.
	protected := http.NewCrossOriginProtection().Handler(mux)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		protected.ServeHTTP(w, r)
	})
}

func parseRequest(r *http.Request, cfg Config) (retrieval.Request, error) {
	q := r.URL.Query()

	req := retrieval.Request{
		Query: q.Get("q"),
		Host:  q.Get("host"),
		Mode:  retrieval.ModeHybrid,
		Limit: cfg.DefaultLimit,
	}

	if mode := q.Get("mode"); mode != "" {
		req.Mode = retrieval.Mode(mode)
	}

	if raw := q.Get("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 1 || limit > cfg.MaxLimit {
			return req, fmt.Errorf("limit must be an integer in [1, %d]", cfg.MaxLimit)
		}

		req.Limit = limit
	}

	var err error

	req.PageRankWeight, req.SiteRankWeight, req.Rerank, err = rankingOptions(q)

	return req, err
}

// rankingOptions reads the optional pagerank and siterank weights and the rerank
// switch; an absent parameter leaves the configured default.
func rankingOptions(q url.Values) (pageWeight, siteWeight *float64, rerank *bool, err error) {
	weight := func(name string) (*float64, error) {
		raw := q.Get(name)
		if raw == "" {
			return nil, nil
		}

		v, err := strconv.ParseFloat(raw, 64)
		if err != nil || v < 0 || math.IsInf(v, 0) || math.IsNaN(v) {
			return nil, fmt.Errorf("%s must be a number >= 0", name)
		}

		return &v, nil
	}

	if pageWeight, err = weight("pagerank"); err != nil {
		return nil, nil, nil, err
	}

	if siteWeight, err = weight("siterank"); err != nil {
		return nil, nil, nil, err
	}

	if raw := q.Get("rerank"); raw != "" {
		v, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("rerank must be true or false")
		}

		rerank = &v
	}

	return pageWeight, siteWeight, rerank, nil
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
