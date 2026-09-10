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

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

// maxLine is the scanner's buffer. The default 64KiB truncates real assistant
// answers -- a lesson the Hermes runner recorded after its SSE scanner hit
// exactly this.
const maxLine = 8 << 20

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
	return nil
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
