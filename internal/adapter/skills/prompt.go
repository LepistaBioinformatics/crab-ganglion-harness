package skills

import (
	"context"
	"os"
	"strings"
	"sync"
	"time"
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

	mu       sync.Mutex
	cached   string
	cachedAt time.Time
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
func (p *Prompt) System(context.Context) string {
	p.mu.Lock()
	defer p.mu.Unlock()

	ttl := p.TTL
	if ttl <= 0 {
		ttl = time.Second
	}
	if p.cached != "" && p.now().Sub(p.cachedAt) < ttl {
		return p.cached
	}

	var b strings.Builder
	b.WriteString(p.persona())
	if idx := p.Loader.Index(p.Loader.Load()); idx != "" {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(idx)
	}
	p.cached, p.cachedAt = b.String(), p.now()
	return p.cached
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
