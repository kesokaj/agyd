// Package acp implements an Agent Client Protocol (ACP v1) JSON-RPC 2.0 client over stdio.
package acp

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ProtocolVersion is the ACP wire protocol version supported by this client.
const ProtocolVersion = 1

var (
	// ErrProcessClosed indicates the ACP child process exited or its pipes closed.
	ErrProcessClosed = errors.New("acp child process closed")
	// ErrSessionNotFound indicates the requested sessionId is not in the active registry.
	ErrSessionNotFound = errors.New("acp session not found")
	// ErrPromptInFlight indicates a session already has an active session/prompt call.
	ErrPromptInFlight = errors.New("acp prompt already in flight for session")
	// ErrInvalidConfig indicates invalid ACP client configuration.
	ErrInvalidConfig = errors.New("invalid acp config")
	// ErrInvalidArgument wraps a JSON-RPC -32602 (invalid params) from the
	// agent, e.g. an unknown model or config option id.
	ErrInvalidArgument = errors.New("agent rejected the request parameters")
)

// PermissionPolicy controls how the gateway responds to agent session/request_permission calls.
type PermissionPolicy string

const (
	// PermissionAllowAll selects the first allow_once or allow_always option.
	PermissionAllowAll PermissionPolicy = "ALLOW_ALL"
	// PermissionDenyAll selects the first reject_once or reject_always option (or cancels).
	PermissionDenyAll PermissionPolicy = "DENY_ALL"
)

// ParsePermissionPolicy normalizes a case-insensitive policy string.
func ParsePermissionPolicy(raw string) (PermissionPolicy, error) {
	switch strings.ToUpper(strings.TrimSpace(raw)) {
	case "", "ALLOW_ALL":
		return PermissionAllowAll, nil
	case "DENY_ALL":
		return PermissionDenyAll, nil
	default:
		return "", fmt.Errorf("%w: unsupported permission policy %q (want allow_all or deny_all)", ErrInvalidConfig, raw)
	}
}

// Config configures the ACP stdio subprocess and session defaults.
type Config struct {
	Binary           string
	Args             []string
	Env              []string
	DefaultWorkspace string
	PermissionPolicy PermissionPolicy
}

// AgentInfo describes the ACP agent binary returned by initialize.
type AgentInfo struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version"`
}

// Status reports the readiness and metadata of the managed ACP child process.
type Status struct {
	Ready              bool             `json:"ready"`
	ProtocolVersion    int              `json:"protocolVersion"`
	Backend            string           `json:"backend"`
	AgentInfo          AgentInfo        `json:"agentInfo"`
	PermissionPolicy   PermissionPolicy `json:"permissionPolicy"`
	ActiveSessionCount int              `json:"activeSessionCount"`
}

// ContentBlock represents an ACP prompt or update content item.
type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// SessionInfo holds metadata for an active ACP session.
type SessionInfo struct {
	SessionID     string          `json:"sessionId"`
	Cwd           string          `json:"cwd"`
	Backend       string          `json:"backend"`
	Title         string          `json:"title,omitempty"`
	CreateTime    time.Time       `json:"createTime"`
	Modes         json.RawMessage `json:"modes,omitempty"`
	Models        json.RawMessage `json:"models,omitempty"`
	ConfigOptions []ConfigOption  `json:"configOptions,omitempty"`
}

// PromptResult contains the aggregated outcome of an ACP session/prompt turn.
type PromptResult struct {
	SessionID  string            `json:"sessionId"`
	StopReason string            `json:"stopReason"`
	Text       string            `json:"text"`
	Updates    []json.RawMessage `json:"updates,omitempty"`
	Usage      json.RawMessage   `json:"usage,omitempty"`
}

// RPCError represents a JSON-RPC 2.0 error returned by the ACP child process.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("acp rpc error %d: %s", e.Code, e.Message)
}

// IsAuthError reports whether the JSON-RPC error indicates missing/expired authentication.
func (e *RPCError) IsAuthError() bool {
	if e == nil {
		return false
	}
	msg := strings.ToLower(e.Message)
	return e.Code == -32000 && (strings.Contains(msg, "auth") || strings.Contains(msg, "login") || strings.Contains(msg, "credential"))
}

// rpcMessage is the wire envelope for newline-delimited JSON-RPC 2.0 messages.
type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

type initializeParams struct {
	ProtocolVersion    int                `json:"protocolVersion"`
	ClientCapabilities clientCapabilities `json:"clientCapabilities"`
	ClientInfo         clientInfo         `json:"clientInfo"`
}

type clientCapabilities struct {
	FS       fsCapabilities `json:"fs"`
	Terminal bool           `json:"terminal"`
}

type fsCapabilities struct {
	ReadTextFile  bool `json:"readTextFile"`
	WriteTextFile bool `json:"writeTextFile"`
}

type clientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type initializeResult struct {
	ProtocolVersion int       `json:"protocolVersion"`
	AgentInfo       AgentInfo `json:"agentInfo"`
}

type sessionNewParams struct {
	Cwd        string `json:"cwd"`
	MCPServers []any  `json:"mcpServers"`
}

type sessionNewResult struct {
	SessionID     string          `json:"sessionId"`
	Modes         json.RawMessage `json:"modes,omitempty"`
	Models        json.RawMessage `json:"models,omitempty"`
	ConfigOptions json.RawMessage `json:"configOptions,omitempty"`
}

type sessionPromptParams struct {
	SessionID string         `json:"sessionId"`
	Prompt    []ContentBlock `json:"prompt"`
}

type sessionPromptResult struct {
	StopReason string          `json:"stopReason"`
	Usage      json.RawMessage `json:"usage,omitempty"`
}

type sessionIDParams struct {
	SessionID string `json:"sessionId"`
}

type sessionUpdateParams struct {
	SessionID string          `json:"sessionId"`
	Update    json.RawMessage `json:"update"`
}

type sessionUpdateEnvelope struct {
	SessionUpdate string       `json:"sessionUpdate"`
	Content       ContentBlock `json:"content"`
}

type permissionOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
}

type requestPermissionParams struct {
	SessionID string             `json:"sessionId"`
	ToolCall  json.RawMessage    `json:"toolCall,omitempty"`
	Options   []permissionOption `json:"options"`
}

type requestPermissionResult struct {
	Outcome permissionOutcome `json:"outcome"`
}

type permissionOutcome struct {
	Outcome  string `json:"outcome"`
	OptionID string `json:"optionId,omitempty"`
}
