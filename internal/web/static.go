package web

import (
	"embed"
	"io/fs"
	"net/http"
)

// DEPLOY-04: assets are embedded, so the binary runs with no files beside it.
//
//go:embed static
var staticFS embed.FS

// staticHandler serves the embedded assets with a long cache lifetime. Asset
// names are stable within a build, and the binary is replaced wholesale on
// upgrade, so a long max-age is safe.
func staticHandler() http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic("web: static assets missing from binary: " + err.Error())
	}
	fileServer := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		fileServer.ServeHTTP(w, r)
	})
}
