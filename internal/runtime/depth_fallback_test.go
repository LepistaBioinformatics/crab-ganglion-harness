package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

// depthLoop is a loop whose single tool raises the depth on its first call, over
// a fixed model chain -- the shape every test below needs.
func depthLoop(p domain.Provider, models domain.ModelChain, tc thinkingChain, reason string) *Loop {
	return &Loop{
		Provider:   p,
		Models:     models,
		Thinking:   tc,
		Transcript: &fakeTranscript{},
		Context:    &fakeContext{},
		Tools:      &depthTools{level: "high", reason: reason},
	}
}

// deepenedTurn is the two-iteration shape: the model calls set_reasoning_depth,
// then answers. The depth is therefore raised BETWEEN the two completions, which
// is the whole point -- the agent asks for depth after it has seen enough to
// know the problem is hard.
func deepenedTurn(models ...string) *scripted {
	s := &scripted{
		toolsOnce: map[string][]domain.ToolCall{},
		content:   map[string]string{},
		deltas:    map[string][]string{},
	}
	for _, m := range models {
		s.content[m] = "pronto"
		s.deltas[m] = []string{"pronto"}
	}
	// Only the FIRST model raises the depth; a model reached after it is
	// answering a turn whose depth is already set.
	s.toolsOnce[models[0]] = []domain.ToolCall{
		{ID: "c1", Name: "set_reasoning_depth", Args: args(`{}`)},
	}
	return s
}

// errTestDown is a model that will not answer.
var errTestDown = errors.New("connection refused")

// Case 3 of the design: nothing deeper exists, so the depth is carried in the
// PROMPT. This is the floor the whole feature rests on -- it needs no second
// model, no provider support and no configuration, which is what makes "the
// agent may always ask to think harder" true rather than aspirational.
func TestWithNoDeeperModelTheDepthIsCarriedInThePrompt(t *testing.T) {
	var systems []string
	p := deepenedTurn("shallow")
	p.observe = func(c domain.Completion) { systems = append(systems, c.System) }

	l := depthLoop(p, chain{"shallow"}, thinkingChain{sends: map[string]bool{}}, "two schemas disagree")
	if _, err := l.Run(context.Background(), turn(), (&collect{}).sink()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(systems) < 2 {
		t.Fatalf("want at least two completions, got %d", len(systems))
	}
	// Before the tool ran, nothing was asked for and nothing is added.
	if strings.Contains(systems[0], "deeper reasoning") {
		t.Error("the directive was added before the agent asked for depth")
	}
	last := systems[len(systems)-1]
	if !strings.Contains(last, "deeper reasoning") {
		t.Fatalf("the depth never reached the model:\n%s", last)
	}
	// The agent's own reason travels with it. A model told to think harder
	// without being told about what is being asked to guess.
	if !strings.Contains(last, "two schemas disagree") {
		t.Errorf("the reason was dropped:\n%s", last)
	}
}

// Case 2: a model that accepts a depth field exists, so the turn goes to it.
// This is how providers actually express thinking -- deepseek-chat against
// deepseek-reasoner -- so a registry holding one of each needs nothing else.
func TestDepthRoutesToAModelThatAcceptsIt(t *testing.T) {
	p := deepenedTurn("shallow", "reasoner")
	l := depthLoop(p, chain{"shallow"}, thinkingChain{
		sends: map[string]bool{"reasoner": true},
		deep:  []string{"reasoner"},
	}, "")
	if _, err := l.Run(context.Background(), turn(), (&collect{}).sink()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(p.asked) < 2 {
		t.Fatalf("asked = %v, want at least two completions", p.asked)
	}
	if p.asked[0] != "shallow" {
		t.Errorf("the first completion ran on %q, want the ordinary model", p.asked[0])
	}
	if last := p.asked[len(p.asked)-1]; last != "reasoner" {
		t.Errorf("after the depth was raised the turn ran on %q, want reasoner", last)
	}
}

// Routed, and therefore NOT also told in prose. The field already carries the
// choice; adding the directive as well would change a path that works today for
// nothing, and would spend system-prompt attention twice on one instruction.
func TestARoutedTurnDoesNotAlsoGetTheDirective(t *testing.T) {
	var systems []string
	p := deepenedTurn("shallow", "reasoner")
	p.observe = func(c domain.Completion) { systems = append(systems, c.System) }

	l := depthLoop(p, chain{"shallow"}, thinkingChain{
		sends: map[string]bool{"reasoner": true},
		deep:  []string{"reasoner"},
	}, "")
	if _, err := l.Run(context.Background(), turn(), (&collect{}).sink()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for i, s := range systems {
		if strings.Contains(s, "deeper reasoning") {
			t.Errorf("completion %d carried the directive as well as the field", i)
		}
	}
}

// The ordinary chain stays BEHIND the deep model rather than being replaced: a
// reasoning model that is down must not take the turn with it.
func TestTheOrdinaryChainSurvivesBehindTheDeepModel(t *testing.T) {
	p := deepenedTurn("shallow", "reasoner")
	p.fail = map[string]error{"reasoner": errTestDown}

	l := depthLoop(p, chain{"shallow"}, thinkingChain{
		sends: map[string]bool{"reasoner": true},
		deep:  []string{"reasoner"},
	}, "")
	out, err := l.Run(context.Background(), turn(), (&collect{}).sink())
	if err != nil {
		t.Fatalf("a dead reasoning model failed the whole turn: %v", err)
	}
	if out != "pronto" {
		t.Errorf("answer = %q", out)
	}
	if !contains(p.asked, "reasoner") {
		t.Error("the deep model was never tried")
	}
	if !contains(p.asked, "shallow") {
		t.Error("the turn did not fall back to the ordinary model")
	}
}

// A model that ALREADY accepts the field is left exactly where it is. Depth
// routing exists for the model that cannot express it; reaching past a model
// that can would swap a deployment's chosen model for another one behind its
// back.
func TestAModelThatAcceptsTheFieldIsNotRerouted(t *testing.T) {
	p := deepenedTurn("thinker", "other")
	l := depthLoop(p, chain{"thinker"}, thinkingChain{
		sends: map[string]bool{"thinker": true, "other": true},
		deep:  []string{"other"},
	}, "")
	if _, err := l.Run(context.Background(), turn(), (&collect{}).sink()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, m := range p.asked {
		if m != "thinker" {
			t.Fatalf("the turn moved to %q; asked = %v", m, p.asked)
		}
	}
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}
