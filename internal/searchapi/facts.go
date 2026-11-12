package searchapi

import (
	"context"
	"net/http"
	"web-crawler/internal/facts"
)

// FactService is optional: without it, labs have no ledger.
type FactService interface {
	Ledger(ctx context.Context, ownerID, labID string) (facts.Ledger, error)
}

type factsHandler struct {
	service FactService
	labs    *labsHandler
}

// ledger is a lab's facts, and how far digesting its pages has come.
func (h *factsHandler) ledger(w http.ResponseWriter, r *http.Request) {
	if h.service == nil {
		writeJSON(w, http.StatusServiceUnavailable, errorResponse{Error: "facts are not kept on this server"})
		return
	}

	h.labs.withLab(w, r, func(ctx context.Context, owner, lab string) (int, any, error) {
		l, err := h.service.Ledger(ctx, owner, lab)
		return http.StatusOK, l, err
	})
}
