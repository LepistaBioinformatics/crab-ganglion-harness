package runtime

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

func answering(text string) *fakeProvider {
	return &fakeProvider{turns: []fakeTurn{{
		deltas: []string{text},
		msg:    domain.Message{Role: domain.RoleAssistant, Content: text},
	}}}
}

func parentLoop(p *fakeProvider, tl domain.ToolExecutor) *Loop {
	return &Loop{
		Provider: p, Transcript: &fakeTranscript{}, Context: &fakeContext{}, Tools: tl,
		ApprovalHeartbeat: time.Millisecond,
		Now:               func() time.Time { return time.Unix(0, 0) },
	}
}

func TestAChildRunsTheTaskAndReturnsItsAnswer(t *testing.T) {
	parent := parentLoop(answering("the child's conclusion"), &fakeTools{})
	c := &Child{Parent: parent, MaxIterations: 3, MaxDepth: 1}

	rep := c.Run(domain.WithFanout(context.Background(), domain.NewFanout(4)),
		domain.SubTask{Label: "x", Task: "do the thing"})
	if rep.Err != nil {
		t.Fatalf("Run: %v", rep.Err)
	}
	if rep.Answer != "the child's conclusion" {
		t.Errorf("answer = %q", rep.Answer)
	}
}

// AC-9 / NFR-5. A child's transcript is IN MEMORY and never on disk. A child
// writing into workspace/sessions/ would make the proxy's history endpoint
// report conversations no member ever had -- and the finding is already durable
// inside the parent's tool result.
func TestAChildNeverTouchesTheParentsStores(t *testing.T) {
	tr, cs := &fakeTranscript{}, &fakeContext{}
	parent := &Loop{
		Provider: answering("done"), Transcript: tr, Context: cs, Tools: &fakeTools{},
		ApprovalHeartbeat: time.Millisecond, Now: func() time.Time { return time.Unix(0, 0) },
	}
	c := &Child{Parent: parent, MaxIterations: 3, MaxDepth: 1}

	c.Run(context.Background(), domain.SubTask{Label: "x", Task: "t"})

	if len(tr.log) != 0 {
		t.Errorf("the child wrote %d messages into the parent's transcript", len(tr.log))
	}
	if len(cs.saved) != 0 || cs.loaded != 0 {
		t.Errorf("the child touched the parent's window store (loaded=%d saved=%d)", cs.loaded, len(cs.saved))
	}
}

// Evolution observes MEMBER turns. Counting a fan-out's children as turns would
// make any pattern that used the dispatcher look several times as common as it
// is, which is exactly the signal min_task_count measures.
func TestAChildIsNotObservedByTheLearner(t *testing.T) {
	lr := &recordingLearner{}
	parent := parentLoop(answering("done"), &fakeTools{})
	parent.Learner = lr

	(&Child{Parent: parent, MaxIterations: 3, MaxDepth: 1}).Run(
		context.Background(), domain.SubTask{Label: "x", Task: "t"})

	if len(lr.seen) != 0 {
		t.Fatalf("the learner observed %d child turns", len(lr.seen))
	}
}

// AC-6. At the cap the dispatcher is ABSENT from what the child is told it can
// do, not present and refusing -- a tool a model can see and cannot use costs a
// turn to discover.
func TestAtMaxDepthTheDispatcherIsWithheldFromTheChild(t *testing.T) {
	tools := &fakeTools{schemas: []domain.ToolSchema{
		{Name: "shell"}, {Name: "subagents"}, {Name: "research"},
	}}
	var offered []domain.ToolSchema
	p := answering("done")
	p.onComplete = func(c domain.Completion) { offered = c.Tools }

	parent := parentLoop(p, tools)
	c := &Child{
		Parent: parent, MaxIterations: 3, MaxDepth: 1,
		HideAtDepth: []string{"subagents", "research"},
	}

	// Depth 0 -> the child is at depth 1, which IS the cap.
	c.Run(domain.WithFanout(context.Background(), domain.NewFanout(4)),
		domain.SubTask{Label: "x", Task: "t"})

	var names []string
	for _, s := range offered {
		names = append(names, s.Name)
	}
	if strings.Join(names, ",") != "shell" {
		t.Fatalf("the child was offered %v, want shell alone", names)
	}
}

// Withholding from the SCHEMA is not enough: a model can name a tool it was
// never offered, and the answer has to be a refusal rather than a dispatch.
func TestAHiddenToolIsAlsoRefusedIfCalledAnyway(t *testing.T) {
	inner := &fakeTools{result: domain.Result{Content: "ran"}}
	f := withoutTools(inner, []string{"subagents"})

	res, err := f.Invoke(context.Background(), domain.ToolCall{Name: "subagents"})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(inner.invoked) != 0 {
		t.Fatal("the hidden tool was dispatched anyway")
	}
	if !strings.Contains(res.Content, "not available") {
		t.Errorf("content = %q", res.Content)
	}
}

// The budget is the TURN's, shared by everything beneath it. A child that got a
// fresh one would turn a whole-turn bound into a per-child bound -- which is
// the shape picoclaw's nil depth-0 semaphore ends up with by another route.
func TestAChildSharesTheTurnsBudgetRatherThanGettingItsOwn(t *testing.T) {
	var seen *domain.Fanout
	tools := &fakeTools{}
	p := answering("done")
	parent := parentLoop(p, tools)
	// The child's own Run reaches the loop, which must NOT install a new
	// Fanout. Observed through the tool, which runs inside the child's turn.
	parent.Tools = &capturingTools{inner: tools, onInvoke: func(ctx context.Context) {
		seen = domain.FanoutFrom(ctx)
	}}
	p.turns = []fakeTurn{
		{msg: domain.Message{Role: domain.RoleAssistant,
			ToolCalls: []domain.ToolCall{{ID: "c1", Name: "noop", Args: args(`{}`)}}}},
		{msg: domain.Message{Role: domain.RoleAssistant, Content: "done"}},
	}

	fan := domain.NewFanout(9)
	fan.Take(4) // the parent already spent four
	(&Child{Parent: parent, MaxIterations: 3, MaxDepth: 2}).Run(
		domain.WithFanout(context.Background(), fan), domain.SubTask{Label: "x", Task: "t"})

	if seen == nil {
		t.Fatal("no fanout reached the child's tools")
	}
	if seen.Depth() != 1 {
		t.Errorf("child depth = %d, want 1", seen.Depth())
	}
	if seen.Remaining() != 5 {
		t.Errorf("the child sees %d children left, want the turn's remaining 5", seen.Remaining())
	}
}

// A child is bounded by its OWN iteration cap, which is the term that
// multiplies in the worst case the spec states.
func TestAChildIsBoundedByItsOwnIterationCap(t *testing.T) {
	// A provider that always calls a tool never terminates on its own.
	p := &fakeProvider{turns: []fakeTurn{{msg: domain.Message{
		Role:      domain.RoleAssistant,
		ToolCalls: []domain.ToolCall{{ID: "c", Name: "noop", Args: args(`{}`)}},
	}}}}
	var calls int
	p.onComplete = func(domain.Completion) { calls++ }

	(&Child{Parent: parentLoop(p, &fakeTools{}), MaxIterations: 2, MaxDepth: 1}).Run(
		context.Background(), domain.SubTask{Label: "x", Task: "t"})

	if calls != 2 {
		t.Errorf("the child made %d model calls, want its cap of 2", calls)
	}
}

func TestAChildTimesOutRatherThanRunningForever(t *testing.T) {
	p := &fakeProvider{turns: []fakeTurn{{msg: domain.Message{Role: domain.RoleAssistant}}}}
	p.gate = func(domain.Completion) error {
		time.Sleep(200 * time.Millisecond)
		return nil
	}
	c := &Child{Parent: parentLoop(p, &fakeTools{}), MaxIterations: 5, MaxDepth: 1,
		Timeout: 20 * time.Millisecond}

	start := time.Now()
	c.Run(context.Background(), domain.SubTask{Label: "x", Task: "t"})
	if time.Since(start) > time.Second {
		t.Errorf("the child ran for %v past its 20ms timeout", time.Since(start))
	}
}

// Sequential mode's findings reach the child as part of what it reads, after
// the instruction -- a child that read four paragraphs before learning what to
// do with them tends to summarise them instead.
func TestInheritedFindingsFollowTheTaskInThePrompt(t *testing.T) {
	got := prompt(domain.SubTask{Task: "decide", Context: "the earlier answer"})
	if !strings.HasPrefix(got, "decide") {
		t.Errorf("the task is not first: %q", got)
	}
	if !strings.Contains(got, "the earlier answer") {
		t.Errorf("the context was dropped: %q", got)
	}
	if prompt(domain.SubTask{Task: "decide"}) != "decide" {
		t.Error("an empty context still added a heading")
	}
}

type capturingTools struct {
	inner    domain.ToolExecutor
	onInvoke func(context.Context)
}

func (c *capturingTools) Available(ctx context.Context) []domain.ToolSchema {
	return c.inner.Available(ctx)
}

func (c *capturingTools) Invoke(ctx context.Context, call domain.ToolCall) (domain.Result, error) {
	c.onInvoke(ctx)
	return c.inner.Invoke(ctx, call)
}
