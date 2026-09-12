package skills

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

func projectWorkspace(t *testing.T, project string, files map[string]string) string {
	t.Helper()
	ws := t.TempDir()
	dir := domain.ProjectWorkspace(ws, project)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		// MemoryFileName is a relative PATH (memory/MEMORY.md), not a name, so
		// the parent has to exist. That is the whole point of the move: picoclaw
		// puts a workspace's memory in memory/ and so does this now.
		full := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return ws
}

func promptFor(t *testing.T, ws, project string) string {
	t.Helper()
	p := &Prompt{Persona: "You are a crab.", Loader: Loader{Workspace: ws}}
	return p.System(domain.WithProject(context.Background(), project))
}

// AC-A4. A project turn reads the project's instructions and its memory; a turn
// in the same workspace with no project reads neither.
func TestAProjectTurnCarriesItsInstructionsAndItsMemory(t *testing.T) {
	ws := projectWorkspace(t, "seed-trial", map[string]string{
		ProjectFileName: "Only discuss the 2026 seed trial.",
		MemoryFileName:  "Plot 7 was flooded in March.",
	})

	got := promptFor(t, ws, "seed-trial")
	for _, want := range []string{"You are a crab.", "Only discuss the 2026 seed trial.", "Plot 7 was flooded in March."} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q from:\n%s", want, got)
		}
	}

	unscoped := promptFor(t, ws, "")
	if strings.Contains(unscoped, "seed trial") || strings.Contains(unscoped, "Plot 7") {
		t.Errorf("a turn with no project read a project's files:\n%s", unscoped)
	}
}

// The persona stays FIRST. The rule on the type says a skill never displaces
// identity; a project's instructions are a member's writing and must not either.
func TestThePersonaStillComesFirst(t *testing.T) {
	ws := projectWorkspace(t, "p", map[string]string{ProjectFileName: "PROJECT INSTRUCTIONS"})
	got := promptFor(t, ws, "p")
	if strings.Index(got, "You are a crab.") > strings.Index(got, "PROJECT INSTRUCTIONS") {
		t.Fatalf("the project's instructions preceded the persona:\n%s", got)
	}
}

// THE TRAP THIS CACHE WAS ONE LINE FROM.
//
// A single cache slot would have served whichever project asked first to every
// project after it for a whole TTL -- and the symptom, an agent following
// another project's instructions, reads as a model problem rather than a cache
// one.
func TestTheCacheIsKeyedByProject(t *testing.T) {
	ws := t.TempDir()
	for _, name := range []string{"alpha", "beta"} {
		dir := domain.ProjectWorkspace(ws, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ProjectFileName), []byte("I am "+name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	p := &Prompt{Persona: "crab", Loader: Loader{Workspace: ws}, TTL: time.Hour}

	a := p.System(domain.WithProject(context.Background(), "alpha"))
	b := p.System(domain.WithProject(context.Background(), "beta"))

	if !strings.Contains(a, "I am alpha") {
		t.Errorf("alpha got:\n%s", a)
	}
	if !strings.Contains(b, "I am beta") {
		t.Fatalf("beta was served alpha's cached prompt:\n%s", b)
	}
}

// A new project has no instructions and no memory, and that is not an error.
func TestAProjectWithNoFilesIsSilent(t *testing.T) {
	ws := projectWorkspace(t, "fresh", nil)
	var logged []string
	p := &Prompt{
		Persona: "crab", Loader: Loader{Workspace: ws},
		Logf: func(f string, a ...any) { logged = append(logged, f) },
	}
	got := p.System(domain.WithProject(context.Background(), "fresh"))
	if got != "crab" {
		t.Errorf("prompt = %q, want the persona alone", got)
	}
	if len(logged) != 0 {
		t.Errorf("a new project logged %v", logged)
	}
}

// The MAIN workspace's memory reaches every turn, a project's included, and at
// picoclaw's path -- which is what a migrated member's directory already has.
func TestTheWorkspaceMemoryReachesEveryTurn(t *testing.T) {
	ws := t.TempDir()
	dir := filepath.Join(ws, "memory")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "MEMORY.md"),
		[]byte("The member prefers metric units."), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, project := range []string{"", "seed-trial"} {
		got := promptFor(t, ws, project)
		if !strings.Contains(got, "metric units") {
			t.Errorf("project %q did not carry the workspace memory:\n%s", project, got)
		}
	}
}

// EVERY document in memory/, not a fixed list. The proxy mounts three today and
// an admin may add a fourth; a harness that enumerated the ones it knew about
// would silently ignore it.
//
// FILE_DELIVERY.md is the one that matters most: it is what tells an agent where
// a deliverable has to be written to be visible to the member at all.
func TestEveryManagedMemoryDocumentIsInjected(t *testing.T) {
	ws := t.TempDir()
	dir := filepath.Join(ws, "memory")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	docs := map[string]string{
		"MEMORY.md":           "what the agent learned",
		"FILE_DELIVERY.md":    "write deliverables into public/attachments",
		"CONTEXT_RECOVERY.md": "how to recover context",
		"SOMETHING_NEW.md":    "a document nobody hardcoded",
	}
	for name, body := range docs {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	got := promptFor(t, ws, "")
	for _, body := range docs {
		if !strings.Contains(got, body) {
			t.Errorf("missing from the prompt: %q\n%s", body, got)
		}
	}
	// MEMORY.md leads, so the render is stable and the agent's own memory is not
	// buried under the managed documents.
	if i, j := strings.Index(got, "what the agent learned"), strings.Index(got, "how to recover context"); i > j {
		t.Error("MEMORY.md did not come first")
	}
}

// A workspace with no memory directory is the ordinary state of a new one, and
// must render exactly as it did before any of this existed.
func TestAWorkspaceWithNoMemoryDirectoryIsUnchanged(t *testing.T) {
	got := promptFor(t, t.TempDir(), "")
	if strings.TrimSpace(got) != "You are a crab." {
		t.Fatalf("prompt = %q", got)
	}
}
