package recall

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

type fakeTranscript struct {
	msgs []domain.Message
	err  error
}

func (f *fakeTranscript) Append(context.Context, domain.ConversationID, domain.Message) error {
	return nil
}

func (f *fakeTranscript) Read(context.Context, domain.ConversationID) ([]domain.Message, error) {
	return f.msgs, f.err
}

func said(role domain.Role, content string) domain.Message {
	return domain.Message{Role: role, Content: content}
}

// ask runs the tool inside a turn, which is where a conversation id comes from.
func ask(t *testing.T, tr *fakeTranscript, args string) string {
	t.Helper()
	ctx := domain.WithConversation(context.Background(), "conv-1")
	res, err := New(tr).Invoke(ctx, json.RawMessage(args))
	if err != nil {
		t.Fatalf("Invoke returned an error rather than a Result: %v", err)
	}
	return res.Content
}

// THE POINT OF THE TOOL. Compaction tells the agent that messages are missing;
// without this it has been told about a conversation it cannot read, which is
// worse than not being told because it invites it to claim it remembers.
func TestItFindsAMessageThatLeftTheWindow(t *testing.T) {
	tr := &fakeTranscript{msgs: []domain.Message{
		said(domain.RoleUser, "o relatorio de setembro fechou em 340 toneladas"),
		said(domain.RoleAssistant, "anotado"),
		said(domain.RoleUser, "e agora?"),
	}}

	got := ask(t, tr, `{"pattern":"340 toneladas"}`)

	if !strings.Contains(got, "340 toneladas") {
		t.Errorf("the match is not in the answer:\n%s", got)
	}
	if !strings.Contains(got, "message 1") {
		t.Errorf("the answer does not say how far back the match is:\n%s", got)
	}
}

func TestMatchingIsCaseInsensitive(t *testing.T) {
	tr := &fakeTranscript{msgs: []domain.Message{said(domain.RoleUser, "O Relatorio De Setembro")}}

	if got := ask(t, tr, `{"pattern":"relatorio"}`); !strings.Contains(got, "Relatorio") {
		t.Errorf("a differently-cased pattern found nothing:\n%s", got)
	}
}

// A transcript also holds the loop's own per-iteration event records, which
// carry no content. Matching one would answer a search with an empty quotation.
func TestItSearchesOnlyWhatWasSaid(t *testing.T) {
	tr := &fakeTranscript{msgs: []domain.Message{
		{Role: domain.RoleAssistant, Events: []domain.TurnEvent{{Kind: domain.EventTool, Name: "relatorio"}}},
		{Role: domain.RoleTool, Content: "relatorio", ToolCallID: "1"},
		said(domain.RoleUser, "relatorio de setembro"),
	}}

	got := ask(t, tr, `{"pattern":"relatorio"}`)

	if !strings.Contains(got, "1 match") {
		t.Errorf("an events entry or a tool result was counted as something said:\n%s", got)
	}
}

// The NEWEST matches, not the first. A conversation that says "the report"
// forty times is asking about the most recent one, and a head-biased answer
// would hand back the oldest and stop.
func TestACappedAnswerKeepsTheMostRecentMatches(t *testing.T) {
	var msgs []domain.Message
	for i := 0; i < maxHits+5; i++ {
		msgs = append(msgs, said(domain.RoleUser, "relatorio numero "+strconv.Itoa(i)))
	}
	tr := &fakeTranscript{msgs: msgs}

	got := ask(t, tr, `{"pattern":"relatorio"}`)

	if !strings.Contains(got, "numero "+strconv.Itoa(maxHits+4)) {
		t.Errorf("the newest match is missing:\n%s", got)
	}
	if strings.Contains(got, "numero 0\n") {
		t.Errorf("the oldest match survived the cap:\n%s", got)
	}
	// A capped answer that does not say it was capped is one the agent reads as
	// "this is everything".
	if !strings.Contains(got, "there may be older ones") {
		t.Errorf("the cap is not disclosed:\n%s", got)
	}
}

// A match 3000 characters into an answer is invisible in a head clamp, which
// would report a hit and show nothing resembling it.
func TestASnippetIsCutAroundTheMatch(t *testing.T) {
	long := strings.Repeat("a", 3000) + " AGULHA " + strings.Repeat("b", 3000)
	tr := &fakeTranscript{msgs: []domain.Message{said(domain.RoleAssistant, long)}}

	got := ask(t, tr, `{"pattern":"agulha"}`)

	if !strings.Contains(got, "AGULHA") {
		t.Errorf("the match is not in the snippet:\n%s", got[:min(400, len(got))])
	}
	if len(got) > 2*maxSnippet {
		t.Errorf("the answer is %d bytes; it is a pointer into the conversation, not a copy of it", len(got))
	}
}

func TestItCanSearchOneSpeaker(t *testing.T) {
	tr := &fakeTranscript{msgs: []domain.Message{
		said(domain.RoleUser, "relatorio"),
		said(domain.RoleAssistant, "relatorio"),
	}}

	got := ask(t, tr, `{"pattern":"relatorio","role":"user"}`)

	if !strings.Contains(got, "1 match") {
		t.Errorf("the role filter did not narrow the search:\n%s", got)
	}
}

// Nothing found is an ANSWER, and it says how much was looked at -- otherwise
// the agent cannot tell "not in this conversation" from "the search did not
// run".
func TestNothingFoundSaysHowMuchWasSearched(t *testing.T) {
	tr := &fakeTranscript{msgs: []domain.Message{said(domain.RoleUser, "oi"), said(domain.RoleAssistant, "ola")}}

	got := ask(t, tr, `{"pattern":"relatorio"}`)

	if !strings.Contains(got, "2 earlier messages") {
		t.Errorf("the answer does not say what was searched:\n%s", got)
	}
}

// Every refusal is a Result, never an error: the agent should be able to carry
// on without this rather than lose the turn to it.
func TestEveryRefusalIsAnAnswerAndNotAnError(t *testing.T) {
	tr := &fakeTranscript{err: errors.New("disk is gone")}

	for _, c := range []struct{ name, args string }{
		{"a broken transcript", `{"pattern":"x"}`},
		{"an empty pattern", `{"pattern":"  "}`},
		{"malformed arguments", `{`},
	} {
		ctx := domain.WithConversation(context.Background(), "conv-1")
		res, err := New(tr).Invoke(ctx, json.RawMessage(c.args))
		if err != nil {
			t.Errorf("%s returned an error: %v", c.name, err)
		}
		if res.Content == "" {
			t.Errorf("%s returned nothing for the agent to react to", c.name)
		}
	}
}

// Outside a turn there is no conversation to search, and guessing one would
// search somebody else's.
func TestWithNoConversationItRefuses(t *testing.T) {
	res, err := New(&fakeTranscript{}).Invoke(context.Background(), json.RawMessage(`{"pattern":"x"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !strings.Contains(res.Content, "no conversation") {
		t.Errorf("content = %q, want a refusal naming the reason", res.Content)
	}
}

// AN AGENT TOLD ONLY "search this conversation" would look here for a command
// it ran, find nothing, and conclude the command never happened -- a confident
// negative about its own history, which is worse than having no tool at all.
//
// A transcript holds what was SAID; tool results were never written to one
// because the member never saw them. The behaviour is pinned by
// TestItSearchesOnlyWhatWasSaid; this pins that the agent is TOLD.
func TestTheDescriptionSaysWhatItDoesNotCover(t *testing.T) {
	d := New(&fakeTranscript{}).Schema().Description

	if !strings.Contains(d, "does NOT cover tool output") {
		t.Errorf("the description does not disclaim tool output:\n%s", d)
	}
	if !strings.Contains(d, ".tool-output/") {
		t.Errorf("the description does not say where tool output actually is:\n%s", d)
	}
}
