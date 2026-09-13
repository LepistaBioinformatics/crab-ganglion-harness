package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

// The whole feature in one shape: what the loop DID is written down, for the
// member, beside what it SAID. Before this the block above an answer showed ten
// narration steps for a turn that had run fourteen tools, and nothing said which.

func eventsOf(log []domain.Message) []domain.TurnEvent {
	var out []domain.TurnEvent
	for _, m := range log {
		out = append(out, m.Events...)
	}
	return out
}

func TestEvents_ACallCarriesItsNameArgumentsAndOutcome(t *testing.T) {
	tr := &fakeTranscript{}
	p := &fakeProvider{turns: []fakeTurn{
		{msg: domain.Message{Role: domain.RoleAssistant, Content: "vou ver.", ToolCalls: []domain.ToolCall{
			{ID: "1", Name: "sh", Args: args(`{"cmd":"ls"}`)},
		}}},
		{msg: domain.Message{Role: domain.RoleAssistant, Content: "pronto"}},
	}}
	l := newLoop(p, tr, &fakeContext{}, &fakeTools{result: domain.Result{Content: "saida"}}, nil)

	if _, err := l.Run(context.Background(), turn(), domain.Sink{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := eventsOf(tr.log)
	if len(got) != 1 {
		t.Fatalf("events = %+v, want one for the one call", got)
	}
	if got[0].Kind != domain.EventTool || got[0].Name != "sh" {
		t.Errorf("event = %+v, want a tool event naming sh", got[0])
	}
	// The arguments are what make a step VERIFIABLE: "ran sh" says nothing a
	// member can check, "ran sh with ls" says what happened.
	if !strings.Contains(got[0].Arguments, `"cmd":"ls"`) {
		t.Errorf("arguments = %q, want the call's own", got[0].Arguments)
	}
	if got[0].Status != domain.EventOK {
		t.Errorf("status = %q, want %q", got[0].Status, domain.EventOK)
	}
}

// A denial is a Result the agent reacts to, not a failure -- and the member is
// owed the difference. "The agent chose not to" and "it broke" are not the same
// story about the same turn.
func TestEvents_ADenialIsNotAFailure(t *testing.T) {
	tr := &fakeTranscript{}
	p := &fakeProvider{turns: []fakeTurn{
		{msg: domain.Message{Role: domain.RoleAssistant, Content: "vou ver.", ToolCalls: []domain.ToolCall{
			{ID: "1", Name: "sh", Args: args(`{}`)},
		}}},
		{msg: domain.Message{Role: domain.RoleAssistant, Content: "pronto"}},
	}}
	tools := &fakeTools{result: domain.Result{Content: "no", Denied: true}}
	l := newLoop(p, tr, &fakeContext{}, tools, nil)

	if _, err := l.Run(context.Background(), turn(), domain.Sink{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := eventsOf(tr.log)
	if len(got) != 1 || got[0].Status != domain.EventDenied {
		t.Fatalf("events = %+v, want one denied call", got)
	}
}

// The path that loses the most if it is not written: a turn that dies inside a
// tool. The narration was durable before the call ran; the event says which call
// it was and that it failed, which is the one thing the narration cannot say.
func TestEvents_AFailedCallIsRecordedBeforeTheTurnGivesUp(t *testing.T) {
	tr := &fakeTranscript{}
	p := &fakeProvider{turns: []fakeTurn{
		{msg: domain.Message{Role: domain.RoleAssistant, Content: "vou ver.", ToolCalls: []domain.ToolCall{
			{ID: "1", Name: "sh", Args: args(`{}`)},
		}}},
	}}
	tools := &fakeTools{err: errors.New("container gone")}
	l := newLoop(p, tr, &fakeContext{}, tools, nil)

	if _, err := l.Run(context.Background(), turn(), domain.Sink{}); err == nil {
		t.Fatal("expected the tool failure to surface")
	}
	got := eventsOf(tr.log)
	if len(got) != 1 || got[0].Status != domain.EventFailed {
		t.Fatalf("events = %+v, want one failed call", got)
	}
	if !strings.Contains(got[0].Detail, "container gone") {
		t.Errorf("detail = %q, want the failure's own words", got[0].Detail)
	}
}

// A tool that fans out reports what the loop cannot see. The loop records one
// event per CALL, so without this a batch of four children is one line saying
// "subagents: ok" -- which is precisely the report this feature answers.
func TestEvents_AToolsOwnEventsFollowTheCall(t *testing.T) {
	tr := &fakeTranscript{}
	p := &fakeProvider{turns: []fakeTurn{
		{msg: domain.Message{Role: domain.RoleAssistant, Content: "vou ver.", ToolCalls: []domain.ToolCall{
			{ID: "1", Name: "subagents", Args: args(`{}`)},
		}}},
		{msg: domain.Message{Role: domain.RoleAssistant, Content: "pronto"}},
	}}
	tools := &fakeTools{result: domain.Result{Content: "saida", Events: []domain.TurnEvent{
		{Kind: domain.EventSubagent, Name: "q1", Status: domain.EventOK},
		{Kind: domain.EventSubagent, Name: "q2", Status: domain.EventFailed, Detail: "timed out"},
	}}}
	l := newLoop(p, tr, &fakeContext{}, tools, nil)

	if _, err := l.Run(context.Background(), turn(), domain.Sink{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := eventsOf(tr.log)
	if len(got) != 3 {
		t.Fatalf("events = %+v, want the call plus its two children", got)
	}
	// ORDER IS THE MEANING. The children come after the call that started them,
	// so the list reads as a tree flattened rather than as four peers.
	if got[0].Name != "subagents" || got[1].Name != "q1" || got[2].Name != "q2" {
		t.Errorf("events out of order: %+v", got)
	}
	if got[0].Status != domain.EventOK {
		t.Errorf("the call itself ended %q; a tool's own events must not overwrite it", got[0].Status)
	}
}

// A write_file call's arguments are the whole file. Uncapped, the transcript
// grows by everything the agent ever wrote -- and the cap is HERE, in the
// harness, because capping downstream would save nothing: the bytes would
// already be on disk.
func TestEventArgs_IsFlattenedAndCapped(t *testing.T) {
	long := `{"path":"a.md","body":"` + strings.Repeat("x", 500) + `"}`
	got := eventArgs([]byte(long))
	if len([]rune(got)) != maxEventArgs+1 { // +1 for the ellipsis
		t.Errorf("length = %d runes, want the cap plus its marker", len([]rune(got)))
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("a truncated value must say so, got %q", got[len(got)-8:])
	}
}

// The cap measures what will be SHOWN, so a pretty-printed argument object is
// flattened first -- otherwise most of the budget is spent on indentation.
func TestEventArgs_CollapsesWhitespace(t *testing.T) {
	got := eventArgs([]byte("{\n  \"url\": \"https://x\"\n}"))
	if got != `{ "url": "https://x" }` {
		t.Errorf("args = %q, want one line", got)
	}
}

// Multi-byte input must not be cut mid-character: invalid UTF-8 on disk is
// something every reader from here to the browser would have to cope with.
func TestEventArgs_CutsOnRunes(t *testing.T) {
	got := eventArgs([]byte(`{"q":"` + strings.Repeat("ç", 400) + `"}`))
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("expected truncation, got %q", got)
	}
	if strings.ContainsRune(got, '�') {
		t.Error("the cut produced invalid UTF-8")
	}
}
