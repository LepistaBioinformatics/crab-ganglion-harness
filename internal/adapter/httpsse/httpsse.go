// Package httpsse is the driving Ingress: native HTTP with Server-Sent Events.
//
// There is no protocol translation here and no WebSocket. The turn ends when
// the handler returns, which is why this harness has no analogue of
// crab-shell-proxy's 500ms graceWindow -- there is no out-of-band signal to
// interpret as "the turn is over".
package httpsse

import (
	"context"
	"encoding/base64"
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
		Input: domain.Message{
			Role:        domain.RoleUser,
			Content:     last,
			Attachments: req.attachments(),
		},
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
		// Attachments carry images the member sent with this message.
		//
		// A field of our own rather than OpenAI's multi-part `content` array:
		// crab-shell-proxy is the only caller, the array form would make
		// `content` a union this parser has to disambiguate on every request,
		// and the harness's own wire adapter builds that array anyway when it
		// talks to a provider. Compatibility with picoclaw's INPUT shape was
		// never a requirement -- nothing but the proxy speaks to either.
		Attachments []struct {
			Kind string `json:"kind"`
			MIME string `json:"mime"`
			Name string `json:"name,omitempty"`
			// Data is standard base64. Sent inline because the proxy already
			// holds the bytes and a URL would have to be reachable from inside
			// the agent network, which is the one place they must not be.
			Data string `json:"data"`
		} `json:"attachments,omitempty"`
	} `json:"messages"`
	SessionID string `json:"session_id"`
}

// attachments decodes the media on the last user message.
//
// Only the last one: earlier messages in the request are history the harness
// already holds in its own window, and re-decoding a conversation's worth of
// base64 on every turn would grow the cost of a turn with the age of the
// conversation.
//
// A malformed attachment is DROPPED rather than failing the turn. The member
// sent a message; answering it without the image beats refusing it, and the
// degradation path already exists for the case where nothing can read one.
func (r request) attachments() []domain.Attachment {
	for i := len(r.Messages) - 1; i >= 0; i-- {
		if r.Messages[i].Role != string(domain.RoleUser) {
			continue
		}
		var out []domain.Attachment
		for _, a := range r.Messages[i].Attachments {
			data, err := base64.StdEncoding.DecodeString(a.Data)
			if err != nil || len(data) == 0 {
				continue
			}
			kind := domain.AttachmentKind(a.Kind)
			if kind == "" {
				kind = domain.AttachmentImage
			}
			out = append(out, domain.Attachment{Kind: kind, MIME: a.MIME, Name: a.Name, Data: data})
		}
		return out
	}
	return nil
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
