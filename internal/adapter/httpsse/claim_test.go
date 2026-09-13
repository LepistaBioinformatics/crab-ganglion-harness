package httpsse

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

// ONE TURN AT A TIME on a conversation.
//
// Nothing used to enforce this, anywhere in the chain, and two POSTs on one
// conversation were two concurrent Loop.Run: each loads the context window,
// appends its iterations and saves, so the second save overwrote the first
// turn's work and the agent forgot what it had just done.
//
// It was reachable in production -- the webapp's client-side queue is what kept
// a second message from being POSTed mid-turn, and a page reload wipes it.

func post(s *Server, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()
	s.completions(rec, req)
	return rec
}

func bodyFor(conv string) string {
	return `{"model":"m","stream":true,"session_id":"` + conv + `",` +
		`"messages":[{"role":"user","content":"oi"}]}`
}

func TestClaim_TwoTurnsOnOneConversationDoNotOverlap(t *testing.T) {
	s := New(":0", "")
	s.Heartbeat = 0

	var mu sync.Mutex
	var running, maxRunning int
	started := make(chan struct{}, 2)
	unblock := make(chan struct{})

	s.handler = func(_ context.Context, _ domain.Turn, _ domain.Sink) (string, error) {
		mu.Lock()
		running++
		if running > maxRunning {
			maxRunning = running
		}
		mu.Unlock()
		started <- struct{}{}
		<-unblock
		mu.Lock()
		running--
		mu.Unlock()
		return "pronto", nil
	}

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			post(s, bodyFor("conv-1"))
		}()
	}

	// The first turn is in the handler; the second must be waiting OUTSIDE it.
	<-started
	select {
	case <-started:
		t.Fatal("both turns entered the handler: the conversation is not serialized")
	case <-time.After(50 * time.Millisecond):
	}

	close(unblock)
	wg.Wait()
	if maxRunning != 1 {
		t.Errorf("%d turns ran at once, want 1", maxRunning)
	}
}

// The serialization is per CONVERSATION, not per container. A member with two
// chats open is two conversations, and making them wait for each other would
// turn a correctness fix into a queue nobody asked for.
func TestClaim_DifferentConversationsRunAtTheSameTime(t *testing.T) {
	s := New(":0", "")
	s.Heartbeat = 0

	both := make(chan struct{}, 2)
	release := make(chan struct{})
	s.handler = func(_ context.Context, _ domain.Turn, _ domain.Sink) (string, error) {
		both <- struct{}{}
		<-release
		return "pronto", nil
	}

	var wg sync.WaitGroup
	for _, conv := range []string{"conv-1", "conv-2"} {
		wg.Add(1)
		go func(c string) { defer wg.Done(); post(s, bodyFor(c)) }(conv)
	}

	for i := 0; i < 2; i++ {
		select {
		case <-both:
		case <-time.After(2 * time.Second):
			t.Fatal("two conversations did not run concurrently")
		}
	}
	close(release)
	wg.Wait()
}

// The second turn RUNS once the first is done -- it is queued, not refused. A
// refusal would make the member resend a message while waiting on a turn they
// cannot see.
func TestClaim_TheWaitingTurnRunsAfterwards(t *testing.T) {
	s := New(":0", "")
	s.Heartbeat = 0

	var mu sync.Mutex
	var order []string
	gate := make(chan struct{})
	first := true

	s.handler = func(_ context.Context, _ domain.Turn, _ domain.Sink) (string, error) {
		mu.Lock()
		mine := first
		first = false
		mu.Unlock()
		if mine {
			<-gate
		}
		mu.Lock()
		order = append(order, "ran")
		mu.Unlock()
		return "pronto", nil
	}

	done := make(chan *httptest.ResponseRecorder, 2)
	for i := 0; i < 2; i++ {
		go func() { done <- post(s, bodyFor("conv-1")) }()
	}
	time.Sleep(30 * time.Millisecond)
	close(gate)

	for i := 0; i < 2; i++ {
		rec := <-done
		if !strings.Contains(rec.Body.String(), "data: [DONE]") {
			t.Errorf("a queued turn did not finish its stream:\n%s", rec.Body.String())
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(order) != 2 {
		t.Errorf("%d turns ran, want both", len(order))
	}
}

// A wait that outlives its request must not start a turn nobody is reading. The
// proxy's own budget cancels the request, and the member may simply have
// navigated away.
func TestClaim_AWaitThatIsCancelledStartsNothing(t *testing.T) {
	s := New(":0", "")
	s.Heartbeat = 0

	var mu sync.Mutex
	var calls int
	held := make(chan struct{})
	release := make(chan struct{})
	s.handler = func(_ context.Context, _ domain.Turn, _ domain.Sink) (string, error) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			close(held)
			<-release
		}
		return "pronto", nil
	}

	go post(s, bodyFor("conv-1"))
	<-held

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(bodyFor("conv-1")))
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	waited := make(chan struct{})
	go func() { s.completions(rec, req); close(waited) }()

	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case <-waited:
	case <-time.After(2 * time.Second):
		t.Fatal("a cancelled wait never returned")
	}

	close(release)
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Errorf("the handler ran %d times; the cancelled request must start nothing", calls)
	}
}
