package httpsse

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

func serve(t *testing.T, s *Server, h domain.TurnHandler, body string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	s.handler = h
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.completions(rec, req)
	return rec
}

const oneUser = `{"model":"m","stream":true,"messages":[{"role":"user","content":"oi"}]}`

func TestCompletions_StreamsDeltasAsOpenAIChunks(t *testing.T) {
	s := New(":0", "")
	s.Heartbeat = 0
	rec := serve(t, s, func(_ context.Context, _ domain.Turn, sink domain.Sink) (string, error) {
		sink.EmitContent("Oi")
		sink.EmitContent(", tudo bem?")
		return "Oi, tudo bem?", nil
	}, oneUser, nil)

	body := rec.Body.String()
	if got := strings.Count(body, `"content":"`); got != 2 {
		t.Errorf("expected 2 content chunks, got %d:\n%s", got, body)
	}
	if !strings.Contains(body, `"object":"chat.completion.chunk"`) {
		t.Errorf("not OpenAI-shaped:\n%s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Errorf("stream not terminated:\n%s", body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q", ct)
	}
	// One buffering hop turns progressive delivery into a blob at the end.
	if rec.Header().Get("X-Accel-Buffering") != "no" {
		t.Error("X-Accel-Buffering: no is missing")
	}
}

// The heartbeat must be an SSE COMMENT. As a data frame it would stamp the
// client's last-event clock and destroy staleness detection.
func TestHeartbeat_IsACommentNotADataFrame(t *testing.T) {
	s := New(":0", "")
	s.Heartbeat = 5 * time.Millisecond
	rec := serve(t, s, func(_ context.Context, _ domain.Turn, sink domain.Sink) (string, error) {
		time.Sleep(40 * time.Millisecond) // a quiet agent
		sink.EmitContent("pronto")
		return "pronto", nil
	}, oneUser, nil)

	body := rec.Body.String()
	if !strings.Contains(body, ": ping") {
		t.Fatalf("no heartbeat during a quiet turn:\n%s", body)
	}
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "data:") && strings.Contains(line, "ping") {
			t.Errorf("the heartbeat was written as a DATA frame: %q", line)
		}
	}
}

func TestCompletions_SessionScopeComesFromHeadersNotDerived(t *testing.T) {
	var got domain.Turn
	s := New(":0", "")
	s.Heartbeat = 0
	serve(t, s, func(_ context.Context, tn domain.Turn, _ domain.Sink) (string, error) {
		got = tn
		return "", nil
	}, oneUser, map[string]string{
		"X-Ganglion-Session-Id":  "conv-9",
		"X-Ganglion-Session-Key": "sk-9",
	})

	if got.SessionID != "conv-9" || got.SessionKey != "sk-9" {
		t.Errorf("session scope = %q/%q, want conv-9/sk-9", got.SessionID, got.SessionKey)
	}
	if got.Input.Content != "oi" {
		t.Errorf("input = %q", got.Input.Content)
	}
}

func TestCompletions_RejectsAWrongBearer(t *testing.T) {
	s := New(":0", "secret")
	rec := serve(t, s, func(context.Context, domain.Turn, domain.Sink) (string, error) {
		t.Error("the handler ran despite a bad token")
		return "", nil
	}, oneUser, map[string]string{"Authorization": "Bearer wrong"})

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("code = %d, want 401", rec.Code)
	}
}

// A turn that fails must still terminate the stream, or the client waits for
// bytes that are never coming.
func TestCompletions_FailedTurnStillTerminatesTheStream(t *testing.T) {
	s := New(":0", "")
	s.Heartbeat = 0
	rec := serve(t, s, func(_ context.Context, _ domain.Turn, sink domain.Sink) (string, error) {
		sink.EmitError("provider exploded")
		return "", context.DeadlineExceeded
	}, oneUser, nil)

	body := rec.Body.String()
	if !strings.Contains(body, "x_crab_error") {
		t.Errorf("the failure was not signalled:\n%s", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Errorf("stream left open after a failure:\n%s", body)
	}
}
