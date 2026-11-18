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
		{Route{http.MethodGet, "/v1/current"}, s.handleGetCurrent},
		{Route{http.MethodPatch, "/v1/current"}, s.handleUpdateCurrent},
		{Route{http.MethodPost, "/v1/prompt"}, s.handlePromptCurrent},
		{Route{http.MethodPost, "/v1/cancel"}, s.handleCancelCurrent},
		{Route{http.MethodPost, "/v1/conversations"}, s.handleCreateConversation},
		{Route{http.MethodGet, "/v1/conversations"}, s.handleListConversations},
		{Route{http.MethodGet, "/v1/conversations/{conversationId}"}, s.handleGetConversation},
		{Route{http.MethodPatch, "/v1/conversations/{conversationId}"}, s.handleUpdateConversation},
		{Route{http.MethodDelete, "/v1/conversations/{conversationId}"}, s.handleDeleteConversation},
		{Route{http.MethodPost, "/v1/conversations/{conversationId}/prompt"}, s.handlePromptConversation},
		{Route{http.MethodPost, "/v1/conversations/{conversationId}/cancel"}, s.handleCancelConversation},
		{Route{http.MethodPost, "/v1/conversations/{conversationId}/use"}, s.handleUseConversation},
		{Route{http.MethodGet, "/v1/models"}, s.handleModels},
		{Route{http.MethodGet, "/v1/defaults"}, s.handleGetDefaults},
		{Route{http.MethodPut, "/v1/defaults"}, s.handlePutDefaults},
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

// ServiceName identifies agyd in GET /healthz responses so clients probing
// candidate ports can distinguish agyd from unrelated local listeners.
const ServiceName = "agyd"

type healthResponse struct {
	Service string `json:"service"`
}

func (s *server) handleHealthz(w http.ResponseWriter, _ *http.Request) error {
	writeJSON(w, http.StatusOK, healthResponse{Service: ServiceName})
	return nil
}
