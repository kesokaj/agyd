package config

import (
	"strings"
	"testing"
)

func TestParseEnv(t *testing.T) {
	t.Run("reads AGYD keys, skips comments, blanks and foreign keys", func(t *testing.T) {
		in := strings.Join([]string{
			"# project settings",
			"",
			"DATABASE_URL=postgres://u:p@h/db",
			"weird line without equals that is not ours",
			"export AGYD_MODEL=gemini-4-carbon",
			"  AGYD_THINKING = high  ",
			`AGYD_MODE="plan"`,
			"AGYD_HOST='127.0.0.1'",
			"AGYD_WORKSPACE=/tmp/ws # trailing comment",
			"AGYD_EMPTY=",
		}, "\n")
		got, err := ParseEnv(strings.NewReader(in), ".env")
		if err != nil {
			t.Fatalf("ParseEnv: %v", err)
		}
		want := map[string]string{
			"AGYD_MODEL":     "gemini-4-carbon",
			"AGYD_THINKING":  "high",
			"AGYD_MODE":      "plan",
			"AGYD_HOST":      "127.0.0.1",
			"AGYD_WORKSPACE": "/tmp/ws",
			"AGYD_EMPTY":     "",
		}
		if len(got) != len(want) {
			t.Fatalf("ParseEnv = %v, want %v", got, want)
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("%s = %q, want %q", k, got[k], v)
			}
		}
		if _, ok := got["DATABASE_URL"]; ok {
			t.Error("non-AGYD key was read")
		}
	})

	t.Run("malformed AGYD lines are rejected with file and line", func(t *testing.T) {
		for name, line := range map[string]string{
			"no equals":         "AGYD_MODEL",
			"bad key":           "AGYD_model=x",
			"unterminated":      `AGYD_MODEL="gemini`,
			"mismatched quotes": `AGYD_MODEL="gemini'`,
			"text after quote":  `AGYD_MODEL="a" b`,
			"nul byte":          "AGYD_MODEL=a\x00b",
			"empty suffix":      "AGYD_=x",
		} {
			_, err := ParseEnv(strings.NewReader("# c\n"+line+"\n"), "/p/.env")
			if err == nil {
				t.Errorf("%s: ParseEnv(%q) err = nil, want an error", name, line)
				continue
			}
			if !strings.Contains(err.Error(), "/p/.env:2") {
				t.Errorf("%s: err = %q, want it to name /p/.env:2", name, err)
			}
		}
	})

	t.Run("later duplicates win", func(t *testing.T) {
		got, err := ParseEnv(strings.NewReader("AGYD_MODEL=a\nAGYD_MODEL=b\n"), ".env")
		if err != nil || got["AGYD_MODEL"] != "b" {
			t.Fatalf("got %v, %v; want AGYD_MODEL=b", got, err)
		}
	})

	// A long comment or unrelated line (a certificate, a JWT) must not stop
	// startup: bufio.Scanner's default 64 KiB token limit is raised.
	t.Run("lines over 64 KiB are read", func(t *testing.T) {
		in := "# " + strings.Repeat("x", 100<<10) + "\nOTHER=" + strings.Repeat("y", 100<<10) + "\nAGYD_MODEL=m\n"
		got, err := ParseEnv(strings.NewReader(in), ".env")
		if err != nil || got["AGYD_MODEL"] != "m" {
			t.Fatalf("got %v, %v; want AGYD_MODEL=m", got, err)
		}
	})
}

func TestValidSettingValue(t *testing.T) {
	for v, ok := range map[string]bool{
		"gemini-4-carbon": true, "claude-sonnet-5-5": true, "gemini-3.8-flash": true,
		"high": true, "accept-edits": true, "a_b": true,
		"": false, "has space": false, "a=b": false, "a\nb": false, "#x": false,
		`"q"`: false, "a/b": false, strings.Repeat("a", 65): false, strings.Repeat("a", 64): true,
	} {
		if got := ValidSettingValue(v); got != ok {
			t.Errorf("ValidSettingValue(%q) = %v, want %v", v, got, ok)
		}
	}
}
