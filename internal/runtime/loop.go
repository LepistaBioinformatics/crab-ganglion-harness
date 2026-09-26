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
	// DefaultMaxChildren is the whole-turn child budget. With
	// DefaultMaxIterations and a child cap of six, this is what makes the worst
	// case a number somebody wrote down: 12 + 16*6 = 108 model calls.
	DefaultMaxChildren  = 16
	DefaultWindowBudget = 40
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
	// ToolOutput may be nil: nothing is parked and a large result stays whole
	// in the window, which is the behaviour every turn had before this existed.
	ToolOutput domain.ToolOutputStore
	// ToolAudit may be nil: no record is kept and the member's sheet says the
	// call was not recorded, which is also what every transcript written before
	// this existed says.
	ToolAudit domain.ToolAuditStore
	Tools     domain.ToolExecutor
	Approver  domain.Approver
	Telemetry domain.Telemetry

	// Model is the single model this harness was configured with, used when
	// Models is nil.
	//
	// The turn's own Model field is NOT trusted on this path. crab-shell-proxy
	// fills it with a placeholder -- literally the harness name, because that
	// is what its /v1/models advertises and what a client echoes back -- and
	// forwarding that to a provider gets:
	//
	//   The supported API model names are deepseek-flash, deepseek-v4-pro,
	//   but you passed picoclaw.
	//
	// Models is what honours it safely: a name is used only when the registry
	// recognises it, so the placeholder resolves to the default chain instead
	// of reaching an endpoint.
	Model string

	// Models is the candidate chain for a turn. Nil means the single Model
	// above, which is what every deployment did before the registry existed
	// and what one configured entirely from the environment still does.
	Models domain.ModelChain

	// Prompt assembles the system message. Nil means the static System below,
	// which is what a deployment with no skills and no persona file uses.
	Prompt domain.SystemPrompt

	// Learner observes completed turns. Nil means nothing observes them, which
	// is every deployment with evolution switched off -- and switched off is
	// the default.
	// Thinking answers "would this model carry a depth field", which is the
	// only thing that makes the degradation in tryChain worth attempting. Nil
	// means no model does, and no request is ever retried for that reason.
	Thinking domain.ThinkingChain

	Learner           domain.Learner
	System            string
	MaxIterations     int
	ApprovalTimeout   time.Duration
	ApprovalHeartbeat time.Duration
	WindowBudget      int
	// MaxChildren bounds the child turns ONE member turn may start, across the
	// whole tree beneath it. Zero disables sub-agents by starvation, which is
	// why the dispatcher tool is also gated at boot rather than relying on it.
	MaxChildren     int
	CheckpointEvery time.Duration

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
	if c.MaxChildren <= 0 {
		c.MaxChildren = DefaultMaxChildren
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

	// The turn's reasoning depth, and its whole lifetime. A tool writes it, the
	// next completion reads it, and it dies with the turn -- so a question the
	// agent decided was hard does not silently bill every question after it.
	// The turn's project reaches the stores and the workspace tools through the
	// context, because their ports take (ctx, id) and (ctx, args) -- widening
	// either would make every implementation carry a parameter only the
	// project-aware ones use.
	ctx = domain.WithProject(ctx, t.Project)
	// And the conversation, for the one tool that reads this conversation's own
	// transcript. Same reasoning, same port.
	ctx = domain.WithConversation(ctx, t.SessionID)

	depth := &domain.Depth{}
	ctx = domain.WithDepth(ctx, depth)
	// What this turn DID, for the member rather than for the model. On the
	// context because the two places that record without owning it -- the
	// fallback ladder and the depth change -- are frames below this one.
	events := &domain.Recorder{}
	ctx = domain.WithRecorder(ctx, events)
	// The turn's child budget, created ONCE and shared by everything beneath
	// it. A child runs this same function, so creating one unconditionally
	// would hand every child a full budget and make the whole-turn bound a
	// per-child bound -- which is the shape picoclaw's uncapped depth-0
	// semaphore ends up with, by a different route.
	if domain.FanoutFrom(ctx) == nil {
		ctx = domain.WithFanout(ctx, domain.NewFanout(c.MaxChildren))
	}

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
	window = repair(window)
	// How much THIS turn's compaction dropped, summed across every run of it.
	// One marker is written from this at whichever exit ends the turn.
	compacted := 0
	window.Messages = append(window.Messages, in)

	// What the member sees, accumulated across every iteration of this turn.
	// It is what Run returns, what the Learner records, and nothing else: the
	// transcript is written one message per ITERATION, as each one lands.
	//
	// THE RULE THIS TURNS ON: the reply visibly rewrites itself IFF the live
	// stream's content/progress partition differs from the transcript's message
	// partition. Both have to move together or neither may.
	//
	// One message per turn was the first way of making them agree. Per-iteration
	// writes while narration still streamed as ordinary content tore the single
	// bubble into narration plus answer the moment the client reconciled --
	// crab-shell-proxy marks an assistant message carrying tool_calls as a
	// "step" -- so the transcript was collapsed to match the stream. It agreed by
	// DELETING THE STEPS: a ganglion turn rendered as one block of concatenated
	// text with its narration inlined, and the proxy's marker became dead code
	// for every conversation this harness serves.
	//
	// So both partitions moved instead. An iteration that ends in tool calls is
	// narration by definition: its text leaves the content run entirely and goes
	// out as progress (see complete, which buffers it, and the ProgressTool frame
	// runTool emits), and the iteration is written as its own message carrying
	// its tool_calls. The iteration that ends without them is the answer, and it
	// is the only one that streams as content.
	//
	// Tool RESULTS still reach the provider and only the provider: they go into
	// the context window, which is a separate artifact built separately below.
	// The tool_calls the transcript carries are a display marker with no results
	// behind them -- see window.rebuild, which strips them before a window is
	// ever rebuilt from the served history.
	var answer strings.Builder
	var total domain.Usage
	// Collected for the Learner. Nothing else reads it, and it costs one
	// append per tool call -- which is why it is gathered unconditionally
	// rather than behind a nil check that would have to be repeated at every
	// append site.
	var outcomes []domain.ToolOutcome
	started := c.Now()
	observe := func(failed bool) {
		if c.Learner == nil {
			return
		}
		c.Learner.Observe(ctx, domain.TurnRecord{
			SessionID: t.SessionID, SessionKey: t.SessionKey, Model: c.modelFor(t),
			Input: t.Input.Content, Answer: answer.String(), Tools: outcomes,
			Usage: total, Duration: c.Now().Sub(started), Failed: failed, At: c.Now(),
			Thinking: depth.Level(), Why: depth.Reason(),
		})
	}

	for i := 0; i < c.MaxIterations; i++ {
		// THE MEMBER PRESSED STOP, and this is where a turn notices between one
		// piece of work and the next.
		//
		// Nothing is emitted. A stop is not a failure -- and the sink it would be
		// written to is the connection whose closing IS the stop, so the only
		// reader of that error is a log. What the turn produced up to here is
		// already durable: every completed iteration was appended as it landed,
		// and the frame that was in flight is in the checkpoint sidecar.
		//
		// The guard is cheap and the alternative is not: without it the next
		// provider call fails on a dead context and completeWithFallback reads
		// that as "this model did not answer", walking the whole chain.
		if err := ctx.Err(); err != nil {
			runErr = err
			observe(true)
			return answer.String(), runErr
		}
		// The instant this ITERATION began. It dates the checkpoint sidecar,
		// which now stands in for one frame rather than for a whole turn -- see
		// complete, where the supersession rule that depends on it is written
		// out.
		began := c.Now()
		msg, usage, err := c.completeWithFallback(ctx, t, window, sink, began)
		if err != nil {
			runErr = err
			sink.EmitError(err.Error())
			// Nothing is appended: every earlier iteration is already a durable
			// message of its own, and what THIS one produced before it died is
			// in the sidecar, which is where an interrupted frame belongs.
			observe(true)
			return answer.String(), runErr
		}
		total.Add(usage)

		msg.CreatedAt = c.Now()
		// The WINDOW gets the message as the PROVIDER needs it: content and
		// tool_calls together, so the tool results below answer a call it can
		// see. The TRANSCRIPT gets its own copy here rather than at the end of
		// the turn, and BEFORE the tools run: a tool call is the part of a turn
		// that can take minutes and die, and the sentence explaining why it was
		// made has to survive that.
		window.Messages = append(window.Messages, msg)
		answer.WriteString(msg.Content)
		if ferr := c.record(ctx, t, msg); ferr != nil {
			runErr = ferr
			return answer.String(), runErr
		}

		if len(msg.ToolCalls) == 0 {
			c.recordUsage(ctx, total, t)
			var n int
			window, n = compact(window, c.WindowBudget)
			compacted += n
			c.markCompaction(ctx, t, compacted, sink)
			runErr = c.Context.Save(ctx, t.SessionID, window)
			// AFTER the answer is durable and the member has it. A learner that
			// ran first would put an analysis pass between the model finishing
			// and the transcript being written.
			observe(runErr != nil)
			return answer.String(), runErr
		}

		// Media follow-ups are held until every call in this batch has answered.
		//
		// THEY USED TO BE APPENDED INSIDE THE LOOP, and with a single tool call that
		// is the same thing. With two, the first call's image landed BETWEEN the two
		// tool results and split the run: providers require the results answering one
		// assistant message to be contiguous, and the one this stack runs says so in
		// as many words -- "insufficient tool messages following tool_calls message".
		//
		// It is permanent when it happens, which is what makes it worth a variable.
		// The split message is saved with the window, so every later turn in that
		// conversation replays it and fails identically. See repairToolRuns, which
		// heals the conversations this already cost.
		var media []domain.Message

		for callIdx, call := range msg.ToolCalls {
			chosen := depth.Level()
			// msg.Content is this iteration's NARRATION, and runTool is where it
			// reaches the member: it rides on the tool frame rather than on a
			// frame of its own because the client shows the latest progress
			// event and nothing else, so a separate narration frame would be
			// wiped by the tool frame that follows it a millisecond later.
			// Recorded BEFORE the call, so a turn that dies inside a tool still
			// says which one it was in. Its status is filled in below; an event
			// with none is a call whose outcome nobody ever learned, which is
			// the truth about an interrupted turn.
			//
			// The audit record is written HERE, beside the event and for the same
			// reason: it carries the WHOLE command, where the event carries 200
			// runes of it, and a record written only after the call returned
			// would be missing on exactly the turn that died inside one.
			auditID := mintAuditID(c.Now(), callIdx)
			events.Add(domain.TurnEvent{
				Kind: domain.EventTool, Name: call.Name, Arguments: eventArgs(call.Args),
				AuditID: c.putAudit(ctx, t, auditID, call),
			})
			res, err := c.runTool(ctx, t, call, msg.Content, sink)
			// Said out loud when it changes. A turn that suddenly takes four
			// times as long, and costs four times as much, is owed a sentence
			// saying the agent decided the question was hard -- and the reason
			// it gave is the only evidence anyone will ever have about whether
			// it decided well.
			if lvl := depth.Level(); lvl != chosen {
				text := "thinking at depth " + lvl
				if why := depth.Reason(); why != "" {
					text += ": " + why
				}
				sink.EmitProgress(domain.Progress{Kind: domain.ProgressThought, Text: text})
				events.Add(domain.TurnEvent{
					Kind: domain.EventDepth, Name: lvl, Detail: depth.Reason(),
				})
			}
			// A tool that stopped because the TURN was cancelled, told apart
			// from one that failed. The exec tool reports the member leaving as
			// ctx.Err() precisely so this branch exists: the events are still
			// flushed, since a call whose outcome nobody learned is the truth
			// about an interrupted turn, but nothing is announced as an error.
			if err != nil && errors.Is(ctx.Err(), context.Canceled) {
				runErr = err
				events.Finish(domain.EventFailed, "stopped")
				c.completeAudit(ctx, t, auditID, "", domain.EventFailed, "stopped")
				c.flushEvents(ctx, t, events, sink)
				outcomes = append(outcomes, domain.ToolOutcome{Name: call.Name, Failed: true})
				observe(true)
				return answer.String(), runErr
			}
			if err != nil {
				runErr = err
				sink.EmitError(err.Error())
				events.Finish(domain.EventFailed, err.Error())
				c.completeAudit(ctx, t, auditID, "", domain.EventFailed, err.Error())
				// The narration that led to this call was appended before the
				// call ran, so there is no message to write here -- but the
				// events are the one thing this path would otherwise lose, and
				// a failed call is exactly what a member wants to see.
				c.flushEvents(ctx, t, events, sink)
				outcomes = append(outcomes, domain.ToolOutcome{Name: call.Name, Failed: true})
				observe(true)
				return answer.String(), runErr
			}
			outcomes = append(outcomes, domain.ToolOutcome{Name: call.Name, Denied: res.Denied})
			if res.Denied {
				events.Finish(domain.EventDenied, "")
				c.completeAudit(ctx, t, auditID, res.Content, domain.EventDenied, "")
			} else {
				events.Finish(domain.EventOK, "")
				// The FULL result, not the clamped copy the window gets below. A
				// member opening the sheet is asking for what the offload took
				// away.
				c.completeAudit(ctx, t, auditID, res.Content, domain.EventOK, "")
			}
			// Whatever the tool did that this loop cannot see. Appended AFTER
			// the call's own event, so a dispatcher's children read as belonging
			// to the call that started them.
			for _, e := range res.Events {
				events.Add(e)
			}
			// The tool result goes to the window only. The served transcript
			// records what the member saw, and they never saw this -- which is
			// also why a large one is PARKED on the way in. The window being
			// its sole copy is what makes compaction dropping it a loss of the
			// bytes and not merely of the context.
			window.Messages = append(window.Messages, c.offload(ctx, t, call.ID, res.Content))

			// Media a tool produced follows as a SYNTHETIC USER MESSAGE rather
			// than riding on the result above.
			//
			// Not a stylistic choice: most providers reject image parts on a
			// `tool` role message, and the ones that accept them disagree about
			// the shape. A user message carrying the image is the one form every
			// OpenAI-compatible endpoint understands, and it is what picoclaw
			// does too (agent_media.go, toolImageFollowUpPromptMessage).
			if len(res.Attachments) > 0 {
				media = append(media, domain.Message{
					Role:        domain.RoleUser,
					Content:     "Here is the media that tool loaded.",
					Attachments: res.Attachments,
					CreatedAt:   c.Now(),
				})
			}
		}

		// After the whole batch, so the tool results stay contiguous.
		window.Messages = append(window.Messages, media...)

		// ONE ENTRY PER ITERATION, written here because an event can only say how
		// a call ENDED once it has. The narration was written before the tools
		// ran and stays there -- a turn that dies mid-tool keeps the sentence
		// explaining why the call was made, and simply has no events, which is
		// the same detail the transcript carried before this existed.
		//
		// It carries no content: it is the iteration's detail, not a second thing
		// the agent said. The proxy keeps an entry like that precisely because it
		// has events; without them it would be dropped as empty.
		c.flushEvents(ctx, t, events, sink)

		// ACCUMULATED, not written. Compaction runs once per iteration, so a
		// long turn compacts many times -- and a marker per run would put a
		// dozen dividers in the member's transcript for one turn's worth of
		// shortening. The marker is written once, at whichever exit ends the
		// turn.
		var dropped int
		window, dropped = compact(window, c.WindowBudget)
		compacted += dropped
		if err := c.Context.Save(ctx, t.SessionID, window); err != nil {
			runErr = fmt.Errorf("save context: %w", err)
			return answer.String(), runErr
		}
	}

	// FR-5: hitting the cap must be said out loud, not inferred later.
	c.recordUsage(ctx, total, t)
	sink.EmitError(ErrMaxIterations.Error())
	// Recorded as a FAILURE. A turn that ran out of iterations produced
	// something, but it did not finish the work -- counting it as a success
	// would teach the pattern that whatever it was doing works.
	observe(true)
	// Nothing to finish: the last iteration's narration was appended when it
	// arrived, like every one before it.
	window, lastDrop := compact(window, c.WindowBudget)
	c.markCompaction(ctx, t, compacted+lastDrop, sink)
	if saveErr := c.Context.Save(ctx, t.SessionID, window); saveErr != nil {
		return answer.String(), saveErr
	}
	return answer.String(), nil
}

// record writes ONE ITERATION's assistant message and clears the checkpoint
// that was standing in for it.
//
// Called as each frame lands, including on the paths the turn then fails on: a
// turn that died after saying something still said it, and after three
// iterations it has said three things rather than one.
//
// The tool_calls are carried because they ARE the step marker -- the only thing
// crab-shell-proxy reads to tell narration from an answer
// (internal/history/history.go, KindStep). The reasoning is not: it was already
// delivered as progress while the frame streamed, and a second copy on the
// message would show the member the same thoughts twice.
func (l *Loop) record(ctx context.Context, t domain.Turn, msg domain.Message) error {
	if msg.Content == "" {
		// Nothing was said. A frame that only asked for a tool is not a step
		// anybody can see -- the proxy's history reader drops an entry with no
		// text -- so writing it would put an empty band in the served history.
		// The sidecar is cleared for the reason it always was: a later reader
		// must not resurrect a fragment of a frame that produced no text.
		l.clearPartial(ctx, t)
		return nil
	}
	if err := l.Transcript.Append(ctx, t.SessionID, domain.Message{
		Role:      domain.RoleAssistant,
		Content:   msg.Content,
		ToolCalls: msg.ToolCalls,
		CreatedAt: msg.CreatedAt,
	}); err != nil {
		return fmt.Errorf("append assistant message: %w", err)
	}
	l.clearPartial(ctx, t)
	return nil
}

// clearPartial drops the sidecar standing in for a frame that is now durable.
//
// D-3. A failed clear is ignored: it leaves a leftover the supersession rule
// already hides, and failing here would trade that for a lost answer.
func (l *Loop) clearPartial(ctx context.Context, t domain.Turn) {
	if l.Checkpoints != nil {
		_ = l.Checkpoints.ClearPartial(ctx, t.SessionID)
	}
}

// completeWithFallback walks the turn's candidate models until one answers.
//
// THE RULE, and it is the whole of the design: a candidate is abandoned only
// while NOTHING has reached the member. Once a byte of content has been
// emitted, the turn is committed to that model and its failure surfaces --
// restarting under another model would splice two voices into one bubble, and
// the member has already seen the first half of the first one.
//
// The error returned when the chain runs out is the LAST provider's, not a
// synthetic summary: an operator reading the log needs the reason the final
// attempt failed, and a wrapper that said "all 3 models failed" would bury it.
func (l *Loop) completeWithFallback(
	ctx context.Context, t domain.Turn, w domain.Window, sink domain.Sink,
	answersAt time.Time,
) (domain.Message, domain.Usage, error) {
	msg, usage, err := l.tryChain(ctx, t, w, sink, answersAt, l.modelsFor(ctx, t, w))
	if err == nil || !hasAttachments(w) {
		return msg, usage, err
	}

	// THE DEGRADATION, and it is the feature rather than a courtesy.
	//
	// picoclaw ends the turn here (pipeline_llm.go:282, ControlBreak) with an
	// error naming agents.defaults.image_model. That alone would be a bad
	// afternoon; what makes it permanent is that the media reference stays in
	// the session history, so EVERY LATER TURN IN THAT CONVERSATION FAILS THE
	// SAME WAY. This stack has already met that in production -- it is why
	// deploy/picoclaw-glob/vision-unsupported-glm.patch exists.
	//
	// So the image is dropped and the turn is retried once, text-only, with the
	// model told what happened. The answer is degraded and says so, and the
	// conversation stays usable.
	sink.EmitProgress(domain.Progress{
		Kind: domain.ProgressPlaceholder,
		Text: "the image could not be read by any configured model; answering from the text alone",
	})
	stripped := stripAttachments(w)
	stripped.Messages = append([]domain.Message{{
		Role: domain.RoleSystem,
		Content: "An image was attached to this conversation and no configured model could read it. " +
			"Answer from the text, and say plainly that you could not see the image.",
	}}, stripped.Messages...)
	return l.tryChain(ctx, t, stripped, sink, answersAt, l.modelsFor(ctx, t, domain.Window{}))
}

// tryChain walks one ordered list of candidates.
func (l *Loop) tryChain(
	ctx context.Context, t domain.Turn, w domain.Window, sink domain.Sink,
	answersAt time.Time, chain []string,
) (domain.Message, domain.Usage, error) {
	var lastErr error
	depth := domain.DepthFrom(ctx)
	for i, model := range chain {
		msg, usage, emitted, err := l.complete(ctx, t, w, sink, answersAt, model)

		// THE DEPTH DEGRADATION, and it is the same argument the image
		// degradation above makes. A model that rejects `reasoning_effort`
		// rejects it on every turn, so a chain that walked past it would spend
		// the fallback budget on a field nobody asked for -- and a chain of one
		// would simply stop answering. So the field comes off and the SAME
		// model is asked again, once.
		//
		// Guarded by emitted for the reason the whole file is: once a byte has
		// reached the member, this turn belongs to this model and a second
		// attempt would splice two answers into one bubble.
		if err != nil && !emitted && l.sendsThinking(model) && !depth.Suppressed(model) {
			depth.Suppress(model)
			msg, usage, emitted, err = l.complete(ctx, t, w, sink, answersAt, model)
		}

		if err == nil {
			return msg, usage, nil
		}
		lastErr = err
		// A CANCELLED TURN IS NOT A MODEL THAT DID NOT ANSWER, and walking the
		// chain on one is how a member who pressed Stop was told their models
		// were down: every remaining candidate fails instantly on the same dead
		// context, each announcing itself in a progress frame on the way past.
		if cerr := ctx.Err(); cerr != nil {
			return domain.Message{}, domain.Usage{}, cerr
		}
		if emitted || i == len(chain)-1 {
			return domain.Message{}, domain.Usage{}, err
		}
		// Said out loud rather than logged: a member watching a turn stall for
		// eight seconds and then answer is owed the reason, and this is the
		// same channel the tool narration already uses.
		sink.EmitProgress(domain.Progress{
			Kind: domain.ProgressPlaceholder,
			Text: fmt.Sprintf("%s did not answer (%v); trying %s", model, err, chain[i+1]),
		})
		// The same fact, kept. A turn that took eight seconds longer because its
		// first model was down is owed that sentence after the fact too -- the
		// progress frame above is gone the moment the next one replaces it.
		domain.RecorderFrom(ctx).Add(domain.TurnEvent{
			Kind: domain.EventModel, Name: model, Status: domain.EventFailed,
			Detail: fmt.Sprintf("%v; trying %s", err, chain[i+1]),
		})
	}
	if lastErr == nil {
		lastErr = errors.New("no model is configured for this turn")
	}
	return domain.Message{}, domain.Usage{}, lastErr
}

// systemFor assembles the system message for one provider call.
//
// Falls back to the static string rather than to nothing: a Prompt that returns
// "" because a file went missing must not silently strip an agent's identity
// mid-conversation, which is the failure a persona bind that never existed
// already caused once.
func (l *Loop) systemFor(ctx context.Context) string {
	if l.Prompt != nil {
		if s := l.Prompt.System(ctx); s != "" {
			return s
		}
	}
	return l.System
}

// modelsFor resolves the ordered candidates for a turn, falling back to the
// single configured model when no registry is wired.
//
// The window decides the KIND: a turn carrying an image needs a model that can
// see one. Read from the window rather than from the turn's own input because
// an image sent three messages ago is still in the context being sent, and it
// is the REQUEST that has to be answerable, not the last thing typed.
func (l *Loop) modelsFor(ctx context.Context, t domain.Turn, w domain.Window) []string {
	kind := domain.ModelText
	if hasAttachments(w) {
		kind = domain.ModelVision
	}
	if l.Models != nil {
		if chain := l.Models.Chain(t.Model, kind); len(chain) > 0 {
			return l.preferDeep(ctx, kind, chain)
		}
		// A ModelChain that has no vision slot is not a reason to give up on
		// the turn: a deployment whose only model is multimodal configures no
		// second entry, and asking for the text chain is what serves it. The
		// registry already does this internally; asserting it HERE too keeps
		// the loop correct whatever implements the port.
		if kind != domain.ModelText {
			if chain := l.Models.Chain(t.Model, domain.ModelText); len(chain) > 0 {
				return l.preferDeep(ctx, domain.ModelText, chain)
			}
		}
	}
	return []string{l.modelFor(t)}
}

// preferDeep puts a thinking-capable model at the head of the chain when the
// agent has asked to think harder and the model it would otherwise use cannot
// express that.
//
// Re-evaluated on EVERY iteration, which is the point: set_reasoning_depth is a
// tool, so the depth is raised in the middle of a turn, after the agent has seen
// enough to know the problem is hard. A choice made once at the turn's start
// would be made before the evidence for it existed.
//
// The original chain stays behind the new head rather than being replaced. A
// deep model that is down must not take the turn with it, and the ordinary
// fallback ladder is exactly the recovery that belongs here.
//
// TEXT ONLY. A turn carrying an image was routed to a model that can SEE, and
// trading that for one that can think would answer a question about a picture
// nobody looked at.
func (l *Loop) preferDeep(ctx context.Context, kind domain.ModelKind, chain []string) []string {
	if kind != domain.ModelText || l.Thinking == nil || len(chain) == 0 {
		return chain
	}
	depth := domain.DepthFrom(ctx)
	if depth.Level() == "" || l.Thinking.SendsThinking(chain[0]) {
		return chain
	}
	deep := l.Thinking.DeepModels()
	if len(deep) == 0 {
		return chain // nothing deeper exists; the directive carries the depth
	}
	out := make([]string, 0, len(deep)+len(chain))
	seen := map[string]bool{}
	for _, name := range append(append([]string{}, deep...), chain...) {
		if seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

// deliberate is the depth the WIRE cannot carry, said in the prompt instead.
//
// The floor under the whole feature: it needs no configuration, no second model
// and no provider support, so an agent can always ask to think harder and have
// it mean something. It is appended only when the chosen model takes no depth
// field -- when the field is available the turn already carries the choice, and
// adding prose as well would change a path that works today for no gain.
//
// Deliberately plain and short. A long instruction competes with the agent's own
// system prompt for attention, and what is being asked for here is one thing.
func deliberate(reason string) string {
	out := "\n\nThis turn has been marked as needing deeper reasoning. Work the problem " +
		"through step by step before answering: state what is being asked, consider the " +
		"alternatives that are actually different from one another, and check your answer " +
		"against the evidence you have rather than against what sounds right."
	if reason != "" {
		out += " The agent raised the depth because: " + reason
	}
	return out
}

// sendsThinking is nil-safe: no chain wired means no model carries depth, so
// nothing is ever retried for that reason.
func (l *Loop) sendsThinking(model string) bool {
	return l.Thinking != nil && l.Thinking.SendsThinking(model)
}

func hasAttachments(w domain.Window) bool {
	for _, m := range w.Messages {
		if len(m.Attachments) > 0 {
			return true
		}
	}
	return false
}

// stripAttachments copies a window without its media.
//
// A COPY, not an edit: the window is saved after the turn, and dropping the
// member's image from the durable record because one model could not read it
// would mean a model configured later could never read it either.
func stripAttachments(w domain.Window) domain.Window {
	out := domain.Window{Summary: w.Summary, Messages: make([]domain.Message, 0, len(w.Messages))}
	for _, m := range w.Messages {
		if len(m.Attachments) > 0 {
			m.Attachments = nil
			if m.Content == "" {
				// A message that was ONLY an image would otherwise become an
				// empty user turn, which some providers reject outright.
				m.Content = "[an image was attached here and could not be read]"
			}
		}
		out.Messages = append(out.Messages, m)
	}
	return out
}

// complete runs one provider call, delivering what it produces to the sink.
//
// The reported bool is "did anything reach the member": it is what makes the
// fallback above safe. It is true once the frame has been delivered -- as
// content, or as the tool calls the caller narrates -- and not from the first
// byte off the socket, because a provider that returns headers and then dies
// has emitted nothing the member can see.
func (l *Loop) complete(
	ctx context.Context, t domain.Turn, w domain.Window, sink domain.Sink,
	answersAt time.Time, model string,
) (domain.Message, domain.Usage, bool, error) {
	ctx, end := l.span(ctx, "provider.complete", domain.Attr{Key: "model", Value: model})
	var err error
	defer func() { end(err) }()

	var emitted bool
	// Read from the context rather than passed down: the cell is per turn and
	// every caller of complete is inside one, so threading it through four
	// signatures would say nothing the context does not already say.
	depth := domain.DepthFrom(ctx)
	system := l.systemFor(ctx)
	if depth.Level() != "" && !l.sendsThinking(model) {
		system += deliberate(depth.Reason())
	}
	stream, err := l.Provider.Complete(ctx, domain.Completion{
		Model:    model,
		Window:   w,
		System:   system,
		Tools:    l.Tools.Available(ctx),
		Messages: w.Messages,

		ThinkingLevel: depth.Level(),
		NoThinking:    depth.Suppressed(model),
	})
	if err != nil {
		return domain.Message{}, domain.Usage{}, emitted, fmt.Errorf("provider: %w", err)
	}
	defer stream.Close()

	// THE FRAME'S CONTENT IS HELD UNTIL THE FRAME ENDS, and that is the price
	// the visible steps are bought with rather than an oversight.
	//
	// An iteration's text is NARRATION when the frame ends in tool calls and the
	// ANSWER when it does not, and nothing says which until it ends: Delta
	// carries content and reasoning only, Stream.Message is valid only after
	// io.EOF, and "no call has appeared yet" is not evidence -- crab-shell-proxy
	// measured 7 turns in 112 delivering a whole reply in the same frame as a
	// trailing call. Emitting optimistically and re-marking afterwards is
	// precisely the reflow this design exists to prevent: the live partition
	// would disagree with the transcript's for the length of the turn.
	//
	// What it costs: narration loses a reveal nobody was reading anyway, and THE
	// ANSWER LOSES ITS TOKEN-BY-TOKEN ARRIVAL -- it lands in one emission once
	// the model has finished writing it. The only thing that would buy it back
	// is a provider saying a frame will make no tool call BEFORE its text
	// arrives, which no OpenAI-compatible stream offers.
	//
	// Reasoning is different and still streams, coalesced at 1s: it is already
	// progress, so it crosses no partition. The interval is coarse because
	// reasoning IS narration -- picoclaw emits a frame per thought, not per
	// token, and this channel is consumed as though that were true.
	var said strings.Builder
	lastCheckpoint := l.Now()

	reasoning := newCoalescer(time.Second, l.Now, func(text string) {
		sink.EmitProgress(domain.Progress{Kind: domain.ProgressThought, Text: text})
	})

	for {
		d, nerr := stream.Next(ctx)
		if errors.Is(nerr, io.EOF) {
			break
		}
		if nerr != nil {
			// Whatever arrived is the member's, even though the turn failed --
			// and it goes out as CONTENT. A frame that died will run no tool, so
			// there is no step for it to belong to, and Stream.Message cannot be
			// asked what it would have been. This is also what keeps `emitted`
			// true exactly where it was true before the buffering above: the
			// fallback ladder must not restart a model that has already spoken.
			if said.Len() > 0 {
				sink.EmitContent(said.String())
				emitted = true
			}
			reasoning.Flush()
			err = fmt.Errorf("provider stream: %w", nerr)
			return domain.Message{}, domain.Usage{}, emitted, err
		}
		said.WriteString(d.Content)
		reasoning.Add(d.Reasoning)

		// THE SIDECAR STANDS IN FOR THIS FRAME, not for the turn.
		//
		// It used to be seeded with what earlier iterations had said, because
		// the turn wrote one message at the end and the sidecar had to be able
		// to replace it. Now each iteration writes its own as it lands, so
		// seeding would show the member their narration twice after a crash:
		// once as the step it was written as, once inside the recovered
		// fragment.
		//
		// answersAt is the instant this iteration began, and that is what keeps
		// the sidecar readable at all. Both readers -- this store's ReadPartial
		// and crab-shell-proxy's livePartial -- call a sidecar stale once the
		// transcript holds an assistant message at or after answersAt. Dated by
		// the member's question, as it was when a turn wrote one message, the
		// FIRST narration step would supersede every checkpoint after it and a
		// turn interrupted in its third iteration would recover nothing.
		//
		// D-2. On the delta path rather than in a goroutine: an 887us write
		// every 2s of streaming is 0.04% of that window, so there is nothing
		// to parallelise and a second writer would need a lock for no gain.
		if l.Checkpoints != nil && said.Len() > 0 &&
			l.Now().Sub(lastCheckpoint) >= l.CheckpointEvery {
			if cerr := l.Checkpoints.Checkpoint(ctx, t.SessionID, answersAt, said.String()); cerr != nil {
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
	reasoning.Flush()
	msg := stream.Message()
	if len(msg.ToolCalls) > 0 {
		// NARRATION. Its text does not enter the content run at all -- Run
		// emits it as a tool progress frame, and records it as a step. A tool
		// call is not content, but it IS work the turn has committed to:
		// re-running it under another model would re-run the tool.
		return msg, stream.Usage(), true, nil
	}
	// THE ANSWER. Emitted from the message rather than from the buffer above so
	// that what the member sees and what the transcript records are the same
	// bytes by construction -- an equality the one-message-per-turn design got
	// for free and this one has to hold across several messages.
	if msg.Content != "" {
		sink.EmitContent(msg.Content)
		emitted = true
	}
	return msg, stream.Usage(), emitted, nil
}

// runTool asks the Approver, then invokes -- or turns a refusal into a Result.
//
// narration is the text the model wrote in the frame that asked for this call,
// and this frame is the only place the member sees it while the turn runs.
func (l *Loop) runTool(
	ctx context.Context, t domain.Turn, call domain.ToolCall, narration string, sink domain.Sink,
) (domain.Result, error) {
	ctx, end := l.span(ctx, "tool.invoke", domain.Attr{Key: "tool.name", Value: call.Name})
	var err error
	defer func() { end(err) }()

	// Shaped exactly like picoclaw's: kind "tool", the agent's own sentence as
	// the text, the call it narrates as the tool. crab-shell-proxy builds that
	// frame in internal/pico (progressFor, the tool_calls branch) and the webapp
	// renders it, so FR-4 is satisfied by using the vocabulary that already
	// exists rather than by inventing a second shape for the same event.
	sink.EmitProgress(domain.Progress{Kind: domain.ProgressTool, Text: narration, Tool: call.Name})
	// The sink, reachable from inside the tool. Only one tool asks for it -- the
	// dispatcher, which is the only one that can run for minutes -- and it
	// serialises its own emissions, because Sink is a struct of plain funcs with
	// no mutex.
	ctx = domain.WithSink(ctx, sink)

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

// flushEvents writes one iteration's events as their own transcript entry.
//
// It carries no content, which is the whole shape: this is the iteration's
// DETAIL, not a second thing the agent said. crab-shell-proxy keeps an entry
// like that precisely because it has events -- without them it is dropped as
// empty, which is what used to happen to a frame that called a tool and
// narrated nothing.
//
// A failed write is said out loud and swallowed. These are commentary; losing
// the turn over them would trade the answer for the log of how it was produced.
func (l *Loop) flushEvents(
	ctx context.Context, t domain.Turn, events *domain.Recorder, sink domain.Sink,
) {
	recorded := events.Take()
	if len(recorded) == 0 {
		return
	}
	if err := l.Transcript.Append(ctx, t.SessionID, domain.Message{
		Role: domain.RoleAssistant, Events: recorded, CreatedAt: l.Now(),
	}); err != nil {
		sink.EmitProgress(domain.Progress{
			Kind: domain.ProgressPlaceholder,
			Text: "could not record this step's events: " + err.Error(),
		})
	}
}

// eventArgs is a tool call's arguments, in the form the member reads them.
//
// FLATTENED AND CAPPED, and the cap is in the harness on purpose: a write_file
// call's arguments contain the whole file and a shell call's contain the whole
// command, so an uncapped event log would grow a transcript by everything the
// agent ever wrote. Capping downstream would save nothing -- the bytes would
// already be on disk.
//
// Whitespace is collapsed first so the cap measures what will be SHOWN rather
// than the document's indentation, which is most of a pretty-printed argument
// object and none of its meaning.
func eventArgs(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	flat := strings.Join(strings.Fields(string(raw)), " ")
	// Runes, not bytes: a cut through a multi-byte character would put invalid
	// UTF-8 on disk, and every reader from here to the browser would have to
	// cope with it.
	r := []rune(flat)
	if len(r) <= maxEventArgs {
		return flat
	}
	return string(r[:maxEventArgs]) + "…"
}

// maxEventArgs is how much of a call's arguments the member is shown. Long
// enough for a URL, a path, or a short command -- which is what makes a step
// verifiable -- and far short of a file's contents.
const maxEventArgs = 200

// mintAuditID names a tool call's record, uniquely within a conversation.
//
// THE HARNESS MINTS IT, rather than using the provider's call id, because that
// id MAY BE EMPTY: the OpenAI adapter assigns ToolCall.ID only when the stream
// carried one (`openai.go`, absorbToolCalls) and has no fallback. A store keyed
// on it would put every idless call in a conversation into one file -- silently,
// because `safe("")` is a valid filename.
//
// Nanoseconds plus the call's index within its iteration. Unique without
// consulting anything (two calls in one iteration differ by index; two
// iterations differ by clock), sorts chronologically, and is already within
// [a-z0-9_-] so no store has to reshape it.
func mintAuditID(now time.Time, callIdx int) string {
	return fmt.Sprintf("%d-%02d", now.UnixNano(), callIdx)
}

// putAudit records the command and returns the id to put on the event, or ""
// when there is no store or the write failed.
//
// THE RETURN IS THE POINTER, so an event only ever names a record that was
// actually written. A turn whose disk is full keeps working and its rows simply
// do not open, which is the same degradation as a transcript written before any
// of this existed.
//
// Failure is never an error here, for the reason the offload gives: this moves
// bytes, it does not change what the agent is told, so a turn that works today
// cannot start failing because this did.
func (l *Loop) putAudit(ctx context.Context, t domain.Turn, auditID string, call domain.ToolCall) string {
	if l.ToolAudit == nil {
		return ""
	}
	if err := l.ToolAudit.Put(ctx, t.SessionID, auditID, call.Name, string(call.Args)); err != nil {
		return ""
	}
	return auditID
}

// completeAudit fills in what the call returned. Silent on every failure,
// including a record that was never written.
func (l *Loop) completeAudit(ctx context.Context, t domain.Turn, auditID, output, status, detail string) {
	if l.ToolAudit == nil || auditID == "" {
		return
	}
	_ = l.ToolAudit.Complete(ctx, t.SessionID, auditID, output, status, detail)
}
