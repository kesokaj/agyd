package web

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed static
var staticFS embed.FS

// Route is one web route: a ServeMux path pattern, the HTTP methods it
// accepts, and its handler.
type Route struct {
	Methods []string
	Pattern string
	Handler http.Handler
}

// Routes returns the web routes so callers can mount them on a shared mux and
// method table.
func Routes() []Route {
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

	getHead := func(pattern string, h http.Handler) Route {
		return Route{
			Methods: []string{http.MethodGet, http.MethodHead},
			Pattern: pattern,
			Handler: cspMiddleware(h),
		}
	}

	return []Route{
		getHead("/{$}", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/docs", http.StatusFound)
		})),
		getHead("/docs", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.URL.Path = "/docs.html"
			fileServer.ServeHTTP(w, r)
		})),
		getHead("/openapi.json", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.URL.Path = "/openapi.json"
			fileServer.ServeHTTP(w, r)
		})),
		getHead("/static/", http.StripPrefix("/static/", fileServer)),
	}
}

// Register adds the web routes to the mux.
func Register(mux *http.ServeMux) {
	for _, r := range Routes() {
		for _, m := range r.Methods {
			mux.Handle(m+" "+r.Pattern, r.Handler)
		}
	}
}
