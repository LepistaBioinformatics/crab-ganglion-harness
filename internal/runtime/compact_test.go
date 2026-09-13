package runtime

import (
	"strings"
	"testing"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

func win(roles ...domain.Role) domain.Window {
	w := domain.Window{}
	for _, r := range roles {
		w.Messages = append(w.Messages, domain.Message{Role: r})
	}
	return w
}

func roles(w domain.Window) string {
	out := make([]string, 0, len(w.Messages))
	for _, m := range w.Messages {
		out = append(out, string(m.Role))
	}
	return strings.Join(out, ",")
}

// The failure this prevents is not degraded context, it is a dead
// conversation:
//
//	400: Messages with role 'tool' must be a response to a preceding
//	message with 'tool_calls'
//
// Drop-oldest cut between an assistant carrying tool_calls and the results
// answering it, and every later turn in that conversation failed.
func TestCompact_NeverLeavesAToolWithoutItsCall(t *testing.T) {
	// user, assistant(tool_calls), tool, tool, assistant, user, assistant
	w := win(domain.RoleUser, domain.RoleAssistant, domain.RoleTool, domain.RoleTool,
		domain.RoleAssistant, domain.RoleUser, domain.RoleAssistant)

	// A budget that lands the cut squarely between the call and its results.
	got := compact(w, 5)

	if strings.HasPrefix(roles(got), "tool") {
		t.Fatalf("window starts with an orphaned tool: %s", roles(got))
	}
	if len(got.Messages) > 5 {
		t.Errorf("kept %d messages against a budget of 5", len(got.Messages))
	}
}

// Dropping the orphans must be counted, or the summary understates what is
// missing.
func TestCompact_CountsTheOrphansItDrops(t *testing.T) {
	w := win(domain.RoleUser, domain.RoleAssistant, domain.RoleTool, domain.RoleTool, domain.RoleAssistant)
	got := compact(w, 3)

	if !strings.Contains(got.Summary, "4 earlier") {
		t.Errorf("summary = %q; it must count the orphaned tools it also removed", got.Summary)
	}
}

// A window already saved broken -- by a build that predates this -- must heal
// rather than fail at the provider forever. Nothing else revisits the front of
// a window.
func TestCompact_RepairsAnAlreadyBrokenWindow(t *testing.T) {
	w := win(domain.RoleTool, domain.RoleAssistant, domain.RoleUser)

	got := compact(w, 100) // under budget: no compaction, repair only
	if roles(got) != "assistant,user" {
		t.Errorf("roles = %s, want the orphan gone", roles(got))
	}
}

// A tool later in the window still has its assistant somewhere before it.
// Only the leading ones can be orphaned, and over-trimming would throw away
// context for nothing.
func TestDropOrphanTools_LeavesInteriorToolsAlone(t *testing.T) {
	w := win(domain.RoleUser, domain.RoleAssistant, domain.RoleTool, domain.RoleAssistant)
	if got := roles(dropOrphanTools(w)); got != "user,assistant,tool,assistant" {
		t.Errorf("roles = %s; an interior tool was removed", got)
	}
}

func TestCompact_UnderBudgetKeepsEverything(t *testing.T) {
	w := win(domain.RoleUser, domain.RoleAssistant)
	if got := compact(w, 10); len(got.Messages) != 2 || got.Summary != "" {
		t.Errorf("compacted a window that fits: %+v", got)
	}
}

// The fault this repository met in production: a conversation that could not be
// answered again, ever, because of the order two messages were written in.
//
// The provider's own words were "insufficient tool messages following tool_calls
// message" -- all the results were present, one of them was just on the far side
// of the image the first tool returned.
func TestAMediaFollowUpDoesNotSplitAToolRun(t *testing.T) {
	w := domain.Window{Messages: []domain.Message{
		{Role: domain.RoleUser, Content: "look at both"},
		{Role: domain.RoleAssistant, ToolCalls: []domain.ToolCall{{ID: "a"}, {ID: "b"}}},
		{Role: domain.RoleTool, ToolCallID: "a"},
		{Role: domain.RoleUser, Content: "Here is the media that tool loaded.",
			Attachments: []domain.Attachment{{Kind: domain.AttachmentImage, MIME: "image/png"}}},
		{Role: domain.RoleTool, ToolCallID: "b"},
		{Role: domain.RoleAssistant, Content: "done"},
	}}

	got := repair(w)

	roles := make([]domain.Role, 0, len(got.Messages))
	for _, m := range got.Messages {
		roles = append(roles, m.Role)
	}
	want := []domain.Role{
		domain.RoleUser, domain.RoleAssistant,
		domain.RoleTool, domain.RoleTool,
		domain.RoleUser, domain.RoleAssistant,
	}
	if len(roles) != len(want) {
		t.Fatalf("message count changed: got %v, want %v", roles, want)
	}
	for i := range want {
		if roles[i] != want[i] {
			t.Fatalf("at %d: got %v, want %v", i, roles, want)
		}
	}
	// MOVED, not dropped. The image is what the member or the tool produced and
	// the model is meant to see it; healing the conversation by losing it would
	// trade a dead turn for a quietly wrong answer.
	if len(got.Messages[4].Attachments) != 1 {
		t.Fatalf("the media message lost its attachment: %+v", got.Messages[4])
	}
}

// A batch that died halfway leaves fewer results than calls. The regrouping must
// not treat everything after it as part of the run and reorder the rest of the
// conversation into it.
func TestAnIncompleteToolRunDoesNotSwallowWhatFollows(t *testing.T) {
	w := domain.Window{Messages: []domain.Message{
		{Role: domain.RoleAssistant, ToolCalls: []domain.ToolCall{{ID: "a"}, {ID: "b"}}},
		{Role: domain.RoleTool, ToolCallID: "a"},
		{Role: domain.RoleAssistant, Content: "gave up"},
		{Role: domain.RoleUser, Content: "and then?"},
	}}

	got := repair(w)

	if len(got.Messages) != 4 {
		t.Fatalf("message count changed: %d", len(got.Messages))
	}
	if got.Messages[2].Content != "gave up" || got.Messages[3].Content != "and then?" {
		t.Fatalf("the tail was reordered: %+v", got.Messages)
	}
}

// A window with nothing wrong with it must come back byte for byte. Repair runs
// on every load, so a rewrite that "fixes" a healthy window is a rewrite of every
// conversation in the deployment.
func TestRepairLeavesAWellFormedWindowAlone(t *testing.T) {
	w := domain.Window{Messages: []domain.Message{
		{Role: domain.RoleUser, Content: "hi"},
		{Role: domain.RoleAssistant, ToolCalls: []domain.ToolCall{{ID: "a"}}},
		{Role: domain.RoleTool, ToolCallID: "a"},
		{Role: domain.RoleAssistant, Content: "there"},
	}}
	before := append([]domain.Message(nil), w.Messages...)

	got := repair(w)

	if len(got.Messages) != len(before) {
		t.Fatalf("count changed: %d -> %d", len(before), len(got.Messages))
	}
	for i := range before {
		if got.Messages[i].Role != before[i].Role || got.Messages[i].Content != before[i].Content {
			t.Fatalf("at %d: %+v became %+v", i, before[i], got.Messages[i])
		}
	}
}
