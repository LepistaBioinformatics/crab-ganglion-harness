package domain

import (
	"context"
	"time"
)

// The ports. Adapters implement these; the runtime depends on nothing else.
//
// AR-2 names them here, in one file, so an adapter cannot be invented against an
// interface that was declared next to it.

// Provider is the LLM. Streaming is not optional: emitting deltas as the model
// produces them is FR-2, and it is the capability picoclaw does not have.
type Provider interface {
	Complete(ctx context.Context, req Completion) (Stream, error)
}

// ModelChain answers which models a turn may run on, in order.
//
// A port rather than a field on the loop because the answer changes at runtime:
// an admin edits the registry and the next turn must see it, without the loop
// knowing that a file exists. The loop asks once per turn and then owns the
// order; deciding WHEN to move down it is the loop's, not the chain's.
type ModelChain interface {
	// Chain returns the candidates for one turn, for one KIND of work.
	//
	// The kind is a parameter rather than three methods because the caller's
	// question is always the same -- "what may I use for this" -- and the
	// difference is which slot answers it.
	Chain(turnModel string, kind ModelKind) []string
}

// ThinkingChain reports whether a model would be sent a reasoning-depth field.
//
// It exists for exactly one decision: whether a failed request is worth
// retrying without depth. Retrying unconditionally would double the latency of
// every failure on every model, most of which carry no depth field and cannot
// have failed because of one. Only the component holding the registry knows,
// so it is asked rather than guessed.
type ThinkingChain interface {
	SendsThinking(model string) bool
	// DeepModels are the enabled models that DO accept a depth field, best
	// first, or nothing when the registry has none.
	//
	// It exists because "the agent may choose how hard to think" and "this
	// provider takes a reasoning field" are different facts, and tying the
	// first to the second left an agent whose only model is deepseek-chat
	// unable to ask for depth at all. Depth selecting a MODEL is also how the
	// providers themselves express it -- deepseek-chat against
	// deepseek-reasoner, gpt-5 against the o-series -- so this is the shape the
	// world already has rather than one invented here.
	//
	// Kindless on purpose: the loop applies it only to a text turn. A turn
	// carrying an image has already been routed to a model that can SEE, and
	// trading that for one that can think would answer a question about a
	// picture nobody looked at.
	DeepModels() []string
}

// Learner observes completed turns.
//
// A port rather than a call into a package, for the reason AR-2 gives: what
// happens to a finished turn is a design decision with more than one possible
// answer -- record it, cluster it, ignore it -- and the loop should not know
// which one is installed.
//
// Observe MUST NOT block the turn. It is called after the answer is durable and
// after the member has it; an implementation that is slow costs latency nobody
// is waiting on, but one that returns an error must not fail a turn that
// already succeeded.
type Learner interface {
	Observe(ctx context.Context, r TurnRecord)
}

// TurnRecord is one completed turn, as evolution sees it.
//
// It carries Usage, which picoclaw's equivalent cannot: token accounting is one
// of the two capabilities that justified building this harness, and the analysis
// pass can weigh a pattern by what it COST as well as by whether it worked.
type TurnRecord struct {
	SessionID  ConversationID
	SessionKey SessionKey
	Model      string
	Input      string
	Answer     string
	Tools      []ToolOutcome
	Usage      Usage
	Duration   time.Duration
	// Failed marks a turn that ended in an error. Recorded rather than dropped:
	// the ratio of failures to successes for a pattern is what the threshold in
	// min_success_ratio measures, so discarding them would make every pattern
	// look perfect.
	Failed bool
	At     time.Time
	// Thinking is the depth the agent chose for this turn, if it chose one, and
	// Why is the line it gave. Recorded so evolution can observe whether going
	// deep correlates with succeeding -- which is the only way anyone will ever
	// learn whether the tool is being used well.
	Thinking string
	Why      string
}

// ToolOutcome is one tool invocation inside a turn.
type ToolOutcome struct {
	Name string
	// Denied marks an approval refusal, which is not the same as a failure and
	// must not be counted as one.
	Denied bool
	Failed bool
}

// SystemPrompt assembles the system message for one turn.
//
// A port rather than a string on the loop because what it contains changes while
// the process runs: the persona file an admin edits, and the skills an
// administrator or the agent itself adds. Asked once per provider call, so a
// turn's every iteration sees one coherent prompt.
type SystemPrompt interface {
	System(ctx context.Context) string
}

// ModelKind is what a completion is FOR.
type ModelKind string

const (
	// ModelText is ordinary conversation.
	ModelText ModelKind = "text"
	// ModelVision reads images.
	ModelVision ModelKind = "vision"
	// ModelImageGen produces them.
	ModelImageGen ModelKind = "image"
)

// Stream yields deltas as they arrive.
//
// Next returns io.EOF when the completion is finished. Message and Usage are
// valid only after that -- assembling partial tool-call arguments is the
// adapter's job, not the loop's.
type Stream interface {
	Next(ctx context.Context) (Delta, error)
	Message() Message
	Usage() Usage
	Close() error
}

// TranscriptStore is append-only and complete.
//
// It has no Truncate, no Rewrite and no Compact, and that ABSENCE is FR-9
// expressed as a type: the code that compacts the context window cannot reach
// the served transcript, so it cannot shorten it. This is the structural answer
// to the 102-of-465 loss measured on picoclaw.
type TranscriptStore interface {
	Append(ctx context.Context, id ConversationID, m Message) error
	Read(ctx context.Context, id ConversationID) ([]Message, error)
}

// Checkpointer records an answer that is still streaming, so a turn that dies
// mid-stream does not lose what the member already watched appear.
//
// Separate from TranscriptStore on purpose. TranscriptStore's whole value is
// that it CANNOT rewrite anything; a checkpoint is rewritten constantly. Two
// interfaces keep that distinction enforceable instead of a comment -- and a
// store that cannot checkpoint stays usable, which is what the loop's
// nil-check relies on.
type Checkpointer interface {
	Checkpoint(ctx context.Context, id ConversationID, answersAt time.Time, content string) error
	ClearPartial(ctx context.Context, id ConversationID) error
}

// ContextStore holds the derived window. Rewriting it is normal and lossless in
// the sense that matters: it can always be rebuilt from the transcript.
type ContextStore interface {
	Load(ctx context.Context, id ConversationID) (Window, error)
	Save(ctx context.Context, id ConversationID, w Window) error
}

// ToolExecutor runs one tool call.
type ToolExecutor interface {
	Available(ctx context.Context) []ToolSchema
	Invoke(ctx context.Context, call ToolCall) (Result, error)
}

// Approver decides whether a proposed action may run (FR-7).
//
// The harness never resolves WHO may approve -- it asks, and the proxy answers,
// because that is where mycelium's account id already lands (DEC-1).
// SubAgent runs one child turn to completion and reports what it concluded.
//
// A PORT rather than a call into the runtime, so the dispatcher tool holds an
// interface and the composition root supplies the implementation -- the same
// shape ToolExecutor already has. A tool that imported internal/runtime would
// pass arch_test (runtime is not an adapter) and still invert the hexagon.
type SubAgent interface {
	Run(ctx context.Context, task SubTask) SubReport
}

// SubTask is everything a child is told.
//
// It is deliberately small. A child inherits the system prompt, the workspace,
// the model chain and the tool set, and inherits NO conversation -- so Task is
// the whole of what it knows about why it exists, and the dispatcher's schema
// says so in the description the model reads.
type SubTask struct {
	Label string
	Task  string
	// Context carries the earlier children's answers in sequential mode, and is
	// empty in parallel mode. It is what makes the two modes different: without
	// it, sequential would merely be parallel run slowly.
	Context string
}

// SubReport is what came back. Err is a FIELD rather than a second return
// value: one child failing is data about that child, not a failure of the call
// that ran it.
type SubReport struct {
	Label      string
	Answer     string
	Iterations int
	Usage      Usage
	Err        error
}

type Approver interface {
	Request(ctx context.Context, a ActionRequest) (Decision, error)
}

// Telemetry is a port so neither the domain nor the runtime imports OTel (FR-10).
type Telemetry interface {
	// Span starts a span and returns a function that ends it, recording err.
	Span(ctx context.Context, name string, attrs ...Attr) (context.Context, func(error))
	// Usage records token accounting (FR-8).
	Usage(ctx context.Context, u Usage, attrs ...Attr)
}

// TurnHandler answers one inbound turn. Ingress adapters call it; they never
// reach the loop directly.
type TurnHandler func(ctx context.Context, t Turn, sink Sink) (string, error)

// Ingress is the DRIVING port: something that brings turns in.
//
// It exists because Telegram is in scope as a second connector. With one
// adapter the driving side can be "the HTTP server"; with two, the second one
// becomes a special case unless the first was a port all along.
type Ingress interface {
	Serve(ctx context.Context, h TurnHandler) error
}
