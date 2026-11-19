package main

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"text/tabwriter"
	"time"

	"agyd/internal/acp"
	"agyd/internal/api"
	"agyd/internal/conversation"
)

// settingsBody is the JSON body for create, PATCH and PUT defaults; empty
// fields are omitted so the server keeps their value.
type settingsBody struct {
	Cwd           string `json:"cwd,omitempty"`
	Model         string `json:"model,omitempty"`
	ThinkingLevel string `json:"thinkingLevel,omitempty"`
	Mode          string `json:"mode,omitempty"`
}

func (o *cliOpts) settingsBody() settingsBody {
	return settingsBody{Model: o.model, ThinkingLevel: o.thinking, Mode: o.mode}
}

// runNew: POST /v1/conversations with cwd = the folder the CLI runs in.
func runNew(ctx context.Context, c *cli, o *cliOpts, _ []string) error {
	cwd, err := c.env.getwd()
	if err != nil {
		return fmt.Errorf("getting current directory: %w", err)
	}
	body := o.settingsBody()
	body.Cwd = cwd
	var conv conversation.Conversation
	if err := c.call(ctx, http.MethodPost, "/v1/conversations", body, &conv); err != nil || c.json {
		return err
	}
	c.printConversation(conv)
	return nil
}

// runClear: POST /v1/clear, or POST /v1/conversations/{id}/clear when an id is given.
func runClear(ctx context.Context, c *cli, _ *cliOpts, args []string) error {
	path := "/v1/clear"
	if len(args) == 1 {
		id := args[0]
		if strings.TrimSpace(id) == "" || id == "." || id == ".." {
			return usageErr("invalid conversation id; usage: agyd clear [<id>]")
		}
		path = "/v1/conversations/" + url.PathEscape(id) + "/clear"
	}
	return c.conversationCall(ctx, http.MethodPost, path, nil)
}

// runList: GET /v1/conversations.
func runList(ctx context.Context, c *cli, _ *cliOpts, _ []string) error {
	var res struct {
		Items []conversation.Conversation `json:"items"`
	}
	if err := c.call(ctx, http.MethodGet, "/v1/conversations", nil, &res); err != nil || c.json {
		return err
	}
	tw := tabwriter.NewWriter(c.env.stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "  ID\tLIVE\tMODEL\tTHINKING\tUPDATED\tTITLE")
	for _, it := range res.Items {
		mark := " "
		if it.Current {
			mark = "*"
		}
		live := "no"
		if it.Live {
			live = "yes"
		}
		fmt.Fprintf(tw, "%s %s\t%s\t%s\t%s\t%s\t%s\n", mark, it.ConversationID, live,
			dash(it.Model), dash(it.ThinkingLevel), when(it), oneLine(it.Title, 60))
	}
	return tw.Flush()
}

// runUse: POST /v1/conversations/{id}/use.
func runUse(ctx context.Context, c *cli, _ *cliOpts, args []string) error {
	return c.conversationCall(ctx, http.MethodPost, "/v1/conversations/"+url.PathEscape(args[0])+"/use", nil)
}

// runCurrent: GET /v1/current.
func runCurrent(ctx context.Context, c *cli, _ *cliOpts, _ []string) error {
	return c.conversationCall(ctx, http.MethodGet, "/v1/current", nil)
}

// runModel: PATCH /v1/current with the family and/or --thinking/--mode.
func runModel(ctx context.Context, c *cli, o *cliOpts, args []string) error {
	if len(args) == 1 {
		o.model = args[0]
	}
	if !o.hasSettings() {
		return usageErr("nothing to change; usage: agyd model <family> [--thinking T] [--mode X]")
	}
	return c.conversationCall(ctx, http.MethodPatch, "/v1/current", o.settingsBody())
}

// runModels: GET /v1/models; the current conversation's values are starred.
func runModels(ctx context.Context, c *cli, _ *cliOpts, _ []string) error {
	var res struct {
		Models         []acp.ConfigValue `json:"models"`
		ThinkingLevels []acp.ConfigValue `json:"thinkingLevels"`
		Modes          []acp.ConfigValue `json:"modes"`
	}
	if err := c.call(ctx, http.MethodGet, "/v1/models", nil, &res); err != nil || c.json {
		return err
	}
	resp := c.header
	tw := tabwriter.NewWriter(c.env.stdout, 0, 4, 2, ' ', 0)
	for i, sec := range []struct {
		title, header string
		values        []acp.ConfigValue
	}{
		{"MODELS", api.HeaderModel, res.Models},
		{"THINKING LEVELS", api.HeaderThinkingLevel, res.ThinkingLevels},
		{"MODES", api.HeaderMode, res.Modes},
	} {
		if i > 0 {
			fmt.Fprintln(tw)
		}
		fmt.Fprintln(tw, sec.title)
		for _, v := range sec.values {
			mark := " "
			if v.Value == resp.Get(sec.header) {
				mark = "*"
			}
			fmt.Fprintf(tw, "%s %s\t%s\n", mark, v.Value, v.Name)
		}
	}
	return tw.Flush()
}

// runDefaults: GET /v1/defaults, or PUT /v1/defaults when a flag is given
// (agyd merges and saves them to state.json).
func runDefaults(ctx context.Context, c *cli, o *cliOpts, _ []string) error {
	method, body := http.MethodGet, any(nil)
	if o.hasSettings() {
		method, body = http.MethodPut, o.settingsBody()
	}
	var d conversation.Settings
	if err := c.call(ctx, method, "/v1/defaults", body, &d); err != nil || c.json {
		return err
	}
	tw := tabwriter.NewWriter(c.env.stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "model\t%s\nthinking\t%s\nmode\t%s\n",
		agyDefault(d.Model), agyDefault(d.ThinkingLevel), agyDefault(d.Mode))
	if method == http.MethodPut {
		fmt.Fprintln(tw, "(saved to state.json)")
	}
	return tw.Flush()
}

// conversationCall runs a request that answers with one Conversation.
func (c *cli) conversationCall(ctx context.Context, method, path string, body any) error {
	var conv conversation.Conversation
	if err := c.call(ctx, method, path, body, &conv); err != nil || c.json {
		return err
	}
	c.printConversation(conv)
	return nil
}

func (c *cli) printConversation(conv conversation.Conversation) {
	tw := tabwriter.NewWriter(c.env.stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "conversation\t%s\n", conv.ConversationID)
	fmt.Fprintf(tw, "cwd\t%s\n", conv.Cwd)
	if conv.Title != "" {
		fmt.Fprintf(tw, "title\t%s\n", oneLine(conv.Title, 80))
	}
	fmt.Fprintf(tw, "model\t%s\nthinking\t%s\nmode\t%s\n", dash(conv.Model), dash(conv.ThinkingLevel), dash(conv.Mode))
	_ = tw.Flush()
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func agyDefault(s string) string {
	if s == "" {
		return "(agy default)"
	}
	return s
}

func when(c conversation.Conversation) string {
	t := c.UpdateTime
	if t == nil {
		t = c.CreateTime
	}
	if t == nil {
		return "-"
	}
	return t.Local().Format(time.DateTime[:16])
}

// oneLine flattens s to one line of at most n runes.
func oneLine(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}
