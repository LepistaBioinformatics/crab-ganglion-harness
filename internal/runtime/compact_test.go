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
