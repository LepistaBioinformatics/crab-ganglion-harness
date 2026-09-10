package openai

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

// sseServer replays a recorded event stream. The provider path is testable
// without a key or a network because the adapter's only job is this parse.
func sseServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, body)
	}))
}

func drain(t *testing.T, s domain.Stream) []domain.Delta {
	t.Helper()
	var out []domain.Delta
	for {
		d, err := s.Next(context.Background())
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		out = append(out, d)
	}
}

func TestStream_EmitsContentDeltasAndSkipsRoleOnlyFrames(t *testing.T) {
	srv := sseServer(t, strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant"}}]}`,
		`data: {"choices":[{"delta":{"content":"Oi"}}]}`,
		`: keep-alive`,
		`data: {"choices":[{"delta":{"content":", tudo bem?"}}]}`,
		`data: {"usage":{"prompt_tokens":11,"completion_tokens":4,"total_tokens":15}}`,
		`data: [DONE]`,
		``,
	}, "\n\n"))
	defer srv.Close()

	st, err := New(srv.URL, "k", srv.Client()).Complete(context.Background(), domain.Completion{Model: "m"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	defer st.Close()

	got := drain(t, st)
	if len(got) != 2 {
		t.Fatalf("got %d deltas, want 2 (the role-only frame must not become an empty delta): %+v", len(got), got)
	}
	if st.Message().Content != "Oi, tudo bem?" {
		t.Errorf("assembled content = %q", st.Message().Content)
	}
	// FR-8: without stream_options.include_usage most providers omit this, which
	// is why the request asks for it.
	if st.Usage().TotalTokens != 15 {
		t.Errorf("usage = %+v, want 15 total", st.Usage())
	}
}

// The fiddly one: providers stream tool-call arguments one fragment at a time.
// The loop must never see a half-parsed object.
func TestStream_ReassemblesFragmentedToolCallArguments(t *testing.T) {
	srv := sseServer(t, strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"sh"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"cmd\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"ls\"}"}}]}}]}`,
		`data: [DONE]`,
		``,
	}, "\n\n"))
	defer srv.Close()

	st, err := New(srv.URL, "k", srv.Client()).Complete(context.Background(), domain.Completion{Model: "m"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	defer st.Close()
	drain(t, st)

	msg := st.Message()
	if len(msg.ToolCalls) != 1 {
		t.Fatalf("got %d tool calls, want 1", len(msg.ToolCalls))
	}
	tc := msg.ToolCalls[0]
	if tc.ID != "call_1" || tc.Name != "sh" {
		t.Errorf("tool call identity lost: %+v", tc)
	}
	if string(tc.Args) != `{"cmd":"ls"}` {
		t.Errorf("arguments = %s, want {\"cmd\":\"ls\"} -- fragments were not reassembled", tc.Args)
	}
}

// A provider that interleaves its own event types must not inject raw JSON into
// the member's answer. Hermes' hermes.tool.progress was exactly this.
func TestStream_SkipsUnparseableLinesInsteadOfFailing(t *testing.T) {
	srv := sseServer(t, strings.Join([]string{
		`event: vendor.progress`,
		`data: not json at all`,
		`data: {"choices":[{"delta":{"content":"ok"}}]}`,
		`data: [DONE]`,
		``,
	}, "\n\n"))
	defer srv.Close()

	st, err := New(srv.URL, "k", srv.Client()).Complete(context.Background(), domain.Completion{Model: "m"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	defer st.Close()

	if got := drain(t, st); len(got) != 1 || got[0].Content != "ok" {
		t.Errorf("deltas = %+v, want exactly the one real content frame", got)
	}
}

func TestComplete_NonOKStatusIsAnErrorNamingTheBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":"bad key"}`)
	}))
	defer srv.Close()

	_, err := New(srv.URL, "k", srv.Client()).Complete(context.Background(), domain.Completion{Model: "m"})
	if err == nil || !strings.Contains(err.Error(), "bad key") {
		t.Errorf("err = %v, want it to name the provider's own message", err)
	}
}

// The request must ask for usage, or FR-8 is unimplementable against the very
// endpoint it depends on.
func TestComplete_RequestsUsageInTheStream(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	st, err := New(srv.URL, "k", srv.Client()).Complete(context.Background(), domain.Completion{Model: "m"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	st.Close()

	if !strings.Contains(body, `"include_usage":true`) {
		t.Errorf("request did not ask for usage: %s", body)
	}
	if !strings.Contains(body, `"stream":true`) {
		t.Errorf("request did not ask to stream: %s", body)
	}
}
