// Package runtime holds the agent loop. It imports the domain and nothing else.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

// Defaults applied by withDefaults when a field is left zero.
const (
	DefaultMaxIterations     = 12
	DefaultApprovalTimeout   = 5 * time.Minute
	DefaultApprovalHeartbeat = 10 * time.Second
	DefaultWindowBudget      = 40
	// DefaultCheckpointEvery is a WALL CLOCK, not a token count: a fast stream
	// must not checkpoint per token, and a slow one must still checkpoint.
	DefaultCheckpointEvery = 2 * time.Second
)

// ErrMaxIterations is returned inside the turn's report when the loop stopped
// because it hit the cap. It is not returned as the function's error: the turn
// produced work, and throwing that away to report a limit is the failure mode
// picoclaw had -- a session that ended at max_tool_iterations with no answer at
// all, discoverable only from a summary written afterwards.
var ErrMaxIterations = errors.New("iteration cap reached before the agent finished")

// Loop runs one conversational turn against the five ports.
//
// Telemetry and Approver may be left unset: they default to no-op values (see
// noop.go). Every other port is required.
type Loop struct {
	Provider   domain.Provider
	Transcript domain.TranscriptStore
	// Checkpoints may be nil: a store that cannot checkpoint simply does not,
	// and the turn behaves as it did before D-2.
	Checkpoints domain.Checkpointer
	Context     domain.ContextStore
	Tools       domain.ToolExecutor
	Approver    domain.Approver
	Telemetry   domain.Telemetry

	// Model is the model this harness was configured with, and it is what
	// every turn uses.
	//
	// The turn's own Model field is deliberately IGNORED in v1. The proxy fills
	// it with a placeholder -- literally "picoclaw", the harness name, because
	// that is what its /v1/models advertises and what a client echoes back --
	// and forwarding that to a provider gets:
	//
	//   The supported API model names are deepseek-flash, deepseek-v4-pro,
	//   but you passed picoclaw.
	//
	// Per-turn model selection is DF-4, deferred and answered 501 by the proxy,
	// so there is nothing to honour yet. When it lands, this is where it goes.
	Model             string
	System            string
	MaxIterations     int
	ApprovalTimeout   time.Duration
	ApprovalHeartbeat time.Duration
	WindowBudget      int
	CheckpointEvery   time.Duration

	// Now is injectable so tests do not sleep.
	Now func() time.Time
}

func (l *Loop) withDefaults() *Loop {
	c := *l
	if c.MaxIterations <= 0 {
		c.MaxIterations = DefaultMaxIterations
	}
	if c.ApprovalTimeout <= 0 {
		c.ApprovalTimeout = DefaultApprovalTimeout
	}
	if c.ApprovalHeartbeat <= 0 {
		c.ApprovalHeartbeat = DefaultApprovalHeartbeat
	}
	if c.WindowBudget <= 0 {
		c.WindowBudget = DefaultWindowBudget
	}
	if c.CheckpointEvery <= 0 {
		c.CheckpointEvery = DefaultCheckpointEvery
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Approver == nil {
		c.Approver = allowAll{}
	}
	if c.Telemetry == nil {
		c.Telemetry = noTelemetry{}
	}
	return &c
}

// Run answers one turn, streaming content to sink as it arrives, and returns
// the complete answer.
func (l *Loop) Run(ctx context.Context, t domain.Turn, sink domain.Sink) (string, error) {
	c := l.withDefaults()

	ctx, end := c.span(ctx, "turn", domain.Attr{Key: "conversation.id", Value: string(t.SessionID)})
	var runErr error
	defer func() { end(runErr) }()

	// The member's message is durable BEFORE the model is called. A crash
	// mid-turn must lose the answer, never the question.
	in := t.Input
	in.Role = domain.RoleUser
	if in.CreatedAt.IsZero() {
		in.CreatedAt = c.Now()
	}
	if runErr = c.Transcript.Append(ctx, t.SessionID, in); runErr != nil {
		return "", fmt.Errorf("append user message: %w", runErr)
	}

	window, err := c.Context.Load(ctx, t.SessionID)
	if err != nil {
		runErr = fmt.Errorf("load context: %w", err)
		return "", runErr
	}
	// Repair on load, not only on save. A window saved with an orphaned tool
	// result -- by a build that predates dropOrphanTools -- would otherwise
	// fail at the provider on every turn forever, because nothing else ever
	// revisits the front of the window.
	window = dropOrphanTools(window)
	window.Messages = append(window.Messages, in)

	var answer string
	var total domain.Usage

	for i := 0; i < c.MaxIterations; i++ {
		msg, usage, err := c.complete(ctx, t, window, sink, in.CreatedAt)
		if err != nil {
			runErr = err
			sink.EmitError(err.Error())
			return answer, runErr
		}
		total.Add(usage)

		msg.CreatedAt = c.Now()
		if err := c.Transcript.Append(ctx, t.SessionID, msg); err != nil {
			runErr = fmt.Errorf("append assistant message: %w", err)
			return answer, runErr
		}
		// D-3. The real message is durable now, so the sidecar is stale. The
		// crash window between the two lines above and this one is exactly what
		// answersAt makes harmless -- a reader finding both sees an assistant
		// message at or after it and ignores the sidecar.
		//
		// A failed clear is deliberately ignored: it leaves a stale sidecar,
		// which the supersession rule already renders invisible. Failing the
		// turn here would trade a harmless leftover for a lost answer.
		if c.Checkpoints != nil {
			_ = c.Checkpoints.ClearPartial(ctx, t.SessionID)
		}
		window.Messages = append(window.Messages, msg)
		if msg.Content != "" {
			answer = msg.Content
		}

		if len(msg.ToolCalls) == 0 {
			c.recordUsage(ctx, total, t)
			window = compact(window, c.WindowBudget)
			runErr = c.Context.Save(ctx, t.SessionID, window)
			return answer, runErr
		}

		for _, call := range msg.ToolCalls {
			res, err := c.runTool(ctx, t, call, sink)
			if err != nil {
				runErr = err
				sink.EmitError(err.Error())
				return answer, runErr
			}
			out := domain.Message{
				Role:       domain.RoleTool,
				Content:    res.Content,
				ToolCallID: call.ID,
				CreatedAt:  c.Now(),
			}
			if err := c.Transcript.Append(ctx, t.SessionID, out); err != nil {
				runErr = fmt.Errorf("append tool result: %w", err)
				return answer, runErr
			}
			window.Messages = append(window.Messages, out)
		}

		window = compact(window, c.WindowBudget)
		if err := c.Context.Save(ctx, t.SessionID, window); err != nil {
			runErr = fmt.Errorf("save context: %w", err)
			return answer, runErr
		}
	}

	// FR-5: hitting the cap must be said out loud, not inferred later.
	c.recordUsage(ctx, total, t)
	sink.EmitError(ErrMaxIterations.Error())
	if saveErr := c.Context.Save(ctx, t.SessionID, compact(window, c.WindowBudget)); saveErr != nil {
		return answer, saveErr
	}
	return answer, nil
}

// complete runs one provider call, streaming its deltas to the sink.
func (l *Loop) complete(
	ctx context.Context, t domain.Turn, w domain.Window, sink domain.Sink, answersAt time.Time,
) (domain.Message, domain.Usage, error) {
	ctx, end := l.span(ctx, "provider.complete")
	var err error
	defer func() { end(err) }()

	stream, err := l.Provider.Complete(ctx, domain.Completion{
		Model:    l.modelFor(t),
		Window:   w,
		System:   l.System,
		Tools:    l.Tools.Available(ctx),
		Messages: w.Messages,
	})
	if err != nil {
		return domain.Message{}, domain.Usage{}, fmt.Errorf("provider: %w", err)
	}
	defer stream.Close()

	var partial strings.Builder
	lastCheckpoint := l.Now()

	// Deltas are COALESCED before they reach the sink, and that is not an
	// optimisation -- it is the difference between working and not.
	//
	// A provider emits sub-word tokens ("Be", "le", "za"). Each one that
	// reaches the client costs a re-render and, in the webapp, a re-parse of
	// the whole revealed markdown; its reveal driver additionally re-plans on
	// every content delta, having been written when "picoclaw sends the whole
	// answer in one frame, so in practice this runs once per turn". Emitting
	// per token turned one re-plan into hundreds and the reply visibly
	// rewrote itself as it arrived.
	//
	// 50ms is ~20 updates a second: past what anyone perceives as anything but
	// smooth, and an order of magnitude fewer renders. Reasoning is coarser
	// still at 1s, because it is NARRATION -- picoclaw emits a frame per
	// thought, not per token, and this channel is consumed as though that were
	// true.
	content := newCoalescer(50*time.Millisecond, l.Now, func(text string) {
		sink.EmitContent(text)
	})
	reasoning := newCoalescer(time.Second, l.Now, func(text string) {
		sink.EmitProgress(domain.Progress{Kind: domain.ProgressThought, Text: text})
	})

	for {
		d, nerr := stream.Next(ctx)
		if errors.Is(nerr, io.EOF) {
			break
		}
		if nerr != nil {
			// Flush before reporting: whatever arrived is the member's, even
			// though the turn failed.
			content.Flush()
			reasoning.Flush()
			err = fmt.Errorf("provider stream: %w", nerr)
			return domain.Message{}, domain.Usage{}, err
		}
		// FR-2: content reaches the client as the model produces it. The sink
		// comes FIRST -- D-5: durability must never delay delivery.
		content.Add(d.Content)
		reasoning.Add(d.Reasoning)
		partial.WriteString(d.Content)

		// D-2. On the delta path rather than in a goroutine: an 887us write
		// every 2s of streaming is 0.04% of that window, so there is nothing
		// to parallelise and a second writer would need a lock for no gain.
		if l.Checkpoints != nil && partial.Len() > 0 &&
			l.Now().Sub(lastCheckpoint) >= l.CheckpointEvery {
			if cerr := l.Checkpoints.Checkpoint(ctx, t.SessionID, answersAt, partial.String()); cerr != nil {
				// A failed checkpoint must not fail the turn: it costs recovery
				// of THIS answer, and failing here would cost the answer itself.
				sink.EmitProgress(domain.Progress{
					Kind: domain.ProgressPlaceholder,
					Text: "could not checkpoint this answer: " + cerr.Error(),
				})
			}
			lastCheckpoint = l.Now()
		}
	}
	content.Flush()
	reasoning.Flush()
	return stream.Message(), stream.Usage(), nil
}

// runTool asks the Approver, then invokes -- or turns a refusal into a Result.
func (l *Loop) runTool(ctx context.Context, t domain.Turn, call domain.ToolCall, sink domain.Sink) (domain.Result, error) {
	ctx, end := l.span(ctx, "tool.invoke", domain.Attr{Key: "tool.name", Value: call.Name})
	var err error
	defer func() { end(err) }()

	sink.EmitProgress(domain.Progress{Kind: domain.ProgressTool, Tool: call.Name})

	dec := l.approve(ctx, t, call, sink)
	if !dec.Allowed {
		// DEC-2: a denial is a Result the agent can react to, not a turn failure.
		return domain.Result{
			Denied:  true,
			Content: fmt.Sprintf("The action %q was not approved: %s", call.Name, dec.Reason),
		}, nil
	}

	res, err := l.Tools.Invoke(ctx, call)
	if err != nil {
		return domain.Result{}, fmt.Errorf("tool %s: %w", call.Name, err)
	}
	return res, nil
}

// approve runs the Approver with a deadline, emitting progress while it waits.
//
// DEC-3: an approval can legitimately take minutes, and a silent stream for
// minutes is the exact failure turn-stream-continuity documents. DEC-4: a
// timeout denies -- fail closed, and say why.
func (l *Loop) approve(ctx context.Context, t domain.Turn, call domain.ToolCall, sink domain.Sink) domain.Decision {
	ctx, cancel := context.WithTimeout(ctx, l.ApprovalTimeout)
	defer cancel()

	type answer struct {
		dec domain.Decision
		err error
	}
	ch := make(chan answer, 1)
	go func() {
		dec, err := l.Approver.Request(ctx, domain.ActionRequest{
			SessionKey: t.SessionKey,
			SessionID:  t.SessionID,
			Call:       call,
		})
		ch <- answer{dec, err}
	}()

	tick := time.NewTicker(l.ApprovalHeartbeat)
	defer tick.Stop()

	for {
		select {
		case a := <-ch:
			if a.err != nil {
				return domain.Decision{Allowed: false, Reason: "approval failed: " + a.err.Error()}
			}
			return a.dec
		case <-tick.C:
			sink.EmitProgress(domain.Progress{
				Kind: domain.ProgressPlaceholder,
				Text: fmt.Sprintf("waiting for approval to run %s", call.Name),
				Tool: call.Name,
			})
		case <-ctx.Done():
			return domain.Decision{Allowed: false, Reason: "no answer from an approver in time"}
		}
	}
}

// modelFor resolves the model for a turn. See Loop.Model for why the turn's
// own label is not trusted.
func (l *Loop) modelFor(t domain.Turn) string {
	if l.Model != "" {
		return l.Model
	}
	// No configured model: fall back to whatever the turn named, so a
	// misconfiguration surfaces as the provider's own error naming the bad
	// value rather than as an empty-model request nobody can attribute.
	return t.Model
}

func (l *Loop) span(ctx context.Context, name string, attrs ...domain.Attr) (context.Context, func(error)) {
	return l.Telemetry.Span(ctx, name, attrs...)
}

func (l *Loop) recordUsage(ctx context.Context, u domain.Usage, t domain.Turn) {
	l.Telemetry.Usage(ctx, u,
		domain.Attr{Key: "conversation.id", Value: string(t.SessionID)},
		domain.Attr{Key: "model", Value: l.modelFor(t)},
	)
}
