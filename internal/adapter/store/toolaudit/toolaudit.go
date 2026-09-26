// Package toolaudit keeps one durable record per tool call: the full command,
// and the output it produced.
//
// It exists because neither of those is recoverable from anything else the
// harness writes. The transcript records what the MEMBER saw, and they never
// saw a tool result -- `loop.go` writes it to the window alone. The window is
// derived and rewritten, and compaction elides its results down to a pointer.
// And the arguments that reach the transcript are a display string capped at
// 200 runes, deliberately, so that an event log does not grow by everything the
// agent ever wrote.
//
// So: the capped string stays on the event and is what the member scans; the
// whole of it lives here and is fetched only when somebody asks to see it.
//
// WHY NOT .tool-output. That directory is the agent's scratch -- `Put` returns a
// path relative to the turn workspace and the loop hands it to the model, where
// `elide` leaves it in the window after compaction. Two policies follow from
// that and neither belongs to an audit record: its files cannot be renamed,
// because a conversation resumed weeks later may still hold a pointer at one,
// so they cannot be compressed; and it deletes past `Retain`, which is what an
// audit record must never do. Nothing here is ever named to the model, so these
// files can be compressed and are never removed.
//
// NOT TAMPER-EVIDENT. This runs inside the container and writes under the
// agent's own bind, and the ganglion's only tool is `/bin/sh -c` with no path
// restriction. It is exactly as trustworthy as the transcript in `sessions/`
// beside it -- which is already what the member reads.
package toolaudit

import (
	"compress/gzip"
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

// DirName is where a conversation's records live, relative to the turn's
// workspace. A dotfile for the reason .tool-output is one: it stays out of the
// way when the agent lists its workspace and when the proxy reads the member's
// files.
//
// THE PROXY READS THIS NAME TOO, from outside the container. It is half of a
// contract no compiler checks; the other half is the JSON field names below.
// Drift would not fail anything -- it would serve an empty sheet forever.
const DirName = ".tool-audit"

// CompressAfter is how old a record must be before the boot sweep gzips it.
//
// A CONSTANT, NOT CONFIGURATION. Nobody has asked to turn this, and a knob that
// exists before its first use is a knob to maintain and a value to get wrong.
//
// Seven days is chosen against the one thing that could go wrong, which is not
// storage: it is compressing a record somebody is about to read. Nothing here is
// ever named to the model, so the only reader is a member opening the sheet, and
// the route decompresses transparently -- so being wrong costs a few
// milliseconds rather than a broken feature. A week is simply long enough that
// the ordinary case reads an uncompressed file.
const CompressAfter = 7 * 24 * time.Hour

// Record is the on-disk shape, and the second half of the contract with the
// proxy. Every name here is read by `internal/history` on the other side.
type Record struct {
	// ID is the harness-minted id, which is also the file's name.
	ID string `json:"id"`
	// CallID is the PROVIDER's id, kept for correlation and never used as a key
	// -- it may be empty, which is why this store mints its own.
	CallID string `json:"call_id,omitempty"`
	Name   string `json:"name,omitempty"`
	// Arguments is the raw call arguments, UNCAPPED, as the provider sent them.
	// This is the one place they survive in full.
	Arguments string `json:"arguments,omitempty"`
	// Output is absent until the call returns, and absent forever if the turn
	// died inside it. That absence is information and is rendered as such.
	Output    string `json:"output,omitempty"`
	Status    string `json:"status,omitempty"`
	Detail    string `json:"detail,omitempty"`
	StartedAt string `json:"started_at,omitempty"`
	EndedAt   string `json:"ended_at,omitempty"`
}

type Store struct {
	// Workspace is the MAIN workspace directory; a project's is a sibling of it,
	// resolved per turn exactly as the transcript and window stores resolve
	// theirs.
	Workspace string
	// Now is injected so a test can age a record without sleeping.
	Now func() time.Time

	mu sync.Mutex
}

func New(workspace string) *Store {
	return &Store{Workspace: workspace, Now: time.Now}
}

var _ domain.ToolAuditStore = (*Store)(nil)

func (s *Store) now() time.Time {
	if s.Now == nil {
		return time.Now()
	}
	return s.Now()
}

func (s *Store) dir(ctx context.Context, id domain.ConversationID) string {
	return filepath.Join(domain.ProjectRoot(ctx, s.Workspace), DirName, safe(string(id)))
}

// Put records a call that is ABOUT TO RUN.
//
// Before rather than after, and for the same reason the loop adds its event
// before running the tool: the call that kills a turn is the one most worth
// having a record of, and a record written only on success would be missing
// exactly then.
func (s *Store) Put(ctx context.Context, id domain.ConversationID, auditID, name, arguments string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.write(ctx, id, Record{
		ID:        auditID,
		Name:      name,
		Arguments: arguments,
		StartedAt: s.now().UTC().Format(time.RFC3339Nano),
	})
}

// Complete rewrites the record with what the call returned.
//
// A Complete with no prior Put is a NO-OP rather than a record with an output
// and no command. Half a record is worse than none: it would read as a call
// that ran with no arguments.
func (s *Store) Complete(ctx context.Context, id domain.ConversationID, auditID, output, status, detail string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	rec, err := s.read(ctx, id, auditID)
	if err != nil {
		return err
	}
	rec.Output = output
	rec.Status = status
	if detail != "" {
		rec.Detail = detail
	}
	rec.EndedAt = s.now().UTC().Format(time.RFC3339Nano)
	return s.write(ctx, id, rec)
}

func (s *Store) read(ctx context.Context, id domain.ConversationID, auditID string) (Record, error) {
	raw, err := os.ReadFile(filepath.Join(s.dir(ctx, id), safe(auditID)+".json"))
	if err != nil {
		return Record{}, err
	}
	var rec Record
	if err := json.Unmarshal(raw, &rec); err != nil {
		return Record{}, fmt.Errorf("tool audit record: %w", err)
	}
	return rec, nil
}

// write is temp-file-plus-rename, so a reader never sees half a record. The
// proxy reads these from outside the container while turns are running.
func (s *Store) write(ctx context.Context, id domain.ConversationID, rec Record) error {
	dir := s.dir(ctx, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("tool audit dir: %w", err)
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("tool audit encode: %w", err)
	}
	final := filepath.Join(dir, safe(rec.ID)+".json")
	tmp, err := os.CreateTemp(dir, ".write-*")
	if err != nil {
		return fmt.Errorf("tool audit temp: %w", err)
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return fmt.Errorf("write tool audit: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Rename(tmp.Name(), final); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// Sweep gzips every record older than CompressAfter, in the main workspace AND
// in every project workspace beside it.
//
// AT BOOT, which is frequent rather than rare: containers scale to zero, so a
// boot is the ordinary event in this harness's life. A timer would be a second
// thing to schedule, stop and test for the same effect.
//
// THE SIBLINGS ARE SWEPT HERE AND NOT BY THEMSELVES. A project's workspace is
// `workspace-<id>`, a sibling of the main one (domain.ProjectWorkspace), and
// ONE process serves every project -- `ProjectRoot` picks the root per turn from
// the context, it does not start a second harness. So a sweep of the main
// workspace alone would leave every project's records uncompressed forever,
// which is exactly the growth this exists to bound. This was written that way
// first, with a comment claiming the projects swept themselves.
//
// It returns how many it compressed, for the caller to log. An error on one
// record does not stop the others: this is housekeeping, and a directory with
// one unreadable file must still be swept.
func (s *Store) Sweep(workspace string) int {
	n := sweepRoot(workspace, s.now().Add(-CompressAfter))
	siblings, err := os.ReadDir(filepath.Dir(workspace))
	if err != nil {
		return n
	}
	for _, e := range siblings {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), domain.ProjectWorkspacePrefix) {
			continue
		}
		n += sweepRoot(filepath.Join(filepath.Dir(workspace), e.Name()), s.now().Add(-CompressAfter))
	}
	return n
}

func sweepRoot(root string, cutoff time.Time) int {
	conversations, err := os.ReadDir(filepath.Join(root, DirName))
	if err != nil {
		return 0
	}
	n := 0
	for _, conv := range conversations {
		if !conv.IsDir() {
			continue
		}
		dir := filepath.Join(root, DirName, conv.Name())
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			// `.json` only: a `.json.gz` is already done, and compressing one
			// again is how a sweep grows its own work every boot.
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			info, err := e.Info()
			if err != nil || info.ModTime().After(cutoff) {
				continue
			}
			if compress(filepath.Join(dir, e.Name()), info.ModTime()) == nil {
				n++
			}
		}
	}
	return n
}

// compress writes <name>.gz, carries the original's mtime onto it, and only
// then removes the original.
//
// THE MTIME IS CARRIED, and that is not tidiness. "Older than CompressAfter" is
// read off the file, so a compressed copy stamped `now` would be a record that
// claims to be new -- and the next sweep would have to re-derive the truth from
// somewhere else. Carrying it keeps "not edited since" true of the file after
// the sweep, which is what the request asked for.
//
// Rename-then-unlink, so a crash mid-sweep leaves the original or the compressed
// copy and never neither.
func compress(path string, mod time.Time) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".gz-*")
	if err != nil {
		return err
	}
	zw := gzip.NewWriter(tmp)
	if _, err := zw.Write(raw); err != nil {
		zw.Close()
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := zw.Close(); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	final := path + ".gz"
	if err := os.Rename(tmp.Name(), final); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	// Best effort: a filesystem that refuses the stamp costs this record an
	// early re-read on some future sweep, not correctness.
	_ = os.Chtimes(final, mod, mod)
	return os.Remove(path)
}

// safe is the rule every store here applies to a path segment it did not
// choose. A conversation id is refused at the ingress and an audit id is minted
// by this harness -- but a store that trusted either would be one refactor away
// from writing wherever a value said.
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
