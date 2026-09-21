// Package window is the ContextStore: the derived context the model sees.
//
// Unlike the transcript, this file is rewritten freely -- that is the whole
// point of the split. Losing it costs the next turn some context and can be
// rebuilt from the transcript; it can never cost the member their history.
//
// "Can be rebuilt from the transcript" was a claim this package made and did not
// keep: a missing window was an EMPTY window, so the agent answered a
// conversation it could not see. That was survivable while the only way to lose
// a window was to delete one. It stopped being survivable with migration, where
// every conversation moved from picoclaw arrives with a full transcript and no
// window at all -- the member would see their history on screen and the agent
// would answer as though the conversation had just begun. See Load.
package window

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

type Store struct {
	Root string
	// Workspace, when set, is the MAIN workspace directory -- the one a project's
	// is a sibling of. Empty means this store serves no projects, which is every
	// deployment that has none. See dir.
	Workspace string
	// Transcript, when set, is what a missing window is rebuilt from. Nil keeps
	// the old behaviour (a miss is an empty window), which is what every test
	// that does not care about rebuilding gets.
	//
	// The narrow port, not the concrete store: this package must not import
	// another adapter (AR-4), and reading messages is all it needs.
	Transcript domain.TranscriptStore
	// SeedBudget bounds a rebuilt window, and is the loop's own WindowBudget.
	// Seeding an unbounded transcript would hand the first completion after a
	// migration a context the compaction has not seen yet.
	SeedBudget int

	mu sync.Mutex
}

func New(root string) *Store { return &Store{Root: root} }

func (s *Store) path(ctx context.Context, id domain.ConversationID) string {
	return filepath.Join(s.dir(ctx), safe(string(id))+".window.json")
}

func (s *Store) Load(ctx context.Context, id domain.ConversationID) (domain.Window, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	b, err := os.ReadFile(s.path(ctx, id))
	if os.IsNotExist(err) {
		return s.rebuild(ctx, id), nil
	}
	if err != nil {
		return domain.Window{}, fmt.Errorf("read window: %w", err)
	}
	var w domain.Window
	if err := json.Unmarshal(b, &w); err != nil {
		// A corrupt window is recoverable by construction: rebuild rather than
		// failing the turn. The transcript still holds everything.
		return s.rebuild(ctx, id), nil
	}
	return w, nil
}

// rebuild reconstructs a window from the transcript, or returns an empty one.
//
// The two cases it serves look different and are the same: a conversation
// MIGRATED from picoclaw, which has a transcript and has never had a window, and
// a window lost to a deleted file or a corrupt write. Both are "the durable
// record survived and the derived one did not", which is exactly the split this
// package exists to make survivable.
//
// A read failure yields an EMPTY window rather than an error. Failing the turn
// would take an agent off the air over a derived file, and the empty window is
// the behaviour this had before rebuilding existed -- degrading to it is the
// safe direction.
//
// Only the tail is taken. The whole transcript would hand the first completion a
// context the loop's compaction has not run on yet, and a migrated conversation
// can be thousands of messages long.
func (s *Store) rebuild(ctx context.Context, id domain.ConversationID) domain.Window {
	if s.Transcript == nil {
		return domain.Window{}
	}
	msgs, err := s.Transcript.Read(ctx, id)
	if err != nil || len(msgs) == 0 {
		return domain.Window{}
	}
	left := 0
	if s.SeedBudget > 0 && len(msgs) > s.SeedBudget {
		left = len(msgs) - s.SeedBudget
		msgs = msgs[len(msgs)-s.SeedBudget:]
	}
	w := domain.Window{Messages: conversational(msgs)}
	if left > 0 {
		// SAID, not silently done. A rebuilt window used to begin mid-conversation
		// with nothing marking the cut, so the agent read a seed as though it were
		// the whole exchange -- the same silence compaction is being taught to
		// break, arriving from the other direction.
		//
		// Counted HERE rather than read off a compaction marker in the transcript.
		// A marker records what the LIVE window dropped, against a history this
		// rebuild is not reconstructing: it seeds a fresh tail and leaves a
		// different amount behind. Carrying the marker's number over would state a
		// count that was true of a window this one is replacing.
		w.Summary = fmt.Sprintf(
			"[%d earlier messages are not in this window. The full transcript is preserved "+
				"and is searchable.]", left)
	}
	return w
}

// conversational keeps the part of a transcript a provider will accept, which
// is what was SAID: the tool plumbing is dropped.
//
// The transcript is the served history, and its tool_calls are a DISPLAY
// MARKER -- crab-shell-proxy reads them to render an iteration as a step. The
// results answering them were never written there, because the member never saw
// one. Carried into a window as they are, they are a call with no reply, and a
// provider rejects the whole request for it ("insufficient tool messages
// following tool_calls message") on every later turn of that conversation --
// the same permanent failure dropOrphanTools exists for, arriving from the
// other direction.
//
// A picoclaw transcript brings the mirror image: it logs `tool` entries inline,
// and the seed takes a TAIL, so the cut lands wherever it lands. Dropping both
// halves is one rule instead of a pair-matching pass, and it loses nothing the
// transcript keeps: this is a seed for the next turn's context, not the record.
func conversational(msgs []domain.Message) []domain.Message {
	out := make([]domain.Message, 0, len(msgs))
	for _, m := range msgs {
		if m.Role == domain.RoleTool {
			continue
		}
		// An events-only entry is dropped whole rather than emptied. It has no
		// content, so what survives the strip below is an assistant message
		// saying nothing -- which some providers reject outright and none can
		// use. Events are written for the member; the model already knows what
		// it did, because the tool results were in the window at the time.
		if m.Content == "" && len(m.Events) > 0 {
			continue
		}
		m.ToolCalls = nil
		m.ToolCallID = ""
		m.Events = nil
		out = append(out, m)
	}
	return out
}

// Save writes atomically. A window truncated by a crash mid-write would be read
// back as corrupt on the next turn -- survivable, but needlessly.
func (s *Store) Save(ctx context.Context, id domain.ConversationID, w domain.Window) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.MkdirAll(s.dir(ctx), 0o755); err != nil {
		return fmt.Errorf("window dir: %w", err)
	}
	b, err := json.Marshal(w)
	if err != nil {
		return fmt.Errorf("marshal window: %w", err)
	}
	final := s.path(ctx, id)
	tmp, err := os.CreateTemp(s.dir(ctx), ".window-*")
	if err != nil {
		return fmt.Errorf("temp window: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("write window: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close window: %w", err)
	}
	if err := os.Rename(tmp.Name(), final); err != nil {
		return fmt.Errorf("commit window: %w", err)
	}
	return nil
}

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
// <workspace>-<id>/sessions -- a SIBLING of the main workspace, which is how
// picoclaw lays a project out and therefore how the proxy reads one -- and the
// same store type serves windows without knowing it.
//
// safe() is applied to the project too. The ingress already refuses anything
// outside [a-z0-9_-], and a store that trusted that would be one refactor away
// from writing wherever a header said.
func (s *Store) dir(ctx context.Context) string {
	p := domain.ProjectFrom(ctx)
	if p == "" || s.Workspace == "" {
		return s.Root
	}
	return filepath.Join(domain.ProjectWorkspace(s.Workspace, safe(p)), filepath.Base(s.Root))
}
