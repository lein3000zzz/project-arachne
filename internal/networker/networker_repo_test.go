package networker

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"
)

func TestFetchSendsTheConfiguredUserAgent(t *testing.T) {
	var got string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html></html>"))
	}))
	defer srv.Close()

	const ua = "project-arachne/0.1 (+https://example.com/bot)"

	res, err := NewNetworker(zap.NewNop().Sugar(), ua).Fetch(srv.URL)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	if got != ua {
		t.Errorf("User-Agent = %q, want %q", got, ua)
	}

	if res.ContentType != "text/html" {
		t.Errorf("ContentType = %q: the response header must win over sniffing", res.ContentType)
	}
}
