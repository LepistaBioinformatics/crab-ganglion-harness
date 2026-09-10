package window

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

func TestSaveAndLoad_RoundTrips(t *testing.T) {
	s := New(t.TempDir())
	ctx := context.Background()
	w := domain.Window{Summary: "resumo", Messages: []domain.Message{{Role: domain.RoleUser, Content: "oi"}}}
	if err := s.Save(ctx, "sk-1", w); err != nil {
		t.Fatalf("Save: %v", err)
	}
	got, err := s.Load(ctx, "sk-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.Summary != "resumo" || len(got.Messages) != 1 {
		t.Errorf("got %+v", got)
	}
}

// The window is derived, so a corrupt one costs the next turn some context and
// nothing else. Failing the turn here would turn a recoverable state into an
// outage.
func TestLoad_CorruptWindowStartsEmptyRatherThanFailing(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	os.WriteFile(filepath.Join(dir, "sk-1.window.json"), []byte("{ not json"), 0o644)

	got, err := s.Load(context.Background(), "sk-1")
	if err != nil {
		t.Fatalf("Load must not fail on a corrupt window: %v", err)
	}
	if len(got.Messages) != 0 {
		t.Errorf("got %+v", got)
	}
}

// Save must be atomic: a crash mid-write must not leave a truncated window.
func TestSave_LeavesNoPartialFileBehind(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	if err := s.Save(context.Background(), "sk-1", domain.Window{Summary: "x"}); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("expected only the committed file, got %d entries (temp file left behind?)", len(entries))
	}
}
