// Package api implements the HTTP + Server-Sent Events (SSE) gateway for agyd.
package api

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"runtime/debug"
	"strings"

	"github.com/kesokaj/agyd/internal/acp"
	"github.com/kesokaj/agyd/internal/conversation"
)

// Config holds HTTP server configuration.
type Config struct {
	DefaultWorkspace string
	ExtraRoutes      []ExtraRoute
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
	csrf.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeError(w, &apiError{
			Status: http.StatusForbidden,
			Code:   "PERMISSION_DENIED",
			Reason: "CROSS_ORIGIN_REQUEST",
			Msg:    "cross-origin request refused",
		})
	}))
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
	// paths matches a route's path for any method, so the fallback below can
	// tell a wrong method (405 with Allow) from an unknown path (404).
	paths := http.NewServeMux()
	allow := map[string][]string{}
	add := func(method, pattern string, h http.Handler) {
		mux.Handle(method+" "+pattern, h)
		if _, seen := allow[pattern]; !seen {
			paths.Handle(pattern, http.NotFoundHandler())
		}
		allow[pattern] = append(allow[pattern], method)
	}
	for _, r := range s.routes() {
		add(r.Method, r.Pattern, s.currentHeaders(s.adapt(r.handler)))
	}
	for _, r := range cfg.ExtraRoutes {
		for _, m := range r.Methods {
			add(m, r.Pattern, r.Handler)
		}
	}

	// The catch-all shadows ServeMux's own 405, so it answers both cases in
	// the AIP-193 shape.
	mux.Handle("/", s.currentHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, pattern := paths.Handler(r); pattern != "" {
			if methods := allow[pattern]; len(methods) > 0 {
				allowed := strings.Join(methods, ", ")
				w.Header().Set("Allow", allowed)
				writeError(w, &apiError{
					Status: http.StatusMethodNotAllowed,
					Code:   "UNIMPLEMENTED",
					Reason: "METHOD_NOT_ALLOWED",
					Msg:    "method " + r.Method + " not allowed; allowed: " + allowed,
				})
				return
			}
		}
		writeError(w, &apiError{
			Status: http.StatusNotFound,
			Code:   "NOT_FOUND",
			Reason: "NOT_FOUND",
			Msg:    "Not found",
		})
	})))

	return s.accessLogMiddleware(s.recoverMiddleware(mux))
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
			// The client went away mid-request (the turn was cancelled for
			// it): nobody reads a reply, and it is not a server fault.
			if r.Context().Err() != nil && errors.Is(err, context.Canceled) {
				s.log.InfoContext(r.Context(), "client closed request", "path", r.URL.Path)
				return
			}
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
