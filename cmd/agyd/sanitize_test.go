package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kesokaj/agyd/internal/acp"
	"github.com/kesokaj/agyd/internal/api"
)

func assertCleanTerminalText(t *testing.T, label, s string, forbiddenSubstrings ...string) {
	t.Helper()
	for _, bad := range append([]string{"\x1b", "\x07", "\x08", "\r", "\u202e", "\u200b", "\u009b", "\u009d"}, forbiddenSubstrings...) {
		if strings.Contains(s, bad) {
			t.Errorf("%s leaked unsafe sequence %q in %q", label, bad, s)
		}
	}
}

func TestCLIStripsTerminalControlChars(t *testing.T) {
	evilPayload := "clean\x1b[31mRed\x1b[0m\x1b]52;c;ZGF0YQ==\x07\x1b]0;spoofedTitle\x1b\\\u202e\u200b\r\nfake"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// UTF-8 C1 CSI (\u009b) and bidi (\u202e) pass HTTP/1.x header validation.
		w.Header().Set(api.HeaderConversation, "c1\u009b2J")
		w.Header().Set(api.HeaderModel, "gemini\u009d0;hdr\u009c")
		w.Header().Set(api.HeaderThinkingLevel, "high\u202e")
		w.Header().Set(api.HeaderMode, "plan\u200b")
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/conversations":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []map[string]any{{
					"conversationId": "c1\x1b[31m",
					"cwd":            "/work/" + evilPayload,
					"title":          evilPayload,
					"live":           true,
					"current":        true,
					"model":          "m\x1b[32m1",
					"thinkingLevel":  "high\tinjected",
				}},
			})
		case "/v1/current":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"conversationId": "c1\x1b[31m",
				"cwd":            "/work/" + evilPayload,
				"title":          evilPayload,
				"live":           true,
				"current":        true,
				"model":          "m\x1b[32m1",
				"thinkingLevel":  "high\x07",
				"mode":           "plan\u202e",
			})
		case "/v1/models":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"models":         []map[string]any{{"value": "m\x1b[31m1", "name": evilPayload}},
				"thinkingLevels": []map[string]any{{"value": "high", "name": "High\x1b[33m"}},
				"modes":          []map[string]any{{"value": "plan", "name": "Plan\x1b]0;mode\x07"}},
			})
		case "/v1/defaults":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"model":         "m\x1b[31m1",
				"thinkingLevel": "high\x07",
				"mode":          "plan\u202e",
			})
		default:
			w.WriteHeader(http.StatusBadGateway)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{
					"code":    502,
					"status":  "BAD_GATEWAY\x1b[31m",
					"message": "boom " + evilPayload,
					"details": []map[string]any{{"reason": "ACP_ERR\x1b]0;err\x07"}},
				},
			})
		}
	}))
	defer srv.Close()

	addr := strings.TrimPrefix(srv.URL, "http://")
	for _, args := range [][]string{{"list"}, {"current"}, {"models"}, {"defaults"}} {
		t.Run("text_"+args[0], func(t *testing.T) {
			env, stdout, stderr := testCLIEnv(t, addr, nil)
			if code := runCLI(t, t.Context(), env, args...); code != 0 {
				t.Fatalf("exit = %d, stderr = %q", code, stderr.String())
			}
			assertCleanTerminalText(t, args[0]+" stdout", stdout.String(), "ZGF0YQ==", "spoofedTitle", "[31m", "[32m", "[33m")
			assertCleanTerminalText(t, args[0]+" stderr", stderr.String(), "hdr", "2J")
			if strings.Contains(stdout.String(), "\t") {
				t.Errorf("%s stdout contains raw tab: %q", args[0], stdout.String())
			}
		})
	}

	t.Run("error_message_sanitized", func(t *testing.T) {
		env, _, stderr := testCLIEnv(t, addr, nil)
		if code := runCLI(t, t.Context(), env, "clear"); code != 1 {
			t.Fatalf("exit = %d, want 1", code)
		}
		assertCleanTerminalText(t, "error stderr", stderr.String(), "ZGF0YQ==", "spoofedTitle", "[31m", "hdr", "2J")
	})

	t.Run("json_flag_remains_valid_raw_json", func(t *testing.T) {
		env, stdout, stderr := testCLIEnv(t, addr, nil)
		if code := runCLI(t, t.Context(), env, "current", "--json"); code != 0 {
			t.Fatalf("exit = %d, stderr = %q", code, stderr.String())
		}
		var raw map[string]any
		if err := json.Unmarshal(stdout.Bytes(), &raw); err != nil {
			t.Fatalf("--json stdout is not valid JSON: %v\n%s", err, stdout.String())
		}
		assertCleanTerminalText(t, "--json stderr", stderr.String(), "hdr", "2J")
	})
}

func TestPickerStripsTerminalControlChars(t *testing.T) {
	opts := []acp.ConfigOption{{
		ID:           "model",
		CurrentValue: "m1\x1b[31m",
		Options: []acp.ConfigValue{
			{Value: "m1\x1b[31m", Name: "Model\x1b]52;c;ZGF0YQ==\x07\u202e One"},
			{Value: "m2", Name: "Safe\r\nTwo"},
		},
	}}
	var out strings.Builder
	got, save := pickDefaults(strings.NewReader("2\nn\n"), &out, opts, "/cfg/state.json")
	if got.Model != "m2" || save {
		t.Fatalf("pickDefaults = %+v, save=%v; want model=m2 save=false", got, save)
	}
	assertCleanTerminalText(t, "picker output", out.String(), "ZGF0YQ==", "[31m")
	if !strings.Contains(out.String(), "Model One") {
		t.Errorf("picker output lost readable option name: %q", out.String())
	}
}

func TestSanitizeTerminal(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain utf8 preserved", "Café — résumé 日本語", "Café — résumé 日本語"},
		{"CSI SGR and cursor", "hi\x1b[31;1mred\x1b[0m\x1b[?25l!", "hired!"},
		{"8-bit CSI", "a\u009b2Jb", "ab"},
		{"raw byte 8-bit CSI", "a\x9b31mb", "ab"},
		{"OSC BEL terminated", "pre\x1b]52;c;ZGF0YQ==\x07post", "prepost"},
		{"OSC ST terminated", "pre\x1b]0;title\x1b\\post", "prepost"},
		{"8-bit OSC", "pre\u009d0;title\u009cpost", "prepost"},
		{"raw byte 8-bit OSC BEL", "pre\x9d52;c;ZGF0YQ==\x07post", "prepost"},
		{"raw byte 8-bit OSC raw ST", "pre\x9d0;title\x9cpost", "prepost"},
		{"raw byte 8-bit DCS raw ST", "x\x90+q544e\x9cy", "xy"},
		{"raw byte 8-bit APC raw ST", "x\x9fGdata\x9cy", "xy"},
		{"OSC payload with utf8 0x9c continuation byte", "pre\x1b]0;\u011csecret\x1b\\post", "prepost"},
		{"unterminated OSC drops payload", "safe\x1b]52;c;secret", "safe"},
		{"DCS and APC stripped", "x\x1bP+q544e\x1b\\y\x1b_Gdata\x1b\\z", "xyz"},
		{"2-byte and nF ESC sequences", "a\x1bcb\x1b(Bc", "abc"},
		{"lone trailing ESC", "ok\x1b", "ok"},
		{"C0, DEL, C1, bidi, zero-width", "a\x00\x07\x08\x7f\u0085\u202e\u200b\u2066b", "ab"},
		{"raw C1 and invalid utf8 dropped", "a\x80\x85\x97\xffb", "ab"},
		{"valid utf8 replacement rune preserved", "a\ufffdb", "a\ufffdb"},
		{"CRLF, LF, TAB become spaces", "a\r\nb\nc\td", "a b c d"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := sanitizeTerminal(tc.in); got != tc.want {
				t.Errorf("sanitizeTerminal(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestPrintInfoSanitizesRawHeaders(t *testing.T) {
	var stderr strings.Builder
	c := &cli{env: cliEnv{stderr: &stderr}}
	h := http.Header{}
	h.Set(api.HeaderConversation, "c1\x1b[2J")
	h.Set(api.HeaderModel, "m1\x1b]0;spoof\x07")
	h.Set(api.HeaderThinkingLevel, "high\u202e")
	h.Set(api.HeaderMode, "plan\r\nFAKE")
	c.printInfo(h)
	want := "agyd: conversation=c1 model=m1 thinking=high mode=plan FAKE\n"
	if stderr.String() != want {
		t.Errorf("printInfo = %q, want %q", stderr.String(), want)
	}
}
