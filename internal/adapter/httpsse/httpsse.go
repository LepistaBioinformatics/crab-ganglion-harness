// Package httpsse is the driving Ingress: native HTTP with Server-Sent Events.
//
// There is no protocol translation here and no WebSocket. The turn ends when
// the handler returns, which is why this harness has no analogue of
// crab-shell-proxy's 500ms graceWindow -- there is no out-of-band signal to
// interpret as "the turn is over".
package httpsse

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

// Server serves the OpenAI-compatible surface.
type Server struct {
	Addr string
	// AuthToken, when set, is required as a bearer. The container is per-user
	// and reachable only from the proxy, so this is a second factor rather than
	// the primary one.
	AuthToken string
	// Heartbeat keeps the stream carrying bytes while the agent is quiet.
	// Ten seconds, chosen against the tightest plausible hop rather than the
	// gateway's 60s -- same reasoning as turn-stream-continuity FR-3.
	Heartbeat time.Duration
	Logf      func(string, ...any)

	handler domain.TurnHandler
}

const DefaultHeartbeat = 10 * time.Second

func New(addr, token string) *Server {
	return &Server{Addr: addr, AuthToken: token, Heartbeat: DefaultHeartbeat}
}

// Serve implements domain.Ingress.
func (s *Server) Serve(ctx context.Context, h domain.TurnHandler) error {
	s.handler = h
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", s.completions)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"status":"ok"}`)
	})

	srv := &http.Server{Addr: s.Addr, Handler: mux}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *Server) completions(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var req request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	last, ok := req.lastUserMessage()
	if !ok {
		http.Error(w, "no user message", http.StatusBadRequest)
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// One nginx-shaped hop between here and the member turns progressive
	// delivery into a single blob at the end, which presents as "it does not
	// stream" and is very hard to attribute.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	sw := &writer{w: w, f: flusher, id: req.id(), model: req.Model}
	stop := sw.heartbeat(r.Context(), s.Heartbeat)
	defer stop()

	turn := domain.Turn{
		SessionID:  domain.ConversationID(req.sessionID(r)),
		SessionKey: domain.SessionKey(req.sessionKey(r)),
		Model:      req.Model,
		Input:      domain.Message{Role: domain.RoleUser, Content: last},
	}

	if _, err := s.handler(r.Context(), turn, sw.sink()); err != nil {
		s.logf("turn %s failed: %v", turn.SessionID, err)
	}
	sw.done()
}

func (s *Server) authorized(r *http.Request) bool {
	if s.AuthToken == "" {
		return true
	}
	got := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer"))
	return got == s.AuthToken
}

func (s *Server) logf(f string, a ...any) {
	if s.Logf != nil {
		s.Logf(f, a...)
	}
}

// --- request ----------------------------------------------------------------

type request struct {
	Model    string `json:"model"`
	Stream   bool   `json:"stream"`
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
	SessionID string `json:"session_id"`
}

func (r request) lastUserMessage() (string, bool) {
	for i := len(r.Messages) - 1; i >= 0; i-- {
		if r.Messages[i].Role == string(domain.RoleUser) {
			return r.Messages[i].Content, true
		}
	}
	return "", false
}

// sessionID and sessionKey come from headers the proxy sets. The harness never
// derives them: the proxy owns the preimage, and computing it twice is how two
// components silently disagree.
func (r request) sessionID(hr *http.Request) string {
	if v := hr.Header.Get("X-Ganglion-Session-Id"); v != "" {
		return v
	}
	return r.SessionID
}

func (r request) sessionKey(hr *http.Request) string {
	if v := hr.Header.Get("X-Ganglion-Session-Key"); v != "" {
		return v
	}
	return r.sessionID(hr)
}

func (r request) id() string { return "chatcmpl-ganglion" }

// --- SSE writer --------------------------------------------------------------

type writer struct {
	w     http.ResponseWriter
	f     http.Flusher
	id    string
	model string

	// Every write to the response goes through mu. The heartbeat is a second
	// writer by construction, and interleaved output would corrupt a frame the
	// client is mid-parse on.
	mu     sync.Mutex
	closed bool
}

func (s *writer) sink() domain.Sink {
	return domain.Sink{
		Content: func(delta string) {
			s.chunk(map[string]any{"content": delta})
		},
		Progress: func(p domain.Progress) {
			s.chunk(map[string]any{"x_crab_progress": p})
		},
		Error: func(msg string) {
			s.chunk(map[string]any{"x_crab_error": msg})
		},
	}
}

func (s *writer) chunk(delta map[string]any) {
	s.emit(map[string]any{
		"id":      s.id,
		"object":  "chat.completion.chunk",
		"model":   s.model,
		"choices": []any{map[string]any{"index": 0, "delta": delta}},
	})
}

func (s *writer) emit(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	fmt.Fprintf(s.w, "data: %s\n\n", b)
	s.f.Flush()
}

// heartbeat writes an SSE COMMENT, never a data frame.
//
// This is the requirement most likely to be "simplified" into a bug: a
// heartbeat shaped as an empty progress chunk would stamp the client's
// last-event clock every ten seconds, pinning "quiet for" at zero and
// destroying the very staleness detection the webapp uses. A comment line is
// dropped by the client before any bookkeeping.
func (s *writer) heartbeat(ctx context.Context, every time.Duration) func() {
	if every <= 0 {
		return func() {}
	}
	ctx, cancel := context.WithCancel(ctx)
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.mu.Lock()
				if !s.closed {
					fmt.Fprint(s.w, ": ping\n\n")
					s.f.Flush()
				}
				s.mu.Unlock()
			}
		}
	}()
	return cancel
}

func (s *writer) done() {
	s.emit(map[string]any{
		"id":      s.id,
		"object":  "chat.completion.chunk",
		"model":   s.model,
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
	})
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	fmt.Fprint(s.w, "data: [DONE]\n\n")
	s.f.Flush()
	s.closed = true
}
