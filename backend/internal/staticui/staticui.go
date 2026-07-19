// Package staticui embeds the built frontend (Vite/React) static assets
// and serves them as a single-page application. In local development
// without a frontend build, dist/ contains only a small placeholder page
// (see dist/index.html); the Docker image build always overwrites dist/
// with the real production build before compiling the Go binary.
package staticui

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed dist
var distFS embed.FS

// Handler returns an http.Handler that serves the embedded frontend build,
// falling back to index.html for any path that doesn't match a real file
// so client-side routing (if any) keeps working on refresh/deep links.
func Handler() (http.Handler, error) {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		return nil, err
	}
	fileServer := http.FileServer(http.FS(sub))

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cleanPath := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if cleanPath == "" {
			cleanPath = "index.html"
		}
		if _, err := fs.Stat(sub, cleanPath); err != nil {
			// Not a real asset (e.g. a client-side route): serve the SPA
			// shell and let the frontend router take over.
			r2 := new(http.Request)
			*r2 = *r
			r2.URL.Path = "/"
			fileServer.ServeHTTP(w, r2)
			return
		}
		fileServer.ServeHTTP(w, r)
	}), nil
}
