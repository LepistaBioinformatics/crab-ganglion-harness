// Package tooloutput parks a large tool result on disk so the window can stop
// carrying it.
//
// It exists because of an asymmetry the rest of the storage split does not
// have. The transcript is append-only and complete; the window is derived and
// rebuildable from it. A TOOL RESULT is in neither: the loop writes it to the
// window only -- the member never saw it, so it is not part of the served
// history -- which makes the window its sole copy. Compaction dropping it is
// therefore not a loss of context, it is a loss of the bytes.
//
// That would be a small problem if tool results were small. They are the
// largest thing in a window: `exec` caps one at 64 KiB, which the budget counts
// as one of forty messages. So the content the budget is blindest to is exactly
// the content nothing can recover.
//
// This store is the second copy. It writes under the TURN'S OWN WORKSPACE --
// the directory a command starts in and the root the sandbox ends at -- so the
// path it returns is one the agent can actually read back with the shell.
package tooloutput

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

// DirName is where a conversation's parked results live, relative to the turn's
// workspace.
//
// A dotfile for the reason exec's TmpDirName is one: it stays out of the way
// when the agent lists its own workspace and when the proxy reads the member's
// files. Unlike that directory it is NOT cleared at boot -- a pointer into a
// directory something empties is a pointer that resolves until the next restart
// and dangles after it, which is worse than never having parked the bytes,
// because nothing would report the loss.
const DirName = ".tool-output"

// Store writes under Workspace, or under the turn's project sibling of it.
type Store struct {
	// Workspace is the MAIN workspace directory. A project's is a sibling of
	// it, resolved per turn from the context exactly as the transcript and
	// window stores resolve theirs.
	Workspace string

	mu sync.Mutex
}

func New(workspace string) *Store { return &Store{Workspace: workspace} }

var _ domain.ToolOutputStore = (*Store)(nil)

// Put writes content and returns the path RELATIVE to the turn's workspace.
//
// Relative because that is what the agent can use: a command starts in the
// turn's workspace, so `sed -n '1,200p' .tool-output/<conv>/<call>.txt` works
// as written, from inside the sandbox, with no knowledge of where the container
// mounted anything.
func (s *Store) Put(ctx context.Context, id domain.ConversationID, callID, content string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rel := filepath.Join(DirName, safe(string(id)))
	dir := filepath.Join(domain.ProjectRoot(ctx, s.Workspace), rel)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("tool output dir: %w", err)
	}
	name := safe(callID) + ".txt"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("write tool output: %w", err)
	}
	// Pruned AFTER the write, so a failure here costs old files and never the
	// one just parked. The error is dropped for the same reason: retention is
	// housekeeping, and a turn that produced output must not fail because the
	// sweep of what it replaced did.
	_ = prune(dir, name)
	return filepath.Join(rel, name), nil
}

// Retain is how many parked results one conversation keeps.
//
// IT IS A FUNCTION OF THE WINDOW BUDGET, not a guess. A pointer is only ever
// read out of the window, and the window holds at most WindowBudget messages --
// so a conversation can have at most that many live pointers, and deleting
// anything older than the most recent WindowBudget files cannot orphan one.
// 128 is that bound (40) with room for it to be raised without anyone
// remembering this file.
//
// Deleting at all is a choice worth stating: nothing else in this harness
// removes what it writes, and the transcript specifically may not be shortened.
// The difference is that a transcript entry is the member's, and a parked tool
// result is the agent's scratch -- output the member never saw, kept only so
// the agent can re-read what it ran. The proxy's storage limits do not cover
// this (they are specified and not implemented, and say in their own spec that
// they are "not a retention or clean-up policy"), so unbounded is what the
// alternative means in practice.
const Retain = 128

// prune deletes all but the Retain most recently modified files in dir, and
// never `keep`.
//
// `keep` is not belt and braces. Modification time is the only ordering a
// directory offers, and its granularity is the filesystem's -- on one that
// stamps whole seconds, a burst of parks shares a timestamp and the sort
// between them is arbitrary, so the file this very call just wrote could land
// in the delete set. Naming it makes the one pointer that is certainly live
// safe regardless of what the clock resolves to.
func prune(dir, keep string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	files := make([]os.DirEntry, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			files = append(files, e)
		}
	}
	if len(files) <= Retain {
		return nil
	}
	// One slot is spent on `keep`, which is excluded from the candidates below.
	type aged struct {
		name string
		mod  time.Time
	}
	dated := make([]aged, 0, len(files))
	for _, f := range files {
		if f.Name() == keep {
			continue
		}
		info, err := f.Info()
		if err != nil {
			// Gone between the listing and the stat. Nothing to delete.
			continue
		}
		dated = append(dated, aged{name: f.Name(), mod: info.ModTime()})
	}
	sort.Slice(dated, func(i, j int) bool { return dated[i].mod.After(dated[j].mod) })
	for _, d := range dated[min(Retain-1, len(dated)):] {
		if err := os.Remove(filepath.Join(dir, d.name)); err != nil {
			return err
		}
	}
	return nil
}

// safe is the same rule the transcript and window stores apply to a path
// segment they did not choose. The ingress already refuses a conversation id
// outside [a-z0-9_-] and a call id comes from a provider, not a member -- but a
// store that trusted either would be one refactor away from writing wherever a
// response said.
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
