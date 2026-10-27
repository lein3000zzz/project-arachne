package searchapi

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed web/index.html
var indexHTML []byte

//go:embed web/login.html
var loginHTML []byte

//go:embed web/favicon.svg
var faviconSVG []byte

//go:embed web/static
var webFiles embed.FS

// Inline scripts stay allowed for the theme, which has to be set before the
// first paint; styles come only from /static.
const pagePolicy = "default-src 'self'; script-src 'self' 'unsafe-inline'; style-src 'self'; img-src 'self' data:; " +
	"frame-ancestors 'none'; base-uri 'none'; form-action 'self'"

func servePage(w http.ResponseWriter, page []byte) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", pagePolicy)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(page)
}

func serveIndex(w http.ResponseWriter, _ *http.Request) { servePage(w, indexHTML) }

func serveFavicon(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(faviconSVG)
}

func staticHandler() http.Handler {
	files, err := fs.Sub(webFiles, "web/static")
	if err != nil {
		panic(err)
	}

	server := http.StripPrefix("/static/", http.FileServerFS(files))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}

		w.Header().Set("Cache-Control", "public, max-age=300")
		server.ServeHTTP(w, r)
	})
}
