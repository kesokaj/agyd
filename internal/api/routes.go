package api

import "net/http"

type handlerFunc func(w http.ResponseWriter, r *http.Request) error

// Route is one API operation: an HTTP method and a ServeMux path pattern.
type Route struct {
	Method  string
	Pattern string
}

type route struct {
	Route
	handler handlerFunc
}

// routes is the single source of truth for the API surface: NewHandler
// registers exactly these, and Routes exposes them for the OpenAPI parity test.
func (s *server) routes() []route {
	return []route{
		{Route{http.MethodGet, "/healthz"}, s.handleHealthz},
		{Route{http.MethodGet, "/v1/status"}, s.handleStatus},
		{Route{http.MethodPost, "/v1/sessions"}, s.handleCreateSession},
		{Route{http.MethodGet, "/v1/sessions"}, s.handleListSessions},
		{Route{http.MethodGet, "/v1/sessions/{sessionId}"}, s.handleGetSession},
		{Route{http.MethodPost, "/v1/sessions/{sessionId}/prompt"}, s.handlePrompt},
		{Route{http.MethodPost, "/v1/sessions/{sessionId}/cancel"}, s.handleCancel},
		{Route{http.MethodDelete, "/v1/sessions/{sessionId}"}, s.handleDeleteSession},
	}
}

// Routes returns every operation NewHandler registers.
func Routes() []Route {
	rs := (&server{}).routes()
	out := make([]Route, len(rs))
	for i, r := range rs {
		out[i] = r.Route
	}
	return out
}

func (s *server) handleHealthz(w http.ResponseWriter, _ *http.Request) error {
	w.WriteHeader(http.StatusNoContent)
	return nil
}
