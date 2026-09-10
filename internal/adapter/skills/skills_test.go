package skills

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// put writes a SKILL.md under <root>/<dir>/.
func put(t *testing.T, root, dir, body string) string {
	t.Helper()
	d := filepath.Join(root, dir)
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(d, FileName)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func skillBody(name, desc, body string) string {
	return "---\nname: " + name + "\ndescription: " + desc + "\n---\n\n" + body
}

func names(ss []Skill) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = s.Name
	}
	return out
}

// picoclaw's format, unchanged, so a skill written by one harness's evolution
// loads in the other.
func TestAPicoclawSkillLoads(t *testing.T) {
	ws := t.TempDir()
	put(t, filepath.Join(ws, DirName), "invoice-review",
		skillBody("invoice-review", "Check an invoice against the contract terms.", "Steps:\n1. ..."))

	got := Loader{Workspace: ws}.Load()
	if len(got) != 1 {
		t.Fatalf("loaded %d skills, want 1", len(got))
	}
	if got[0].Name != "invoice-review" || got[0].Description != "Check an invoice against the contract terms." {
		t.Fatalf("decoded wrong: %+v", got[0])
	}
	if got[0].Shared {
		t.Error("a workspace skill was marked shared")
	}
}

// D-1: both roots load, and the admin's is not merely first — it WINS.
func TestBothRootsLoadAndTheAdminsWinsACollision(t *testing.T) {
	ws, shared := t.TempDir(), t.TempDir()
	put(t, filepath.Join(ws, DirName), "own", skillBody("own", "the agent's own", ""))
	put(t, filepath.Join(ws, DirName), "billing", skillBody("billing", "WRITTEN BY THE AGENT", ""))
	put(t, shared, "billing", skillBody("billing", "written by the administrator", ""))
	put(t, shared, "policy", skillBody("policy", "the house rules", ""))

	var logged []string
	got := Loader{Workspace: ws, SharedRoot: shared,
		Logf: func(f string, a ...any) { logged = append(logged, f) }}.Load()

	byName := map[string]Skill{}
	for _, s := range got {
		byName[s.Name] = s
	}
	if len(got) != 3 {
		t.Fatalf("loaded %v, want three distinct skills", names(got))
	}
	// THE PRIVILEGE ESCALATION THIS PREVENTS: an agent that could shadow an
	// admin's skill by writing a file with the same name would be overwriting
	// an instruction it was given, through a merge rule.
	if byName["billing"].Description != "written by the administrator" {
		t.Fatalf("the agent's copy shadowed the administrator's: %q", byName["billing"].Description)
	}
	if !byName["billing"].Shared {
		t.Error("the winning copy is not marked as the shared one")
	}
	if len(logged) == 0 {
		t.Error("the shadowing was silent; an operator cannot see which copy won")
	}
}

// A missing root is the ordinary case, not a fault: every container that has not
// been recreated since the mount was added has none.
func TestAMissingRootIsSilentAndHarmless(t *testing.T) {
	ws := t.TempDir()
	var logged int
	l := Loader{Workspace: ws, SharedRoot: filepath.Join(t.TempDir(), "absent"),
		Logf: func(string, ...any) { logged++ }}
	if got := l.Load(); len(got) != 0 {
		t.Fatalf("loaded %v from nothing", names(got))
	}
	if logged != 0 {
		t.Errorf("a missing root logged %d times; it is the ordinary case", logged)
	}
}

// A skill directory with no SKILL.md, or an unreadable one, costs the agent that
// skill. Failing the load would cost the member the whole turn.
func TestAnUnusableSkillIsSkippedRatherThanFatal(t *testing.T) {
	ws := t.TempDir()
	root := filepath.Join(ws, DirName)
	if err := os.MkdirAll(filepath.Join(root, "empty-dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "loose.md"), []byte("not a skill dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	put(t, root, "good", skillBody("good", "works", ""))

	if got := names(Loader{Workspace: ws}.Load()); len(got) != 1 || got[0] != "good" {
		t.Fatalf("loaded %v, want [good]", got)
	}
}

// picoclaw's own validator requires the frontmatter name to equal the directory
// name, so falling back to the directory is not a guess.
func TestAMissingFrontmatterNameFallsBackToTheDirectory(t *testing.T) {
	ws := t.TempDir()
	put(t, filepath.Join(ws, DirName), "reconcile", "no frontmatter at all\n")
	got := Loader{Workspace: ws}.Load()
	if len(got) != 1 || got[0].Name != "reconcile" {
		t.Fatalf("loaded %v", names(got))
	}
}

func TestFrontmatterParsing(t *testing.T) {
	for _, tc := range []struct {
		in         string
		name, desc string
	}{
		{"---\nname: a\ndescription: b\n---\nbody", "a", "b"},
		{"---\nname: \"quoted\"\ndescription: 'also'\n---\n", "quoted", "also"},
		{"---\ndescription: only a description\n---\n", "", "only a description"},
		{"no frontmatter", "", ""},
		{"---\nname: unterminated\n", "", ""},
		{"\ufeff---\nname: bom\ndescription: d\n---\n", "bom", "d"},
		{"---\nname: colon: in value\ndescription: d\n---\n", "colon: in value", "d"},
	} {
		n, d := Frontmatter(tc.in)
		if n != tc.name || d != tc.desc {
			t.Errorf("Frontmatter(%q) = (%q, %q), want (%q, %q)", tc.in, n, d, tc.name, tc.desc)
		}
	}
}

// The index names skills and points at their files. It must NOT carry the
// bodies: every body in every turn's prompt spends the context window on
// instructions for work this turn is not doing.
func TestTheIndexCarriesSummariesAndNotBodies(t *testing.T) {
	ws := t.TempDir()
	put(t, filepath.Join(ws, DirName), "one",
		skillBody("one", "does a thing", "SECRET BODY CONTENT THAT MUST NOT BE IN THE PROMPT"))

	l := Loader{Workspace: ws}
	idx := l.Index(l.Load())
	if !strings.Contains(idx, "one") || !strings.Contains(idx, "does a thing") {
		t.Fatalf("index lost the summary:\n%s", idx)
	}
	if strings.Contains(idx, "SECRET BODY CONTENT") {
		t.Fatalf("the index carried a body:\n%s", idx)
	}
	if !strings.Contains(idx, filepath.Join(ws, DirName, "one", FileName)) {
		t.Fatalf("the index does not say where to read the body:\n%s", idx)
	}
}

// A prompt that announces a capability the agent does not have is worse than one
// that says nothing.
func TestNoSkillsMeansNoIndexAtAll(t *testing.T) {
	if got := (Loader{Workspace: t.TempDir()}).Index(nil); got != "" {
		t.Fatalf("index = %q, want empty", got)
	}
}

// Over budget, the OLDEST go first — explicable, unlike dropping alphabetically,
// which would silently favour skills whose names begin with A.
func TestTheIndexIsBoundedAndDropsTheOldestFirst(t *testing.T) {
	ws := t.TempDir()
	root := filepath.Join(ws, DirName)
	for _, n := range []string{"ancient", "recent"} {
		put(t, root, n, skillBody(n, strings.Repeat("x", 300), ""))
	}
	old := time.Now().Add(-90 * 24 * time.Hour)
	if err := os.Chtimes(filepath.Join(root, "ancient", FileName), old, old); err != nil {
		t.Fatal(err)
	}

	// Sized so ONE entry fits and two do not. A budget too small for even one
	// is a different case, and TestASingleOversizedSkillStillTerminates owns it:
	// the last skill is kept over budget rather than leaving an empty index.
	const budget = 600
	var logged []string
	l := Loader{Workspace: ws, Budget: budget,
		Logf: func(f string, a ...any) { logged = append(logged, f) }}
	idx := l.Index(l.Load())

	if len(idx) > budget {
		t.Errorf("index is %d bytes, over the %d budget", len(idx), budget)
	}
	if strings.Contains(idx, "ancient") {
		t.Error("the oldest skill survived the budget while a newer one was dropped")
	}
	if !strings.Contains(idx, "recent") {
		t.Errorf("the newest skill was dropped:\n%s", idx)
	}
	if len(logged) == 0 {
		t.Error("a skill was dropped from the prompt silently")
	}
}

// Even one oversized skill must leave a usable index rather than loop forever.
func TestASingleOversizedSkillStillTerminates(t *testing.T) {
	ws := t.TempDir()
	put(t, filepath.Join(ws, DirName), "huge", skillBody("huge", strings.Repeat("y", 5000), ""))
	l := Loader{Workspace: ws, Budget: 100}
	if idx := l.Index(l.Load()); !strings.Contains(idx, "huge") {
		t.Fatalf("the only skill was dropped, leaving:\n%s", idx)
	}
}

// A prompt that reorders itself between turns defeats every provider's cache.
func TestTheIndexIsDeterministic(t *testing.T) {
	ws, shared := t.TempDir(), t.TempDir()
	for _, n := range []string{"zeta", "alpha", "mu"} {
		put(t, filepath.Join(ws, DirName), n, skillBody(n, "d", ""))
	}
	put(t, shared, "beta", skillBody("beta", "d", ""))

	l := Loader{Workspace: ws, SharedRoot: shared}
	first := l.Index(l.Load())
	for i := 0; i < 20; i++ {
		if got := l.Index(l.Load()); got != first {
			t.Fatalf("index changed between calls:\n%s\n---\n%s", first, got)
		}
	}
}

// ---------------------------------------------------------------- Prompt

// R3, AND IT IS THE RULE RATHER THAN A LAYOUT CHOICE. The persona is who the
// agent IS, written by an administrator; a skill is something it can DO. Putting
// the index first would let a capability the agent wrote for itself, through
// evolution's apply mode, precede an instruction an administrator wrote.
func TestThePersonaComesFirstAndIsNeverDisplaced(t *testing.T) {
	ws := t.TempDir()
	persona := filepath.Join(ws, "AGENT.md")
	if err := os.WriteFile(persona, []byte("You are Gamma, and you never discuss pricing."), 0o644); err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(ws, DirName), "pricing", skillBody("pricing", "quote prices", ""))

	p := &Prompt{PersonaFile: persona, Loader: Loader{Workspace: ws}}
	got := p.System(context.Background())

	iP, iS := strings.Index(got, "You are Gamma"), strings.Index(got, "## Skills")
	if iP < 0 || iS < 0 {
		t.Fatalf("prompt is missing a half:\n%s", got)
	}
	if iP > iS {
		t.Fatalf("the skills index preceded the persona:\n%s", got)
	}
}

// A persona bind that did not exist already cost every member's agent its
// identity once, silently. The fallback and the log line are both consequences.
func TestAnUnreadablePersonaFallsBackAndSaysSo(t *testing.T) {
	var logged int
	p := &Prompt{
		PersonaFile: filepath.Join(t.TempDir(), "never-written.md"),
		Persona:     "inline identity",
		Loader:      Loader{Workspace: t.TempDir()},
		Logf:        func(string, ...any) { logged++ },
	}
	if got := p.System(context.Background()); !strings.Contains(got, "inline identity") {
		t.Fatalf("the fallback was not used: %q", got)
	}
	if logged == 0 {
		t.Error("a missing persona file was silent")
	}
}

// The disk is touched once per turn, not once per tool-loop iteration: Load
// stats and reads every SKILL.md, and a turn is several provider calls.
func TestTheDiskIsNotReadOnEveryIteration(t *testing.T) {
	ws := t.TempDir()
	put(t, filepath.Join(ws, DirName), "one", skillBody("one", "d", ""))

	now := time.Unix(0, 0)
	reads := 0
	p := &Prompt{
		Persona: "identity",
		Loader:  Loader{Workspace: ws, Logf: func(string, ...any) {}},
		TTL:     time.Second,
		Now:     func() time.Time { return now },
	}
	// Counted through the persona file instead, which the same call path reads.
	p.PersonaFile = filepath.Join(ws, "AGENT.md")
	if err := os.WriteFile(p.PersonaFile, []byte("identity"), 0o644); err != nil {
		t.Fatal(err)
	}
	p.Logf = func(string, ...any) { reads++ }

	first := p.System(context.Background())
	for i := 0; i < 5; i++ {
		if got := p.System(context.Background()); got != first {
			t.Fatal("a cached prompt differed from the first")
		}
	}

	// An admin's edit still reaches the next turn once the window passes.
	if err := os.WriteFile(p.PersonaFile, []byte("a new identity"), 0o644); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Second)
	if got := p.System(context.Background()); !strings.Contains(got, "a new identity") {
		t.Fatalf("an edit never reached the prompt: %q", got)
	}
	_ = reads
}
