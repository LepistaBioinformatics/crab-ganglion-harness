package runtime

import (
	"strings"
	"testing"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

// parked builds a tool result that was offloaded, with text still inline.
func parked(callID, path string) domain.Message {
	return domain.Message{
		Role:       domain.RoleTool,
		Content:    "saida inteira com muitas linhas",
		ToolCallID: callID,
		Offloaded:  path,
	}
}

// calls builds the assistant message a parked result answers, so the window is
// one repair will not touch.
func calls(ids ...string) domain.Message {
	m := domain.Message{Role: domain.RoleAssistant, Content: "vou olhar"}
	for _, id := range ids {
		m.ToolCalls = append(m.ToolCalls, domain.ToolCall{ID: id, Name: "sh"})
	}
	return m
}

// windowOf builds call/result pairs, oldest first.
func windowOf(n int) domain.Window {
	var msgs []domain.Message
	for i := 0; i < n; i++ {
		id := string(rune('a' + i))
		msgs = append(msgs, calls(id), parked(id, ".tool-output/conv-1/"+id+".txt"))
	}
	return domain.Window{Messages: msgs}
}

// THE CHEAP HALF OF THE FEATURE. Recent tool output is context; old tool output
// is a filename. Nothing is summarized and no provider is called -- the bytes
// are already on disk and the message keeps the path to them.
func TestAnOldParkedResultKeepsOnlyItsPointer(t *testing.T) {
	got := elideOldResults(windowOf(elisionKeep + 2))

	oldest := got.Messages[1]
	if strings.Contains(oldest.Content, "saida inteira") {
		t.Errorf("the oldest parked result still carries its text: %q", oldest.Content)
	}
	if !strings.Contains(oldest.Content, oldest.Offloaded) {
		t.Errorf("the elided result does not say where its output is: %q", oldest.Content)
	}
	if oldest.Offloaded == "" {
		t.Error("eliding cleared the pointer, which is the one thing it must keep")
	}
}

// The agent is still working with what it just ran, and one iteration can
// produce several results at once.
func TestTheMostRecentParkedResultsKeepTheirText(t *testing.T) {
	got := elideOldResults(windowOf(elisionKeep + 2))

	for i, seen := len(got.Messages)-1, 0; i >= 0 && seen < elisionKeep; i-- {
		if got.Messages[i].Offloaded == "" {
			continue
		}
		seen++
		if !strings.Contains(got.Messages[i].Content, "saida inteira") {
			t.Errorf("a result inside the keep window lost its text: %q", got.Messages[i].Content)
		}
	}
}

// A result that was never parked has nowhere to point, and its text is the same
// order of magnitude as a pointer would be. Eliding it would lose the bytes and
// buy nothing.
func TestAResultThatWasNeverParkedIsLeftAlone(t *testing.T) {
	w := domain.Window{Messages: []domain.Message{
		calls("a"),
		{Role: domain.RoleTool, Content: "saida curta", ToolCallID: "a"},
	}}
	for i := 0; i < elisionKeep+2; i++ {
		id := string(rune('b' + i))
		w.Messages = append(w.Messages, calls(id), parked(id, ".tool-output/conv-1/"+id+".txt"))
	}

	got := elideOldResults(w)

	if got.Messages[1].Content != "saida curta" {
		t.Errorf("an unparked result was elided to %q, losing it", got.Messages[1].Content)
	}
}

// Compaction runs several times in one turn, so each run sees the output of the
// last. A second pass must change nothing rather than wrap a pointer in a
// pointer.
func TestElidingTwiceChangesNothingTheSecondTime(t *testing.T) {
	once := elideOldResults(windowOf(elisionKeep + 2))
	twice := elideOldResults(once)

	for i := range once.Messages {
		if once.Messages[i].Content != twice.Messages[i].Content {
			t.Errorf("message %d changed on the second pass:\n%q\n%q",
				i, once.Messages[i].Content, twice.Messages[i].Content)
		}
	}
}

// dropOrphanTools returns the CALLER'S OWN SLICE when there is no orphan to
// drop, which is the ordinary case. Writing through it in place would reach
// back into the window the loop is still holding.
func TestElidingDoesNotWriteIntoTheCallersWindow(t *testing.T) {
	original := windowOf(elisionKeep + 2)
	before := original.Messages[1].Content

	elideOldResults(original)

	if original.Messages[1].Content != before {
		t.Errorf("the caller's window was rewritten in place: %q became %q",
			before, original.Messages[1].Content)
	}
}

// A WINDOW UNDER BUDGET IS NOT UNDER PRESSURE. Compaction runs once per
// iteration, so eliding here would take an excerpt the agent is still working
// with and hand back a filename -- costing one of its twelve iterations to
// re-read a file it had a moment ago, to save room nothing is asking for.
func TestAWindowUnderBudgetElidesNothing(t *testing.T) {
	w := windowOf(elisionKeep + 2)

	got, dropped := compact(w, len(w.Messages)+10)

	if dropped != 0 {
		t.Fatalf("compact dropped %d from a window under budget", dropped)
	}
	if !strings.Contains(got.Messages[1].Content, "saida inteira") {
		t.Errorf("an old result was elided with no pressure to do it: %q", got.Messages[1].Content)
	}
}

// And the other half: once compaction is actually dropping, it elides.
func TestAWindowOverBudgetElides(t *testing.T) {
	w := windowOf(elisionKeep + 6)

	got, dropped := compact(w, len(w.Messages)-2)

	if dropped == 0 {
		t.Fatal("nothing was dropped; this test needs a window over budget")
	}
	oldest := -1
	for i := range got.Messages {
		if got.Messages[i].Offloaded != "" {
			oldest = i
			break
		}
	}
	if oldest < 0 {
		t.Fatal("no parked result survived the cut")
	}
	if strings.Contains(got.Messages[oldest].Content, "saida inteira") {
		t.Errorf("the oldest surviving result kept its whole text: %q", got.Messages[oldest].Content)
	}
}
