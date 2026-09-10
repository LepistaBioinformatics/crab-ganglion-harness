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
	dir := filepath.Join(ws, domain.ProjectsDirName, project)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
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
		dir := filepath.Join(ws, domain.ProjectsDirName, name)
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
