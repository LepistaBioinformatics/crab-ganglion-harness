package skills

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

// Prompt is the SystemPrompt adapter: persona first, skills index below it.
//
// THE ORDER IS THE RULE, not a layout choice. The persona is who the agent IS --
// written by an administrator through the cascade, mounted read-only. A skill is
// something it can DO. Putting the index first, or letting a skill's text sit
// above the persona, would let a capability written by the agent itself (through
// evolution's apply mode) precede an instruction written by an administrator.
// A skill adds capability; it never displaces identity.
type Prompt struct {
	// PersonaFile is the cascade's AGENT.md. Re-read, not cached: an admin's
	// edit reaches the next turn, which is the property GANGLION_SYSTEM_FILE
	// exists for.
	PersonaFile string
	// Persona is the inline fallback, used when no file is configured or the
	// file cannot be read.
	Persona string
	Loader  Loader
	Logf    func(string, ...any)

	// TTL bounds how often the disk is touched. Zero means every turn.
	//
	// It exists because Load() stats and reads every SKILL.md, and a turn is
	// several provider calls -- doing that per ITERATION rather than per turn
	// would multiply the cost by the tool loop's depth for no gain.
	TTL time.Duration
	Now func() time.Time

	mu sync.Mutex
	// cached is keyed by PROJECT, because the assembled message differs per
	// project: each carries its own instructions and its own memory. A single
	// slot would have served whichever project asked first to every project
	// after it, for a whole TTL, and the symptom -- an agent following the
	// wrong project's instructions -- looks like a model problem.
	cached map[string]cacheEntry
}

type cacheEntry struct {
	text string
	at   time.Time
}

func (p *Prompt) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *Prompt) logf(f string, a ...any) {
	if p.Logf != nil {
		p.Logf(f, a...)
	}
}

// System assembles the message.
//
// Order: persona, the project's instructions, the project's memory, the skills
// index. The persona rule is stated on the type and is unchanged; the two new
// sections sit between it and the index for the same reason the index sits
// below the persona -- an instruction an administrator wrote outranks one a
// member wrote, which outranks a capability the agent may have written itself.
func (p *Prompt) System(ctx context.Context) string {
	project := domain.ProjectFrom(ctx)

	p.mu.Lock()
	defer p.mu.Unlock()

	ttl := p.TTL
	if ttl <= 0 {
		ttl = time.Second
	}
	if e, ok := p.cached[project]; ok && e.text != "" && p.now().Sub(e.at) < ttl {
		return e.text
	}

	var b strings.Builder
	b.WriteString(p.persona())
	for _, sec := range p.projectSections(project) {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(sec)
	}
	if idx := p.Loader.Index(p.Loader.Load()); idx != "" {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(idx)
	}

	if p.cached == nil {
		p.cached = map[string]cacheEntry{}
	}
	p.cached[project] = cacheEntry{text: b.String(), at: p.now()}
	return b.String()
}

// ProjectFileName holds the instructions a member gave a project. Written by
// the proxy from its own store on every ensure, so an edit in the webapp
// reaches the next turn -- and so an edit the AGENT makes to the file is
// reverted, which is the same trade composeProjectAgentMD documents.
const ProjectFileName = "PROJECT.md"

// MemoryFileName is the project's memory: a plain Markdown file the agent edits
// with the shell it already has.
//
// A FILE AND NOT A TOOL, which is what picoclaw does too -- its memory is
// <workspace>/memory/MEMORY.md, injected into the prompt and edited with the
// ordinary file tools. A memory tool would be a second way to write a file this
// agent can already write, and a store the member could not read.
//
// This is the first memory the ganglion harness has had at all: until now its
// only persistence was the transcript and the window.
const MemoryFileName = "MEMORY.md"

// projectSections reads the project's instructions and memory, in that order.
//
// Missing is the ordinary case and is silent: a project whose member has
// written no instructions and whose agent has learned nothing yet is a new
// project, not a broken one. An unreadable file that EXISTS is logged, because
// that is a permissions problem somebody has to see.
func (p *Prompt) projectSections(project string) []string {
	if project == "" || p.Loader.Workspace == "" {
		return nil
	}
	root := domain.ProjectRoot(domain.WithProject(context.Background(), project), p.Loader.Workspace)
	var out []string
	for _, f := range []struct{ name, heading string }{
		{ProjectFileName, "# This project"},
		{MemoryFileName, "# What you have learned in this project"},
	} {
		b, err := os.ReadFile(filepath.Join(root, f.name))
		if err != nil {
			if !os.IsNotExist(err) {
				p.logf("skills: project %q: %s: %v", project, f.name, err)
			}
			continue
		}
		if body := strings.TrimSpace(string(b)); body != "" {
			out = append(out, f.heading+"\n\n"+body)
		}
	}
	return out
}

// persona reads the identity file, falling back to the inline value.
//
// An unreadable file falls back RATHER THAN returning empty. A persona bind that
// did not exist already cost this stack every member's agent its identity once,
// silently; the fallback and the log line are both consequences of that.
func (p *Prompt) persona() string {
	if p.PersonaFile != "" {
		b, err := os.ReadFile(p.PersonaFile)
		if err == nil {
			if s := strings.TrimSpace(string(b)); s != "" {
				return s
			}
		} else {
			p.logf("persona file %s: %v (using the inline value)", p.PersonaFile, err)
		}
	}
	return strings.TrimSpace(p.Persona)
}
