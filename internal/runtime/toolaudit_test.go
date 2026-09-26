package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

// THE CALLER, NOT THE STORE. `toolaudit`'s own tests cover what a record is;
// these cover that the loop still writes one, which is the half that goes
// silently missing -- a store keeps passing its tests long after nothing calls
// it.

type auditRecord struct {
	name, arguments, output, status, detail string
	completed                               bool
}

type fakeAudit struct {
	records map[string]*auditRecord
	order   []string
	failPut error
}

func (f *fakeAudit) Put(_ context.Context, _ domain.ConversationID, auditID, name, arguments string) error {
	if f.failPut != nil {
		return f.failPut
	}
	if f.records == nil {
		f.records = map[string]*auditRecord{}
	}
	f.records[auditID] = &auditRecord{name: name, arguments: arguments}
	f.order = append(f.order, auditID)
	return nil
}

func (f *fakeAudit) Complete(_ context.Context, _ domain.ConversationID, auditID, output, status, detail string) error {
	rec, ok := f.records[auditID]
	if !ok {
		return errors.New("no such record")
	}
	rec.output, rec.status, rec.detail, rec.completed = output, status, detail, true
	return nil
}

func withAudit(l *Loop, a *fakeAudit) *Loop { l.ToolAudit = a; return l }

// The whole point: the event carries 200 runes, the record carries the command.
func TestRun_RecordsTheWholeCommandAndTheWholeOutput(t *testing.T) {
	a := &fakeAudit{}
	whole := `{"command":"` + string(make([]byte, 0)) + `find / -name '*.go' -print0 | xargs -0 grep -l TODO"}`
	p := &fakeProvider{turns: []fakeTurn{
		{msg: domain.Message{Role: domain.RoleAssistant, Content: "vou ver. ",
			ToolCalls: []domain.ToolCall{{ID: "1", Name: "sh", Args: args(whole)}}}},
		{msg: domain.Message{Role: domain.RoleAssistant, Content: "pronto"}},
	}}
	l := withAudit(newLoop(p, &fakeTranscript{}, &fakeContext{}, &fakeTools{result: domain.Result{Content: "a.go\nb.go\n"}}, nil), a)

	if _, err := l.Run(context.Background(), turn(), domain.Sink{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(a.order) != 1 {
		t.Fatalf("recorded %d calls, want 1", len(a.order))
	}
	rec := a.records[a.order[0]]
	if rec.arguments != whole {
		t.Errorf("arguments = %q\nwant the uncapped original %q", rec.arguments, whole)
	}
	if rec.output != "a.go\nb.go\n" {
		t.Errorf("output = %q, want what the tool returned", rec.output)
	}
	if rec.status != domain.EventOK {
		t.Errorf("status = %q", rec.status)
	}
}

// THE POINTER IS ON THE EVENT, and it is how the member's client finds the
// record at all. Without it the sheet has nothing to ask for.
func TestRun_PutsTheRecordsIdOnTheEvent(t *testing.T) {
	a := &fakeAudit{}
	tr := &fakeTranscript{}
	p := &fakeProvider{turns: []fakeTurn{
		{msg: domain.Message{Role: domain.RoleAssistant, Content: "vou ver. ",
			ToolCalls: []domain.ToolCall{{ID: "1", Name: "sh", Args: args(`{}`)}}}},
		{msg: domain.Message{Role: domain.RoleAssistant, Content: "pronto"}},
	}}
	l := withAudit(newLoop(p, tr, &fakeContext{}, &fakeTools{result: domain.Result{Content: "saida"}}, nil), a)

	if _, err := l.Run(context.Background(), turn(), domain.Sink{}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var found string
	for _, m := range tr.log {
		for _, e := range m.Events {
			if e.Kind == domain.EventTool {
				found = e.AuditID
			}
		}
	}
	if found == "" {
		t.Fatal("the tool event carries no AuditID, so nothing can open its record")
	}
	if _, ok := a.records[found]; !ok {
		t.Errorf("the event names record %q, which was never written", found)
	}
}

// AC-1.3.1. A turn that dies inside a tool is the one most worth auditing, so
// the command is written before the call runs and survives the failure.
func TestRun_AFailedCallStillLeavesItsCommand(t *testing.T) {
	a := &fakeAudit{}
	p := &fakeProvider{turns: []fakeTurn{
		{msg: domain.Message{Role: domain.RoleAssistant, Content: "vou ver. ",
			ToolCalls: []domain.ToolCall{{ID: "1", Name: "sh", Args: args(`{"command":"boom"}`)}}}},
	}}
	tl := &fakeTools{err: errors.New("tool exploded")}
	l := withAudit(newLoop(p, &fakeTranscript{}, &fakeContext{}, tl, nil), a)

	if _, err := l.Run(context.Background(), turn(), domain.Sink{}); err == nil {
		t.Fatal("want the turn's error")
	}
	if len(a.order) != 1 {
		t.Fatalf("recorded %d calls, want 1 -- the command must outlive the failure", len(a.order))
	}
	rec := a.records[a.order[0]]
	if rec.arguments != `{"command":"boom"}` {
		t.Errorf("arguments = %q, want what it was running when it died", rec.arguments)
	}
	if rec.status != domain.EventFailed {
		t.Errorf("status = %q, want %q", rec.status, domain.EventFailed)
	}
	// The loop's own wrapping, which names the tool -- the same string it puts
	// on the event's Detail, so the row and the sheet cannot disagree.
	if rec.detail != "tool sh: tool exploded" {
		t.Errorf("detail = %q, want the error as the event carries it", rec.detail)
	}
}

// AC-1.3.2. This moves bytes; it does not change what the agent is told. A turn
// that works today cannot start failing because the disk filled.
func TestRun_AStoreThatRefusesNeverFailsTheTurn(t *testing.T) {
	a := &fakeAudit{failPut: errors.New("no space left on device")}
	tr := &fakeTranscript{}
	p := &fakeProvider{turns: []fakeTurn{
		{msg: domain.Message{Role: domain.RoleAssistant, Content: "vou ver. ",
			ToolCalls: []domain.ToolCall{{ID: "1", Name: "sh", Args: args(`{}`)}}}},
		{msg: domain.Message{Role: domain.RoleAssistant, Content: "pronto"}},
	}}
	l := withAudit(newLoop(p, tr, &fakeContext{}, &fakeTools{result: domain.Result{Content: "saida"}}, nil), a)

	got, err := l.Run(context.Background(), turn(), domain.Sink{})
	if err != nil {
		t.Fatalf("a refused record must not fail the turn: %v", err)
	}
	if got != "vou ver. pronto" {
		t.Errorf("answer = %q, want the turn to have run normally", got)
	}
	// And the event must NOT name a record that was never written: a pointer at
	// nothing is what makes a row open an empty sheet.
	for _, m := range tr.log {
		for _, e := range m.Events {
			if e.Kind == domain.EventTool && e.AuditID != "" {
				t.Errorf("event names record %q, but the write failed", e.AuditID)
			}
		}
	}
}

// A loop with no store behaves exactly as every turn did before this existed.
func TestRun_NoStoreMeansNoPointerAndNoFailure(t *testing.T) {
	tr := &fakeTranscript{}
	p := &fakeProvider{turns: []fakeTurn{
		{msg: domain.Message{Role: domain.RoleAssistant, Content: "vou ver. ",
			ToolCalls: []domain.ToolCall{{ID: "1", Name: "sh", Args: args(`{}`)}}}},
		{msg: domain.Message{Role: domain.RoleAssistant, Content: "pronto"}},
	}}
	l := newLoop(p, tr, &fakeContext{}, &fakeTools{result: domain.Result{Content: "saida"}}, nil)

	if _, err := l.Run(context.Background(), turn(), domain.Sink{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, m := range tr.log {
		for _, e := range m.Events {
			if e.AuditID != "" {
				t.Errorf("no store, so no event may name a record; got %q", e.AuditID)
			}
		}
	}
}

// Two calls in ONE iteration. The minted id carries the call's index precisely
// so these do not collide on a clock that has not ticked between them.
func TestRun_TwoCallsInOneIterationGetTwoRecords(t *testing.T) {
	a := &fakeAudit{}
	p := &fakeProvider{turns: []fakeTurn{
		{msg: domain.Message{Role: domain.RoleAssistant, Content: "duas. ", ToolCalls: []domain.ToolCall{
			{ID: "1", Name: "sh", Args: args(`{"command":"one"}`)},
			{ID: "2", Name: "sh", Args: args(`{"command":"two"}`)},
		}}},
		{msg: domain.Message{Role: domain.RoleAssistant, Content: "pronto"}},
	}}
	l := withAudit(newLoop(p, &fakeTranscript{}, &fakeContext{}, &fakeTools{result: domain.Result{Content: "saida"}}, nil), a)

	if _, err := l.Run(context.Background(), turn(), domain.Sink{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(a.records) != 2 {
		t.Fatalf("got %d records for 2 calls -- the clock does not tick between them, the index does", len(a.records))
	}
}

// A CHILD WRITES NO RECORD, and the reason is the one memoryTranscript already
// gives for the transcript: a child runs under SessionID "subagent", which is
// not a conversation. Left on, every child of every conversation in a container
// would write into one `.tool-audit/subagent/` directory that nothing can ever
// reach -- the route composes its path from the member's own session key -- so
// they would be bytes that only grow.
//
// `SubAgentName`'s own comment says it "never reaches disk". This is what keeps
// that true.
func TestAChildWritesNoAuditRecord(t *testing.T) {
	a := &fakeAudit{}
	// The child's provider: one iteration that runs a tool, then an answer.
	childProvider := &fakeProvider{turns: []fakeTurn{
		{msg: domain.Message{Role: domain.RoleAssistant, Content: "olhando. ",
			ToolCalls: []domain.ToolCall{{ID: "c1", Name: "sh", Args: args(`{"command":"ls"}`)}}}},
		{msg: domain.Message{Role: domain.RoleAssistant, Content: "achei"}},
	}}
	parent := withAudit(
		newLoop(childProvider, &fakeTranscript{}, &fakeContext{},
			&fakeTools{result: domain.Result{Content: "saida"}}, nil),
		a,
	)

	child := &Child{Parent: parent, MaxIterations: 4}
	rep := child.Run(context.Background(), domain.SubTask{Label: "one", Task: "look"})
	if rep.Err != nil {
		t.Fatalf("child: %v", rep.Err)
	}
	if len(a.order) != 0 {
		t.Errorf("the child wrote %d records under a conversation that does not exist", len(a.order))
	}
}
