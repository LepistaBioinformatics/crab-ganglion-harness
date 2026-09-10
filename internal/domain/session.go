package domain

// SessionKey is the stable per-(user, agent) scope: long-term memory that spans
// conversations. The proxy derives it and the harness only ever carries it --
// deriving it here would duplicate a preimage the proxy owns, which is how two
// components silently disagree.
//
// It is NOT what a transcript is keyed by. Use ConversationID for that.
type SessionKey string

// ConversationID identifies ONE conversation. Transcripts, context windows and
// checkpoints are all keyed by it.
//
// A distinct type, not a string, because the first cut of this keyed every
// store by SessionKey -- so every conversation a member had shared a single
// transcript file, and the proxy, which looks a conversation up by its own id,
// found nothing and showed an empty history. Two ids of the same underlying
// shape, one comment apart, and the compiler had nothing to say. Now it does.
type ConversationID string

// Turn is one inbound request to answer.
type Turn struct {
	// SessionID identifies the conversation. Everything durable is keyed by it.
	SessionID ConversationID
	// SessionKey scopes long-term memory across conversations.
	SessionKey SessionKey
	// Model is the model label for this turn.
	Model string
	// Project scopes the turn to one of the member's projects: its own files,
	// its own transcripts, its own window and its own MEMORY.md. Empty means
	// the main workspace and the behaviour this harness has always had.
	//
	// It comes from a header the proxy sets and is never derived here -- the
	// proxy owns the preimage, and computing it twice is how two components
	// silently disagree.
	Project string
	// Input is the member's message.
	Input Message
}

// Window is the DERIVED context the model actually sees. It is rebuilt from the
// transcript whenever it grows past budget, and rewriting it is normal.
//
// It is a separate type from the transcript, stored through a separate port, for
// the reason FR-9 exists: picoclaw compacts by rewriting the one file that also
// serves the conversation, and a measured session held 102 entries where the
// proxy's own append-only copy held 465. Two artifacts cannot do that to
// each other.
type Window struct {
	// Summary folds everything older than Messages.
	Summary string `json:"summary,omitempty"`
	// Messages is the recent tail, newest last.
	Messages []Message `json:"messages"`
}

// Completion is a request to the provider.
type Completion struct {
	Model    string
	Window   Window
	Tools    []ToolSchema
	System   string
	Messages []Message
	// ThinkingLevel is the depth THIS turn asked for, one of the names in
	// thinking.go. Empty means the model's own configured level applies, which
	// is the only case that existed before agent-selected depth.
	//
	// A name, never a wire shape: the adapter decides what `high` looks like on
	// the request (AR-2).
	ThinkingLevel string
	// NoThinking suppresses the depth field entirely, whatever either level
	// says. The loop sets it after a request carrying depth failed on this
	// model, so the retry is the same request minus the field that may have
	// caused the failure. Without it there is no way to say "send none": an
	// empty ThinkingLevel means "use the configured one".
	NoThinking bool
}

// ActionRequest is a proposed tool invocation put to an Approver (FR-7).
type ActionRequest struct {
	// SessionKey is the memory scope; SessionID the conversation. An approver
	// needs both: who is asking, and which conversation to show them.
	SessionKey SessionKey
	SessionID  ConversationID
	Call       ToolCall
}

// Decision is an Approver's answer.
type Decision struct {
	Allowed bool
	// Reason is shown to the agent on a denial, so it can react to WHY.
	Reason string
	// By identifies the approver, for the audit trail. The harness never
	// interprets it -- resolving identity is the proxy's job (DEC-1).
	By string
}

// Attr is one telemetry attribute. Modelled as a pair rather than as OTel's own
// type so the domain keeps AR-1.
type Attr struct {
	Key   string
	Value string
}
