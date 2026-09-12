package runtime

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

// chain is a fixed ModelChain.
type chain []string

func (c chain) Chain(string, domain.ModelKind) []string { return []string(c) }

// scripted answers per MODEL rather than per call, which is what a fallback
// test needs: the assertion is "the second model was asked", not "the provider
// was called twice".
type scripted struct {
	asked []string
	// fail maps a model to the error Complete returns for it.
	fail map[string]error
	// streamFail maps a model to an error raised MID-STREAM, after the
	// deltas below have already been delivered.
	streamFail map[string]error
	deltas     map[string][]string
	content    map[string]string
	// observe sees every completion before it is answered.
	observe func(domain.Completion)
	// toolsOnce makes a model's FIRST completion return these tool calls and
	// every later one answer normally.
	//
	// One call, not a script per call, because what the depth tests need is a
	// turn whose depth is raised BETWEEN two completions -- which is the real
	// shape: set_reasoning_depth is a tool, so the agent asks for depth only
	// after it has seen enough to know the problem is hard.
	toolsOnce map[string][]domain.ToolCall
	served    map[string]int
}

func (s *scripted) Complete(_ context.Context, c domain.Completion) (domain.Stream, error) {
	s.asked = append(s.asked, c.Model)
	if s.observe != nil {
		s.observe(c)
	}
	if err := s.fail[c.Model]; err != nil {
		return nil, err
	}
	if s.served == nil {
		s.served = map[string]int{}
	}
	n := s.served[c.Model]
	s.served[c.Model] = n + 1
	if calls := s.toolsOnce[c.Model]; n == 0 && len(calls) > 0 {
		return &scriptedStream{calls: calls}, nil
	}
	return &scriptedStream{
		deltas:  s.deltas[c.Model],
		err:     s.streamFail[c.Model],
		content: s.content[c.Model],
	}, nil
}

type scriptedStream struct {
	deltas  []string
	i       int
	err     error
	content string
	calls   []domain.ToolCall
}

func (s *scriptedStream) Next(context.Context) (domain.Delta, error) {
	if s.i < len(s.deltas) {
		d := domain.Delta{Content: s.deltas[s.i]}
		s.i++
		return d, nil
	}
	if s.err != nil {
		return domain.Delta{}, s.err
	}
	return domain.Delta{}, io.EOF
}
func (s *scriptedStream) Message() domain.Message {
	return domain.Message{Role: domain.RoleAssistant, Content: s.content, ToolCalls: s.calls}
}
func (s *scriptedStream) Usage() domain.Usage { return domain.Usage{} }
func (s *scriptedStream) Close() error        { return nil }

func fallbackLoop(t *testing.T, p domain.Provider, models domain.ModelChain) *Loop {
	t.Helper()
	return &Loop{
		Provider:   p,
		Models:     models,
		Transcript: &fakeTranscript{},
		Context:    &fakeContext{},
		Tools:      &fakeTools{},
	}
}

// FR-5. The primary is unreachable and nothing has been said yet, so the turn
// moves down the chain and the member gets an answer instead of an error.
func TestAFailedModelFallsThroughToTheNext(t *testing.T) {
	p := &scripted{
		fail:    map[string]error{"a": errors.New("connection refused")},
		content: map[string]string{"b": "answered by b"},
		deltas:  map[string][]string{"b": {"answered by b"}},
	}
	var sink recorder
	out, err := fallbackLoop(t, p, chain{"a", "b"}).Run(context.Background(),
		domain.Turn{SessionID: "s", Input: domain.Message{Role: domain.RoleUser, Content: "hi"}}, sink.sink())
	if err != nil {
		t.Fatalf("the turn should have been answered by the fallback: %v", err)
	}
	if out != "answered by b" {
		t.Fatalf("answer = %q, want the fallback's", out)
	}
	if len(p.asked) != 2 || p.asked[0] != "a" || p.asked[1] != "b" {
		t.Fatalf("models asked = %v, want [a b]", p.asked)
	}
}

// FR-5.1. A member watching a turn stall and then answer is owed the reason,
// and the frame has to name BOTH models or it explains nothing.
func TestTheFallbackHopIsSaidOutLoud(t *testing.T) {
	p := &scripted{
		fail:    map[string]error{"a": errors.New("connection refused")},
		content: map[string]string{"b": "ok"},
		deltas:  map[string][]string{"b": {"ok"}},
	}
	var sink recorder
	if _, err := fallbackLoop(t, p, chain{"a", "b"}).Run(context.Background(),
		domain.Turn{SessionID: "s", Input: domain.Message{Role: domain.RoleUser}}, sink.sink()); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(sink.progress, "\n")
	if !strings.Contains(joined, "a") || !strings.Contains(joined, "b") {
		t.Fatalf("progress %q must name the model that failed and the one being tried", joined)
	}
}

// THE RULE OF THE WHOLE DESIGN.
//
// Once a byte has reached the member, the turn is committed to that model.
// Restarting under another one would splice two voices into one bubble, and the
// member has already read the first half of the first one.
func TestAModelThatHasALREADYSPOKENIsNotAbandoned(t *testing.T) {
	p := &scripted{
		deltas:     map[string][]string{"a": {"I was already saying"}},
		streamFail: map[string]error{"a": errors.New("connection reset mid-answer")},
		content:    map[string]string{"b": "a completely different answer"},
	}
	var sink recorder
	_, err := fallbackLoop(t, p, chain{"a", "b"}).Run(context.Background(),
		domain.Turn{SessionID: "s", Input: domain.Message{Role: domain.RoleUser}}, sink.sink())
	if err == nil {
		t.Fatal("a model that failed after speaking must surface its error, not be replaced")
	}
	if len(p.asked) != 1 {
		t.Fatalf("models asked = %v, want [a] only -- the turn was already committed", p.asked)
	}
	if strings.Contains(strings.Join(sink.content, ""), "completely different") {
		t.Fatal("the second model's answer reached the member after the first had spoken")
	}
}

// A stream that dies before producing anything HAS said nothing, so it is
// abandoned like any other pre-first-byte failure. The distinction being drawn
// is "did the member see something", not "did the socket open".
func TestAStreamThatDiesBeforeSayingAnythingStillFallsThrough(t *testing.T) {
	p := &scripted{
		streamFail: map[string]error{"a": errors.New("reset")},
		content:    map[string]string{"b": "ok"},
		deltas:     map[string][]string{"b": {"ok"}},
	}
	var sink recorder
	if _, err := fallbackLoop(t, p, chain{"a", "b"}).Run(context.Background(),
		domain.Turn{SessionID: "s", Input: domain.Message{Role: domain.RoleUser}}, sink.sink()); err != nil {
		t.Fatalf("expected the fallback to answer: %v", err)
	}
	if len(p.asked) != 2 {
		t.Fatalf("models asked = %v, want both", p.asked)
	}
}

// When the chain runs out the operator needs the reason the LAST attempt
// failed. A wrapper saying "all 3 models failed" would bury it.
func TestExhaustingTheChainReportsTheLastError(t *testing.T) {
	p := &scripted{fail: map[string]error{
		"a": errors.New("first failure"),
		"b": errors.New("the last failure"),
	}}
	var sink recorder
	_, err := fallbackLoop(t, p, chain{"a", "b"}).Run(context.Background(),
		domain.Turn{SessionID: "s", Input: domain.Message{Role: domain.RoleUser}}, sink.sink())
	if err == nil || !strings.Contains(err.Error(), "the last failure") {
		t.Fatalf("err = %v, want the last provider's error", err)
	}
}

// Back-compatibility: no registry wired means the single configured model, and
// the turn's own label is still not trusted.
func TestWithNoChainTheConfiguredModelIsUsed(t *testing.T) {
	p := &scripted{content: map[string]string{"configured": "ok"}, deltas: map[string][]string{"configured": {"ok"}}}
	l := fallbackLoop(t, p, nil)
	l.Model = "configured"
	var sink recorder
	if _, err := l.Run(context.Background(),
		domain.Turn{SessionID: "s", Model: "picoclaw", Input: domain.Message{Role: domain.RoleUser}}, sink.sink()); err != nil {
		t.Fatal(err)
	}
	if len(p.asked) != 1 || p.asked[0] != "configured" {
		t.Fatalf("models asked = %v, want [configured]", p.asked)
	}
}

// An empty chain must not silently answer nothing: it falls back to the single
// configured model, which is what a registry that failed to load leaves behind.
func TestAnEmptyChainFallsBackToTheConfiguredModel(t *testing.T) {
	p := &scripted{content: map[string]string{"configured": "ok"}, deltas: map[string][]string{"configured": {"ok"}}}
	l := fallbackLoop(t, p, chain{})
	l.Model = "configured"
	var sink recorder
	if _, err := l.Run(context.Background(),
		domain.Turn{SessionID: "s", Input: domain.Message{Role: domain.RoleUser}}, sink.sink()); err != nil {
		t.Fatal(err)
	}
	if len(p.asked) != 1 || p.asked[0] != "configured" {
		t.Fatalf("models asked = %v, want [configured]", p.asked)
	}
}

// recorder collects what reached the member.
type recorder struct {
	content  []string
	progress []string
	errs     []string
}

func (r *recorder) sink() domain.Sink {
	return domain.Sink{
		Content:  func(s string) { r.content = append(r.content, s) },
		Progress: func(p domain.Progress) { r.progress = append(r.progress, p.Text) },
		Error:    func(s string) { r.errs = append(r.errs, s) },
	}
}
