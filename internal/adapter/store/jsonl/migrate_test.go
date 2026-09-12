package jsonl

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

func legacyTree(t *testing.T, projects ...string) string {
	t.Helper()
	ws := filepath.Join(t.TempDir(), "workspace")
	for _, p := range projects {
		dir := filepath.Join(ws, legacyProjectsDirName, p, "sessions")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "conv.jsonl"), []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return ws
}

// A member who already has transcripts must not lose them to a rename.
func TestMigrateMovesEveryProjectBesideTheWorkspace(t *testing.T) {
	ws := legacyTree(t, "seedtrial", "fieldnotes")

	moved, err := MigrateProjects(ws)
	if err != nil {
		t.Fatalf("MigrateProjects: %v", err)
	}
	if moved != 2 {
		t.Fatalf("moved = %d, want 2", moved)
	}
	for _, p := range []string{"seedtrial", "fieldnotes"} {
		want := filepath.Join(domain.ProjectWorkspace(ws, p), "sessions", "conv.jsonl")
		if _, err := os.Stat(want); err != nil {
			t.Errorf("%s did not arrive: %v", p, err)
		}
	}
	// The empty shell is removed, so the next boot reads nothing.
	if _, err := os.Stat(filepath.Join(ws, legacyProjectsDirName)); !os.IsNotExist(err) {
		t.Error("the legacy directory survived an otherwise complete migration")
	}
}

// Idempotent by construction: the second pass has nothing to read.
func TestMigrateRunsTwiceWithoutHarm(t *testing.T) {
	ws := legacyTree(t, "seedtrial")
	if _, err := MigrateProjects(ws); err != nil {
		t.Fatal(err)
	}
	moved, err := MigrateProjects(ws)
	if err != nil || moved != 0 {
		t.Fatalf("second pass: moved=%d err=%v", moved, err)
	}
	if _, err := os.Stat(filepath.Join(domain.ProjectWorkspace(ws, "seedtrial"), "sessions", "conv.jsonl")); err != nil {
		t.Errorf("the second pass disturbed the migrated tree: %v", err)
	}
}

// NFR-1: a workspace that never had the old layout does nothing at all.
func TestMigrateIsANoOpWithoutTheLegacyDirectory(t *testing.T) {
	ws := filepath.Join(t.TempDir(), "workspace")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	moved, err := MigrateProjects(ws)
	if moved != 0 || err != nil {
		t.Fatalf("moved=%d err=%v", moved, err)
	}
}

// An existing destination is NOT overwritten and NOT merged. Both are guesses
// about which copy is current, and the cost of guessing wrong is a member's
// transcripts.
func TestMigrateLeavesAConflictAloneAndSaysSo(t *testing.T) {
	ws := legacyTree(t, "seedtrial")
	dst := domain.ProjectWorkspace(ws, "seedtrial")
	if err := os.MkdirAll(filepath.Join(dst, "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "sessions", "newer.jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	moved, err := MigrateProjects(ws)
	if moved != 0 {
		t.Errorf("moved = %d, want 0", moved)
	}
	if err == nil {
		t.Fatal("a conflict was not reported")
	}
	// Neither copy is touched.
	if _, serr := os.Stat(filepath.Join(dst, "sessions", "newer.jsonl")); serr != nil {
		t.Error("the destination was overwritten")
	}
	if _, serr := os.Stat(filepath.Join(ws, legacyProjectsDirName, "seedtrial", "sessions", "conv.jsonl")); serr != nil {
		t.Error("the source was removed despite the conflict")
	}
}

// A traversal-shaped name in the legacy tree is not a project and must not be
// moved anywhere -- the same rule the store applies on the way in.
func TestMigrateSkipsWhatIsNotAProject(t *testing.T) {
	ws := legacyTree(t)
	for _, name := range []string{"..", "Not-A-Project", ".hidden"} {
		dir := filepath.Join(ws, legacyProjectsDirName, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			continue // ".." is not creatable, which is the point
		}
	}
	moved, _ := MigrateProjects(ws)
	if moved != 0 {
		t.Fatalf("moved = %d, want 0", moved)
	}
}
