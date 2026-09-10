// Package skills loads SKILL.md files and turns them into the part of the
// system prompt that says what this agent knows how to do.
//
// # WHY THIS EXISTS BEFORE EVOLUTION DOES
//
// The harness had no skills at all: it did not read them, did not put them in a
// prompt, and the proxy mounted no skills root for it. Evolution's `apply` mode
// writes SKILL.md files -- so shipping evolution first would have written files
// nothing reads, which is precisely the "stores a setting that changes nothing"
// failure the harness spec's deferred table exists to prevent.
//
// TWO SOURCES, AND THEY ARE NOT EQUIVALENT
//
//   - the admin's shared root, mounted READ-ONLY by the proxy;
//   - <workspace>/skills, writable, where evolution's own drafts land.
//
// A name in both resolves to the ADMIN's copy and the shadowed one is logged.
// The other way round would let an agent overwrite an administrator's
// instruction by writing a file with the same name -- a privilege escalation
// dressed up as a merge rule.
//
// # WHAT REACHES THE MODEL IS AN INDEX, NOT THE BODIES
//
// Name and description per skill, under a budget. The bodies stay on disk and
// the agent reads the ones it needs with the shell tool, which already reaches
// the workspace. Putting every body in every turn's prompt would spend the
// context window on instructions for work this turn is not doing -- and it is
// how a skill library stops being an asset and becomes a tax.
package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/skillfile"
)

// DefaultBudget bounds the rendered index.
//
// 8 KiB is about 2 000 tokens: enough for a few dozen skills at a sentence each,
// small enough that the index never competes with the conversation for room.
const DefaultBudget = 8 << 10

// Re-exported so callers of this package do not have to import two.
const (
	DirName  = skillfile.DirName
	FileName = skillfile.FileName
)

// Frontmatter is skillfile's, re-exported for the same reason.
var Frontmatter = skillfile.Frontmatter

// Skill is one loaded SKILL.md.
type Skill struct {
	// Name comes from the frontmatter, falling back to the directory name --
	// which is what picoclaw's own validator requires them to agree on.
	Name        string
	Description string
	// Path is the file, so the index can tell the model where to read the body.
	Path string
	// Shared marks a skill from the admin's read-only root. It is why a
	// collision resolves the way it does, and it is reported so an operator can
	// see which copy won.
	Shared  bool
	ModTime int64
}

// Loader reads both roots.
type Loader struct {
	// Workspace is the agent's own root; its skills live at <Workspace>/skills.
	Workspace string
	// SharedRoot is the admin's, mounted read-only. Empty when none is bound,
	// which is every deployment that has not been recreated since D-1.
	SharedRoot string
	Budget     int
	Logf       func(string, ...any)
}

func (l Loader) logf(f string, a ...any) {
	if l.Logf != nil {
		l.Logf(f, a...)
	}
}

// Load reads every skill from both roots, admin first.
//
// A directory that cannot be read is LOGGED AND SKIPPED rather than failing.
// Skills are additive capability: an unreadable one costs the agent that skill,
// and refusing to answer the turn over it would cost the member everything.
func (l Loader) Load() []Skill {
	byName := map[string]Skill{}
	var order []string

	add := func(s Skill) {
		if prev, seen := byName[s.Name]; seen {
			// Admin wins. The loop below reads the shared root first, so a
			// later duplicate is always the workspace's.
			if prev.Shared && !s.Shared {
				l.logf("skills: %q in the workspace is shadowed by the shared one", s.Name)
				return
			}
		} else {
			order = append(order, s.Name)
		}
		byName[s.Name] = s
	}

	if l.SharedRoot != "" {
		for _, s := range l.read(l.SharedRoot, true) {
			add(s)
		}
	}
	for _, s := range l.read(filepath.Join(l.Workspace, DirName), false) {
		add(s)
	}

	out := make([]Skill, 0, len(order))
	for _, n := range order {
		out = append(out, byName[n])
	}
	return out
}

// read scans one root: <root>/<dir>/SKILL.md, picoclaw's layout.
func (l Loader) read(root string, shared bool) []Skill {
	entries, err := os.ReadDir(root)
	if err != nil {
		if !os.IsNotExist(err) {
			l.logf("skills: cannot read %s: %v", root, err)
		}
		return nil
	}
	var out []Skill
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		path := filepath.Join(root, e.Name(), FileName)
		b, err := os.ReadFile(path)
		if err != nil {
			if !os.IsNotExist(err) {
				l.logf("skills: cannot read %s: %v", path, err)
			}
			continue
		}
		name, desc := Frontmatter(string(b))
		if name == "" {
			// picoclaw's validator requires the frontmatter name to equal the
			// directory name, so falling back to the directory is not a guess.
			name = e.Name()
		}
		var mod int64
		if st, serr := os.Stat(path); serr == nil {
			mod = st.ModTime().Unix()
		}
		out = append(out, Skill{Name: name, Description: desc, Path: path, Shared: shared, ModTime: mod})
	}
	// Deterministic: an index that reorders itself between turns is a prompt
	// that changes for no reason, which defeats every provider's prompt cache.
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Index renders the prompt fragment.
//
// Returns "" for no skills, so the caller appends nothing rather than a heading
// over an empty list -- a prompt that announces a capability the agent does not
// have is worse than one that says nothing.
func (l Loader) Index(skills []Skill) string {
	if len(skills) == 0 {
		return ""
	}
	budget := l.Budget
	if budget <= 0 {
		budget = DefaultBudget
	}

	const head = "## Skills\n\nYou have these skills. Read a skill's file with the shell tool " +
		"before using it; the line below is only its summary.\n"
	var b strings.Builder
	b.WriteString(head)

	// Over budget, the OLDEST by modification time go first. A skill nobody has
	// touched in months is the one least likely to matter this turn, and
	// dropping by that order is at least explicable -- dropping alphabetically
	// would silently favour skills whose names begin with A.
	kept := append([]Skill(nil), skills...)
	for {
		var out strings.Builder
		out.WriteString(head)
		for _, s := range kept {
			out.WriteString(line(s))
		}
		if out.Len() <= budget || len(kept) <= 1 {
			b.Reset()
			b.WriteString(out.String())
			break
		}
		oldest, at := kept[0], 0
		for i, s := range kept[1:] {
			if s.ModTime < oldest.ModTime {
				oldest, at = s, i+1
			}
		}
		l.logf("skills: %q dropped from the prompt index, over the %d-byte budget", oldest.Name, budget)
		kept = append(kept[:at], kept[at+1:]...)
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

func line(s Skill) string {
	desc := s.Description
	if desc == "" {
		desc = "(no description)"
	}
	return fmt.Sprintf("- **%s** — %s (`%s`)\n", s.Name, desc, s.Path)
}
