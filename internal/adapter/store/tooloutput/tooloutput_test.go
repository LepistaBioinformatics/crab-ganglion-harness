package tooloutput

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

// scoped builds the real shape: a dedicated parent holding the main workspace,
// with a project's beside it. The parent is what the sibling layout derives
// from, so a bare t.TempDir() would put projects next to whatever else is in
// /tmp.
func scoped(t *testing.T) (*Store, string) {
	t.Helper()
	ws := filepath.Join(t.TempDir(), "workspace")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	return New(ws), ws
}

// THE PATH IS THE WHOLE CONTRACT. A command starts in the turn's workspace and
// the sandbox's root ends there, so a path relative to it is one the agent can
// read back as written. An absolute one would name a location outside its reach
// the moment the container's mount or the workspace moves.
func TestThePathIsRelativeToTheTurnsWorkspace(t *testing.T) {
	s, ws := scoped(t)

	rel, err := s.Put(context.Background(), "conv-1", "call-1", "saida")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	if filepath.IsAbs(rel) {
		t.Errorf("the path is absolute (%q); the agent runs with the workspace as its cwd", rel)
	}
	if got, err := os.ReadFile(filepath.Join(ws, rel)); err != nil || string(got) != "saida" {
		t.Errorf("resolving %q against the workspace did not find the output: %v", rel, err)
	}
}

// A project turn's Landlock root is workspace-<id>, not the main workspace.
// Parking under the main one would hand the agent a path it cannot open -- and
// the failure would arrive as an empty file rather than as an error.
func TestAProjectTurnParksUnderItsOwnWorkspace(t *testing.T) {
	s, ws := scoped(t)
	ctx := domain.WithProject(context.Background(), "seedtrial")

	rel, err := s.Put(ctx, "conv-1", "call-1", "saida")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	project := domain.ProjectWorkspace(ws, "seedtrial")
	if _, err := os.Stat(filepath.Join(project, rel)); err != nil {
		t.Errorf("the project's output is not under %s: %v", project, err)
	}
	if _, err := os.Stat(filepath.Join(ws, rel)); !os.IsNotExist(err) {
		t.Error("a project turn wrote into the main workspace, which it cannot read")
	}
}

// A dotfile for the reason exec's .tmp is one: it stays out of the way when the
// agent lists its own workspace and when the proxy reads the member's files.
func TestTheDirectoryStaysOutOfTheWay(t *testing.T) {
	s, _ := scoped(t)

	rel, err := s.Put(context.Background(), "conv-1", "call-1", "saida")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	if !strings.HasPrefix(rel, ".") {
		t.Errorf("the output landed at %q, in plain sight of an ls", rel)
	}
}

// The ingress already refuses a conversation id outside [a-z0-9_-] and a call
// id comes from a provider rather than a member -- but a store that trusted
// either would be one refactor away from writing wherever a response said.
func TestAHostileIdCannotEscapeTheDirectory(t *testing.T) {
	s, ws := scoped(t)

	rel, err := s.Put(context.Background(), "../../etc", "../../passwd", "saida")
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	full := filepath.Join(ws, rel)
	if !strings.HasPrefix(filepath.Clean(full), filepath.Join(ws, DirName)) {
		t.Errorf("the output escaped to %q", full)
	}
}

// Two calls in one conversation are two files. A second result overwriting the
// first would make every pointer but the newest a lie, and nothing would say so
// -- the file would be there, holding somebody else's output.
func TestTwoCallsDoNotOverwriteEachOther(t *testing.T) {
	s, ws := scoped(t)
	ctx := context.Background()

	first, err := s.Put(ctx, "conv-1", "call-1", "primeira")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Put(ctx, "conv-1", "call-2", "segunda")
	if err != nil {
		t.Fatal(err)
	}

	if first == second {
		t.Fatalf("both calls parked at %q", first)
	}
	got, err := os.ReadFile(filepath.Join(ws, first))
	if err != nil || string(got) != "primeira" {
		t.Errorf("the first call's output is now %q: %v", got, err)
	}
}

// UNBOUNDED IS WHAT THE ALTERNATIVE MEANS. Nothing else removes what this
// harness writes, and the proxy's storage limits do not cover it -- they are
// specified, not implemented, and say in their own spec that they are "not a
// retention or clean-up policy". So a member running large commands would
// accumulate parked output forever.
func TestAConversationKeepsOnlyItsMostRecentResults(t *testing.T) {
	s, ws := scoped(t)
	ctx := context.Background()

	for i := 0; i < Retain+10; i++ {
		if _, err := s.Put(ctx, "conv-1", "call-"+strconv.Itoa(i), "saida"); err != nil {
			t.Fatal(err)
		}
	}

	entries, err := os.ReadDir(filepath.Join(ws, DirName, "conv-1"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) > Retain {
		t.Errorf("the directory holds %d files, over the %d it retains", len(entries), Retain)
	}
}

// THE BOUND THAT MAKES DELETING SAFE. A pointer is only ever read out of the
// window, and the window holds at most WindowBudget messages -- so retaining
// more files than that cannot orphan a live pointer. The most recent must be
// the ones kept, and the most recent is the one just written.
func TestPruningNeverTakesTheResultItJustWrote(t *testing.T) {
	s, ws := scoped(t)
	ctx := context.Background()

	var last string
	for i := 0; i < Retain+5; i++ {
		rel, err := s.Put(ctx, "conv-1", "call-"+strconv.Itoa(i), "saida "+strconv.Itoa(i))
		if err != nil {
			t.Fatal(err)
		}
		// Every pointer handed out must resolve at the moment it is handed out.
		if _, err := os.Stat(filepath.Join(ws, rel)); err != nil {
			t.Fatalf("the pointer returned for call %d does not resolve: %v", i, err)
		}
		last = rel
	}

	got, err := os.ReadFile(filepath.Join(ws, last))
	if err != nil || string(got) != "saida "+strconv.Itoa(Retain+4) {
		t.Errorf("the newest result was pruned or rewritten: %q, %v", got, err)
	}
}

// One conversation's housekeeping must not reach another's. They are separate
// directories precisely so a busy conversation cannot evict a quiet one's
// output from under a pointer that is still in its window.
func TestPruningIsPerConversation(t *testing.T) {
	s, ws := scoped(t)
	ctx := context.Background()

	quiet, err := s.Put(ctx, "conv-quiet", "call-1", "importante")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < Retain+10; i++ {
		if _, err := s.Put(ctx, "conv-busy", "call-"+strconv.Itoa(i), "saida"); err != nil {
			t.Fatal(err)
		}
	}

	if got, err := os.ReadFile(filepath.Join(ws, quiet)); err != nil || string(got) != "importante" {
		t.Errorf("a busy conversation evicted a quiet one's output: %v", err)
	}
}

// Modification time is the only ordering a directory offers, and its
// granularity is the filesystem's. On one that stamps whole seconds a burst of
// parks shares a timestamp, and sort.Slice is unstable -- so the file a call
// has just written, whose pointer is certainly live, can land anywhere in the
// ordering and be deleted.
//
// Exercised at the contract rather than through a burst: `keep` survives
// whatever its mtime says, so the file is given the OLDEST timestamp in the
// directory and must still be there. A burst test would depend on how the sort
// happened to break ties and would pass or fail by luck.
func TestPruneNeverDeletesTheFileItWasToldToKeep(t *testing.T) {
	dir := t.TempDir()
	old := time.Unix(1_000_000_000, 0)
	newer := time.Unix(2_000_000_000, 0)

	// The protected file is the oldest thing in the directory, which is the
	// position pruning removes first.
	if err := os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("live"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(filepath.Join(dir, "keep.txt"), old, old); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < Retain+10; i++ {
		name := filepath.Join(dir, "other-"+strconv.Itoa(i)+".txt")
		if err := os.WriteFile(name, []byte("saida"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(name, newer, newer); err != nil {
			t.Fatal(err)
		}
	}

	if err := prune(dir, "keep.txt"); err != nil {
		t.Fatalf("prune: %v", err)
	}

	if got, err := os.ReadFile(filepath.Join(dir, "keep.txt")); err != nil || string(got) != "live" {
		t.Errorf("the protected file was deleted: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) > Retain {
		t.Errorf("the directory holds %d files, over the %d it retains", len(entries), Retain)
	}
}
