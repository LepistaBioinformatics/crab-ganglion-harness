package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

// depthTools is a tool set whose single tool writes the turn's depth cell, the
// way set_reasoning_depth does. It is here rather than imported because
// internal/runtime must not depend on an adapter.
type depthTools struct {
	level  string
	reason string
	calls  int
}

func (d *depthTools) Available(context.Context) []domain.ToolSchema {
	return []domain.ToolSchema{{Name: "set_reasoning_depth"}}
}

func (d *depthTools) Invoke(ctx context.Context, _ domain.ToolCall) (domain.Result, error) {
	d.calls++
	domain.DepthFrom(ctx).Set(d.level, d.reason)
	return domain.Result{Content: "depth set"}, nil
}

// thinkingChain answers "would this model carry a depth field".
type thinkingChain struct {
	sends map[string]bool
	deep  []string
}

func (t thinkingChain) SendsThinking(m string) bool { return t.sends[m] }
func (t thinkingChain) DeepModels() []string        { return t.deep }

// toolThenAnswer is the two-iteration shape every test here needs: the model
// calls one tool, then answers.
func toolThenAnswer() []fakeTurn {
	return []fakeTurn{
		{msg: domain.Message{Role: domain.RoleAssistant,
			ToolCalls: []domain.ToolCall{{ID: "c1", Name: "set_reasoning_depth", Args: args(`{}`)}}}},
		{deltas: []string{"pronto"}, msg: domain.Message{Role: domain.RoleAssistant, Content: "pronto"}},
	}
}

// AC-3, first half. A depth chosen at iteration 1 reaches EVERY LATER
// COMPLETION of the same turn. Sticky is the whole reason the tool is worth an
// iteration; a level that applied only to the call that set it would change
// nothing, because that call has already been made.
func TestChosenDepthAppliesToEveryLaterCompletionOfTheTurn(t *testing.T) {
	var seen []string
	p := &fakeProvider{turns: toolThenAnswer(), onComplete: func(c domain.Completion) {
		seen = append(seen, c.ThinkingLevel)
	}}
	l := newLoop(p, &fakeTranscript{}, &fakeContext{}, nil, nil)
	l.Tools = &depthTools{level: "xhigh", reason: "two schemas disagree"}

	if _, err := l.Run(context.Background(), turn(), (&collect{}).sink()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(seen) != 2 {
		t.Fatalf("the provider was called %d times, want 2: %v", len(seen), seen)
	}
	if seen[0] != "" {
		t.Errorf("the first completion already carried a level (%q); nothing had chosen one yet", seen[0])
	}
	if seen[1] != "xhigh" {
		t.Errorf("the completion after the tool call carried %q, want xhigh", seen[1])
	}
}

// AC-3, second half. The level dies with the turn. A question the agent decided
// was hard must not silently bill every question after it.
func TestTheChosenDepthDoesNotSurviveIntoTheNextTurn(t *testing.T) {
	var seen []string
	p := &fakeProvider{turns: toolThenAnswer(), onComplete: func(c domain.Completion) {
		seen = append(seen, c.ThinkingLevel)
	}}
	l := newLoop(p, &fakeTranscript{}, &fakeContext{}, nil, nil)
	l.Tools = &depthTools{level: "high"}
	ctx, sink := context.Background(), (&collect{}).sink()

	if _, err := l.Run(ctx, turn(), sink); err != nil {
		t.Fatalf("first Run: %v", err)
	}
	// The second turn answers straight away: turns[] is exhausted, so the fake
	// repeats its last entry, which has no tool call.
	seen = nil
	if _, err := l.Run(ctx, turn(), sink); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if len(seen) == 0 || seen[0] != "" {
		t.Fatalf("the next turn started at %q; it must start at the model's configured level", seen)
	}
}

// The member is told, and told WHY. A turn that suddenly costs four times as
// much is owed a sentence.
func TestRaisingTheDepthIsNarratedWithItsReason(t *testing.T) {
	p := &fakeProvider{turns: toolThenAnswer()}
	c := &collect{}
	l := newLoop(p, &fakeTranscript{}, &fakeContext{}, nil, nil)
	l.Tools = &depthTools{level: "high", reason: "the two migrations conflict"}

	if _, err := l.Run(context.Background(), turn(), c.sink()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var found string
	c.mu.Lock()
	for _, pr := range c.progress {
		if pr.Kind == domain.ProgressThought && strings.Contains(pr.Text, "depth high") {
			found = pr.Text
		}
	}
	c.mu.Unlock()
	if found == "" {
		t.Fatal("the depth change was never narrated")
	}
	if !strings.Contains(found, "the two migrations conflict") {
		t.Errorf("the reason was dropped: %q", found)
	}
}

// AC-6. THE DEGRADATION. A model that rejects the depth field is asked again
// WITHOUT it, on the same model, and the turn answers.
//
// picoclaw's equivalent failure -- an unsupported request field ending the turn
// -- is the one this stack already met in production with images, which is why
// deploy/picoclaw-glob/vision-unsupported-glm.patch exists.
func TestARejectedDepthFieldIsRetriedWithoutItRatherThanEndingTheTurn(t *testing.T) {
	var withDepth, without int
	p := &fakeProvider{turns: []fakeTurn{{
		deltas: []string{"ok"}, msg: domain.Message{Role: domain.RoleAssistant, Content: "ok"},
	}}}
	p.onComplete = func(c domain.Completion) {
		if c.NoThinking {
			without++
			return
		}
		withDepth++
	}
	// The rejection: the provider errors while the depth field is present.
	p.gate = func(c domain.Completion) error {
		if !c.NoThinking {
			return errors.New("400: unknown parameter reasoning_effort")
		}
		return nil
	}
	l := newLoop(p, &fakeTranscript{}, &fakeContext{}, &fakeTools{}, nil)
	l.Thinking = thinkingChain{sends: map[string]bool{"deepseek-chat": true}}
	c := &collect{}

	out, err := l.Run(context.Background(), turn(), c.sink())
	if err != nil {
		t.Fatalf("the turn failed instead of degrading: %v", err)
	}
	if out != "ok" {
		t.Errorf("answer = %q, want ok", out)
	}
	if withDepth != 1 || without != 1 {
		t.Errorf("requests: %d with depth, %d without; want exactly one of each", withDepth, without)
	}
}

// The retry is NOT attempted for a model that carries no depth field. Retrying
// unconditionally would double the latency of every failure in the chain, on
// requests that cannot have failed for this reason.
func TestAModelThatCarriesNoDepthFieldIsNotRetried(t *testing.T) {
	var calls int
	p := &fakeProvider{turns: []fakeTurn{{msg: domain.Message{Role: domain.RoleAssistant}}}}
	p.onComplete = func(domain.Completion) { calls++ }
	p.gate = func(domain.Completion) error { return errors.New("502 bad gateway") }
	l := newLoop(p, &fakeTranscript{}, &fakeContext{}, &fakeTools{}, nil)
	l.Thinking = thinkingChain{sends: map[string]bool{}} // this model carries none

	if _, err := l.Run(context.Background(), turn(), (&collect{}).sink()); err == nil {
		t.Fatal("the turn should have failed")
	}
	if calls != 1 {
		t.Errorf("the provider was called %d times, want 1", calls)
	}
}

// Once per model per TURN, not once per request. A model whose second iteration
// failed for an unrelated reason must not be asked a third time.
func TestTheDepthRetryHappensAtMostOncePerModelPerTurn(t *testing.T) {
	var calls int
	p := &fakeProvider{turns: []fakeTurn{{msg: domain.Message{Role: domain.RoleAssistant}}}}
	p.onComplete = func(domain.Completion) { calls++ }
	p.gate = func(domain.Completion) error { return errors.New("always down") }
	l := newLoop(p, &fakeTranscript{}, &fakeContext{}, &fakeTools{}, nil)
	l.Thinking = thinkingChain{sends: map[string]bool{"deepseek-chat": true}}

	if _, err := l.Run(context.Background(), turn(), (&collect{}).sink()); err == nil {
		t.Fatal("the turn should have failed")
	}
	if calls != 2 {
		t.Errorf("the provider was called %d times, want 2 (the request and one retry)", calls)
	}
}

// What the agent chose reaches the Learner, which is the only way anyone will
// ever find out whether depth correlates with succeeding.
func TestTheChosenDepthIsRecordedOnTheTurn(t *testing.T) {
	p := &fakeProvider{turns: toolThenAnswer()}
	lr := &recordingLearner{}
	l := newLoop(p, &fakeTranscript{}, &fakeContext{}, nil, nil)
	l.Tools = &depthTools{level: "medium", reason: "ambiguous requirement"}
	l.Learner = lr

	if _, err := l.Run(context.Background(), turn(), (&collect{}).sink()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(lr.seen) != 1 {
		t.Fatalf("the learner saw %d turns, want 1", len(lr.seen))
	}
	if lr.seen[0].Thinking != "medium" || lr.seen[0].Why != "ambiguous requirement" {
		t.Errorf("record = %q/%q, want medium/ambiguous requirement", lr.seen[0].Thinking, lr.seen[0].Why)
	}
}

type recordingLearner struct{ seen []domain.TurnRecord }

func (r *recordingLearner) Observe(_ context.Context, rec domain.TurnRecord) {
	r.seen = append(r.seen, rec)
}
