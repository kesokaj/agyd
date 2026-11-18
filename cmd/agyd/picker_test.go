package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agyd/internal/acp"
	"agyd/internal/config"
	"agyd/internal/conversation"
)

func pickerOptions() []acp.ConfigOption {
	return []acp.ConfigOption{
		{ID: "model", CurrentValue: "gemini-4-carbon", Options: []acp.ConfigValue{
			{Value: "gemini-4-carbon", Name: "Gemini 4 Carbon"}, {Value: "claude-sonnet-5-5", Name: "Claude Sonnet 5.5"}, {Value: "gemini-3.8-flash"},
		}},
		{ID: "thought_level", CurrentValue: "medium", Options: []acp.ConfigValue{{Value: "low"}, {Value: "medium"}, {Value: "high"}}},
		{ID: "mode", CurrentValue: "default", Options: []acp.ConfigValue{{Value: "default"}, {Value: "plan"}}},
	}
}

func TestPickDefaults(t *testing.T) {
	cases := []struct {
		name, input string
		want        conversation.Settings
		save        bool
	}{
		{"enter accepts the preselected values", "\n\n\n", conversation.Settings{Model: "gemini-4-carbon", ThinkingLevel: "medium"}, true},
		{"EOF accepts the preselected values", "", conversation.Settings{Model: "gemini-4-carbon", ThinkingLevel: "medium"}, true},
		{"numbers pick and n declines saving", "2\n3\nn\n", conversation.Settings{Model: "claude-sonnet-5-5", ThinkingLevel: "high"}, false},
		{"values are accepted by name", "gemini-3.8-flash\nlow\nyes\n", conversation.Settings{Model: "gemini-3.8-flash", ThinkingLevel: "low"}, true},
		{"invalid input retries then takes the preselected", "9\nx\n0\nbogus\n1\nY\n", conversation.Settings{Model: "gemini-4-carbon", ThinkingLevel: "low"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out strings.Builder
			got, save := pickDefaults(strings.NewReader(c.input), &out, pickerOptions(), "/cfg/config.env")
			if got != c.want || save != c.save {
				t.Errorf("pickDefaults = %+v, save=%v; want %+v, save=%v\n%s", got, save, c.want, c.save, out.String())
			}
			for _, s := range []string{"Gemini 4 Carbon", "claude-sonnet-5-5", "/cfg/config.env", "[Y/n]"} {
				if !strings.Contains(out.String(), s) {
					t.Errorf("prompt lacks %q:\n%s", s, out.String())
				}
			}
		})
	}
}

func TestPickDefaultsWithoutOptions(t *testing.T) {
	var out strings.Builder
	got, save := pickDefaults(strings.NewReader("1\n"), &out, nil, "/cfg/config.env")
	if !got.IsZero() || save {
		t.Errorf("pickDefaults with no options = %+v, %v; want nothing", got, save)
	}
}

func TestWantPicker(t *testing.T) {
	for _, c := range []struct {
		name  string
		cfg   appConfig
		tty   bool
		ready bool
		want  bool
	}{
		{"tty, nothing set", appConfig{}, true, true, true},
		{"no tty", appConfig{}, false, true, false},
		{"model set", appConfig{Model: "m"}, true, true, false},
		{"-no-prompt", appConfig{NoPrompt: true}, true, true, false},
		{"agy not ready", appConfig{}, true, false, false},
	} {
		if got := wantPicker(c.cfg, c.tty, c.ready); got != c.want {
			t.Errorf("%s: wantPicker = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestConfigEnvWriterSavesOnlyGivenKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agyd", "config.env")
	save := configEnvWriter(path)
	if err := save(conversation.Settings{Model: "claude-sonnet-5-5", ThinkingLevel: "high"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got, err := config.ParseEnv(f, path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["AGYD_MODEL"] != "claude-sonnet-5-5" || got["AGYD_THINKING"] != "high" {
		t.Errorf("config.env = %v", got)
	}
}
