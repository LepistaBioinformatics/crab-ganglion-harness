package jsonl

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

func TestCheckpoint_SurvivesAndIsLiveWhileTheTurnIsUnanswered(t *testing.T) {
	s := New(t.TempDir())
	ctx := context.Background()
	asked := time.Now()

	if err := s.Append(ctx, "sk", domain.Message{Role: domain.RoleUser, Content: "oi", CreatedAt: asked}); err != nil {
		t.Fatal(err)
	}
	if err := s.Checkpoint(ctx, "sk", asked, "resposta pela met"); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}

	got, live, err := s.ReadPartial(ctx, "sk")
	if err != nil {
		t.Fatalf("ReadPartial: %v", err)
	}
	if !live {
		t.Fatal("a partial with no answer after it must be live")
	}
	if got.Content != "resposta pela met" {
		t.Errorf("content = %q", got.Content)
	}
}

// The crash window that matters: the real message landed, the sidecar removal
// did not. A reader finding both must show the answer ONCE.
func TestReadPartial_IsStaleOnceAnAnswerExists(t *testing.T) {
	s := New(t.TempDir())
	ctx := context.Background()
	asked := time.Now()

	s.Append(ctx, "sk", domain.Message{Role: domain.RoleUser, Content: "oi", CreatedAt: asked})
	s.Checkpoint(ctx, "sk", asked, "resposta pela met")
	// the turn finished, but ClearPartial never ran
	s.Append(ctx, "sk", domain.Message{
		Role: domain.RoleAssistant, Content: "resposta completa", CreatedAt: asked.Add(time.Second),
	})

	_, live, err := s.ReadPartial(ctx, "sk")
	if err != nil {
		t.Fatalf("ReadPartial: %v", err)
	}
	if live {
		t.Error("a superseded partial was reported live -- the answer would show twice")
	}
}

// An earlier turn's answer must not supersede a LATER turn's partial.
func TestReadPartial_AnOlderAnswerDoesNotSupersedeANewerPartial(t *testing.T) {
	s := New(t.TempDir())
	ctx := context.Background()
	t0 := time.Now()

	s.Append(ctx, "sk", domain.Message{Role: domain.RoleUser, Content: "1", CreatedAt: t0})
	s.Append(ctx, "sk", domain.Message{Role: domain.RoleAssistant, Content: "r1", CreatedAt: t0.Add(time.Second)})
	// second turn, still streaming
	t1 := t0.Add(2 * time.Second)
	s.Append(ctx, "sk", domain.Message{Role: domain.RoleUser, Content: "2", CreatedAt: t1})
	s.Checkpoint(ctx, "sk", t1, "r2 pela met")

	got, live, err := s.ReadPartial(ctx, "sk")
	if err != nil {
		t.Fatalf("ReadPartial: %v", err)
	}
	if !live {
		t.Fatal("the previous turn's answer wrongly superseded this turn's partial")
	}
	if got.Content != "r2 pela met" {
		t.Errorf("content = %q", got.Content)
	}
}

func TestClearPartial_IsIdempotent(t *testing.T) {
	s := New(t.TempDir())
	ctx := context.Background()
	s.Checkpoint(ctx, "sk", time.Now(), "x")
	if err := s.ClearPartial(ctx, "sk"); err != nil {
		t.Fatalf("first clear: %v", err)
	}
	if err := s.ClearPartial(ctx, "sk"); err != nil {
		t.Errorf("clearing an absent sidecar must not error: %v", err)
	}
}

// H-2: the transcript must stay byte-identical in shape. A reader that predates
// checkpoints opens only *.jsonl and must never meet a partial.
func TestCheckpoint_DoesNotTouchTheTranscript(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	ctx := context.Background()
	asked := time.Now()

	s.Append(ctx, "sk", domain.Message{Role: domain.RoleUser, Content: "oi", CreatedAt: asked})
	before, _ := os.ReadFile(filepath.Join(dir, "sk.jsonl"))
	s.Checkpoint(ctx, "sk", asked, "parcial")
	after, _ := os.ReadFile(filepath.Join(dir, "sk.jsonl"))

	if string(before) != string(after) {
		t.Error("checkpointing modified the append-only transcript")
	}
	msgs, _ := s.Read(ctx, "sk")
	if len(msgs) != 1 {
		t.Errorf("the transcript gained %d entries from a checkpoint", len(msgs)-1)
	}
}

// The recovery cycle, end to end: a turn died mid-stream, the process restarts,
// and the interrupted answer becomes an ordinary message.
func TestRecoverPartials_FoldsAnInterruptedAnswer(t *testing.T) {
	s := New(t.TempDir())
	ctx := context.Background()
	asked := time.Now()

	s.Append(ctx, "sk", domain.Message{Role: domain.RoleUser, Content: "explique", CreatedAt: asked})
	s.Checkpoint(ctx, "sk", asked, "comecei a responder e")
	// crash here: no assistant message was ever appended

	folded, dropped, err := s.RecoverPartials(ctx)
	if err != nil {
		t.Fatalf("RecoverPartials: %v", err)
	}
	if folded != 1 || dropped != 0 {
		t.Fatalf("folded=%d dropped=%d, want 1/0", folded, dropped)
	}

	msgs, _ := s.Read(ctx, "sk")
	if len(msgs) != 2 || msgs[1].Role != domain.RoleAssistant {
		t.Fatalf("transcript = %+v", msgs)
	}
	if msgs[1].Content != "comecei a responder e" {
		t.Errorf("recovered content = %q", msgs[1].Content)
	}
	// Folding must be idempotent: the sidecar is gone, so a second start does
	// not append the same answer again.
	if _, _, err := s.RecoverPartials(ctx); err != nil {
		t.Fatal(err)
	}
	if again, _ := s.Read(ctx, "sk"); len(again) != 2 {
		t.Errorf("a second recovery duplicated the answer: %d entries", len(again))
	}
}

// A sidecar left behind after the answer landed is dropped, not folded --
// folding it would show the answer twice.
func TestRecoverPartials_DropsAStaleSidecar(t *testing.T) {
	s := New(t.TempDir())
	ctx := context.Background()
	asked := time.Now()

	s.Append(ctx, "sk", domain.Message{Role: domain.RoleUser, Content: "oi", CreatedAt: asked})
	s.Checkpoint(ctx, "sk", asked, "resposta pela met")
	s.Append(ctx, "sk", domain.Message{
		Role: domain.RoleAssistant, Content: "resposta completa", CreatedAt: asked.Add(time.Second),
	})

	folded, dropped, err := s.RecoverPartials(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if folded != 0 || dropped != 1 {
		t.Errorf("folded=%d dropped=%d, want 0/1", folded, dropped)
	}
	if msgs, _ := s.Read(ctx, "sk"); len(msgs) != 2 {
		t.Errorf("the stale sidecar was folded in: %d entries", len(msgs))
	}
}

// A checkpoint that caught no text is not data.
func TestRecoverPartials_DropsAnEmptyCheckpoint(t *testing.T) {
	s := New(t.TempDir())
	ctx := context.Background()
	s.Checkpoint(ctx, "sk", time.Now(), "   ")

	folded, dropped, err := s.RecoverPartials(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if folded != 0 || dropped != 1 {
		t.Errorf("folded=%d dropped=%d, want 0/1", folded, dropped)
	}
}

func TestRecoverPartials_NoSessionsDirIsNotAnError(t *testing.T) {
	s := New(filepath.Join(t.TempDir(), "nope"))
	if _, _, err := s.RecoverPartials(context.Background()); err != nil {
		t.Errorf("a first-ever start must not fail: %v", err)
	}
}
