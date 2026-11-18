// Package api implements the HTTP + Server-Sent Events (SSE) gateway for agyd.
package api

import (
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"runtime/debug"
	"strings"

	"agyd/internal/acp"
	"agyd/internal/conversation"
)

// Config holds HTTP server configuration.
type Config struct {
	DefaultWorkspace string
}

type server struct {
	log *slog.Logger
	svc acp.Service           // turns (prompt, cancel) and status
	mgr *conversation.Manager // the current pointer, settings and defaults
	cfg Config
}

// Guard applies the Host guard and CrossOriginProtection to the mux.
// Loopback names (localhost, 127.0.0.1, ::1) are always allowed; extraHosts
// adds the names a non-loopback bind is reached by (interface IPs, hostname).
// Any other Host header is refused, which blocks DNS rebinding.
func Guard(next http.Handler, extraHosts ...string) http.Handler {
	csrf := http.NewCrossOriginProtection()
	csrfHandler := csrf.Handler(next)

	allowed := map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true}
	for _, h := range extraHosts {
		if h = normalizeHost(h); h != "" {
			allowed[h] = true
		}
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		if !allowed[normalizeHost(host)] {
			writeError(w, &apiError{
				Status: http.StatusForbidden,
				Code:   "PERMISSION_DENIED",
				Reason: "FORBIDDEN_HOST",
				Msg:    "Host not allowed",
			})
			return
		}
		csrfHandler.ServeHTTP(w, r)
	})
}

// normalizeHost strips IPv6 brackets and returns IPs in canonical form
// (IPv4-mapped unmapped, IPv6 compressed) and names lowercased, so the
// allowlist and Host header compare equal however each was written.
func normalizeHost(h string) string {
	h = strings.TrimPrefix(h, "[")
	h = strings.TrimSuffix(h, "]")
	if a, err := netip.ParseAddr(h); err == nil {
		return a.Unmap().String()
	}
	return strings.ToLower(h)
}

// NewHandler constructs the HTTP mux for agyd. svc runs turns; mgr owns the
// current conversation and the settings (D10).
func NewHandler(log *slog.Logger, svc acp.Service, mgr *conversation.Manager, cfg Config) http.Handler {
	if log == nil {
		log = slog.Default()
	}
	s := &server{log: log, svc: svc, mgr: mgr, cfg: cfg}

	mux := http.NewServeMux()
	for _, r := range s.routes() {
		mux.Handle(r.Method+" "+r.Pattern, s.adapt(r.handler))
	}

	// Default handler for 404s
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, &apiError{
			Status: http.StatusNotFound,
			Code:   "NOT_FOUND",
			Reason: "NOT_FOUND",
			Msg:    "Not found",
		})
	})

	return s.recoverMiddleware(s.currentHeaders(mux))
}

func (s *server) recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.ErrorContext(r.Context(), "panic in http handler",
					"panic", rec,
					"path", r.URL.Path,
					"stack_trace", string(debug.Stack()),
				)
				writeError(w, &apiError{
					Status: http.StatusInternalServerError,
					Code:   "INTERNAL",
					Reason: "INTERNAL",
					Msg:    "internal error",
				})
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *server) adapt(h handlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := h(w, r); err != nil {
			ae := s.classifyError(r, err)
			writeError(w, ae)
		}
	})
}

type statusResponse struct {
	acp.Status
	Workspace string `json:"workspace"`
}

func (s *server) handleStatus(w http.ResponseWriter, r *http.Request) error {
	st, err := s.svc.Status(r.Context())
	if err != nil {
		return err
	}
	res := statusResponse{
		Status:    st,
		Workspace: s.cfg.DefaultWorkspace,
	}
	writeJSON(w, http.StatusOK, res)
	return nil
}
