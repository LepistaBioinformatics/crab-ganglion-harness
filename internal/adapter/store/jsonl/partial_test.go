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
