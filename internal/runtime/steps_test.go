package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

// The steps, from both sides at once.
//
// A turn that used tools renders as a sequence of visible STEPS again, and the
// property that makes that safe is not "the transcript has more messages" -- it
// is that the live stream and the transcript partition the same text the same
// way. A test per half would let the two drift apart and still pass, so the
// partition itself is asserted here (TestRun_TheTranscriptSaysWhatTheStreamSaid)
// alongside each half.

// narrating is a provider that narrates, calls one tool, then answers.
func narrating() *fakeProvider {
	return &fakeProvider{turns: []fakeTurn{
		{
			deltas: []string{"Vou ", "olhar o projeto."},
			msg: domain.Message{
				Role: domain.RoleAssistant, Content: "Vou olhar o projeto.",
				ToolCalls: []domain.ToolCall{{ID: "1", Name: "sh", Args: args(`{}`)}},
			},
		},
		{
			deltas: []string{"Encontrei ", "tres arquivos."},
			msg:    domain.Message{Role: domain.RoleAssistant, Content: "Encontrei tres arquivos."},
		},
	}}
}

// THE SHAPE crab-shell-proxy reads. Its history reader derives the whole step
// distinction from one line -- an assistant message carrying tool_calls is a
// step -- so a transcript with one message per turn left that marker dead and
// every ganglion turn rendering as a single block of concatenated text.
func TestRun_ATurnOfTwoIterationsIsTwoTranscriptMessages(t *testing.T) {
	tr := &fakeTranscript{}
	l := newLoop(narrating(), tr, &fakeContext{}, &fakeTools{result: domain.Result{Content: "saida"}}, nil)

	if _, err := l.Run(context.Background(), turn(), domain.Sink{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(tr.log) != 3 {
		t.Fatalf("transcript has %d entries, want user + step + answer: %+v", len(tr.log), tr.log)
	}
	step, answer := tr.log[1], tr.log[2]
	if step.Content != "Vou olhar o projeto." || len(step.ToolCalls) != 1 {
		t.Errorf("the narration is not a step: %+v", step)
	}
	if step.ToolCalls[0].Name != "sh" {
		t.Errorf("the step names %q, want the call it narrated", step.ToolCalls[0].Name)
	}
	if answer.Content != "Encontrei tres arquivos." || len(answer.ToolCalls) != 0 {
		t.Errorf("the answer is not a plain assistant message: %+v", answer)
	}
}

// THE LIVE HALF. Narration leaves the content run entirely: it goes out as a
// tool progress frame carrying the agent's own sentence, which is the shape
// crab-shell-proxy already builds for picoclaw (internal/pico, progressFor).
//
// Emitting it as content AND marking it a step in the transcript is the exact
// combination that made the reply rewrite itself when the client reconciled.
func TestRun_NarrationIsProgressAndTheAnswerIsContent(t *testing.T) {
	c := &collect{}
	l := newLoop(narrating(), &fakeTranscript{}, &fakeContext{}, &fakeTools{result: domain.Result{Content: "saida"}}, nil)

	if _, err := l.Run(context.Background(), turn(), c.sink()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Contains(c.joined(), "Vou olhar") {
		t.Errorf("narration reached the member as content (%q) -- the bubble will rewrite itself when the client reconciles", c.joined())
	}
	if c.joined() != "Encontrei tres arquivos." {
		t.Errorf("content = %q, want the answer and nothing else", c.joined())
	}
	var narrated bool
	for _, p := range c.progress {
		if p.Kind == domain.ProgressTool && p.Text == "Vou olhar o projeto." && p.Tool == "sh" {
			narrated = true
		}
	}
	if !narrated {
		t.Errorf("the narration never reached the member at all: %+v", c.progress)
	}
}

// THE PARTITION ITSELF, and the property the one-message-per-turn design was
// protecting: what the transcript serves back, read in order, is what the member
// watched arrive -- neither more nor less, or the reply visibly changes when the
// client reconciles the live stream against the history.
//
// It used to hold trivially, with one message holding the whole turn. Now it has
// to hold across a sequence, and across two channels.
func TestRun_TheTranscriptSaysWhatTheStreamSaid(t *testing.T) {
	tr := &fakeTranscript{}
	c := &collect{}
	l := newLoop(narrating(), tr, &fakeContext{}, &fakeTools{result: domain.Result{Content: "saida"}}, nil)

	got, err := l.Run(context.Background(), turn(), c.sink())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	var served strings.Builder
	for _, m := range tr.log {
		if m.Role == domain.RoleAssistant {
			served.WriteString(m.Content)
		}
	}
	// What the member saw, in the order they saw it: each narration frame, then
	// the answer. A tool frame repeats its narration once per call in the batch,
	// so only the first of a run counts -- the member sees one line, not one per
	// call.
	var watched strings.Builder
	var last string
	for _, p := range c.progress {
		if p.Kind == domain.ProgressTool && p.Text != "" && p.Text != last {
			watched.WriteString(p.Text)
			last = p.Text
		}
	}
	watched.WriteString(c.joined())

	if served.String() != watched.String() {
		t.Errorf("the transcript serves %q and the member watched %q arrive", served.String(), watched.String())
	}
	if got != served.String() {
		t.Errorf("Run returned %q, which is not what it served: %q", got, served.String())
	}
}

// A TURN INTERRUPTED MID-WAY still shows the member what was said, and it now
// has two mechanisms for it rather than one: the steps that completed are
// ordinary transcript messages, and the frame that died is in the sidecar.
//
// The sidecar's date is the load-bearing part. Both readers -- this store's
// ReadPartial and crab-shell-proxy's livePartial -- call a partial stale once
// the transcript holds an assistant message at or after the instant it answers.
// Dated by the member's question, as it was when a turn wrote one message, the
// first narration step would supersede every checkpoint that followed it and
// this turn would recover nothing.
func TestRun_AnInterruptedTurnLeavesTheMemberWhatWasSaid(t *testing.T) {
	tr := &fakeTranscript{}
	cp := &fakeCheckpoints{}
	c := &collect{}
	p := &fakeProvider{turns: []fakeTurn{
		{
			deltas: []string{"Vou olhar o projeto."},
			msg: domain.Message{
				Role: domain.RoleAssistant, Content: "Vou olhar o projeto.",
				ToolCalls: []domain.ToolCall{{ID: "1", Name: "sh", Args: args(`{}`)}},
			},
		},
		{deltas: []string{"Encontrei "}, err: errors.New("connection reset")},
	}}
	l := newLoop(p, tr, &fakeContext{}, &fakeTools{result: domain.Result{Content: "saida"}}, nil)
	l.Checkpoints = cp
	l.CheckpointEvery = time.Second
	// An ADVANCING clock: the checkpoint cadence is a wall clock by design, and
	// the supersession rule this test is about compares instants.
	tick := time.Unix(0, 0)
	l.Now = func() time.Time { tick = tick.Add(time.Second); return tick }

	if _, err := l.Run(context.Background(), turn(), c.sink()); err == nil {
		t.Fatal("expected the provider failure to surface")
	}

	if len(tr.log) != 2 || tr.log[1].Content != "Vou olhar o projeto." {
		t.Fatalf("the step the member watched go by is not durable: %+v", tr.log)
	}
	if len(cp.writes) == 0 {
		t.Fatal("the interrupted frame was never checkpointed")
	}
	last := len(cp.writes) - 1
	if cp.writes[last] != "Encontrei " {
		t.Errorf("the sidecar holds %q; it stands in for the frame that died, not for the whole turn -- "+
			"seeding it with the steps already written shows them twice after a crash", cp.writes[last])
	}
	if !cp.dates[last].After(tr.log[1].CreatedAt) {
		t.Errorf("the sidecar answers at %v and the step before it is dated %v: every reader will call it stale",
			cp.dates[last], tr.log[1].CreatedAt)
	}
	// And the member saw it live too: a dying frame will run no tool, so its
	// text is delivered as content rather than held for a step that never came.
	if !strings.Contains(c.joined(), "Encontrei ") {
		t.Errorf("content = %q, want what the dying frame had already produced", c.joined())
	}
}

// A frame that asked for a tool without saying anything writes NOTHING to the
// transcript. An assistant message with no text is dropped by the proxy's
// history reader anyway, so writing one would leave a step that renders as an
// empty band between the question and the answer.
func TestRun_ASilentToolCallIsNotAStep(t *testing.T) {
	tr := &fakeTranscript{}
	p := &fakeProvider{turns: []fakeTurn{
		{msg: domain.Message{Role: domain.RoleAssistant, ToolCalls: []domain.ToolCall{{ID: "1", Name: "sh", Args: args(`{}`)}}}},
		{msg: domain.Message{Role: domain.RoleAssistant, Content: "pronto"}},
	}}
	l := newLoop(p, tr, &fakeContext{}, &fakeTools{result: domain.Result{Content: "saida"}}, nil)

	if _, err := l.Run(context.Background(), turn(), domain.Sink{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(tr.log) != 2 {
		t.Fatalf("transcript has %d entries, want user + answer: %+v", len(tr.log), tr.log)
	}
}
