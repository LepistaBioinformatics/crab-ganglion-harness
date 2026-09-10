// Package evolution is the agent's self-improvement loop.
//
// # WHAT IT IS, AND WHAT IT IS NOT
//
// It is NOT a tool the model can call, and it does not rewrite the harness. It
// is a listener on completed turns with two paths -- a hot one that records what
// happened, and a cold one that looks for repeated successful patterns and
// proposes a SKILL for them. picoclaw's own is the same shape and its docs are
// the same about it: what evolves is the agent's skill library.
//
// # THE LADDER, AND WHY THE DEFAULT IS THE BOTTOM RUNG
//
//	observe  records turns, writes nothing else. The default.
//	draft    generates skill drafts, writes no skill.
//	apply    may write a skill.
//
// Each rung does strictly more than the one below. `apply` additionally
// REQUIRES AN APPROVER (spec D-2 / R10.1): the harness refuses to boot in that
// mode with no approval endpoint configured, because the loop's default approver
// allows everything and "apply requires approval" would then be a sentence that
// changes nothing.
//
// # WHAT IT WRITES
//
//	<state>/task-records.jsonl    one line per completed turn
//	<state>/pattern-records.jsonl one line per pattern that met the thresholds
//	<state>/skill-drafts.json     the drafts and their status
//	<state>/backups/...           what an apply overwrote, before it did
//
// and, in apply mode only, <workspace>/skills/<name>/SKILL.md. It never touches
// AGENT.md or the persona cascade -- those are an administrator's, and an agent
// that could rewrite its own identity is a different feature with a different
// argument behind it.
package evolution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/config"
	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

// File names, picoclaw's where it has one.
const (
	TaskRecordsFile    = "task-records.jsonl"
	PatternRecordsFile = "pattern-records.jsonl"
	DraftsFile         = "skill-drafts.json"
	BackupsDir         = "backups"
)

// DraftStatus is where a draft is on its way to disk.
type DraftStatus string

const (
	// StatusCandidate is generated and reviewed clean.
	StatusCandidate DraftStatus = "candidate"
	// StatusQuarantined failed review. It is KEPT rather than deleted: a
	// quarantined draft is the most interesting thing this package produces,
	// and deleting it would hide what the model tried to write.
	StatusQuarantined DraftStatus = "quarantined"
	// StatusApplied reached disk.
	StatusApplied DraftStatus = "applied"
	// StatusRefused was declined by the approver.
	StatusRefused DraftStatus = "refused"
)

// Record is one line of task-records.jsonl.
type Record struct {
	At         time.Time     `json:"at"`
	SessionID  string        `json:"session_id"`
	SessionKey string        `json:"session_key"`
	Model      string        `json:"model"`
	Input      string        `json:"input"`
	Answer     string        `json:"answer"`
	Tools      []string      `json:"tools,omitempty"`
	Signature  string        `json:"signature"`
	Succeeded  bool          `json:"succeeded"`
	Tokens     int           `json:"tokens"`
	DurationMS int64         `json:"duration_ms"`
	Usage      domain.Usage  `json:"-"`
	Elapsed    time.Duration `json:"-"`
}

// Pattern is a cluster of records that share a tool signature.
type Pattern struct {
	At        time.Time `json:"at"`
	Signature string    `json:"signature"`
	Count     int       `json:"count"`
	Successes int       `json:"successes"`
	Ratio     float64   `json:"ratio"`
	Examples  []string  `json:"examples"`
}

// Draft is a proposed skill.
type Draft struct {
	At      time.Time   `json:"at"`
	Name    string      `json:"name"`
	Body    string      `json:"body"`
	Status  DraftStatus `json:"status"`
	Reason  string      `json:"reason,omitempty"`
	Pattern string      `json:"pattern"`
}

// Engine implements domain.Learner.
type Engine struct {
	Cfg config.Evolution
	// StateDir is where records and drafts live. Inside the workspace, so a
	// container recreate does not lose what the agent has learned.
	StateDir string
	// SkillsDir is <workspace>/skills -- the WRITABLE copy. The admin's shared
	// root is a read-only mount and is not addressable from here.
	SkillsDir string
	// Provider and Model generate a draft. Nil Provider means the deterministic
	// fallback, which produces a usable skeleton rather than nothing.
	Provider domain.Provider
	Model    string
	// Approver gates an apply (D-2). Never nil in apply mode: the composition
	// root refuses to boot otherwise.
	Approver domain.Approver
	Now      func() time.Time
	Logf     func(string, ...any)

	mu sync.Mutex
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

func (e *Engine) logf(f string, a ...any) {
	if e.Logf != nil {
		e.Logf(f, a...)
	}
}

// Observe is the hot path.
//
// It runs INLINE rather than in a goroutine, and that is a deliberate choice
// with a cost: one small append and fsync per turn, after the member already has
// their answer. A goroutine would need its own lifetime, its own shutdown
// handshake and its own error path, and would make "the record for this turn is
// on disk" unobservable -- which is the property the cold path depends on.
func (e *Engine) Observe(ctx context.Context, t domain.TurnRecord) {
	if !e.Cfg.Records() {
		return
	}
	// A heartbeat is not a member asking for something, and counting it would
	// make "the agent always succeeds at doing nothing" the strongest pattern
	// in the store. picoclaw excludes it for the same reason.
	if string(t.SessionKey) == "heartbeat" {
		return
	}

	rec := Record{
		At:         e.now(),
		SessionID:  string(t.SessionID),
		SessionKey: string(t.SessionKey),
		Model:      t.Model,
		Input:      truncate(t.Input, 2000),
		Answer:     truncate(t.Answer, 2000),
		Succeeded:  !t.Failed,
		Tokens:     t.Usage.TotalTokens,
		DurationMS: t.Duration.Milliseconds(),
	}
	for _, tool := range t.Tools {
		rec.Tools = append(rec.Tools, tool.Name)
		if tool.Failed {
			rec.Succeeded = false
		}
	}
	rec.Signature = signature(rec.Tools)

	if err := e.append(TaskRecordsFile, rec); err != nil {
		// Logged, never returned. The turn already succeeded and the member
		// already has the answer; failing here would report a fault for work
		// that was done.
		e.logf("evolution: could not record the turn: %v", err)
		return
	}
	if e.Cfg.Drafts() && e.Cfg.ColdTrigger == config.ColdAfterTurn {
		e.RunCold(ctx)
	}
}

// signature is what makes two turns "the same kind of work".
//
// The ORDERED tool names, deduplicated in place. Order carries meaning -- search
// then fetch is a different shape of work from fetch then search -- while a
// repeated call inside one turn is the model retrying, not a different pattern.
//
// A turn with no tool call has an empty signature and is never clustered: there
// is no procedure to write down in a skill, only an answer.
func signature(tools []string) string {
	var out []string
	for _, t := range tools {
		if len(out) == 0 || out[len(out)-1] != t {
			out = append(out, t)
		}
	}
	return strings.Join(out, ">")
}

// RunCold is the analysis pass.
//
// Reads every record, clusters by signature, and for each cluster that meets
// BOTH thresholds proposes a skill. Idempotent by name: a pattern that already
// has a draft does not get a second one, so `after_turn` does not accumulate a
// draft per turn.
func (e *Engine) RunCold(ctx context.Context) {
	if !e.Cfg.Drafts() {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	records, err := e.records()
	if err != nil {
		e.logf("evolution: could not read the records: %v", err)
		return
	}
	drafts, err := e.drafts()
	if err != nil {
		e.logf("evolution: could not read the drafts: %v", err)
		return
	}
	known := map[string]bool{}
	for _, d := range drafts {
		known[d.Pattern] = true
	}

	for _, p := range patterns(records, e.Cfg.MinTaskCount, e.Cfg.MinSuccessRatio) {
		if known[p.Signature] {
			continue
		}
		if aerr := e.appendPattern(p); aerr != nil {
			e.logf("evolution: could not record the pattern: %v", aerr)
		}
		d := e.draft(ctx, p, records)
		drafts = append(drafts, d)
		e.logf("evolution: drafted %q from %d turns (%.0f%% successful), status %s",
			d.Name, p.Count, p.Ratio*100, d.Status)

		if d.Status == StatusCandidate && e.Cfg.Writes() {
			applied := e.apply(ctx, d, p)
			drafts[len(drafts)-1] = applied
		}
	}
	if werr := e.writeDrafts(drafts); werr != nil {
		e.logf("evolution: could not save the drafts: %v", werr)
	}
}

// patterns clusters records and applies both thresholds.
func patterns(records []Record, minCount int, minRatio float64) []Pattern {
	type acc struct {
		count, ok int
		examples  []string
		last      time.Time
	}
	by := map[string]*acc{}
	for _, r := range records {
		if r.Signature == "" {
			continue
		}
		a := by[r.Signature]
		if a == nil {
			a = &acc{}
			by[r.Signature] = a
		}
		a.count++
		if r.Succeeded {
			a.ok++
			if len(a.examples) < 3 {
				a.examples = append(a.examples, r.Input)
			}
		}
		if r.At.After(a.last) {
			a.last = r.At
		}
	}

	var out []Pattern
	for sig, a := range by {
		ratio := float64(a.ok) / float64(a.count)
		if a.count < minCount || ratio < minRatio {
			continue
		}
		out = append(out, Pattern{
			At: a.last, Signature: sig, Count: a.count,
			Successes: a.ok, Ratio: ratio, Examples: a.examples,
		})
	}
	// Deterministic: two runs over the same records must propose the same
	// drafts in the same order, or `after_turn` produces a different library
	// depending on how the map happened to iterate.
	sort.Slice(out, func(i, j int) bool { return out[i].Signature < out[j].Signature })
	return out
}

// apply is the only path that writes a skill, and every guard lives on it.
func (e *Engine) apply(ctx context.Context, d Draft, p Pattern) Draft {
	// D-2. The approver is asked BEFORE the backup and before the write: a
	// refusal must leave the disk exactly as it was.
	if e.Approver != nil {
		args, _ := json.Marshal(map[string]any{
			"skill": d.Name, "pattern": p.Signature, "turns": p.Count,
		})
		dec, err := e.Approver.Request(ctx, domain.ActionRequest{
			Call: domain.ToolCall{Name: ApprovalAction, Args: args},
		})
		if err != nil {
			// FAIL CLOSED. An approver that cannot be reached is not an
			// approver that said yes, and this is the one write in the whole
			// harness that changes what the agent will do on every later turn.
			d.Status, d.Reason = StatusRefused, "could not reach the approver: "+err.Error()
			e.logf("evolution: %s not applied: %s", d.Name, d.Reason)
			return d
		}
		if !dec.Allowed {
			d.Status, d.Reason = StatusRefused, dec.Reason
			e.logf("evolution: %s refused by %s: %s", d.Name, dec.By, dec.Reason)
			return d
		}
	}

	if err := e.write(d); err != nil {
		d.Status, d.Reason = StatusQuarantined, err.Error()
		e.logf("evolution: %s could not be written: %v", d.Name, err)
		return d
	}
	d.Status = StatusApplied
	e.logf("evolution: applied skill %q", d.Name)
	return d
}

// ApprovalAction is the tool name an approval request carries for an apply.
//
// A name in the same namespace as a tool call rather than a second wire shape:
// the Approver port speaks tool calls, the proxy's endpoint will gate on the
// name, and "evolution.apply" reads as what it is in a gate list beside `shell`.
const ApprovalAction = "evolution.apply"

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

var errNoStateDir = errors.New("no state directory configured")

func (e *Engine) path(name string) (string, error) {
	if e.StateDir == "" {
		return "", errNoStateDir
	}
	if err := os.MkdirAll(e.StateDir, 0o755); err != nil {
		return "", err
	}
	return filepath.Join(e.StateDir, name), nil
}

// append writes one JSON line, fsync'd.
//
// Same durability contract as the transcript, and for a weaker but real reason:
// a scale-to-zero container can be stopped between any two turns, and a record
// lost to a buffer is a turn the analysis pass will never see.
func (e *Engine) append(name string, v any) error {
	p, err := e.path(name)
	if err != nil {
		return err
	}
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

func (e *Engine) appendPattern(p Pattern) error { return e.append(PatternRecordsFile, p) }

// records reads task-records.jsonl.
//
// A malformed line is SKIPPED rather than failing the read. The file is
// append-only and fsync'd, so the only way to get one is a torn write from a
// kill mid-append -- and losing the whole history to the last partial line
// would be the opposite of what the durability is for.
func (e *Engine) records() ([]Record, error) {
	p, err := e.path(TaskRecordsFile)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Record
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var r Record
		if json.Unmarshal([]byte(line), &r) != nil {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

func (e *Engine) drafts() ([]Draft, error) {
	p, err := e.path(DraftsFile)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Draft
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (e *Engine) writeDrafts(d []Draft) error {
	p, err := e.path(DraftsFile)
	if err != nil {
		return err
	}
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// Drafts exposes the stored drafts, for the review surface (R12) and for tests.
func (e *Engine) Drafts() ([]Draft, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.drafts()
}

var _ domain.Learner = (*Engine)(nil)

// unused keeps fmt imported for the draft generator in draft.go.
var _ = fmt.Sprintf
