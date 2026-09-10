package domain

import "context"

// The ports. Adapters implement these; the runtime depends on nothing else.
//
// AR-2 names them here, in one file, so an adapter cannot be invented against an
// interface that was declared next to it.

// Provider is the LLM. Streaming is not optional: emitting deltas as the model
// produces them is FR-2, and it is the capability picoclaw does not have.
type Provider interface {
	Complete(ctx context.Context, req Completion) (Stream, error)
}

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
	Append(ctx context.Context, key SessionKey, m Message) error
	Read(ctx context.Context, key SessionKey) ([]Message, error)
}

// ContextStore holds the derived window. Rewriting it is normal and lossless in
// the sense that matters: it can always be rebuilt from the transcript.
type ContextStore interface {
	Load(ctx context.Context, key SessionKey) (Window, error)
	Save(ctx context.Context, key SessionKey, w Window) error
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
