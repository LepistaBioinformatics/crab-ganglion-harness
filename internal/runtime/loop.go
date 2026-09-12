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
	Tools       domain.ToolExecutor
	Approver    domain.Approver
	Telemetry   domain.Telemetry

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

	depth := &domain.Depth{}
	ctx = domain.WithDepth(ctx, depth)
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
	window = dropOrphanTools(window)
	window.Messages = append(window.Messages, in)

	// What the member sees, accumulated across every iteration of this turn.
	//
	// The transcript records ONE assistant message per turn, holding exactly
	// this. That is not a simplification -- it is what makes the served
	// history match the stream.
	//
	// The stream is one continuous run of content: an iteration's narration,
	// then tool progress, then the next iteration's text, all into one bubble.
	// Writing the transcript per iteration instead produced a DIFFERENT shape
	// -- crab-shell-proxy marks an assistant message carrying tool_calls as a
	// "step" -- so when the turn ended and the client reconciled against the
	// transcript, the single bubble was torn into narration plus answer and
	// the whole reply visibly rewrote itself.
	//
	// Tool calls and results still reach the PROVIDER: they go into the
	// context window, which is a separate artifact built separately below.
	// Only the served history is collapsed.
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
		msg, usage, err := c.completeWithFallback(ctx, t, window, sink, in.CreatedAt, answer.String())
		if err != nil {
			runErr = err
			sink.EmitError(err.Error())
			observe(true)
			return answer.String(), runErr
		}
		total.Add(usage)

		msg.CreatedAt = c.Now()
		// The WINDOW gets the message as the PROVIDER needs it: content and
		// tool_calls together, so the tool results below answer a call it can
		// see. The transcript gets one message for the whole turn, at the end.
		window.Messages = append(window.Messages, msg)
		answer.WriteString(msg.Content)

		if len(msg.ToolCalls) == 0 {
			if ferr := c.finishTurn(ctx, t, answer.String()); ferr != nil {
				runErr = ferr
				return answer.String(), runErr
			}
			c.recordUsage(ctx, total, t)
			window = compact(window, c.WindowBudget)
			runErr = c.Context.Save(ctx, t.SessionID, window)
			// AFTER the answer is durable and the member has it. A learner that
			// ran first would put an analysis pass between the model finishing
			// and the transcript being written.
			observe(runErr != nil)
			return answer.String(), runErr
		}

		for _, call := range msg.ToolCalls {
			chosen := depth.Level()
			res, err := c.runTool(ctx, t, call, sink)
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
			}
			if err != nil {
				runErr = err
				sink.EmitError(err.Error())
				_ = c.finishTurn(ctx, t, answer.String())
				outcomes = append(outcomes, domain.ToolOutcome{Name: call.Name, Failed: true})
				observe(true)
				return answer.String(), runErr
			}
			outcomes = append(outcomes, domain.ToolOutcome{Name: call.Name, Denied: res.Denied})
			out := domain.Message{
				Role:       domain.RoleTool,
				Content:    res.Content,
				ToolCallID: call.ID,
				CreatedAt:  c.Now(),
			}
			// The tool result goes to the window only. The served transcript
			// records what the member saw, and they never saw this.
			window.Messages = append(window.Messages, out)

			// Media a tool produced follows as a SYNTHETIC USER MESSAGE rather
			// than riding on the result above.
			//
			// Not a stylistic choice: most providers reject image parts on a
			// `tool` role message, and the ones that accept them disagree about
			// the shape. A user message carrying the image is the one form every
			// OpenAI-compatible endpoint understands, and it is what picoclaw
			// does too (agent_media.go, toolImageFollowUpPromptMessage).
			if len(res.Attachments) > 0 {
				window.Messages = append(window.Messages, domain.Message{
					Role:        domain.RoleUser,
					Content:     "Here is the media that tool loaded.",
					Attachments: res.Attachments,
					CreatedAt:   c.Now(),
				})
			}
		}

		window = compact(window, c.WindowBudget)
		if err := c.Context.Save(ctx, t.SessionID, window); err != nil {
			runErr = fmt.Errorf("save context: %w", err)
			_ = c.finishTurn(ctx, t, answer.String())
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
	if ferr := c.finishTurn(ctx, t, answer.String()); ferr != nil {
		return answer.String(), ferr
	}
	if saveErr := c.Context.Save(ctx, t.SessionID, compact(window, c.WindowBudget)); saveErr != nil {
		return answer.String(), saveErr
	}
	return answer.String(), nil
}

// finishTurn writes the turn's single assistant message and clears the
// checkpoint that was standing in for it.
//
// Called on every exit path that produced text, including the failing ones:
// a turn that died after saying something still said it.
func (l *Loop) finishTurn(ctx context.Context, t domain.Turn, answer string) error {
	if answer == "" {
		// Nothing was said. Clear any checkpoint so a later reader does not
		// resurrect a fragment of a turn that produced no answer.
		if l.Checkpoints != nil {
			_ = l.Checkpoints.ClearPartial(ctx, t.SessionID)
		}
		return nil
	}
	msg := domain.Message{
		Role:      domain.RoleAssistant,
		Content:   answer,
		CreatedAt: l.Now(),
	}
	if err := l.Transcript.Append(ctx, t.SessionID, msg); err != nil {
		return fmt.Errorf("append assistant message: %w", err)
	}
	// D-3. The real message is durable now, so the sidecar is stale. A failed
	// clear is ignored: it leaves a leftover the supersession rule already
	// hides, and failing here would trade that for a lost answer.
	if l.Checkpoints != nil {
		_ = l.Checkpoints.ClearPartial(ctx, t.SessionID)
	}
	return nil
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
	answersAt time.Time, alreadySaid string,
) (domain.Message, domain.Usage, error) {
	msg, usage, err := l.tryChain(ctx, t, w, sink, answersAt, alreadySaid, l.modelsFor(ctx, t, w))
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
	return l.tryChain(ctx, t, stripped, sink, answersAt, alreadySaid, l.modelsFor(ctx, t, domain.Window{}))
}

// tryChain walks one ordered list of candidates.
func (l *Loop) tryChain(
	ctx context.Context, t domain.Turn, w domain.Window, sink domain.Sink,
	answersAt time.Time, alreadySaid string, chain []string,
) (domain.Message, domain.Usage, error) {
	var lastErr error
	depth := domain.DepthFrom(ctx)
	for i, model := range chain {
		msg, usage, emitted, err := l.complete(ctx, t, w, sink, answersAt, alreadySaid, model)

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
			msg, usage, emitted, err = l.complete(ctx, t, w, sink, answersAt, alreadySaid, model)
		}

		if err == nil {
			return msg, usage, nil
		}
		lastErr = err
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

// complete runs one provider call, streaming its deltas to the sink.
//
// The reported bool is "did anything reach the member": it is what makes the
// fallback above safe, and it is true from the first content delta, not from
// the first byte off the socket -- a provider that returns headers and then
// dies has emitted nothing the member can see.
func (l *Loop) complete(
	ctx context.Context, t domain.Turn, w domain.Window, sink domain.Sink,
	answersAt time.Time, alreadySaid string, model string,
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

	// Seeded with what earlier iterations of this turn already said, so a
	// checkpoint recovers the whole answer rather than only its last leg.
	var partial strings.Builder
	partial.WriteString(alreadySaid)
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
		emitted = true
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
			return domain.Message{}, domain.Usage{}, emitted, err
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
	// A tool call is not content, but it IS work the turn has committed to:
	// re-running it under another model would re-run the tool.
	msg := stream.Message()
	if len(msg.ToolCalls) > 0 {
		emitted = true
	}
	return msg, stream.Usage(), emitted, nil
}

// runTool asks the Approver, then invokes -- or turns a refusal into a Result.
func (l *Loop) runTool(ctx context.Context, t domain.Turn, call domain.ToolCall, sink domain.Sink) (domain.Result, error) {
	ctx, end := l.span(ctx, "tool.invoke", domain.Attr{Key: "tool.name", Value: call.Name})
	var err error
	defer func() { end(err) }()

	sink.EmitProgress(domain.Progress{Kind: domain.ProgressTool, Tool: call.Name})
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
