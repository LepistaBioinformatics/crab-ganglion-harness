package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

func fixedNow() time.Time { return time.Unix(0, 0) }

// fakeToolOutput records what was parked and can refuse to park it.
type fakeToolOutput struct {
	put  map[string]string
	fail error
}

func (f *fakeToolOutput) Put(_ context.Context, id domain.ConversationID, callID, content string) (string, error) {
	if f.fail != nil {
		return "", f.fail
	}
	if f.put == nil {
		f.put = map[string]string{}
	}
	path := ".tool-output/" + string(id) + "/" + callID + ".txt"
	f.put[path] = content
	return path, nil
}

// lines builds an output of n numbered lines, wide enough to cross the
// threshold on its own.
func lines(n int) string {
	out := make([]string, n)
	for i := range out {
		out[i] = "linha " + strings.Repeat("x", 80)
	}
	return strings.Join(out, "\n")
}

// THE POINT OF THE WHOLE FEATURE. A tool result is written to the window and
// nowhere else, so before this the window was its only copy and compaction
// dropping it destroyed the bytes rather than merely shortening the context.
func TestALargeToolResultIsParkedAndPointedAt(t *testing.T) {
	store := &fakeToolOutput{}
	l := &Loop{ToolOutput: store, Now: fixedNow}
	whole := lines(200)

	out := l.offload(context.Background(), turn(), "call-1", whole)

	if out.Offloaded == "" {
		t.Fatalf("nothing was parked; content is %d bytes", len(whole))
	}
	if got := store.put[out.Offloaded]; got != whole {
		t.Errorf("the parked file holds %d bytes, want the original %d", len(got), len(whole))
	}
	if !strings.Contains(out.Content, out.Offloaded) {
		t.Errorf("the message does not name where the output went:\n%s", out.Content)
	}
	if len(out.Content) >= len(whole) {
		t.Errorf("the message is %d bytes, no smaller than the %d it replaced", len(out.Content), len(whole))
	}
}

// Both ends, not a head alone: the useful line of a command's output is as
// often the last one, and a head-only clamp keeps the noise and cuts the
// answer.
func TestAParkedResultKeepsItsFirstAndLastLines(t *testing.T) {
	store := &fakeToolOutput{}
	l := &Loop{ToolOutput: store, Now: fixedNow}
	whole := "primeira\n" + lines(200) + "\nultima"

	out := l.offload(context.Background(), turn(), "call-1", whole)

	if !strings.Contains(out.Content, "primeira") {
		t.Errorf("the first line is gone:\n%s", out.Content)
	}
	if !strings.Contains(out.Content, "ultima") {
		t.Errorf("the last line is gone, which is where an error usually is:\n%s", out.Content)
	}
}

// Below the threshold the pointer and the content are the same order of
// magnitude, so parking would lose the bytes and buy nothing.
func TestASmallToolResultIsLeftWhole(t *testing.T) {
	store := &fakeToolOutput{}
	l := &Loop{ToolOutput: store, Now: fixedNow}

	out := l.offload(context.Background(), turn(), "call-1", "saida curta")

	if out.Content != "saida curta" {
		t.Errorf("content = %q, want it untouched", out.Content)
	}
	if out.Offloaded != "" {
		t.Errorf("a short result was parked at %q", out.Offloaded)
	}
	if len(store.put) != 0 {
		t.Errorf("the store was written to %d times for a short result", len(store.put))
	}
}

// The offload moves bytes; it does not change what the agent is told. A full
// disk has to cost a larger window and nothing else -- a turn that works today
// cannot start failing because this did.
func TestAFailedParkKeepsTheWholeOutputAndTheTurn(t *testing.T) {
	store := &fakeToolOutput{fail: errors.New("no space left on device")}
	l := &Loop{ToolOutput: store, Now: fixedNow}
	whole := lines(200)

	out := l.offload(context.Background(), turn(), "call-1", whole)

	if out.Content != whole {
		t.Errorf("a failed park cost %d bytes of output", len(whole)-len(out.Content))
	}
	if out.Offloaded != "" {
		t.Errorf("a failed park still claimed a path: %q", out.Offloaded)
	}
}

// Nil is the composition every existing test uses, and it must behave exactly
// as it did before this port existed.
func TestWithNoStoreNothingIsParked(t *testing.T) {
	l := &Loop{Now: fixedNow}
	whole := lines(200)

	out := l.offload(context.Background(), turn(), "call-1", whole)

	if out.Content != whole || out.Offloaded != "" {
		t.Errorf("a loop with no ToolOutput store changed the result")
	}
}

// The shape is what the repair passes match on. A parked result that stopped
// looking like a tool result would be dropped as an orphan or split a tool run,
// which is a permanent provider rejection rather than a lost optimisation.
func TestAParkedResultStillAnswersItsCall(t *testing.T) {
	store := &fakeToolOutput{}
	l := &Loop{ToolOutput: store, Now: fixedNow}

	out := l.offload(context.Background(), turn(), "call-1", lines(200))

	if out.Role != domain.RoleTool {
		t.Errorf("role = %q, want %q", out.Role, domain.RoleTool)
	}
	if out.ToolCallID != "call-1" {
		t.Errorf("tool_call_id = %q, want the call it answers", out.ToolCallID)
	}
	w := domain.Window{Messages: []domain.Message{
		{Role: domain.RoleAssistant, ToolCalls: []domain.ToolCall{{ID: "call-1", Name: "sh"}}},
		out,
	}}
	if got := repair(w); len(got.Messages) != 2 {
		t.Errorf("repair left %d messages, want the call and its parked answer: %+v", len(got.Messages), got.Messages)
	}
}

// One enormous line has no useful middle to cut, so keeping a head would keep
// exactly the bytes that made the result too large in the first place.
func TestOneEnormousLineIsReplacedRatherThanClamped(t *testing.T) {
	store := &fakeToolOutput{}
	l := &Loop{ToolOutput: store, Now: fixedNow}
	whole := strings.Repeat("y", 64<<10)

	out := l.offload(context.Background(), turn(), "call-1", whole)

	if len(out.Content) > 200 {
		t.Errorf("the message kept %d bytes of a single %d-byte line", len(out.Content), len(whole))
	}
	if !strings.Contains(out.Content, out.Offloaded) {
		t.Errorf("the message does not name where the output went:\n%s", out.Content)
	}
}
