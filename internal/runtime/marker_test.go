package runtime

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

// refusingTranscript accepts the first n appends and refuses the rest, so a
// test can fail the LAST write of a turn -- the marker -- without failing the
// message that made the turn worth having.
type refusingTranscript struct {
	mu    sync.Mutex
	log   []domain.Message
	after int
}

func (f *refusingTranscript) Append(_ context.Context, _ domain.ConversationID, m domain.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.log) >= f.after {
		return errors.New("transcript is full")
	}
	f.log = append(f.log, m)
	return nil
}

func (f *refusingTranscript) Read(context.Context, domain.ConversationID) ([]domain.Message, error) {
	return nil, nil
}

// markers returns every compaction marker in a transcript.
func markers(log []domain.Message) []domain.TurnEvent {
	var out []domain.TurnEvent
	for _, m := range log {
		for _, e := range m.Events {
			if e.Kind == domain.EventCompact {
				out = append(out, e)
			}
		}
	}
	return out
}

// ONE PER TURN, however many times compaction ran inside it. A budget of one
// compacts on every save, so a turn with tools compacts several times -- and a
// marker per run would put a handful of dividers in the member's transcript for
// one turn's worth of shortening.
func TestATurnThatCompactsSeveralTimesLeavesOneMarker(t *testing.T) {
	tr := &fakeTranscript{}
	p := &fakeProvider{turns: []fakeTurn{
		{msg: domain.Message{Role: domain.RoleAssistant, Content: "um", ToolCalls: []domain.ToolCall{{ID: "1", Name: "sh", Args: args(`{}`)}}}},
		{msg: domain.Message{Role: domain.RoleAssistant, Content: "dois", ToolCalls: []domain.ToolCall{{ID: "2", Name: "sh", Args: args(`{}`)}}}},
		{msg: domain.Message{Role: domain.RoleAssistant, Content: "pronto"}},
	}}
	l := newLoop(p, tr, &fakeContext{}, &fakeTools{result: domain.Result{Content: "saida"}}, nil)
	l.WindowBudget = 1

	if _, err := l.Run(context.Background(), turn(), domain.Sink{}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := markers(tr.log)
	if len(got) != 1 {
		t.Fatalf("the turn left %d markers, want exactly one: %+v", len(got), got)
	}
	if got[0].Count <= 0 {
		t.Errorf("the marker counts %d messages; it is written only when something was dropped", got[0].Count)
	}
}

// A turn that fits its budget compacted nothing, and a divider for nothing is
// worse than no divider: it tells the member their history was shortened when
// it was not.
func TestATurnThatDoesNotCompactLeavesNoMarker(t *testing.T) {
	tr := &fakeTranscript{}
	p := &fakeProvider{turns: []fakeTurn{
		{msg: domain.Message{Role: domain.RoleAssistant, Content: "pronto"}},
	}}
	l := newLoop(p, tr, &fakeContext{}, &fakeTools{}, nil)
	l.WindowBudget = 100

	if _, err := l.Run(context.Background(), turn(), domain.Sink{}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := markers(tr.log); len(got) != 0 {
		t.Errorf("a turn under budget still recorded compaction: %+v", got)
	}
}

// THE INVARIANT THAT KEEPS IT OUT OF THE MODEL'S SIGHT. An events-only entry is
// the one durable shape conversational drops, so the marker rides a rule that
// already exists rather than needing one of its own. A marker with content, or
// with a role of its own, would be seeded into a rebuilt window and sent to the
// provider as something the agent said.
func TestTheMarkerIsTheOneShapeAProviderNeverSees(t *testing.T) {
	tr := &fakeTranscript{}
	p := &fakeProvider{turns: []fakeTurn{
		{msg: domain.Message{Role: domain.RoleAssistant, Content: "um", ToolCalls: []domain.ToolCall{{ID: "1", Name: "sh", Args: args(`{}`)}}}},
		{msg: domain.Message{Role: domain.RoleAssistant, Content: "pronto"}},
	}}
	l := newLoop(p, tr, &fakeContext{}, &fakeTools{result: domain.Result{Content: "saida"}}, nil)
	l.WindowBudget = 1

	if _, err := l.Run(context.Background(), turn(), domain.Sink{}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	for _, m := range tr.log {
		for _, e := range m.Events {
			if e.Kind != domain.EventCompact {
				continue
			}
			if m.Role != domain.RoleAssistant {
				t.Errorf("the marker's role is %q; only an assistant entry is dropped by conversational", m.Role)
			}
			if m.Content != "" {
				t.Errorf("the marker says %q, which a rebuilt window would send to the provider", m.Content)
			}
		}
	}
}

// The count is the TURN'S total, so a member reading it learns how much of the
// conversation stopped being in front of the agent -- not how much one of
// several runs of compaction took.
func TestTheMarkerCountsTheWholeTurn(t *testing.T) {
	tr := &fakeTranscript{}
	p := &fakeProvider{turns: []fakeTurn{
		{msg: domain.Message{Role: domain.RoleAssistant, Content: "um", ToolCalls: []domain.ToolCall{{ID: "1", Name: "sh", Args: args(`{}`)}}}},
		{msg: domain.Message{Role: domain.RoleAssistant, Content: "dois", ToolCalls: []domain.ToolCall{{ID: "2", Name: "sh", Args: args(`{}`)}}}},
		{msg: domain.Message{Role: domain.RoleAssistant, Content: "tres", ToolCalls: []domain.ToolCall{{ID: "3", Name: "sh", Args: args(`{}`)}}}},
		{msg: domain.Message{Role: domain.RoleAssistant, Content: "pronto"}},
	}}
	l := newLoop(p, tr, &fakeContext{}, &fakeTools{result: domain.Result{Content: "saida"}}, nil)
	l.WindowBudget = 1

	if _, err := l.Run(context.Background(), turn(), domain.Sink{}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := markers(tr.log)
	if len(got) != 1 {
		t.Fatalf("want one marker, got %d", len(got))
	}
	// Three tool iterations against a budget of one drop more than any single
	// run of compaction does.
	if got[0].Count < 3 {
		t.Errorf("the marker counts %d; a four-iteration turn at budget 1 dropped more than that", got[0].Count)
	}
	// ONE SOURCE FOR THE NUMBER. The webapp renders Count as the divider and
	// Detail inside it, one click apart -- and the window's Summary holds what
	// the last DROPPING run of compaction wrote, not the turn's total. Reading
	// the note off it would put two different numbers on the same event.
	if !strings.Contains(got[0].Detail, strconv.Itoa(got[0].Count)) {
		t.Errorf("the note says %q but the count is %d", got[0].Detail, got[0].Count)
	}
}

// The marker is a record for the member. A transcript that would not take it
// costs a divider on a screen, and the turn it belongs to has already done its
// work -- failing it would trade an answer the member has for a note about it.
func TestAMarkerThatCannotBeWrittenDoesNotFailTheTurn(t *testing.T) {
	tr := &refusingTranscript{after: 4}
	p := &fakeProvider{turns: []fakeTurn{
		{msg: domain.Message{Role: domain.RoleAssistant, Content: "um", ToolCalls: []domain.ToolCall{{ID: "1", Name: "sh", Args: args(`{}`)}}}},
		{msg: domain.Message{Role: domain.RoleAssistant, Content: "pronto"}},
	}}
	// Built by hand rather than through newLoop: this is the one test that needs
	// a transcript which accepts some writes and refuses others.
	l := &Loop{
		Provider: p, Transcript: tr, Context: &fakeContext{},
		Tools:             &fakeTools{result: domain.Result{Content: "saida"}},
		ApprovalHeartbeat: time.Millisecond,
		Now:               fixedNow,
		WindowBudget:      1,
	}

	got, err := l.Run(context.Background(), turn(), domain.Sink{})
	if err != nil {
		t.Fatalf("a transcript that refused the marker failed the turn: %v", err)
	}
	if got == "" {
		t.Error("the answer was lost with the marker")
	}
}
