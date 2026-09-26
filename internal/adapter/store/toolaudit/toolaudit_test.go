package toolaudit

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

func load(t *testing.T, dir, name string) Record {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, DirName, "conv", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	var rec Record
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
	return rec
}

func TestPutThenCompleteRoundTrips(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	ctx := context.Background()

	if err := s.Put(ctx, "conv", "a1", "sh", `{"command":"ls -la"}`); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := s.Complete(ctx, "conv", "a1", "total 0\n", domain.EventOK, ""); err != nil {
		t.Fatalf("complete: %v", err)
	}

	rec := load(t, dir, "a1.json")
	if rec.Arguments != `{"command":"ls -la"}` {
		t.Errorf("arguments = %q, want the whole thing uncapped", rec.Arguments)
	}
	if rec.Output != "total 0\n" {
		t.Errorf("output = %q", rec.Output)
	}
	if rec.Status != domain.EventOK {
		t.Errorf("status = %q", rec.Status)
	}
	if rec.StartedAt == "" || rec.EndedAt == "" {
		t.Errorf("both timestamps should be set, got %q / %q", rec.StartedAt, rec.EndedAt)
	}
}

// AC-1.3.1. The call that kills a turn is the one most worth auditing, so the
// command is written BEFORE the tool runs -- a record written only on success
// would be missing exactly then.
func TestPutAloneLeavesTheCommandWithNoOutput(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	if err := s.Put(context.Background(), "conv", "a1", "sh", `{"command":"sleep 9000"}`); err != nil {
		t.Fatalf("put: %v", err)
	}
	rec := load(t, dir, "a1.json")
	if rec.Arguments == "" {
		t.Error("a turn that died inside a tool must still say what it was running")
	}
	if rec.Output != "" || rec.Status != "" {
		t.Errorf("nothing returned, so nothing should be recorded: %q / %q", rec.Output, rec.Status)
	}
}

// Half a record reads as a call that ran with no arguments, which is worse than
// no record at all.
func TestCompleteWithoutPutIsANoOp(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	if err := s.Complete(context.Background(), "conv", "ghost", "out", domain.EventOK, ""); err == nil {
		t.Error("want an error rather than a record with an output and no command")
	}
	if _, err := os.Stat(filepath.Join(dir, DirName, "conv", "ghost.json")); !os.IsNotExist(err) {
		t.Error("no record should have been created")
	}
}

// AC-1.4.1, and the reason this store mints its own id. The OpenAI adapter sets
// ToolCall.ID only when the stream carried one, with no fallback, so an idless
// provider would key every call in a conversation onto safe("") == "_".
func TestTwoCallsNeverShareARecord(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	ctx := context.Background()
	for _, id := range []string{"1727300000000000000-00", "1727300000000000000-01"} {
		if err := s.Put(ctx, "conv", id, "sh", "{}"); err != nil {
			t.Fatalf("put %s: %v", id, err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(dir, DirName, "conv"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Errorf("got %d records, want 2 -- one per call", len(entries))
	}
}

func TestPathSegmentsAreSanitised(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	if err := s.Put(context.Background(), "../../etc", "../../passwd", "sh", "{}"); err != nil {
		t.Fatalf("put: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, DirName, "______etc", "______passwd.json")); err != nil {
		t.Errorf("both segments should have been flattened: %v", err)
	}
}

func aged(t *testing.T, dir, name string, age time.Duration) time.Time {
	t.Helper()
	path := filepath.Join(dir, DirName, "conv", name)
	when := time.Now().Add(-age)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
	return when
}

func TestSweepCompressesOldRecordsAndKeepsTheirMtime(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	ctx := context.Background()
	if err := s.Put(ctx, "conv", "old", "sh", `{"command":"echo old"}`); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(ctx, "conv", "fresh", "sh", `{"command":"echo fresh"}`); err != nil {
		t.Fatal(err)
	}
	when := aged(t, dir, "old.json", CompressAfter+time.Hour)

	if n := s.Sweep(dir); n != 1 {
		t.Fatalf("compressed %d, want 1", n)
	}

	convDir := filepath.Join(dir, DirName, "conv")
	if _, err := os.Stat(filepath.Join(convDir, "old.json")); !os.IsNotExist(err) {
		t.Error("the original should be gone once the compressed copy landed")
	}
	if _, err := os.Stat(filepath.Join(convDir, "fresh.json")); err != nil {
		t.Error("a fresh record must be left alone")
	}

	// AC-2.2. "Older than CompressAfter" is read off the file, so a compressed
	// copy stamped `now` would claim to be new and the next sweep would have no
	// way back to the truth.
	info, err := os.Stat(filepath.Join(convDir, "old.json.gz"))
	if err != nil {
		t.Fatal(err)
	}
	if diff := info.ModTime().Sub(when); diff > time.Second || diff < -time.Second {
		t.Errorf("mtime moved by %v; the compressed copy must keep the original's", diff)
	}

	// And it has to still be the record.
	f, err := os.Open(filepath.Join(convDir, "old.json.gz"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	var rec Record
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatalf("the compressed copy should decode: %v", err)
	}
	if rec.Arguments != `{"command":"echo old"}` {
		t.Errorf("arguments = %q", rec.Arguments)
	}
}

// A sweep that re-read its own output would grow its work every boot, and would
// eventually stamp a .gz.gz.
func TestSweepIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	if err := s.Put(context.Background(), "conv", "old", "sh", "{}"); err != nil {
		t.Fatal(err)
	}
	aged(t, dir, "old.json", CompressAfter+time.Hour)

	if n := s.Sweep(dir); n != 1 {
		t.Fatalf("first sweep compressed %d, want 1", n)
	}
	if n := s.Sweep(dir); n != 0 {
		t.Errorf("second sweep compressed %d, want 0", n)
	}
	if _, err := os.Stat(filepath.Join(dir, DirName, "conv", "old.json.gz.gz")); !os.IsNotExist(err) {
		t.Error("a compressed record must never be compressed again")
	}
}

func TestSweepOnAnAbsentDirectoryIsHarmless(t *testing.T) {
	if n := New(t.TempDir()).Sweep(t.TempDir()); n != 0 {
		t.Errorf("compressed %d on a store that never wrote anything", n)
	}
}

// A PROJECT'S RECORDS ARE SWEPT TOO, and nothing else would ever reach them.
//
// A project workspace is `workspace-<id>`, a SIBLING of the main one, and one
// process serves every project -- ProjectRoot picks the root per turn from the
// context rather than starting a second harness. The first cut of Sweep looked
// only at the workspace it was handed and carried a comment claiming each
// project swept itself on its own boot. No such boot happens, so a project's
// records would have grown forever.
func TestSweepReachesProjectWorkspaces(t *testing.T) {
	root := t.TempDir()
	main := filepath.Join(root, "workspace")
	project := filepath.Join(root, domain.ProjectWorkspacePrefix+"legal")
	if err := os.MkdirAll(main, 0o755); err != nil {
		t.Fatal(err)
	}

	s := New(main)
	ctx := domain.WithProject(context.Background(), "legal")
	if err := s.Put(ctx, "conv", "old", "sh", `{"command":"in a project"}`); err != nil {
		t.Fatal(err)
	}
	// It landed in the sibling, which is the premise of the test.
	path := filepath.Join(project, DirName, "conv", "old.json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the record should be under the project's own workspace: %v", err)
	}
	when := time.Now().Add(-(CompressAfter + time.Hour))
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}

	if n := s.Sweep(main); n != 1 {
		t.Fatalf("compressed %d, want the project's one record", n)
	}
	if _, err := os.Stat(filepath.Join(project, DirName, "conv", "old.json.gz")); err != nil {
		t.Errorf("the project's record was not compressed: %v", err)
	}
}

// A directory beside the workspace that is not a project workspace is not one
// to sweep, whatever it holds.
func TestSweepIgnoresSiblingsThatAreNotProjects(t *testing.T) {
	root := t.TempDir()
	main := filepath.Join(root, "workspace")
	if err := os.MkdirAll(filepath.Join(root, "windows", DirName, "conv"), 0o755); err != nil {
		t.Fatal(err)
	}
	stray := filepath.Join(root, "windows", DirName, "conv", "x.json")
	if err := os.WriteFile(stray, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-(CompressAfter + time.Hour))
	if err := os.Chtimes(stray, when, when); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(main, 0o755); err != nil {
		t.Fatal(err)
	}

	if n := New(main).Sweep(main); n != 0 {
		t.Errorf("compressed %d outside any workspace", n)
	}
}
