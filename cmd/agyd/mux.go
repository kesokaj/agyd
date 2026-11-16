package main

import (
	"log/slog"
	"net/http"

	"agy-api/internal/acp"
	"agy-api/internal/api"
	"agy-api/internal/web"
)

func newMux(log *slog.Logger, acpClient acp.Service, workspace string) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/", api.NewHandler(log, acpClient, api.Config{DefaultWorkspace: workspace}))
	web.Register(mux)
	return api.Guard(mux)
}
