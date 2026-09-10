package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

func newLoop(p *fakeProvider, tr *fakeTranscript, cs *fakeContext, tl *fakeTools, ap domain.Approver) *Loop {
	l := &Loop{
		Provider: p, Transcript: tr, Context: cs, Tools: tl,
		ApprovalHeartbeat: time.Millisecond,
		Now:               func() time.Time { return time.Unix(0, 0) },
	}
	// Assigned separately and only when non-nil: passing a typed nil here is
	// what produced a segfault the first time this file was written.
	if ap != nil {
		l.Approver = ap
	}
	return l
}

func turn() domain.Turn {
	return domain.Turn{
		SessionID:  "conv-1",
		SessionKey: "sk-1",
		Model:      "deepseek-chat",
		Input:      domain.Message{Content: "oi"},
	}
}

// FR-2: content reaches the sink as deltas, not as one blob at the end. This is
// the capability the harness exists for, so it is the first test.
func TestRun_StreamsContentAsDeltas(t *testing.T) {
	p := &fakeProvider{turns: []fakeTurn{{
		deltas: []string{"Oi", ", ", "tudo bem?"},
		msg:    domain.Message{Role: domain.RoleAssistant, Content: "Oi, tudo bem?"},
		usage:  domain.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	}}}
	c := &collect{}
	l := newLoop(p, &fakeTranscript{}, &fakeContext{}, &fakeTools{}, nil)

	got, err := l.Run(context.Background(), turn(), c.sink())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got != "Oi, tudo bem?" {
		t.Errorf("answer = %q", got)
	}
	if len(c.content) != 3 {
		t.Errorf("expected 3 deltas, got %d (%q) -- a single delta means the loop buffered", len(c.content), c.content)
	}
	if c.joined() != "Oi, tudo bem?" {
		t.Errorf("deltas joined = %q", c.joined())
	}
}

// The member's message must be durable before the model is called: a crash
// mid-turn loses the answer, never the question.
func TestRun_AppendsUserMessageBeforeCallingProvider(t *testing.T) {
	tr := &fakeTranscript{}
	p := &fakeProvider{turns: []fakeTurn{{msg: domain.Message{Role: domain.RoleAssistant, Content: "ok"}}}}
	l := newLoop(p, tr, &fakeContext{}, &fakeTools{}, nil)

	if _, err := l.Run(context.Background(), turn(), domain.Sink{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(tr.log) == 0 || tr.log[0].Role != domain.RoleUser {
		t.Fatalf("first transcript entry is not the user message: %+v", tr.log)
	}
}

// FR-9, the invariant that came from measured evidence: compaction rewrites the
// window and must never shorten the transcript.
func TestRun_CompactionNeverShortensTheTranscript(t *testing.T) {
	tr := &fakeTranscript{}
	cs := &fakeContext{}
	// Two iterations: a tool call, then an answer.
	p := &fakeProvider{turns: []fakeTurn{
		{msg: domain.Message{Role: domain.RoleAssistant, ToolCalls: []domain.ToolCall{{ID: "1", Name: "sh", Args: args(`{}`)}}}},
		{msg: domain.Message{Role: domain.RoleAssistant, Content: "pronto"}},
	}}
	l := newLoop(p, tr, cs, &fakeTools{result: domain.Result{Content: "saida"}}, nil)
	l.WindowBudget = 1 // force compaction on every save

	if _, err := l.Run(context.Background(), turn(), domain.Sink{}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// user + assistant(tool) + tool result + assistant(answer)
	if len(tr.log) != 4 {
		t.Errorf("transcript has %d entries, want 4 -- compaction reached the transcript", len(tr.log))
	}
	last := cs.saved[len(cs.saved)-1]
	if len(last.Messages) > 1 {
		t.Errorf("window kept %d messages against a budget of 1", len(last.Messages))
	}
	if last.Summary == "" {
		t.Error("compaction dropped messages without recording that it did")
	}
}

// FR-7 + DEC-2: a denial is a Result the agent reacts to, not a turn failure.
func TestRun_DeniedActionBecomesAToolResultAndTheTurnContinues(t *testing.T) {
	tr := &fakeTranscript{}
	tl := &fakeTools{result: domain.Result{Content: "should not run"}}
	p := &fakeProvider{turns: []fakeTurn{
		{msg: domain.Message{Role: domain.RoleAssistant, ToolCalls: []domain.ToolCall{{ID: "1", Name: "rm", Args: args(`{}`)}}}},
		{msg: domain.Message{Role: domain.RoleAssistant, Content: "entendi, nao vou fazer isso"}},
	}}
	ap := &fakeApprover{dec: domain.Decision{Allowed: false, Reason: "fora da politica"}}
	l := newLoop(p, tr, &fakeContext{}, tl, ap)

	got, err := l.Run(context.Background(), turn(), domain.Sink{})
	if err != nil {
		t.Fatalf("a denial must not fail the turn: %v", err)
	}
	if got != "entendi, nao vou fazer isso" {
		t.Errorf("turn did not continue after the denial: %q", got)
	}
	if len(tl.invoked) != 0 {
		t.Errorf("the tool ran despite being denied: %+v", tl.invoked)
	}
	var toolMsg *domain.Message
	for i := range tr.log {
		if tr.log[i].Role == domain.RoleTool {
			toolMsg = &tr.log[i]
		}
	}
	if toolMsg == nil {
		t.Fatal("no tool result was recorded for the denied call")
	}
	if !strings.Contains(toolMsg.Content, "fora da politica") {
		t.Errorf("the agent was not told WHY it was denied: %q", toolMsg.Content)
	}
}

// DEC-4: fail closed. An approver that never answers denies, and the turn still
// completes with an explanation instead of hanging.
func TestRun_ApprovalTimeoutDeniesRatherThanHanging(t *testing.T) {
	tl := &fakeTools{}
	p := &fakeProvider{turns: []fakeTurn{
		{msg: domain.Message{Role: domain.RoleAssistant, ToolCalls: []domain.ToolCall{{ID: "1", Name: "sh", Args: args(`{}`)}}}},
		{msg: domain.Message{Role: domain.RoleAssistant, Content: "sem aprovacao"}},
	}}
	ap := &fakeApprover{block: make(chan struct{})} // never answers
	l := newLoop(p, &fakeTranscript{}, &fakeContext{}, tl, ap)
	l.ApprovalTimeout = 20 * time.Millisecond

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := l.Run(context.Background(), turn(), domain.Sink{}); err != nil {
			t.Errorf("Run: %v", err)
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the loop hung waiting for an approver that never answered")
	}
	if len(tl.invoked) != 0 {
		t.Error("a timed-out approval ran the tool -- it must fail CLOSED")
	}
}

// DEC-3: a blocked approval must not produce a silent stream. That silence is
// exactly the failure turn-stream-continuity documents.
func TestRun_EmitsProgressWhileWaitingForApproval(t *testing.T) {
	p := &fakeProvider{turns: []fakeTurn{
		{msg: domain.Message{Role: domain.RoleAssistant, ToolCalls: []domain.ToolCall{{ID: "1", Name: "sh", Args: args(`{}`)}}}},
		{msg: domain.Message{Role: domain.RoleAssistant, Content: "ok"}},
	}}
	block := make(chan struct{})
	ap := &fakeApprover{block: block, dec: domain.Decision{Allowed: true}}
	c := &collect{}
	l := newLoop(p, &fakeTranscript{}, &fakeContext{}, &fakeTools{}, ap)
	l.ApprovalHeartbeat = 5 * time.Millisecond
	l.ApprovalTimeout = time.Second

	go func() { time.Sleep(60 * time.Millisecond); close(block) }()
	if _, err := l.Run(context.Background(), turn(), c.sink()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if n := c.kinds(domain.ProgressPlaceholder); n < 2 {
		t.Errorf("only %d progress frames while waiting ~60ms at a 5ms heartbeat -- the stream went quiet", n)
	}
}

// FR-5: stopping at the cap must be said, and the work already done kept.
func TestRun_IterationCapIsReportedAndPartialWorkSurvives(t *testing.T) {
	p := &fakeProvider{turns: []fakeTurn{{
		msg: domain.Message{Role: domain.RoleAssistant, Content: "pensando", ToolCalls: []domain.ToolCall{{ID: "1", Name: "sh", Args: args(`{}`)}}},
	}}} // always asks for a tool -> never terminates on its own
	c := &collect{}
	l := newLoop(p, &fakeTranscript{}, &fakeContext{}, &fakeTools{result: domain.Result{Content: "x"}}, nil)
	l.MaxIterations = 3

	got, err := l.Run(context.Background(), turn(), c.sink())
	if err != nil {
		t.Fatalf("hitting the cap must not error the turn: %v", err)
	}
	if got != "pensando" {
		t.Errorf("partial work was discarded: %q", got)
	}
	if len(c.errs) == 0 || !strings.Contains(c.errs[0], "iteration cap") {
		t.Errorf("the cap was hit silently: %v", c.errs)
	}
	if p.calls != 3 {
		t.Errorf("provider called %d times, want 3", p.calls)
	}
}

// A provider failure mid-stream must surface as a signal, not as prose in the
// answer -- the proxy's sink has a separate Error channel for exactly this.
func TestRun_ProviderErrorIsSignalledNotSwallowed(t *testing.T) {
	p := &fakeProvider{err: errors.New("502 from upstream")}
	c := &collect{}
	l := newLoop(p, &fakeTranscript{}, &fakeContext{}, &fakeTools{}, nil)

	if _, err := l.Run(context.Background(), turn(), c.sink()); err == nil {
		t.Fatal("expected an error")
	}
	if len(c.errs) == 0 {
		t.Error("the client was never told the turn failed")
	}
}

// Token accounting is the second capability that justified the build, so it is
// asserted rather than assumed.
func TestRun_AccumulatesUsageAcrossIterations(t *testing.T) {
	var got domain.Usage
	tel := &fakeTelemetry{onUsage: func(u domain.Usage) { got = u }}
	p := &fakeProvider{turns: []fakeTurn{
		{msg: domain.Message{Role: domain.RoleAssistant, ToolCalls: []domain.ToolCall{{ID: "1", Name: "sh", Args: args(`{}`)}}},
			usage: domain.Usage{PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12}},
		{msg: domain.Message{Role: domain.RoleAssistant, Content: "fim"},
			usage: domain.Usage{PromptTokens: 20, CompletionTokens: 3, TotalTokens: 23}},
	}}
	l := newLoop(p, &fakeTranscript{}, &fakeContext{}, &fakeTools{result: domain.Result{Content: "x"}}, nil)
	l.Telemetry = tel

	if _, err := l.Run(context.Background(), turn(), domain.Sink{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got.TotalTokens != 35 {
		t.Errorf("total tokens = %d, want 35 (12+23)", got.TotalTokens)
	}
}

type fakeTelemetry struct{ onUsage func(domain.Usage) }

func (f *fakeTelemetry) Span(ctx context.Context, _ string, _ ...domain.Attr) (context.Context, func(error)) {
	return ctx, func(error) {}
}
func (f *fakeTelemetry) Usage(_ context.Context, u domain.Usage, _ ...domain.Attr) {
	if f.onUsage != nil {
		f.onUsage(u)
	}
}
