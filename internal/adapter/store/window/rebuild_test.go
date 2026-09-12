package window

import (
	"context"
	"os"
	"path/filepath"
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
