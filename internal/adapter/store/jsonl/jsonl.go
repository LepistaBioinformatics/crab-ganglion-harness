// Package jsonl is the append-only TranscriptStore: one JSON object per line,
// one file per session, on the per-user volume the proxy mounts.
//
// It implements no way to shorten a file. That is FR-9, and it is why this
// package does not export a Compact, a Truncate or a Rewrite: the served
// transcript is the one artifact nothing may shorten. Compaction lives in
// package window, against a different file.
package jsonl

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

// maxLine is the scanner's buffer. The default 64KiB truncates real assistant
// answers -- a lesson the Hermes runner recorded after its SSE scanner hit
// exactly this.
const maxLine = 8 << 20

// Compile-time proof that one Store satisfies both ports. Without these, a
// signature drift shows up as a nil field in the composition root at runtime
// rather than as a build failure here.
var (
	_ domain.TranscriptStore = (*Store)(nil)
	_ domain.Checkpointer    = (*Store)(nil)
)

// Store writes transcripts under Root.
type Store struct {
	Root string
	// Projects, when set, is the parent of the per-project subtrees. See dir.
	Projects string

	mu sync.Mutex
}

func New(root string) *Store { return &Store{Root: root} }

func (s *Store) path(ctx context.Context, id domain.ConversationID) string {
	return filepath.Join(s.dir(ctx), safe(string(id))+".jsonl")
}

// Append adds one message. The write is O(1) in the file's size, which is what
// makes an append-only transcript affordable for a long conversation.
func (s *Store) Append(ctx context.Context, id domain.ConversationID, m domain.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.MkdirAll(s.dir(ctx), 0o755); err != nil {
		return fmt.Errorf("transcript dir: %w", err)
	}
	line, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("marshal message: %w", err)
	}
	f, err := os.OpenFile(s.path(ctx, id), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open transcript: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("write transcript: %w", err)
	}
	// D-1. Without this the bytes sit in the page cache, which survives the
	// process and the container but not the host. Measured at 887us on the
	// volume these containers mount -- three per ordinary turn, against seconds
	// of provider latency, so there is nothing here to batch (spec OQ-1).
	if err := f.Sync(); err != nil {
		return fmt.Errorf("sync transcript: %w", err)
	}
	return nil
}

// partialPath is the sidecar holding an in-flight answer.
//
// It is a SEPARATE FILE, and that is the whole design (design.md D-2/D-3):
// the transcript stays byte-identical to what it was, so internal/history's
// existing parser cannot see a checkpoint and render it as a message. Marking
// partials inside the JSONL was the obvious alternative and it fails exactly
// there -- an older reader would show the same growing answer once per
// checkpoint.
func (s *Store) partialPath(ctx context.Context, id domain.ConversationID) string {
	return filepath.Join(s.dir(ctx), safe(string(id))+".partial.json")
}

// Partial is an answer that was still streaming.
type Partial struct {
	// AnswersAt is the created_at of the user message this turn is answering.
	// It is what decides whether the sidecar is live, with no turn id needed:
	// a sidecar is stale once the transcript holds an assistant message at or
	// after this instant.
	AnswersAt time.Time `json:"answers_at"`
	Content   string    `json:"content"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Checkpoint durably records the answer produced so far.
//
// Rewriting this file is fine -- it is derived, like the context window. The
// append-only invariant belongs to the transcript, and the transcript is not
// this file.
func (s *Store) Checkpoint(ctx context.Context, id domain.ConversationID, answersAt time.Time, content string) error {
	p := Partial{AnswersAt: answersAt, Content: content, UpdatedAt: time.Now()}
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.MkdirAll(s.dir(ctx), 0o755); err != nil {
		return fmt.Errorf("transcript dir: %w", err)
	}
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dir(ctx), ".partial-*")
	if err != nil {
		return fmt.Errorf("temp partial: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("write partial: %w", err)
	}
	// Sync before the rename, or the rename can land with no content behind it.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync partial: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), s.partialPath(ctx, id))
}

// ClearPartial removes the sidecar. Called after the real message is appended.
//
// The crash window between that append and this call is exactly what
// Partial.AnswersAt exists to make harmless: a reader finding both sees an
// assistant message at or after AnswersAt and ignores the sidecar.
func (s *Store) ClearPartial(ctx context.Context, id domain.ConversationID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := os.Remove(s.partialPath(ctx, id))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// ReadPartial returns the sidecar when it is LIVE -- when the transcript holds
// no assistant message that answers it.
//
// ok is false for "no sidecar" and for "superseded"; both mean a reader has
// nothing to add, and distinguishing them would invite a caller to treat a
// stale partial as recoverable.
func (s *Store) ReadPartial(ctx context.Context, id domain.ConversationID) (Partial, bool, error) {
	b, err := os.ReadFile(s.partialPath(ctx, id))
	if os.IsNotExist(err) {
		return Partial{}, false, nil
	}
	if err != nil {
		return Partial{}, false, err
	}
	var p Partial
	if err := json.Unmarshal(b, &p); err != nil {
		// A half-written sidecar is not worth failing a history read over. The
		// atomic rename should make this unreachable; treating it as "nothing
		// to recover" is the safe direction.
		return Partial{}, false, nil
	}

	msgs, err := s.Read(ctx, id)
	if err != nil {
		return Partial{}, false, err
	}
	for _, m := range msgs {
		if m.Role == domain.RoleAssistant && !m.CreatedAt.Before(p.AnswersAt) {
			return Partial{}, false, nil // the turn finished; sidecar is stale
		}
	}
	return p, true, nil
}

// Read returns the whole transcript. A missing file is an empty conversation,
// not an error -- the first turn of every session reads before it writes.
func (s *Store) Read(ctx context.Context, id domain.ConversationID) ([]domain.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	f, err := os.Open(s.path(ctx, id))
	if os.IsNotExist(err) {
		return []domain.Message{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open transcript: %w", err)
	}
	defer f.Close()

	out := []domain.Message{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), maxLine)
	for sc.Scan() {
		b := sc.Bytes()
		if len(b) == 0 {
			continue
		}
		var m domain.Message
		if err := json.Unmarshal(b, &m); err != nil {
			// A corrupt line must not hide the rest of the conversation.
			continue
		}
		out = append(out, m)
	}
	if err := sc.Err(); err != nil {
		return out, fmt.Errorf("scan transcript: %w", err)
	}
	return out, nil
}

// RecoverPartials folds interrupted answers into the transcript and drops the
// stale sidecars, returning how many of each it handled.
//
// Run at start, which under scale-to-zero is "the turn after the crash". It is
// what makes recovery PERMANENT: until it runs, a live sidecar is visible only
// because readers fold it on the fly, and it would never enter the context the
// model sees on the next turn. After it runs, the interrupted answer is an
// ordinary message and nothing special reads it.
//
// The partial's text is appended as-is, with no marker. It is what the member
// watched appear; an answer cut mid-sentence already reads as cut, and a marker
// field would be invisible to internal/history's parser anyway (H-2).
func (s *Store) RecoverPartials(ctx context.Context) (folded, dropped int, err error) {
	f, d, err := s.recoverIn(ctx)
	folded, dropped = f, d
	if err != nil {
		return folded, dropped, err
	}
	// EVERY project, not only the main workspace. A crash does not care which
	// project the turn belonged to, and a partial nobody folds is an answer the
	// member watched appear and then never sees again.
	//
	// Read from disk rather than from a project list, because this runs at boot
	// and the harness has no list -- the proxy owns it, and a directory that
	// exists is exactly the set that could hold a sidecar.
	if s.Projects == "" {
		return folded, dropped, nil
	}
	projects, rerr := os.ReadDir(s.Projects)
	if rerr != nil {
		return folded, dropped, nil
	}
	for _, p := range projects {
		if !p.IsDir() {
			continue
		}
		pf, pd, perr := s.recoverIn(domain.WithProject(ctx, p.Name()))
		if perr != nil {
			// One unreadable project must not stop the rest, for the reason
			// one unreadable sidecar must not: this is boot.
			continue
		}
		folded, dropped = folded+pf, dropped+pd
	}
	return folded, dropped, nil
}

// recoverIn does one directory: the main workspace's, or one project's.
func (s *Store) recoverIn(ctx context.Context) (folded, dropped int, err error) {
	entries, rerr := os.ReadDir(s.dir(ctx))
	if os.IsNotExist(rerr) {
		return 0, 0, nil
	}
	if rerr != nil {
		return 0, 0, fmt.Errorf("scan sessions: %w", rerr)
	}

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".partial.json") {
			continue
		}
		id := domain.ConversationID(strings.TrimSuffix(name, ".partial.json"))

		p, live, perr := s.ReadPartial(ctx, id)
		if perr != nil {
			// One unreadable sidecar must not stop the others from being
			// recovered -- this runs at boot, and a boot that fails on a
			// leftover file is worse than the leftover.
			continue
		}
		if !live {
			if s.ClearPartial(ctx, id) == nil {
				dropped++
			}
			continue
		}
		if strings.TrimSpace(p.Content) == "" {
			// A checkpoint that caught nothing. Dropping it is not data loss.
			if s.ClearPartial(ctx, id) == nil {
				dropped++
			}
			continue
		}
		msg := domain.Message{
			Role:    domain.RoleAssistant,
			Content: p.Content,
			// The instant the checkpoint was taken, not now: this message
			// belongs to the turn that died, and dating it now would place it
			// after messages that came later.
			CreatedAt: p.UpdatedAt,
		}
		if aerr := s.Append(ctx, id, msg); aerr != nil {
			continue // leave the sidecar; the next start tries again
		}
		if s.ClearPartial(ctx, id) == nil {
			folded++
		}
	}
	return folded, dropped, nil
}

// safe keeps a session key from escaping Root. Keys come from the proxy, but a
// store that can be made to write outside its own directory is a store that
// will be, eventually.
func safe(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			out = append(out, r)
		default:
			out = append(out, '_')
		}
	}
	if len(out) == 0 {
		return "_"
	}
	return string(out)
}

// dir is the directory this turn reads and writes.
//
// A turn with no project gets Root, byte for byte the path this store has always
// used -- which is the regression bar for the whole feature, because getting it
// wrong orphans every existing transcript silently.
//
// The leaf name is taken from Root rather than configured separately, so the
// two can never disagree: <workspace>/sessions becomes
// <workspace>/projects/<id>/sessions, and the same store type serves windows
// without knowing it.
//
// safe() is applied to the project too. The ingress already refuses anything
// outside [a-z0-9_-], and a store that trusted that would be one refactor away
// from writing wherever a header said.
func (s *Store) dir(ctx context.Context) string {
	p := domain.ProjectFrom(ctx)
	if p == "" || s.Projects == "" {
		return s.Root
	}
	return filepath.Join(s.Projects, safe(p), filepath.Base(s.Root))
}
