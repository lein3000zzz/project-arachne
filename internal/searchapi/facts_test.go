package searchapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"web-crawler/internal/facts"
	"web-crawler/internal/labs"
)

type fakeFacts struct {
	owner, lab string
	err        error
}

func (f *fakeFacts) Ledger(_ context.Context, owner, lab string) (facts.Ledger, error) {
	f.owner, f.lab = owner, lab
	return facts.Ledger{Facts: []facts.LabFact{{Claim: "Henn measures state capacity.", Quote: "I use distance."}}, Pages: 3}, f.err
}

func TestALabsLedgerIsItsOwners(t *testing.T) {
	get := func(services Services, path string) *httptest.ResponseRecorder {
		services.Labs = &fakeLabs{}
		return do(newTestHandler(services, testConfig()), signedIn(httptest.NewRequest(http.MethodGet, path, nil)))
	}

	f := &fakeFacts{}
	rec := get(Services{Facts: f}, "/api/labs/"+tid(7)+"/facts")
	if rec.Code != http.StatusOK || f.owner != testUser.ID || f.lab != tid(7) || !strings.Contains(rec.Body.String(), `"claim":"Henn measures state capacity."`) {
		t.Errorf("%d %s %+v", rec.Code, rec.Body, f)
	}

	f.err = labs.ErrNotFound
	if rec := get(Services{Facts: f}, "/api/labs/"+tid(7)+"/facts"); rec.Code != http.StatusNotFound {
		t.Errorf("not the user's: %d", rec.Code)
	}

	if rec := get(Services{}, "/api/labs/"+tid(7)+"/facts"); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("no ledger here: %d", rec.Code)
	}
}
