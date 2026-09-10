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

	mu sync.Mutex
}

func New(root string) *Store { return &Store{Root: root} }

func (s *Store) path(id domain.ConversationID) string {
	return filepath.Join(s.Root, safe(string(id))+".window.json")
}

func (s *Store) Load(_ context.Context, id domain.ConversationID) (domain.Window, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	b, err := os.ReadFile(s.path(id))
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
func (s *Store) Save(_ context.Context, id domain.ConversationID, w domain.Window) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := os.MkdirAll(s.Root, 0o755); err != nil {
		return fmt.Errorf("window dir: %w", err)
	}
	b, err := json.Marshal(w)
	if err != nil {
		return fmt.Errorf("marshal window: %w", err)
	}
	final := s.path(id)
	tmp, err := os.CreateTemp(s.Root, ".window-*")
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
