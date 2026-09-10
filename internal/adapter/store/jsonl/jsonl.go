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

	mu sync.Mutex
}

func New(root string) *Store { return &Store{Root: root} }

func (s *Store) path(key domain.SessionKey) string {
	return filepath.Join(s.Root, safe(string(key))+".jsonl")
}

// Append adds one message. The write is O(1) in the file's size, which is what
// makes an append-only transcript affordable for a long conversation.
func (s *Store) Append(_ context.Context, key domain.SessionKey, m domain.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.MkdirAll(s.Root, 0o755); err != nil {
		return fmt.Errorf("transcript dir: %w", err)
	}
	line, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("marshal message: %w", err)
	}
	f, err := os.OpenFile(s.path(key), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
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
func (s *Store) partialPath(key domain.SessionKey) string {
	return filepath.Join(s.Root, safe(string(key))+".partial.json")
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
func (s *Store) Checkpoint(_ context.Context, key domain.SessionKey, answersAt time.Time, content string) error {
	p := Partial{AnswersAt: answersAt, Content: content, UpdatedAt: time.Now()}
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.MkdirAll(s.Root, 0o755); err != nil {
		return fmt.Errorf("transcript dir: %w", err)
	}
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.Root, ".partial-*")
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
	return os.Rename(tmp.Name(), s.partialPath(key))
}

// ClearPartial removes the sidecar. Called after the real message is appended.
//
// The crash window between that append and this call is exactly what
// Partial.AnswersAt exists to make harmless: a reader finding both sees an
// assistant message at or after AnswersAt and ignores the sidecar.
func (s *Store) ClearPartial(_ context.Context, key domain.SessionKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := os.Remove(s.partialPath(key))
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
func (s *Store) ReadPartial(ctx context.Context, key domain.SessionKey) (Partial, bool, error) {
	b, err := os.ReadFile(s.partialPath(key))
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

	msgs, err := s.Read(ctx, key)
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
func (s *Store) Read(_ context.Context, key domain.SessionKey) ([]domain.Message, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	f, err := os.Open(s.path(key))
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
