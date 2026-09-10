// Package window is the ContextStore: the derived context the model sees.
//
// Unlike the transcript, this file is rewritten freely -- that is the whole
// point of the split. Losing it costs the next turn some context and can be
// rebuilt from the transcript; it can never cost the member their history.
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
	// Projects, when set, is the parent of the per-project subtrees. See dir.
	Projects string

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
		return domain.Window{}, nil
	}
	if err != nil {
		return domain.Window{}, fmt.Errorf("read window: %w", err)
	}
	var w domain.Window
	if err := json.Unmarshal(b, &w); err != nil {
		// A corrupt window is recoverable by construction: start empty rather
		// than failing the turn. The transcript still holds everything.
		return domain.Window{}, nil
	}
	return w, nil
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
