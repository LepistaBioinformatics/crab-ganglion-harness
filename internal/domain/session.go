package domain

// SessionKey is the stable per-(user, agent) scope. The proxy derives it and the
// harness only ever carries it -- deriving it here would duplicate a preimage the
// proxy owns, which is how two components silently disagree.
type SessionKey string

// Turn is one inbound request to answer.
type Turn struct {
	// SessionID scopes the conversation transcript.
	SessionID string
	// SessionKey scopes long-term memory across conversations.
	SessionKey SessionKey
	// Model is the model label for this turn.
	Model string
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
}

// ActionRequest is a proposed tool invocation put to an Approver (FR-7).
type ActionRequest struct {
	SessionKey SessionKey
	SessionID  string
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
