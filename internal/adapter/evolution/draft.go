package evolution

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/skillfile"
)

// Generating, reviewing and writing one skill draft.
//
// The review is the part worth reading. picoclaw's own documentation calls its
// equivalent "a narrow guardrail, not a complete safety boundary… it does not
// reliably detect prompt injection", and that assessment is carried over
// VERBATIM rather than quietly upgraded: what follows raises the cost of a bad
// draft reaching disk, it does not make one impossible.

// draftInstructions is the rulebook sent with every generation request.
//
// Embedded rather than configurable. A prompt that decides what an agent writes
// about itself, editable from the same admin screen that edits the agent's
// configuration, would be a way to make the agent write anything -- and the
// whole point of the ladder is that `apply` is the only rung that writes.
const draftInstructions = `You are writing a reusable SKILL for an AI agent, describing a procedure the
agent has now completed successfully several times.

Rules, all of them mandatory:
- Return ONLY the skill file. No prose before or after, no markdown fences.
- Begin with YAML frontmatter delimited by --- lines, containing EXACTLY two
  fields: name and description.
- name must be lowercase, hyphen-separated, and match the name you are given.
- description is one sentence saying when to use this skill.
- The body is the procedure: numbered steps, concrete, referring to the tools by
  the names given.
- Never include an API key, a token, a password or any other credential.
- Never include a specific person's name, message or data. Describe the SHAPE of
  the work, not the instance of it.`

// draft generates one, then reviews it. A draft that fails review is returned
// QUARANTINED rather than dropped: it is the most interesting artefact this
// package produces, and hiding it would hide what the model tried to write.
func (e *Engine) draft(ctx context.Context, p Pattern, records []Record) Draft {
	name := skillName(p.Signature)
	d := Draft{At: e.now(), Name: name, Pattern: p.Signature}

	body, err := e.generate(ctx, name, p, records)
	if err != nil {
		e.logf("evolution: the model could not draft %q (%v); using the deterministic skeleton", name, err)
		body = skeleton(name, p)
	}
	d.Body = body

	if err := Review(name, body); err != nil {
		d.Status, d.Reason = StatusQuarantined, err.Error()
		return d
	}
	d.Status = StatusCandidate
	return d
}

// generate asks the model, using the same candidate the turn used.
func (e *Engine) generate(ctx context.Context, name string, p Pattern, records []Record) (string, error) {
	if e.Provider == nil {
		return "", errors.New("no provider configured")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Skill name: %s\n", name)
	fmt.Fprintf(&b, "Tools used, in order: %s\n", strings.ReplaceAll(p.Signature, ">", " then "))
	fmt.Fprintf(&b, "Observed %d times, %d of them successful.\n\n", p.Count, p.Successes)
	b.WriteString("Examples of what the member asked:\n")
	for _, ex := range p.Examples {
		fmt.Fprintf(&b, "- %s\n", truncate(ex, 300))
	}

	// A short deadline. This runs after a turn the member already has, so it
	// must never keep a scale-to-zero container alive waiting on a provider.
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	stream, err := e.Provider.Complete(ctx, domain.Completion{
		Model:  e.Model,
		System: draftInstructions,
		Messages: []domain.Message{
			{Role: domain.RoleUser, Content: b.String()},
		},
	})
	if err != nil {
		return "", err
	}
	defer stream.Close()
	for {
		if _, nerr := stream.Next(ctx); errors.Is(nerr, io.EOF) {
			break
		} else if nerr != nil {
			return "", nerr
		}
	}
	out := strings.TrimSpace(stream.Message().Content)
	// Models fence things they were told not to fence.
	out = strings.TrimPrefix(out, "```markdown")
	out = strings.TrimPrefix(out, "```md")
	out = strings.TrimPrefix(out, "```")
	out = strings.TrimSuffix(strings.TrimSpace(out), "```")
	return strings.TrimSpace(out), nil
}

// skeleton is the deterministic fallback, used when no provider answers.
//
// It produces a real, valid skill rather than nothing: the signature IS the
// procedure at its coarsest, and a stub a human can edit is worth more than a
// draft that never appeared.
func skeleton(name string, p Pattern) string {
	var b strings.Builder
	b.WriteString("---\nname: " + name + "\n")
	b.WriteString("description: A procedure this agent has completed successfully " +
		fmt.Sprint(p.Successes) + " times.\n---\n\n")
	b.WriteString("## Steps\n\n")
	for i, tool := range strings.Split(p.Signature, ">") {
		fmt.Fprintf(&b, "%d. Use `%s`.\n", i+1, tool)
	}
	b.WriteString("\n> Drafted automatically from observed turns; no model was available to " +
		"write it up. Edit before relying on it.\n")
	return b.String()
}

// skillName turns a tool signature into a directory-safe name.
func skillName(signature string) string {
	s := strings.ToLower(strings.ReplaceAll(signature, ">", "-then-"))
	s = regexp.MustCompile(`[^a-z0-9-]+`).ReplaceAllString(s, "-")
	s = strings.Trim(regexp.MustCompile(`-+`).ReplaceAllString(s, "-"), "-")
	if s == "" {
		s = "unnamed"
	}
	if len(s) > 40 {
		s = strings.Trim(s[:40], "-")
	}
	return s
}

// nameRe is picoclaw's own constraint on a skill name, and it is also what makes
// the name safe as a directory: no dot, no slash, no traversal.
var nameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)

// secretPatterns are the shapes a credential takes in text.
//
// Deliberately picoclaw's list, so the two harnesses quarantine the same
// drafts, plus the ones this stack's own credentials actually look like.
var secretPatterns = []string{
	"sk-live-", "sk_test_", "sk-ant-", "api_key=", "apikey=",
	"-----BEGIN", "enc://", "AKIA", "ghp_", "xoxb-",
}

// Review is the gate before disk.
//
// Structural first, because a draft that is not a valid skill cannot be a safe
// one either; then the secret scan. Every failure NAMES what it found, because a
// quarantined draft nobody can explain is a quarantined draft somebody will
// eventually apply by hand.
func Review(name, body string) error {
	if !nameRe.MatchString(name) {
		return fmt.Errorf("skill name %q is not a valid directory name", name)
	}
	got, desc := skillfile.Frontmatter(body)
	if got == "" {
		return errors.New("no YAML frontmatter with a name")
	}
	if got != name {
		// picoclaw's validator requires the same agreement, and for the same
		// reason: the directory and the frontmatter are two names for one
		// thing, and a disagreement makes the loader and the writer refer to
		// different skills.
		return fmt.Errorf("frontmatter name %q does not match the skill name %q", got, name)
	}
	if strings.TrimSpace(desc) == "" {
		return errors.New("frontmatter has no description")
	}
	if err := onlyKnownFrontmatterFields(body); err != nil {
		return err
	}
	if strings.TrimSpace(stripFrontmatter(body)) == "" {
		return errors.New("the skill has no body")
	}
	lower := strings.ToLower(body)
	for _, pat := range secretPatterns {
		if strings.Contains(lower, strings.ToLower(pat)) {
			// The matched text is NOT echoed. Naming the pattern is enough to
			// act on, and quoting the match would copy a credential into a log
			// line, which is the thing being prevented.
			return fmt.Errorf("the draft contains something shaped like a credential (%q)", pat)
		}
	}
	return nil
}

// onlyKnownFrontmatterFields rejects anything but name and description.
//
// picoclaw's validator does the same. It matters more than it looks: an unknown
// field is how a future frontmatter key -- one that some later loader acts on --
// gets written by a model rather than by a person.
func onlyKnownFrontmatterFields(body string) error {
	rest, ok := strings.CutPrefix(strings.TrimLeft(body, "\ufeff \t\r\n"), "---")
	if !ok {
		return errors.New("no frontmatter")
	}
	rest = strings.TrimPrefix(rest, "\n")
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return errors.New("unterminated frontmatter")
	}
	for _, line := range strings.Split(rest[:end], "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		k, _, found := strings.Cut(line, ":")
		if !found {
			return fmt.Errorf("frontmatter line %q is not a key: value pair", strings.TrimSpace(line))
		}
		switch strings.TrimSpace(k) {
		case "name", "description":
		default:
			return fmt.Errorf("frontmatter field %q is not allowed", strings.TrimSpace(k))
		}
	}
	return nil
}

func stripFrontmatter(body string) string {
	rest, ok := strings.CutPrefix(strings.TrimLeft(body, "\ufeff \t\r\n"), "---")
	if !ok {
		return body
	}
	rest = strings.TrimPrefix(rest, "\n")
	if end := strings.Index(rest, "\n---"); end >= 0 {
		return strings.TrimPrefix(rest[end+4:], "\n")
	}
	return body
}

// write puts an approved draft on disk, with a backup and a rollback.
//
// The order is the whole of it: back up what is there, write atomically, then
// re-read and re-validate what landed. A validation failure AFTER the write
// restores the backup -- because the failure mode being guarded is not "the
// draft was bad", which Review already caught, but "the file on disk is not what
// we thought we wrote".
func (e *Engine) write(d Draft) error {
	if e.SkillsDir == "" {
		return errNoStateDir
	}
	dir := filepath.Join(e.SkillsDir, d.Name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	target := filepath.Join(dir, skillfile.FileName)

	restore, err := e.backup(target, d.Name)
	if err != nil {
		return fmt.Errorf("backup: %w", err)
	}

	tmp := target + ".tmp"
	if err := os.WriteFile(tmp, []byte(d.Body), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, target); err != nil {
		return err
	}

	landed, err := os.ReadFile(target)
	if err == nil {
		err = Review(d.Name, string(landed))
	}
	if err != nil {
		if rerr := restore(); rerr != nil {
			// Both errors matter: one says what went wrong, the other says the
			// agent is now running on a skill nobody chose.
			return fmt.Errorf("%w (AND THE ROLLBACK FAILED: %v)", err, rerr)
		}
		return fmt.Errorf("what landed on disk did not validate, rolled back: %w", err)
	}
	return nil
}

// backup snapshots whatever is at target and returns the undo.
//
// Returns a no-op undo when there was nothing there, so the caller never has to
// distinguish "created" from "overwritten" -- and an undo that removes a file
// the agent had before would be worse than the failure it is recovering from.
func (e *Engine) backup(target, name string) (func() error, error) {
	old, err := os.ReadFile(target)
	if os.IsNotExist(err) {
		return func() error {
			if rerr := os.Remove(target); rerr != nil && !os.IsNotExist(rerr) {
				return rerr
			}
			return nil
		}, nil
	}
	if err != nil {
		return nil, err
	}
	dir, err := e.path(filepath.Join(BackupsDir, name, e.now().UTC().Format("20060102-150405.000")))
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	kept := filepath.Join(dir, skillfile.FileName)
	if err := os.WriteFile(kept, old, 0o644); err != nil {
		return nil, err
	}
	return func() error { return os.WriteFile(target, old, 0o644) }, nil
}
