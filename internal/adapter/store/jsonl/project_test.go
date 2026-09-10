package jsonl

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

func scoped(t *testing.T) (*Store, string) {
	t.Helper()
	ws := t.TempDir()
	s := New(filepath.Join(ws, "sessions"))
	s.Projects = filepath.Join(ws, "projects")
	return s, ws
}

func msg(text string) domain.Message {
	return domain.Message{Role: domain.RoleAssistant, Content: text}
}

// AC-A1, AND THE REGRESSION BAR FOR THE WHOLE FEATURE.
//
// A turn with no project writes exactly where it has always written. Getting
// this wrong orphans every existing transcript at once, and silently: the
// harness would keep working and every conversation would start empty.
func TestATurnWithNoProjectWritesWhereItAlwaysHas(t *testing.T) {
	s, ws := scoped(t)
	ctx := context.Background()

	if err := s.Append(ctx, "conv-1", msg("hello")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(ws, "sessions", "conv-1.jsonl")); err != nil {
		t.Fatalf("the unscoped transcript is not at <workspace>/sessions: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws, "projects")); !os.IsNotExist(err) {
		t.Error("an unscoped turn created the projects tree")
	}
}

// AC-A2.
func TestAProjectTurnWritesUnderItsOwnSubtree(t *testing.T) {
	s, ws := scoped(t)
	ctx := domain.WithProject(context.Background(), "seed-trial")

	if err := s.Append(ctx, "conv-1", msg("hello")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(ws, "projects", "seed-trial", "sessions", "conv-1.jsonl")); err != nil {
		t.Fatalf("the project transcript is not where it should be: %v", err)
	}
	if _, err := os.Stat(filepath.Join(ws, "sessions", "conv-1.jsonl")); !os.IsNotExist(err) {
		t.Error("the project turn also wrote into the main workspace")
	}
}

// AC-A5. The same conversation id in two projects is two conversations. If it
// were not, a member's two projects would share a history and neither would be
// what they asked for.
func TestTheSameConversationIdInTwoProjectsIsTwoConversations(t *testing.T) {
	s, _ := scoped(t)
	a := domain.WithProject(context.Background(), "alpha")
	b := domain.WithProject(context.Background(), "beta")

	if err := s.Append(a, "conv", msg("in alpha")); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(b, "conv", msg("in beta")); err != nil {
		t.Fatal(err)
	}
	got, err := s.Read(a, "conv")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Content != "in alpha" {
		t.Fatalf("alpha read %d messages, first %q", len(got), got[0].Content)
	}
}

// A store must not depend on the ingress having checked. Both layers refuse,
// because one refactor away from the other is the whole distance to writing
// wherever a header says.
func TestTheStoreSanitisesTheProjectItIsGiven(t *testing.T) {
	s, ws := scoped(t)
	ctx := domain.WithProject(context.Background(), "../../etc")

	if err := s.Append(ctx, "conv", msg("x")); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(ws, "projects"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() == ".." || e.Name() == "etc" {
			t.Fatalf("the store wrote to %q", e.Name())
		}
	}
	if _, err := os.Stat("/etc/conv.jsonl"); err == nil {
		t.Fatal("the store escaped the workspace entirely")
	}
}

// A crash does not care which project the turn belonged to. A partial nobody
// folds is an answer the member watched appear and then never sees again.
func TestRecoveryReachesEveryProjectAndNotOnlyTheMainWorkspace(t *testing.T) {
	s, _ := scoped(t)
	main := context.Background()
	proj := domain.WithProject(main, "seed-trial")

	// A conversation in each, both with a live checkpoint.
	for _, c := range []context.Context{main, proj} {
		if err := s.Append(c, "conv", msg("question")); err != nil {
			t.Fatal(err)
		}
		if err := s.Checkpoint(c, "conv", time.Now(), "half an answer"); err != nil {
			t.Fatal(err)
		}
	}

	folded, _, err := s.RecoverPartials(main)
	if err != nil {
		t.Fatal(err)
	}
	if folded != 2 {
		t.Fatalf("folded %d partials, want 2 (one per project)", folded)
	}
	got, _ := s.Read(proj, "conv")
	if len(got) != 2 || got[1].Content != "half an answer" {
		t.Errorf("the project's interrupted answer was not folded in: %+v", got)
	}
}
