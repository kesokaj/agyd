package main

import (
	"log/slog"
	"net/http"

	"github.com/kesokaj/agyd/internal/acp"
	"github.com/kesokaj/agyd/internal/api"
	"github.com/kesokaj/agyd/internal/conversation"
	"github.com/kesokaj/agyd/internal/web"
)

func newMux(log *slog.Logger, acpClient acp.Service, mgr *conversation.Manager, workspace string, extraHosts ...string) http.Handler {
	webRoutes := web.Routes()
	extra := make([]api.ExtraRoute, len(webRoutes))
	for i, r := range webRoutes {
		extra[i] = api.ExtraRoute{
			Methods: r.Methods,
			Pattern: r.Pattern,
			Handler: r.Handler,
		}
	}
	h := api.NewHandler(log, acpClient, mgr, api.Config{
		DefaultWorkspace: workspace,
		ExtraRoutes:      extra,
	})
	return api.Guard(h, extraHosts...)
}
