package runtime

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

// The approval record is the only row in a transcript whose subject is the
// MEMBER: what they were asked, and what they answered. Until it existed a
// gated call left nothing behind -- the card vanished when the turn resumed or
// when the deadline passed, and the decision lived only in a file no screen
// read.

// approvalRecords returns every approval entry in a transcript, paired with the
// entry that carried it, so a test can assert on the shape as well as the
// event.
func approvalRecords(log []domain.Message) []domain.Message {
	var out []domain.Message
	for _, m := range log {
		for _, e := range m.Events {
			if e.Kind == domain.EventApproval {
				out = append(out, m)
				break
			}
		}
	}
	return out
}

// oneToolTurn is a provider that asks for one gated call and then answers.
func oneToolTurn(tool string) *fakeProvider {
	return &fakeProvider{turns: []fakeTurn{
		{msg: domain.Message{
			Role:      domain.RoleAssistant,
			Content:   "vou agendar isso",
			ToolCalls: []domain.ToolCall{{ID: "1", Name: tool, Args: args(`{"message":"regar as plantas"}`)}},
		}},
		{msg: domain.Message{Role: domain.RoleAssistant, Content: "pronto"}},
	}}
}

func TestAnAllowedCallIsRecordedWithTheToolAndItsArguments(t *testing.T) {
	tr := &fakeTranscript{}
	ap := &fakeApprover{dec: domain.Decision{Allowed: true, Gate: domain.EventOK, By: "acc-1"}}
	l := newLoop(oneToolTurn("schedule_create"), tr, &fakeContext{},
		&fakeTools{result: domain.Result{Content: "feito"}}, ap)

	if _, err := l.Run(context.Background(), turn(), domain.Sink{}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := approvalRecords(tr.log)
	if len(got) != 1 {
		t.Fatalf("got %d approval records, want 1: %+v", len(got), tr.log)
	}
	// NO CONTENT. That is what keeps the record out of a rebuilt window --
	// window.conversational drops an events-only entry whole -- so the model is
	// never shown a sentence it did not say.
	if got[0].Content != "" {
		t.Errorf("the record carries content, so a rebuilt window would replay it: %q", got[0].Content)
	}
	e := got[0].Events[0]
	if e.Status != domain.EventOK {
		t.Errorf("status = %q, want %q", e.Status, domain.EventOK)
	}
	if e.Name != "schedule_create" {
		t.Errorf("name = %q, want the tool that was gated", e.Name)
	}
	// The member is reading what they allowed, so the arguments have to be on
	// the row -- capped like every other event's, not absent.
	if e.Arguments == "" {
		t.Error("the record names no arguments, so it does not say what was allowed")
	}
	if e.Detail != "" {
		t.Errorf("an allowed call carries a detail: %q", e.Detail)
	}
}

func TestARefusalRecordsTheMembersOwnReason(t *testing.T) {
	tr := &fakeTranscript{}
	ap := &fakeApprover{dec: domain.Decision{
		Allowed: false, Gate: domain.EventDenied, Reason: "nao quero isso agendado",
	}}
	l := newLoop(oneToolTurn("schedule_create"), tr, &fakeContext{}, &fakeTools{}, ap)

	if _, err := l.Run(context.Background(), turn(), domain.Sink{}); err != nil {
		t.Fatalf("a denial must not fail the turn: %v", err)
	}

	got := approvalRecords(tr.log)
	if len(got) != 1 {
		t.Fatalf("got %d approval records, want 1: %+v", len(got), tr.log)
	}
	e := got[0].Events[0]
	if e.Status != domain.EventDenied {
		t.Errorf("status = %q, want %q", e.Status, domain.EventDenied)
	}
	if e.Detail != "nao quero isso agendado" {
		t.Errorf("detail = %q, want the member's own words", e.Detail)
	}
}

// THE HALF NOTHING RECORDED BEFORE. A member who steps away for five minutes
// used to come back to a conversation where the card had vanished and nothing
// said why. The proxy cannot write this one: it drops the pending request on
// the harness's deadline without recording a line.
func TestADeadlineThatPassesIsRecordedAsExpiredRatherThanRefused(t *testing.T) {
	tr := &fakeTranscript{}
	ap := &fakeApprover{block: make(chan struct{})} // never answers
	l := newLoop(oneToolTurn("schedule_create"), tr, &fakeContext{}, &fakeTools{}, ap)
	l.ApprovalTimeout = 20 * time.Millisecond

	if _, err := l.Run(context.Background(), turn(), domain.Sink{}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := approvalRecords(tr.log)
	if len(got) != 1 {
		t.Fatalf("got %d approval records, want 1: %+v", len(got), tr.log)
	}
	e := got[0].Events[0]
	// Not "denied": nobody said no. The call was refused all the same, and the
	// member reads which of the two happened from this field.
	if e.Status != domain.EventExpired {
		t.Errorf("status = %q, want %q", e.Status, domain.EventExpired)
	}
	// The harness's own sentence about the deadline is written for the MODEL,
	// in English. This harness has no locale, so a detail here would be the one
	// thing on the row the member's client could not translate.
	if e.Detail != "" {
		t.Errorf("detail = %q, want nothing: that reason is the model's", e.Detail)
	}
}

// A PRODUCT WITH AN EMPTY GATE LIST WRITES NONE OF THESE. Every tool call goes
// through the approval path -- that is how it stays exercised -- and all of
// them come back allowed. Recording those would put a divider under every tool
// call in the product.
func TestAnUngatedCallLeavesNoRecord(t *testing.T) {
	tr := &fakeTranscript{}
	// What the real adapter returns when the tool is not in GANGLION_GATED_TOOLS,
	// and what the no-op approver returns when no endpoint is configured: allowed,
	// with no gate.
	ap := &fakeApprover{dec: domain.Decision{Allowed: true, Reason: "not gated"}}
	l := newLoop(oneToolTurn("sh"), tr, &fakeContext{},
		&fakeTools{result: domain.Result{Content: "saida"}}, ap)

	if _, err := l.Run(context.Background(), turn(), domain.Sink{}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := approvalRecords(tr.log); len(got) != 0 {
		t.Errorf("an ungated call left %d approval records: %+v", len(got), got)
	}
}

// THE POSITION IS THE REQUIREMENT, not a detail of it. The client merges an
// iteration's events-only entry into the narration before it, so one iteration
// is one step on screen. An approval entry wedged between the two breaks that
// merge: the iteration renders as a step, a divider, and a second step whose
// narration is gone.
func TestTheRecordFollowsTheIterationsEventsRatherThanSplittingThem(t *testing.T) {
	tr := &fakeTranscript{}
	ap := &fakeApprover{dec: domain.Decision{Allowed: true, Gate: domain.EventOK}}
	l := newLoop(oneToolTurn("schedule_create"), tr, &fakeContext{},
		&fakeTools{result: domain.Result{Content: "feito"}}, ap)

	if _, err := l.Run(context.Background(), turn(), domain.Sink{}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	narration, events, approval := -1, -1, -1
	for i, m := range tr.log {
		switch {
		case len(m.ToolCalls) > 0:
			narration = i
		case len(m.Events) > 0 && m.Events[0].Kind == domain.EventTool:
			events = i
		case len(m.Events) > 0 && m.Events[0].Kind == domain.EventApproval:
			approval = i
		}
	}
	if narration < 0 || events < 0 || approval < 0 {
		t.Fatalf("the turn did not write all three entries: %+v", tr.log)
	}
	if !(narration < events && events < approval) {
		t.Errorf("order was narration=%d events=%d approval=%d, want the approval last",
			narration, events, approval)
	}
}

// Losing the record must not lose the turn. It is commentary on work that has
// already happened -- the tool ran, or it did not, minutes ago.
func TestATranscriptThatRefusesTheRecordDoesNotFailTheTurn(t *testing.T) {
	// Refuses the record and nothing else, so what the turn loses is exactly
	// the commentary. Built by hand, like the marker test that needs its own
	// transcript: newLoop takes the plain fake.
	tr := &approvalHostileTranscript{}
	var seen collect
	l := &Loop{
		Provider: oneToolTurn("schedule_create"), Transcript: tr, Context: &fakeContext{},
		Tools:             &fakeTools{result: domain.Result{Content: "feito"}},
		Approver:          &fakeApprover{dec: domain.Decision{Allowed: true, Gate: domain.EventOK}},
		ApprovalHeartbeat: time.Millisecond,
		Now:               func() time.Time { return time.Unix(0, 0) },
	}

	got, err := l.Run(context.Background(), turn(), seen.sink())
	if err != nil {
		t.Fatalf("a refused record failed the turn: %v", err)
	}
	if got != "vou agendar issopronto" {
		t.Errorf("the turn lost its answer: %q", got)
	}
	// Swallowed, but not silently: a record that was not written is said out
	// loud, because the member would otherwise read its absence as "nobody
	// asked me".
	var said bool
	for _, pr := range seen.progress {
		if strings.Contains(pr.Text, "could not record this approval") {
			said = true
		}
	}
	if !said {
		t.Errorf("the lost record was never mentioned: %+v", seen.progress)
	}
}

// approvalHostileTranscript takes everything except an approval record.
type approvalHostileTranscript struct {
	mu  sync.Mutex
	log []domain.Message
}

func (f *approvalHostileTranscript) Append(
	_ context.Context, _ domain.ConversationID, m domain.Message,
) error {
	for _, e := range m.Events {
		if e.Kind == domain.EventApproval {
			return errors.New("transcript is full")
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log = append(f.log, m)
	return nil
}

func (f *approvalHostileTranscript) Read(
	context.Context, domain.ConversationID,
) ([]domain.Message, error) {
	return nil, nil
}

// THE TURN THAT DIED INSIDE THE THING IT WAS ALLOWED TO DO. The record is
// written on the early returns as well as the normal path, for the reason the
// events entry is: a member who allowed a shell command that then blew up is
// owed the part of the story where they said yes.
func TestAnApprovedCallThatFailsStillRecordsTheApproval(t *testing.T) {
	tr := &fakeTranscript{}
	ap := &fakeApprover{dec: domain.Decision{Allowed: true, Gate: domain.EventOK}}
	l := newLoop(oneToolTurn("schedule_create"), tr, &fakeContext{},
		&fakeTools{err: errors.New("the tool exploded")}, ap)

	if _, err := l.Run(context.Background(), turn(), domain.Sink{}); err == nil {
		t.Fatal("a tool that failed did not fail the turn")
	}

	got := approvalRecords(tr.log)
	if len(got) != 1 {
		t.Fatalf("got %d approval records, want the one the member answered: %+v", len(got), tr.log)
	}
	if got[0].Events[0].Status != domain.EventOK {
		t.Errorf("status = %q, want the allow that happened before the tool failed",
			got[0].Events[0].Status)
	}
}

// PRESSING STOP IS NOT AN APPROVAL EVENT. The member ended the turn; nobody
// failed to decide anything and nothing broke. A row reading "permission could
// not be checked" would be a line of blame for their own click.
func TestATurnTheMemberStoppedRecordsNoApproval(t *testing.T) {
	tr := &fakeTranscript{}
	ap := &waitingApprover{asked: make(chan struct{})}
	l := newLoop(oneToolTurn("schedule_create"), tr, &fakeContext{}, &fakeTools{}, ap)
	// Long enough that the deadline cannot be what ends the wait.
	l.ApprovalTimeout = 10 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		// The approver has to be waiting before the stop, or the turn ends
		// before anybody was asked and the test proves nothing.
		<-ap.asked
		cancel()
	}()
	_, _ = l.Run(ctx, turn(), domain.Sink{})

	if got := approvalRecords(tr.log); len(got) != 0 {
		t.Errorf("a stopped turn left %d approval records: %+v", len(got), got)
	}
}

// waitingApprover announces that it was asked and then waits for the context,
// which is what a member staring at the card does. The announcement is a
// channel rather than a counter so the test can wait for it without racing the
// goroutine the loop runs the request on.
type waitingApprover struct {
	asked chan struct{}
	once  sync.Once
}

func (a *waitingApprover) Request(ctx context.Context, _ domain.ActionRequest) (domain.Decision, error) {
	a.once.Do(func() { close(a.asked) })
	<-ctx.Done()
	return domain.Decision{}, ctx.Err()
}
