package searchapi

import (
	_ "embed"
	"net/http"
)

//go:embed web/index.html
var indexHTML []byte

func serveIndex(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; img-src 'self' data:")
	_, _ = w.Write(indexHTML)
}
