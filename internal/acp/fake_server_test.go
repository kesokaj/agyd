package acp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
	"testing"
)

// TestHelperACPProcess runs inside a child process when GO_WANT_ACP_HELPER=1 is set.
func TestHelperACPProcess(t *testing.T) {
	if os.Getenv("GO_WANT_ACP_HELPER") != "1" {
		return
	}
	mode := "standard"
	for i, a := range os.Args {
		if a == "--" && i+1 < len(os.Args) {
			mode = os.Args[i+1]
			break
		}
	}
	runFakeACPServer(mode, os.Stdin, os.Stdout)
	os.Exit(0)
}

func runFakeACPServer(mode string, r io.Reader, w io.Writer) {
	scanner := bufio.NewScanner(r)
	var writeMu sync.Mutex
	send := func(v any) {
		writeMu.Lock()
		defer writeMu.Unlock()
		b, _ := json.Marshal(v)
		_, _ = fmt.Fprintf(w, "%s\n", b)
	}

	var cancelCh chan struct{}
	var cancelMu sync.Mutex
	cancelCount := 0
	seq := 0
	cfg := newFakeConfig()

	for scanner.Scan() {
		var msg rpcMessage
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
			continue
		}
		if cfg.handle(msg, send) {
			continue
		}
		switch msg.Method {
		case "initialize":
			send(map[string]any{
				"jsonrpc": "2.0",
				"id":      msg.ID,
				"result": map[string]any{
					"protocolVersion": 1,
					"agentInfo": map[string]any{
						"name":    "fake-agy",
						"title":   "Fake Antigravity CLI",
						"version": "1.5.9-test",
					},
				},
			})
		case "session/new":
			seq++
			sid := fmt.Sprintf("sess_%d", seq)
			var np sessionNewParams
			_ = json.Unmarshal(msg.Params, &np)
			cfg.created[sid] = np.Cwd
			cfg.reset(sid)
			send(map[string]any{
				"jsonrpc": "2.0",
				"id":      msg.ID,
				"result": map[string]any{
					"sessionId":     sid,
					"modes":         map[string]any{"currentModeId": "default"},
					"models":        map[string]any{"currentModelId": "gemini-test"},
					"configOptions": cfg.options(sid),
				},
			})
		case "session/close":
			send(map[string]any{
				"jsonrpc": "2.0",
				"id":      msg.ID,
				"result":  map[string]any{},
			})
		case "session/cancel":
			cancelMu.Lock()
			cancelCount++
			if cancelCh != nil {
				close(cancelCh)
				cancelCh = nil
			}
			cancelMu.Unlock()
		case "session/prompt":
			var p sessionPromptParams
			_ = json.Unmarshal(msg.Params, &p)
			text := ""
			if len(p.Prompt) > 0 {
				text = p.Prompt[0].Text
			}
			if text == "check_env" {
				send(map[string]any{
					"jsonrpc": "2.0",
					"method":  "session/update",
					"params": map[string]any{
						"sessionId": p.SessionID,
						"update": map[string]any{
							"sessionUpdate": "agent_message_chunk",
							"content":       map[string]any{"type": "text", "text": "token=" + os.Getenv("AGY_API_TOKEN")},
						},
					},
				})
				send(map[string]any{
					"jsonrpc": "2.0",
					"id":      msg.ID,
					"result":  map[string]any{"stopReason": "end_turn"},
				})
				continue
			}
			if text == "was_cancelled" {
				cancelMu.Lock()
				cc := cancelCount
				cancelMu.Unlock()
				send(map[string]any{
					"jsonrpc": "2.0",
					"method":  "session/update",
					"params": map[string]any{
						"sessionId": p.SessionID,
						"update": map[string]any{
							"sessionUpdate": "agent_message_chunk",
							"content":       map[string]any{"type": "text", "text": fmt.Sprintf("cancel_count=%d", cc)},
						},
					},
				})
				send(map[string]any{
					"jsonrpc": "2.0",
					"id":      msg.ID,
					"result":  map[string]any{"stopReason": "end_turn"},
				})
				continue
			}
			if text == "crash_now" {
				os.Exit(2)
			}
			if text == "auth_error" {
				send(map[string]any{
					"jsonrpc": "2.0",
					"id":      msg.ID,
					"error": map[string]any{
						"code":    -32000,
						"message": "Authentication required: please run agy acp --login",
					},
				})
				continue
			}
			if text == "wait_for_cancel" {
				ch := make(chan struct{})
				cancelMu.Lock()
				cancelCh = ch
				cancelMu.Unlock()
				reqID := *msg.ID
				go func(sid string, id int64, cch chan struct{}) {
					send(map[string]any{
						"jsonrpc": "2.0",
						"method":  "session/update",
						"params": map[string]any{
							"sessionId": sid,
							"update": map[string]any{
								"sessionUpdate": "agent_message_chunk",
								"content":       map[string]any{"type": "text", "text": "working..."},
							},
						},
					})
					<-cch
					send(map[string]any{
						"jsonrpc": "2.0",
						"id":      id,
						"result": map[string]any{
							"stopReason": "cancelled",
						},
					})
				}(p.SessionID, reqID, ch)
				continue
			}
			if mode == "fs_request" {
				send(map[string]any{
					"jsonrpc": "2.0",
					"id":      int64(901),
					"method":  "fs/write_text_file",
					"params":  map[string]any{"sessionId": p.SessionID, "path": "/tmp/x", "content": "x"},
				})
				if scanner.Scan() {
					var resp rpcMessage
					_ = json.Unmarshal(scanner.Bytes(), &resp)
					code := 0
					if resp.Error != nil {
						code = resp.Error.Code
					}
					send(map[string]any{
						"jsonrpc": "2.0",
						"method":  "session/update",
						"params": map[string]any{
							"sessionId": p.SessionID,
							"update": map[string]any{
								"sessionUpdate": "agent_message_chunk",
								"content":       map[string]any{"type": "text", "text": fmt.Sprintf("code=%d", code)},
							},
						},
					})
					send(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": map[string]any{"stopReason": "end_turn"}})
				}
				continue
			}
			if mode == "permission" {
				permID := int64(900)
				send(map[string]any{
					"jsonrpc": "2.0",
					"id":      permID,
					"method":  "session/request_permission",
					"params": map[string]any{
						"sessionId": p.SessionID,
						"toolCall":  map[string]any{"toolCallId": "tc_1", "title": "Execute command", "kind": "execute"},
						"options": []map[string]any{
							{"optionId": "allow-once", "name": "Allow once", "kind": "allow_once"},
							{"optionId": "reject-once", "name": "Reject once", "kind": "reject_once"},
						},
					},
				})
				// Read the client's response to session/request_permission.
				if scanner.Scan() {
					var permResp rpcMessage
					_ = json.Unmarshal(scanner.Bytes(), &permResp)
					var out requestPermissionResult
					_ = json.Unmarshal(permResp.Result, &out)
					prefix := "Allowed: "
					if out.Outcome.OptionID == "reject-once" {
						prefix = "Denied: "
					}
					for _, chunk := range []string{prefix, out.Outcome.OptionID + "!"} {
						send(map[string]any{
							"jsonrpc": "2.0",
							"method":  "session/update",
							"params": map[string]any{
								"sessionId": p.SessionID,
								"update": map[string]any{
									"sessionUpdate": "agent_message_chunk",
									"content":       map[string]any{"type": "text", "text": chunk},
								},
							},
						})
					}
					send(map[string]any{
						"jsonrpc": "2.0",
						"id":      msg.ID,
						"result": map[string]any{
							"stopReason": "end_turn",
						},
					})
				}
				continue
			}
			send(map[string]any{
				"jsonrpc": "2.0",
				"method":  "session/update",
				"params": map[string]any{
					"sessionId": p.SessionID,
					"update": map[string]any{
						"sessionUpdate": "agent_message_chunk",
						"content":       map[string]any{"type": "text", "text": "echo: " + text},
					},
				},
			})
			send(map[string]any{
				"jsonrpc": "2.0",
				"id":      msg.ID,
				"result": map[string]any{
					"stopReason": "end_turn",
				},
			})
		}
	}
}
