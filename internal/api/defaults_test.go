package api

import (
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/kesokaj/agyd/internal/conversation"
)

func TestModels(t *testing.T) {
	e := newTestEnv(t)

	want(t, e.do(http.MethodGet, "/v1/models", ""), http.StatusServiceUnavailable, "MODELS_UNKNOWN")

	createConv(t, e, `{}`)
	rec := e.do(http.MethodGet, "/v1/models", "")
	want(t, rec, http.StatusOK, "")
	m := decode[modelsResponse](t, rec)
	if len(m.Models) != 2 || m.Models[1].Value != "claude-sonnet-5-5" ||
		len(m.ThinkingLevels) != 3 || len(m.Modes) != 3 || m.Modes[2].Value != "plan" {
		t.Errorf("models = %+v", m)
	}
}

func TestDefaultsRoundTrip(t *testing.T) {
	e := newTestEnv(t)

	if got := e.do(http.MethodGet, "/v1/defaults", "").Body.String(); strings.TrimSpace(got) != "{}" {
		t.Errorf("fresh defaults = %s, want {}", got)
	}

	rec := e.do(http.MethodPut, "/v1/defaults", `{"model":"claude-sonnet-5-5","thinkingLevel":"high"}`)
	want(t, rec, http.StatusOK, "")
	wantSettings := conversation.Settings{Model: "claude-sonnet-5-5", ThinkingLevel: "high"}
	if got := decode[conversation.Settings](t, rec); got != wantSettings {
		t.Errorf("PUT = %+v, want %+v", got, wantSettings)
	}
	if got := decode[conversation.Settings](t, e.do(http.MethodGet, "/v1/defaults", "")); got != wantSettings {
		t.Errorf("GET after PUT = %+v, want %+v", got, wantSettings)
	}
	file := readFile(t, e.stateFile)
	for _, snippet := range []string{`"model": "claude-sonnet-5-5"`, `"thinkingLevel": "high"`} {
		if !strings.Contains(file, snippet) {
			t.Errorf("state.json lacks %q:\n%s", snippet, file)
		}
	}

	t.Run("partial PUT merges", func(t *testing.T) {
		got := decode[conversation.Settings](t, e.do(http.MethodPut, "/v1/defaults", `{"mode":"plan"}`))
		if got.Model != "claude-sonnet-5-5" || got.ThinkingLevel != "high" || got.Mode != "plan" {
			t.Errorf("merged = %+v", got)
		}
		if file := readFile(t, e.stateFile); !strings.Contains(file, `"model": "claude-sonnet-5-5"`) || !strings.Contains(file, `"mode": "plan"`) {
			t.Errorf("state.json after merge:\n%s", file)
		}
	})

	t.Run("new conversations start from the defaults", func(t *testing.T) {
		c := createConv(t, e, `{}`)
		if c.Model != "claude-sonnet-5-5" || c.ThinkingLevel != "high" || c.Mode != "plan" {
			t.Errorf("created = %+v, want the defaults", c)
		}
	})
}

func TestDefaultsRejectInjection(t *testing.T) {
	e := newTestEnv(t)
	want(t, e.do(http.MethodPut, "/v1/defaults", `{"model":"gemini-4-carbon"}`), http.StatusOK, "")
	before := readFile(t, e.stateFile)

	cases := []struct {
		name, body, reason string
	}{
		{"newline smuggles a key", `{"model":"a\nAGYD_AGY=/bin/evil"}`, "INVALID_SETTING"},
		{"equals sign", `{"thinkingLevel":"x=y"}`, "INVALID_SETTING"},
		{"space", `{"mode":"has space"}`, "INVALID_SETTING"},
		{"over 64 chars", `{"model":"` + strings.Repeat("a", 65) + `"}`, "INVALID_SETTING"},
		{"quote", `{"model":"a\"b"}`, "INVALID_SETTING"},
		{"unknown key", `{"AGYD_AGY":"x"}`, "MALFORMED_BODY"},
		{"empty", `{}`, "INVALID_SETTING"},
		{"no body", "", "MALFORMED_BODY"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want(t, e.do(http.MethodPut, "/v1/defaults", tc.body), http.StatusBadRequest, tc.reason)
			if after := readFile(t, e.stateFile); after != before {
				t.Errorf("state.json changed:\n%s", after)
			}
		})
	}

	t.Run("well-formed but not offered once options are known", func(t *testing.T) {
		createConv(t, e, `{}`)
		beforeKnown := readFile(t, e.stateFile)
		want(t, e.do(http.MethodPut, "/v1/defaults", `{"model":"unknown-family"}`), http.StatusBadRequest, "INVALID_SETTING")
		if after := readFile(t, e.stateFile); after != beforeKnown {
			t.Errorf("state.json changed:\n%s", after)
		}
		if got := e.mgr.Defaults(); got.Model != "gemini-4-carbon" {
			t.Errorf("defaults = %+v, want unchanged", got)
		}
	})
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
