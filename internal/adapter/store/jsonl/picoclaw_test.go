package jsonl

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

// writePico lays down a transcript in picoclaw's shape: a hashed name, and a
// meta carrying the chat marker that is the only link back to the conversation.
func writePico(t *testing.T, dir, basename, sessionKey, kind string, msgs ...string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := map[string]any{
		"key": kind,
		"scope": map[string]any{
			"values": map[string]any{"chat": picoChatMarker + sessionKey},
		},
	}
	raw, _ := json.Marshal(meta)
	if err := os.WriteFile(filepath.Join(dir, basename+".meta.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(dir, basename+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, m := range msgs {
		if err := enc.Encode(domain.Message{Role: domain.RoleUser, Content: m}); err != nil {
			t.Fatal(err)
		}
	}
}

func contents(msgs []domain.Message) []string {
	out := make([]string, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, m.Content)
	}
	return out
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// THE MIGRATION CASE. A member whose agent moves from picoclaw to this harness
// keeps their directory, their conversations and their ids -- but not the file
// names, which are a hash of picoclaw's own that nothing here can compute.
//
// The marker is the route: picoclaw stamps scope.values.chat with the session
// key, and that key is exactly the conversation id the proxy hands this harness.
func TestAPicoclawTranscriptIsReadForTheSameConversation(t *testing.T) {
	s, _ := scoped(t)
	const id = "0123456789abcdef0123456789abcdef"
	writePico(t, s.Root, "sk_v1_deadbeef", id, "agent:direct:pico-user", "oi", "tudo bem?")

	got, err := s.Read(context.Background(), id)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if want := []string{"oi", "tudo bem?"}; !eq(contents(got), want) {
		t.Fatalf("got %v, want %v", contents(got), want)
	}
}

// The migrated conversation continues under THIS harness's name, and both files
// are then read as one conversation -- picoclaw's first, because it holds what
// happened before the move.
func TestAMigratedConversationContinuesInOneReadableWhole(t *testing.T) {
	s, _ := scoped(t)
	const id = "0123456789abcdef0123456789abcdef"
	writePico(t, s.Root, "sk_v1_deadbeef", id, "agent:direct:pico-user", "antes da migracao")

	ctx := context.Background()
	if err := s.Append(ctx, id, msg("depois da migracao")); err != nil {
		t.Fatal(err)
	}
	got, err := s.Read(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"antes da migracao", "depois da migracao"}; !eq(contents(got), want) {
		t.Fatalf("got %v, want %v", contents(got), want)
	}
}

// picoclaw may continue one chat in a FRESH session file and leave the old meta
// in place, so a marker legitimately matches several. Reading one would drop the
// rest of the conversation; the proxy learned this the same way.
func TestEveryMatchingPicoclawFileIsFoldedOldestFirst(t *testing.T) {
	s, _ := scoped(t)
	const id = "0123456789abcdef0123456789abcdef"
	writePico(t, s.Root, "sk_v1_older", id, "agent:direct:pico-user", "primeiro")
	writePico(t, s.Root, "sk_v1_newer", id, "agent:direct:pico-user", "segundo")
	// The names are hashes and carry no sequence, so mtime is the only ordering.
	older := time.Now().Add(-time.Hour)
	for _, ext := range []string{".jsonl", ".meta.json"} {
		if err := os.Chtimes(filepath.Join(s.Root, "sk_v1_older"+ext), older, older); err != nil {
			t.Fatal(err)
		}
	}

	got, err := s.Read(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"primeiro", "segundo"}; !eq(contents(got), want) {
		t.Fatalf("got %v, want %v", contents(got), want)
	}
}

// A SCHEDULED run stamps the originating chat's marker, so a conversation that
// owns a daily task would read that task's transcript as its own. The proxy
// excludes these for the same reason and it is the defect that made a chat with
// cron tasks resolve to a cron transcript.
func TestAScheduledRunIsNotReadAsTheConversation(t *testing.T) {
	s, _ := scoped(t)
	const id = "0123456789abcdef0123456789abcdef"
	writePico(t, s.Root, "sk_v1_chat", id, "agent:direct:pico-user", "a conversa")
	writePico(t, s.Root, "agent_cron-job1-run1", id, "agent:cron-job1-run1", "o resumo diario")

	got, err := s.Read(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"a conversa"}; !eq(contents(got), want) {
		t.Fatalf("got %v, want %v", contents(got), want)
	}
}

// Another conversation's transcript is not this one's.
func TestAnotherConversationsPicoclawTranscriptIsNotRead(t *testing.T) {
	s, _ := scoped(t)
	writePico(t, s.Root, "sk_v1_theirs", "ffffffffffffffffffffffffffffffff",
		"agent:direct:pico-user", "de outra conversa")

	got, err := s.Read(context.Background(), "0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("read another conversation: %v", contents(got))
	}
}

// The regression bar: a workspace with no picoclaw files behaves exactly as it
// did, which is every deployment that was never migrated.
func TestAWorkspaceWithNoPicoclawFilesIsUnchanged(t *testing.T) {
	s, _ := scoped(t)
	ctx := context.Background()
	if err := s.Append(ctx, "conv", msg("so o nosso")); err != nil {
		t.Fatal(err)
	}
	got, err := s.Read(ctx, "conv")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"so o nosso"}; !eq(contents(got), want) {
		t.Fatalf("got %v, want %v", contents(got), want)
	}
}
