package networker

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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

	res, err := NewNetworker(zap.NewNop().Sugar(), ua, 5*time.Second).Fetch(srv.URL)
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

func TestFetchReportsWhetherARedirectRenamedThePage(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/page/", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/login", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("login")) })
	mux.HandleFunc("/old", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/page/", http.StatusMovedPermanently)
	})
	mux.HandleFunc("/slash", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/slash/", http.StatusFound)
	})
	mux.HandleFunc("/slash/", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	mux.HandleFunc("/private", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/login", http.StatusFound)
	})
	mux.HandleFunc("/chain", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/private", http.StatusMovedPermanently)
	})
	mux.HandleFunc("/mirrored", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/mirrored/", http.StatusMovedPermanently)
	})
	mux.HandleFunc("/mirrored/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/page/", http.StatusTemporaryRedirect)
	})

	srv := httptest.NewServer(mux)
	defer srv.Close()

	cases := []struct {
		path         string
		wantFinal    string
		wantIdentity string
	}{
		{"/page/", "/page/", "/page/"},
		{"/old", "/page/", "/page/"},
		{"/slash", "/slash/", "/slash/"},
		{"/private", "/login", "/private"},
		{"/chain", "/login", "/private"},
		{"/mirrored", "/page/", "/mirrored/"},
	}

	nw := NewNetworker(zap.NewNop().Sugar(), "test", 5*time.Second)

	for _, tc := range cases {
		res, err := nw.Fetch(srv.URL + tc.path)
		if err != nil {
			t.Fatalf("Fetch(%s): %v", tc.path, err)
		}

		if res.FinalURL != srv.URL+tc.wantFinal || res.IdentityURL != srv.URL+tc.wantIdentity {
			t.Errorf("Fetch(%s) = final %q identity %q, want %q and %q",
				tc.path, res.FinalURL, res.IdentityURL, srv.URL+tc.wantFinal, srv.URL+tc.wantIdentity)
		}
	}
}

func TestFetchStillStopsRedirectLoops(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, r.URL.Path, http.StatusMovedPermanently)
	}))
	defer srv.Close()

	if _, err := NewNetworker(zap.NewNop().Sugar(), "test", 5*time.Second).Fetch(srv.URL + "/loop"); err == nil {
		t.Fatal("a redirect loop must fail the fetch")
	}
}
