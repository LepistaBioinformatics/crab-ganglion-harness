// Package domain holds the harness's model, its ports, and pure policy.
//
// AR-1: this package imports NOTHING outside the standard library. That is not a
// style preference -- it is what makes the loop testable without a container, a
// provider key or a network, and it is checked by TestDomainImportsOnlyStdlib.
package domain

import (
	"encoding/json"
	"time"
)

// Role is who produced a Message. The values match the OpenAI wire vocabulary
// because the served surface is OpenAI-compatible (FR-3); the domain does not
// otherwise depend on that.
type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message is one entry of a conversation.
//
// The JSON tags are snake_case and match picoclaw's own on-disk session
// vocabulary (role, content, reasoning_content, created_at). That is
// deliberate: crab-shell-proxy's internal/history already parses that shape, so
// a reader for ganglion transcripts is a path change rather than a new format.
// Go-style field names on disk would have made compatibility a rewrite.
type Message struct {
	Role    Role   `json:"role"`
	Content string `json:"content"`
	// Reasoning is the model's own chain of thought when it emitted one. Kept
	// separate from Content so a client can keep it out of the way -- the proxy's
	// history reader makes the same split.
	Reasoning string `json:"reasoning_content,omitempty"`
	// ToolCalls is set on an assistant message that asked for tools.
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
	// ToolCallID links a RoleTool message back to the call it answers.
	ToolCallID string    `json:"tool_call_id,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// ToolCall is one requested invocation.
type ToolCall struct {
	ID   string          `json:"id"`
	Name string          `json:"name"`
	Args json.RawMessage `json:"arguments,omitempty"`
}

// ToolSchema is what the provider is told a tool accepts.
type ToolSchema struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// Result is what a tool produced. A refusal is a Result, never an error -- see
// DEC-2: the agent is told it was refused and can react, instead of losing the
// work the turn already did.
type Result struct {
	Content string
	// Denied marks a result produced by the approval path rather than by running
	// the tool.
	Denied bool
}

// Usage is the token accounting for one provider call (FR-8). It is the whole
// reason this harness can answer a question picoclaw's own dashboard calls
// "not obtainable in this stack at all".
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Add accumulates another call's usage into u.
func (u *Usage) Add(o Usage) {
	u.PromptTokens += o.PromptTokens
	u.CompletionTokens += o.CompletionTokens
	u.TotalTokens += o.TotalTokens
}

// Delta is an incremental piece of a completion as the provider emits it (FR-2).
// Tool calls are NOT streamed as deltas: the provider adapter assembles them and
// exposes the finished set through Stream.Message, because a half-parsed argument
// object is of no use to the loop and every adapter would otherwise reimplement
// the same reassembly.
type Delta struct {
	Content   string
	Reasoning string
}

// Progress is a non-content signal emitted while a turn runs. It NEVER
// contributes to the answer.
//
// The shape mirrors crab-shell-proxy's turn.Progress exactly, because the proxy
// forwards these to clients that already consume them -- changing the vocabulary
// here would be a user-visible regression (FR-4).
type Progress struct {
	// Kind is "thought", "tool", "placeholder" or "typing".
	Kind string `json:"kind"`
	Text string `json:"text,omitempty"`
	Tool string `json:"tool,omitempty"`
	// State is "start" or "stop", only for Kind == "typing".
	State string `json:"state,omitempty"`
}

// Progress kinds, named so a typo is a compile error rather than a silent frame
// the client drops.
const (
	ProgressThought     = "thought"
	ProgressTool        = "tool"
	ProgressPlaceholder = "placeholder"
	ProgressTyping      = "typing"
)

// Sink receives everything a running turn emits. Every field may be nil, so the
// zero value is a valid no-op sink -- same contract as the proxy's turn.Sink.
type Sink struct {
	Content  func(string)
	Progress func(Progress)
	Error    func(string)
}

func (s Sink) EmitContent(delta string) {
	if s.Content != nil && delta != "" {
		s.Content(delta)
	}
}

func (s Sink) EmitProgress(p Progress) {
	if s.Progress != nil {
		s.Progress(p)
	}
}

func (s Sink) EmitError(msg string) {
	if s.Error != nil {
		s.Error(msg)
	}
}
