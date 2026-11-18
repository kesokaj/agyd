package main

import (
	"log/slog"
	"net/http"

	"agyd/internal/acp"
	"agyd/internal/api"
	"agyd/internal/conversation"
	"agyd/internal/web"
)

func newMux(log *slog.Logger, acpClient acp.Service, mgr *conversation.Manager, workspace string, extraHosts ...string) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/", api.NewHandler(log, acpClient, mgr, api.Config{DefaultWorkspace: workspace}))
	web.Register(mux)
	return api.Guard(mux, extraHosts...)
}
