package web

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static
var staticFS embed.FS

// Register adds the web routes to the mux.
func Register(mux *http.ServeMux) {
	subFS, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err)
	}
	fileServer := http.FileServer(http.FS(subFS))

	cspMiddleware := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; frame-ancestors 'none'")
			w.Header().Set("X-Content-Type-Options", "nosniff")
			next.ServeHTTP(w, r)
		})
	}

	mux.Handle("GET /{$}", cspMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/docs", http.StatusFound)
	})))

	mux.Handle("GET /docs", cspMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.URL.Path = "/docs.html"
		fileServer.ServeHTTP(w, r)
	})))

	mux.Handle("GET /openapi.json", cspMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.URL.Path = "/openapi.json"
		fileServer.ServeHTTP(w, r)
	})))

	mux.Handle("GET /static/", cspMiddleware(http.StripPrefix("/static/", fileServer)))
}
