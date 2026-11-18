package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"agyd/internal/acp"
	"agyd/internal/config"
	"agyd/internal/conversation"
)

// startupTimeout bounds resuming or creating the startup conversation (D13).
const startupTimeout = 60 * time.Second

// wantPicker reports whether to ask for the default model at startup (D4):
// only on a terminal, with agy ready, when neither flags/env nor state.json
// set a default model and the operator did not pass -no-prompt.
func wantPicker(cfg appConfig, effectiveModel string, tty, agyReady bool) bool {
	return tty && agyReady && cfg.Model == "" && effectiveModel == "" && !cfg.NoPrompt
}

// stdinIsTerminal reports whether stdin is an interactive terminal (not /dev/null).
func stdinIsTerminal() bool {
	return fileIsTerminal(os.Stdin)
}

// newManager builds the conversation Manager for this instance.
func newManager(log *slog.Logger, cfg appConfig, agent conversation.Agent) *conversation.Manager {
	return conversation.New(conversation.Options{
		Agent:     agent,
		State:     config.NewStateStore(cfg.StateDir),
		Log:       log,
		Workspace: cfg.Workspace,
		Defaults:  conversation.Settings{Model: cfg.Model, ThinkingLevel: cfg.Thinking, Mode: cfg.Mode},
	})
}

// startConversation resumes or creates the startup conversation and, on a
// terminal with no configured model, runs the picker. Failures are logged:
// the first request retries through the Manager.
func startConversation(ctx context.Context, log *slog.Logger, cfg appConfig, mgr *conversation.Manager, in io.Reader, out io.Writer, tty bool) {
	sctx, cancel := context.WithTimeout(ctx, startupTimeout)
	info, fresh, err := mgr.Startup(sctx)
	cancel()
	if err != nil {
		log.Warn("no conversation at startup; the first request will create one", "err", err)
	}
	effectiveModel := mgr.Defaults().Model
	if !wantPicker(cfg, effectiveModel, tty, err == nil) {
		if effectiveModel == "" {
			log.Info("no default model configured; using agy's default", "model", conversation.SettingsOf(info).Model)
		}
		logConversation(log, info.SessionID, conversation.SettingsOf(info), fresh)
		return
	}
	s, save, ok := pickInterruptibly(ctx, in, out, info.ConfigOptions, filepath.Join(cfg.StateDir, "state.json"))
	if !ok || s.IsZero() {
		return
	}
	if _, err := mgr.SetDefaults(s, save); err != nil {
		log.Warn("picked defaults rejected", "err", err)
		return
	}
	if fresh {
		if updated, err := mgr.Update(ctx, "", s); err != nil {
			log.Warn("could not apply the picked settings to the startup conversation", "err", err)
		} else {
			info = updated
		}
	}
	logConversation(log, info.SessionID, conversation.SettingsOf(info), fresh)
}

// pickInterruptibly runs the picker so a signal (ctx) still stops the
// process while it waits on stdin. ok is false when ctx ended first.
func pickInterruptibly(ctx context.Context, in io.Reader, out io.Writer, opts []acp.ConfigOption, path string) (conversation.Settings, bool, bool) {
	type result struct {
		s    conversation.Settings
		save bool
	}
	done := make(chan result, 1)
	go func() {
		s, save := pickDefaults(in, out, opts, path)
		done <- result{s, save}
	}()
	select {
	case r := <-done:
		return r.s, r.save, true
	case <-ctx.Done():
		return conversation.Settings{}, false, false
	}
}

func logConversation(log *slog.Logger, id string, s conversation.Settings, fresh bool) {
	if id == "" {
		return
	}
	log.Info("current conversation", "conversationId", id, "fresh", fresh,
		"model", s.Model, "thinkingLevel", s.ThinkingLevel, "mode", s.Mode)
}
