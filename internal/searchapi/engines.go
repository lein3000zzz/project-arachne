package searchapi

import (
	"context"
	"net/http"
	"slices"
	"time"

	"go.uber.org/zap"
)

// EngineLister is the web search's engines that answer now; optional, as
// without it nothing searches the web.
type EngineLister interface {
	Engines(ctx context.Context) ([]string, error)
}

const enginesTimeout = 10 * time.Second

type enginesHandler struct {
	logger *zap.SugaredLogger
	lister EngineLister
	// What new labs start with, of those that answer.
	defaults []string
	// Whether a question may be looked up on the web, with these engines.
	lookUp bool
}

type enginesResponse struct {
	Available []string `json:"available"`
	Default   []string `json:"default"`
	LookUp    bool     `json:"look_up"`
}

func (h *enginesHandler) engines(w http.ResponseWriter, r *http.Request) {
	none := enginesResponse{Available: []string{}, Default: []string{}}

	if h.lister == nil {
		writeJSON(w, http.StatusOK, none)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), enginesTimeout)
	defer cancel()

	available, err := h.lister.Engines(ctx)
	if err != nil {
		h.logger.Warnw("Web search engines unavailable", "error", err)
		writeJSON(w, http.StatusOK, none)

		return
	}

	defaults := []string{}
	for _, e := range h.defaults {
		if slices.Contains(available, e) {
			defaults = append(defaults, e)
		}
	}

	writeJSON(w, http.StatusOK, enginesResponse{Available: available, Default: defaults, LookUp: h.lookUp && len(available) > 0})
}

// statusError carries its own status through the lab handler's error mapping.
type statusError struct {
	status int
	err    error
}

func (e statusError) Error() string { return e.err.Error() }
func (e statusError) Unwrap() error { return e.err }
