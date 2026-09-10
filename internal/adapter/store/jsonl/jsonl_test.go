package jsonl

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

func TestAppendAndRead_RoundTrips(t *testing.T) {
	s := New(t.TempDir())
	ctx := context.Background()
	for _, c := range []string{"um", "dois", "tres"} {
		if err := s.Append(ctx, "sk-1", domain.Message{Role: domain.RoleUser, Content: c}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	got, err := s.Read(ctx, "sk-1")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 3 || got[2].Content != "tres" {
		t.Errorf("got %d messages: %+v", len(got), got)
	}
}

// The first turn of every session reads before it writes.
func TestRead_MissingSessionIsEmptyNotAnError(t *testing.T) {
	got, err := New(t.TempDir()).Read(context.Background(), "never-seen")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d messages for an unknown session", len(got))
	}
}

// One bad line must not hide the conversation around it.
func TestRead_CorruptLineIsSkippedNotFatal(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	ctx := context.Background()
	if err := s.Append(ctx, "sk-1", domain.Message{Role: domain.RoleUser, Content: "antes"}); err != nil {
		t.Fatal(err)
	}
	f, _ := os.OpenFile(filepath.Join(dir, "sk-1.jsonl"), os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString("{ this is not json\n")
	f.Close()
	if err := s.Append(ctx, "sk-1", domain.Message{Role: domain.RoleUser, Content: "depois"}); err != nil {
		t.Fatal(err)
	}

	got, err := s.Read(ctx, "sk-1")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("got %d messages, want the 2 good ones: %+v", len(got), got)
	}
}

// A session key is data from outside; a store that can be made to write outside
// its own root is one that eventually will be.
func TestAppend_SessionKeyCannotEscapeTheRoot(t *testing.T) {
	root := t.TempDir()
	s := New(root)
	if err := s.Append(context.Background(), "../../escaped", domain.Message{Content: "x"}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 1 {
		t.Fatalf("expected exactly one file inside the root, got %d", len(entries))
	}
	if strings.Contains(entries[0].Name(), "/") || strings.Contains(entries[0].Name(), "..") {
		t.Errorf("path traversal survived sanitisation: %q", entries[0].Name())
	}
}
