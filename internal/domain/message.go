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
// AttachmentKind is what a piece of non-text content IS.
//
// An enum rather than a MIME check because the ROUTING decision is coarse --
// "does this turn need a model that can see" -- while the MIME type is what the
// provider needs on the wire. Audio and video are not routed in v1; they are
// additive here rather than a new field later.
type AttachmentKind string

const (
	AttachmentImage AttachmentKind = "image"
)

// Attachment is non-text content carried by a message.
//
// Data is the bytes, not a path or a reference. The harness holds one turn's
// content in memory for the length of that turn anyway, and a reference would
// need a store, a lifetime and a cleanup policy -- picoclaw has all three
// (pkg/media) and they exist to serve channels this harness does not have.
type Attachment struct {
	Kind AttachmentKind `json:"kind"`
	// MIME is what goes on the wire. A provider rejects an image whose type it
	// was told wrongly, so this is not cosmetic.
	MIME string `json:"mime"`
	// Name is for the member and for the transcript, never for the provider.
	Name string `json:"name,omitempty"`
	Data []byte `json:"data,omitempty"`
}

type Message struct {
	Role    Role   `json:"role"`
	Content string `json:"content"`
	// Reasoning is the model's own chain of thought when it emitted one. Kept
	// separate from Content so a client can keep it out of the way -- the proxy's
	// history reader makes the same split.
	Reasoning string `json:"reasoning_content,omitempty"`
	// ToolCalls is set on an assistant message that asked for tools.
	// Attachments are images (and, later, other media) the member sent with
	// this message. Empty on almost every message, which is why the wire
	// adapter must keep emitting the plain string form when it is.
	Attachments []Attachment
	ToolCalls   []ToolCall `json:"tool_calls,omitempty"`
	// ToolCallID links a RoleTool message back to the call it answers.
	ToolCallID string `json:"tool_call_id,omitempty"`
	// Offloaded is the workspace-relative path of the file holding this tool
	// result's WHOLE output, set when the result was too large to carry inline.
	//
	// Structural on purpose. Compaction has to tell "this message kept a
	// pointer" from "this message is short" without reading its text, and a
	// sentinel prefix in Content would be a parser for a string this package
	// also writes -- one rewording away from silently eliding nothing.
	//
	// It reaches no provider (the wire adapter sends role, content, tool_calls
	// and tool_call_id) and no transcript (a tool result is never appended to
	// one), so it exists only between the window on disk and the loop.
	Offloaded string `json:"offloaded,omitempty"`
	// Events is what the loop DID during one iteration, written for the member
	// rather than for the model. A message carrying them carries no content: it
	// is the iteration's detail, not a second thing the agent said.
	//
	// It never reaches a provider. The window is rebuilt from the served history
	// without it (see window.rebuild, which already strips the display-only
	// tool_calls for the same reason), and the wire adapter sends role and
	// content.
	Events    []TurnEvent `json:"events,omitempty"`
	CreatedAt time.Time   `json:"created_at"`
}

// TurnEvent is one thing the loop did, in the form the member reads it.
//
// ONE FLAT SHAPE for every kind, because the client renders them as one list: a
// per-kind union would be four types the webapp has to switch on to draw four
// lines that differ only in their icon.
//
// Nothing here is a sentence. The harness has no locale -- it does not know
// which language the member reads -- so every word on screen is rendered by the
// client from these fields, and the fields that ARE free text (Name, Arguments,
// Detail) are data that is shown verbatim in either language.
type TurnEvent struct {
	// Kind is one of the constants below.
	Kind string `json:"kind"`
	// Name is the tool's name, the child's label, or the model's name.
	Name string `json:"name,omitempty"`
	// Arguments is the call's arguments, FLATTENED AND CAPPED -- see
	// runtime.eventArgs. A string rather than json.RawMessage precisely because
	// it is truncated: a cut-off JSON document is not JSON, and typing it as raw
	// would put a value on disk that every reader has to defend against. It is a
	// display string from the moment it is written.
	Arguments string `json:"arguments,omitempty"`
	// Status is how it ended. Empty for a kind that has no outcome, and for a
	// call whose outcome was never recorded -- a turn that died mid-tool.
	Status string `json:"status,omitempty"`
	// Detail is the failure's text, the depth's reason, or the fallback's cause.
	Detail string `json:"detail,omitempty"`
	// AuditID names this call's record under .tool-audit, and is how the member's
	// client asks for the full command and the output.
	//
	// MINTED HERE, not the provider's call id, which may be empty: the OpenAI
	// adapter assigns ToolCall.ID only when the stream carried one and has no
	// fallback, so keying a file on it would collapse every idless call in a
	// conversation into one record, silently.
	//
	// `omitempty` is load-bearing twice over. Every transcript written before
	// this field existed has no record to name, and every event that is not a
	// tool call has none either -- a model fallback and a depth change have no
	// command and no output, so a client must be able to tell "nothing to open"
	// from "empty".
	AuditID string `json:"audit_id,omitempty"`
	// Count is how many of something the event is about. Set only by
	// EventCompact, where it is the number of messages compaction dropped.
	//
	// A number rather than a sentence because the reader's language is decided
	// downstream: this harness has no locale, so it writes the count and the
	// webapp writes "N earlier messages" or "N mensagens anteriores".
	Count int `json:"count,omitempty"`
}

// Event kinds and statuses, named so a typo is a compile error.
const (
	EventTool     = "tool"
	EventSubagent = "subagent"
	EventModel    = "model"
	EventDepth    = "depth"
	// EventCompact is the context window being shortened. Unlike the four above
	// it records something the LOOP did to the conversation rather than
	// something the agent did in it, which is why it carries a Count and no
	// Name: there is no tool, no child and no model to name.
	EventCompact = "compact"

	EventOK     = "ok"
	EventDenied = "denied"
	EventFailed = "failed"
)

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
	// Events is work the tool did that the loop cannot see. The loop records one
	// event per CALL; a tool that fans out -- the sub-agent dispatcher is the
	// only one -- reports what happened inside it here, and the loop appends
	// whatever it is handed.
	//
	// This is what keeps the loop generic: no tool is named in it. The field is
	// returned by value all the way up (Registry.Invoke returns t.Invoke(...),
	// filtered.Invoke returns f.inner.Invoke(...)), so nothing rebuilds the
	// struct and drops it.
	Events []TurnEvent
	// Attachments are media the tool produced for the MODEL to look at, not for
	// the member. A tool result cannot carry them itself -- most providers
	// reject image parts on a `tool` role message -- so the loop turns them into
	// a synthetic user message after the result. See Loop.runTool.
	Attachments []Attachment
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
