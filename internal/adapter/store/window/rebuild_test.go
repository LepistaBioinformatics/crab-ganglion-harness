package window

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

// fakeTranscript is the durable record a window is rebuilt from.
type fakeTranscript struct {
	msgs []domain.Message
	err  error
	read int
}

func (f *fakeTranscript) Append(context.Context, domain.ConversationID, domain.Message) error {
	return nil
}

func (f *fakeTranscript) Read(context.Context, domain.ConversationID) ([]domain.Message, error) {
	f.read++
	return f.msgs, f.err
}

func says(n int) []domain.Message {
	out := make([]domain.Message, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, domain.Message{Role: domain.RoleUser, Content: string(rune('a' + i%26))})
	}
	return out
}

func store(t *testing.T, tr domain.TranscriptStore, budget int) *Store {
	t.Helper()
	ws := filepath.Join(t.TempDir(), "workspace")
	if err := os.MkdirAll(filepath.Join(ws, "windows"), 0o755); err != nil {
		t.Fatal(err)
	}
	s := New(filepath.Join(ws, "windows"))
	s.Workspace = ws
	s.Transcript = tr
	s.SeedBudget = budget
	return s
}

// THE MIGRATION CASE, and the reason this exists. Every conversation moved from
// picoclaw arrives with a full transcript and no window. Without rebuilding, the
// member sees their history on screen and the agent answers as though the
// conversation had just begun.
func TestAMissingWindowIsRebuiltFromTheTranscript(t *testing.T) {
	tr := &fakeTranscript{msgs: says(3)}
	w, err := store(t, tr, 40).Load(context.Background(), "conv")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(w.Messages) != 3 {
		t.Fatalf("rebuilt window has %d messages, want 3", len(w.Messages))
	}
}

// Only the tail. The whole transcript would hand the first completion a context
// the loop's compaction has not run on yet, and a migrated conversation can be
// thousands of messages long.
func TestARebuiltWindowIsBounded(t *testing.T) {
	tr := &fakeTranscript{msgs: says(500)}
	w, err := store(t, tr, 40).Load(context.Background(), "conv")
	if err != nil {
		t.Fatal(err)
	}
	if len(w.Messages) != 40 {
		t.Fatalf("rebuilt window has %d messages, want the 40 budgeted", len(w.Messages))
	}
	// The TAIL, not the head: the recent turns are the context.
	if w.Messages[len(w.Messages)-1].Content != tr.msgs[len(tr.msgs)-1].Content {
		t.Error("the rebuild kept the start of the conversation instead of the end")
	}
}

// A window that EXISTS is read, not rebuilt. The transcript is the fallback, not
// a second source of truth -- a window compacted down to ten messages must not
// grow back to forty on every load.
func TestAnExistingWindowIsNotRebuilt(t *testing.T) {
	tr := &fakeTranscript{msgs: says(100)}
	s := store(t, tr, 40)
	ctx := context.Background()
	if err := s.Save(ctx, "conv", domain.Window{Messages: says(2)}); err != nil {
		t.Fatal(err)
	}
	w, err := s.Load(ctx, "conv")
	if err != nil {
		t.Fatal(err)
	}
	if len(w.Messages) != 2 {
		t.Fatalf("window has %d messages, want the 2 that were saved", len(w.Messages))
	}
	if tr.read != 0 {
		t.Error("the transcript was read for a window that exists")
	}
}

// A corrupt window rebuilds too: it is the same case as a missing one -- the
// durable record survived and the derived one did not.
func TestACorruptWindowIsRebuilt(t *testing.T) {
	tr := &fakeTranscript{msgs: says(3)}
	s := store(t, tr, 40)
	if err := os.WriteFile(s.path(context.Background(), "conv"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	w, err := s.Load(context.Background(), "conv")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(w.Messages) != 3 {
		t.Fatalf("a corrupt window was not rebuilt: %d messages", len(w.Messages))
	}
}

// A transcript that cannot be read yields an EMPTY window, not an error. Failing
// the turn would take an agent off the air over a derived file, and empty is
// what this did before rebuilding existed.
func TestAFailedRebuildDegradesToEmpty(t *testing.T) {
	tr := &fakeTranscript{err: os.ErrPermission}
	w, err := store(t, tr, 40).Load(context.Background(), "conv")
	if err != nil {
		t.Fatalf("a failed rebuild failed the turn: %v", err)
	}
	if len(w.Messages) != 0 {
		t.Fatalf("want an empty window, got %d messages", len(w.Messages))
	}
}

// No transcript wired is the old behaviour exactly, which is what every test
// that does not care about rebuilding gets.
func TestWithNoTranscriptAMissIsStillEmpty(t *testing.T) {
	w, err := store(t, nil, 40).Load(context.Background(), "conv")
	if err != nil || len(w.Messages) != 0 {
		t.Fatalf("w=%+v err=%v", w, err)
	}
}

// THE TOOL PLUMBING IS NOT REBUILT, and this is the failure that makes it worth
// a test rather than a comment.
//
// The served transcript now records an iteration that called a tool as its own
// message, carrying the tool_calls crab-shell-proxy reads to render it as a
// step. The RESULTS answering them were never written there -- the member never
// saw one. Copied into a window as they are, they are a call with no reply, and
// the provider rejects the whole request ("insufficient tool messages following
// tool_calls message") on that turn and on every later turn of the conversation,
// because the broken window is what gets saved.
//
// A picoclaw transcript brings the other half: it logs `tool` entries inline,
// and the seed takes a tail, so the cut lands wherever it lands.
func TestARebuiltWindowCarriesNoToolCallsAndNoToolResults(t *testing.T) {
	tr := &fakeTranscript{msgs: []domain.Message{
		{Role: domain.RoleUser, Content: "e ai"},
		{Role: domain.RoleAssistant, Content: "vou olhar", ToolCalls: []domain.ToolCall{{ID: "1", Name: "sh"}}},
		{Role: domain.RoleTool, Content: "saida", ToolCallID: "1"},
		{Role: domain.RoleAssistant, Content: "pronto"},
	}}
	w, err := store(t, tr, 40).Load(context.Background(), "conv")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(w.Messages) != 3 {
		t.Fatalf("rebuilt window has %d messages, want the three that were SAID: %+v", len(w.Messages), w.Messages)
	}
	for _, m := range w.Messages {
		if len(m.ToolCalls) > 0 || m.ToolCallID != "" || m.Role == domain.RoleTool {
			t.Errorf("tool plumbing survived the rebuild: %+v", m)
		}
	}
	// The narration text is what was worth keeping.
	if w.Messages[1].Content != "vou olhar" {
		t.Errorf("the narration was lost with its call: %+v", w.Messages[1])
	}
}

// A rebuilt window used to begin mid-conversation with nothing marking the cut,
// so the agent read a seed as though it were the whole exchange. That is the
// same silence compaction is being taught to break, arriving from the other
// direction: the window is short for a reason and the reason has to be in it.
func TestARebuiltWindowSaysWhatItLeftBehind(t *testing.T) {
	tr := &fakeTranscript{msgs: says(10)}

	w, err := store(t, tr, 4).Load(context.Background(), "conv")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if w.Summary == "" {
		t.Fatal("a rebuilt window that left six messages behind says nothing about them")
	}
	if !strings.Contains(w.Summary, "6") {
		t.Errorf("the summary says %q; it left 6 messages behind", w.Summary)
	}
}

// Counted from the rebuild, never read off a compaction marker. A marker
// records what the LIVE window dropped, against a history this rebuild is not
// reconstructing -- carrying its number over would state a count that was true
// of the window this one replaces.
func TestARebuildIgnoresACompactionMarkersOwnCount(t *testing.T) {
	msgs := append([]domain.Message{{
		Role:   domain.RoleAssistant,
		Events: []domain.TurnEvent{{Kind: domain.EventCompact, Count: 999}},
	}}, says(3)...)
	tr := &fakeTranscript{msgs: msgs}

	w, err := store(t, tr, 100).Load(context.Background(), "conv")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if w.Summary != "" {
		t.Errorf("nothing was left behind, but the window says %q", w.Summary)
	}
	if len(w.Messages) != 3 {
		t.Errorf("seeded %d messages, want the three that were said -- the marker is not one", len(w.Messages))
	}
}
