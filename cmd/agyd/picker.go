package main

import (
	"bufio"
	"fmt"
	"io"
	"strconv"
	"strings"

	"agyd/internal/acp"
	"agyd/internal/conversation"
)

// pickerAttempts is how many invalid answers a question takes before the
// preselected value is used (D16).
const pickerAttempts = 3

// pickDefaults asks for the default model, then the thinking level, then
// whether to save them to configPath (D4, D16). Enter or EOF keeps the
// preselected value (agy's current one). It returns zero Settings when agy
// reported no model option.
func pickDefaults(in io.Reader, out io.Writer, opts []acp.ConfigOption, configPath string) (conversation.Settings, bool) {
	model, ok := findOption(opts, acp.ConfigModel)
	if !ok {
		return conversation.Settings{}, false
	}
	r := bufio.NewReader(in)
	s := conversation.Settings{Model: choose(r, out, "Default model", model)}
	if thinking, ok := findOption(opts, acp.ConfigThinking); ok {
		s.ThinkingLevel = choose(r, out, "Default thinking level", thinking)
	}
	fmt.Fprintf(out, "Save as default in %s? [Y/n] ", configPath)
	answer, _ := readAnswer(r)
	switch strings.ToLower(answer) {
	case "", "y", "yes":
		return s, true
	}
	return s, false
}

// choose lists o's values and returns the one picked by number or exact
// value, or o.CurrentValue on Enter, EOF or too many invalid answers.
func choose(r *bufio.Reader, out io.Writer, title string, o acp.ConfigOption) string {
	fmt.Fprintf(out, "%s:\n", title)
	for i, v := range o.Options {
		line := fmt.Sprintf("  %d) %s", i+1, v.Value)
		if v.Name != "" && v.Name != v.Value {
			line += "  " + v.Name
		}
		if v.Value == o.CurrentValue {
			line += "  (default)"
		}
		fmt.Fprintln(out, line)
	}
	for range pickerAttempts {
		fmt.Fprintf(out, "Choose [%s]: ", o.CurrentValue)
		answer, eof := readAnswer(r)
		if answer == "" {
			return o.CurrentValue
		}
		if v, ok := match(o, answer); ok {
			return v
		}
		fmt.Fprintf(out, "%q is not one of the choices.\n", answer)
		if eof {
			break
		}
	}
	fmt.Fprintf(out, "Using %s.\n", o.CurrentValue)
	return o.CurrentValue
}

// match resolves a 1-based number or an exact value against o's values.
func match(o acp.ConfigOption, answer string) (string, bool) {
	if n, err := strconv.Atoi(answer); err == nil {
		if n >= 1 && n <= len(o.Options) {
			return o.Options[n-1].Value, true
		}
		return "", false
	}
	for _, v := range o.Options {
		if v.Value == answer {
			return v.Value, true
		}
	}
	return "", false
}

// readAnswer returns the next trimmed line and whether input has ended.
func readAnswer(r *bufio.Reader) (string, bool) {
	line, err := r.ReadString('\n')
	return strings.TrimSpace(line), err != nil
}

func findOption(opts []acp.ConfigOption, id string) (acp.ConfigOption, bool) {
	for _, o := range opts {
		if o.ID == id && len(o.Options) > 0 {
			return o, true
		}
	}
	return acp.ConfigOption{}, false
}
