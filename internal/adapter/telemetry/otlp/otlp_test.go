package otlp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

type capture struct {
	mu     sync.Mutex
	paths  []string
	bodies []string
}

func (c *capture) server(t *testing.T, status int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		c.mu.Lock()
		c.paths = append(c.paths, r.URL.Path)
		c.bodies = append(c.bodies, string(b))
		c.mu.Unlock()
		w.WriteHeader(status)
	}))
}

// The capability the harness was built for: a token count that leaves the
// process. The stack's own dashboard records that picoclaw's never can.
func TestUsage_ExportsThreeSums(t *testing.T) {
	c := &capture{}
	srv := c.server(t, 200)
	defer srv.Close()

	e := New(srv.URL, "crab-ganglion", srv.Client())
	e.Usage(context.Background(),
		domain.Usage{PromptTokens: 10, CompletionTokens: 4, TotalTokens: 14},
		domain.Attr{Key: "conversation.id", Value: "conv-1"})

	if len(c.paths) != 1 || c.paths[0] != "/v1/metrics" {
		t.Fatalf("posted to %v, want /v1/metrics once", c.paths)
	}
	body := c.bodies[0]
	for _, want := range []string{
		"ganglion.tokens.prompt", "ganglion.tokens.completion", "ganglion.tokens.total",
		`"asInt":"10"`, `"asInt":"4"`, `"asInt":"14"`,
		"conversation.id", "crab-ganglion",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in payload:\n%s", want, body)
		}
	}
	// It must be valid JSON, or the collector rejects the batch silently.
	var parsed map[string]any
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Errorf("payload is not JSON: %v", err)
	}
}

// An all-zero sample is noise. A provider that reports no usage must not
// produce a metric claiming zero tokens were spent.
func TestUsage_SkipsAnEmptySample(t *testing.T) {
	c := &capture{}
	srv := c.server(t, 200)
	defer srv.Close()

	New(srv.URL, "s", srv.Client()).Usage(context.Background(), domain.Usage{})
	if len(c.paths) != 0 {
		t.Errorf("exported an all-zero usage: %v", c.bodies)
	}
}

func TestSpan_ExportsOnceOnEndWithItsDuration(t *testing.T) {
	c := &capture{}
	srv := c.server(t, 200)
	defer srv.Close()

	e := New(srv.URL, "s", srv.Client())
	_, end := e.Span(context.Background(), "turn", domain.Attr{Key: "conversation.id", Value: "conv-1"})
	if len(c.paths) != 0 {
		t.Fatal("a span exported at START; it must export once, on end")
	}
	end(nil)

	if len(c.paths) != 1 || c.paths[0] != "/v1/traces" {
		t.Fatalf("posted to %v, want /v1/traces once", c.paths)
	}
	for _, want := range []string{"startTimeUnixNano", "endTimeUnixNano", "conv-1"} {
		if !strings.Contains(c.bodies[0], want) {
			t.Errorf("missing %q in span payload", want)
		}
	}
}

func TestSpan_RecordsAFailure(t *testing.T) {
	c := &capture{}
	srv := c.server(t, 200)
	defer srv.Close()

	_, end := New(srv.URL, "s", srv.Client()).Span(context.Background(), "turn")
	end(io.ErrUnexpectedEOF)

	if !strings.Contains(c.bodies[0], `"code":2`) {
		t.Errorf("a failed span was not marked as an error:\n%s", c.bodies[0])
	}
}

// Telemetry that can fail a turn is worse than no telemetry: the member would
// lose their answer so a counter could be recorded.
func TestPost_ACollectorFailureIsInvisibleToTheCaller(t *testing.T) {
	c := &capture{}
	srv := c.server(t, 503)
	defer srv.Close()

	var logged []string
	e := New(srv.URL, "s", srv.Client())
	e.Logf = func(f string, a ...any) { logged = append(logged, f) }

	e.Usage(context.Background(), domain.Usage{TotalTokens: 1}) // must not panic
	if len(logged) == 0 {
		t.Error("a 503 from the collector was swallowed without a word")
	}
}

// No endpoint configured: post nothing, log nothing. A deployment with no
// collector must not log a failed request per turn.
func TestPost_NoEndpointIsSilent(t *testing.T) {
	var logged int
	e := New("", "s", nil)
	e.Logf = func(string, ...any) { logged++ }
	e.Usage(context.Background(), domain.Usage{TotalTokens: 1})
	if logged != 0 {
		t.Errorf("logged %d times with no endpoint configured", logged)
	}
}

// A 404 on an OTLP signal path means the collector declares no pipeline for that
// signal -- a permanent fact about its configuration, not a failure to retry.
//
// This stack's collector is exactly that case: it has a metrics pipeline and no
// traces one, because Prometheus cannot store spans. Retrying meant two error
// lines per turn in the AGENT's own log, which is where a member-facing failure
// has to stay visible.
func TestA404DropsThatSignalAndKeepsTheOthers(t *testing.T) {
	var traces, metrics int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/v1/traces":
			traces++
			w.WriteHeader(http.StatusNotFound)
		case "/v1/metrics":
			metrics++
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer srv.Close()

	var lines []string
	e := New(srv.URL, "ganglion", nil)
	e.Logf = func(format string, args ...any) {
		lines = append(lines, fmt.Sprintf(format, args...))
	}

	for i := 0; i < 5; i++ {
		_, end := e.Span(context.Background(), "turn")
		end(nil)
		e.Usage(context.Background(), domain.Usage{PromptTokens: 1, TotalTokens: 1})
	}

	mu.Lock()
	defer mu.Unlock()
	if traces != 1 {
		t.Errorf("traces posted %d times, want 1 -- a 404 must not be retried", traces)
	}
	// The capability the exporter was built for keeps working. Giving up the
	// token counts because the spans have nowhere to go would be the wrong trade.
	if metrics != 5 {
		t.Errorf("metrics posted %d times, want 5", metrics)
	}

	said := 0
	for _, l := range lines {
		if strings.Contains(l, "/v1/traces") {
			said++
		}
	}
	if said != 1 {
		t.Errorf("the drop was reported %d times, want exactly one line", said)
	}
	if said == 1 && !strings.Contains(lines[0], "does not accept") {
		t.Errorf("the line does not say what happened: %q", lines[0])
	}
}

// A non-404 failure is transient -- a collector restarting, a batch refused --
// and must keep being retried and keep being reported.
func TestAServerErrorIsStillRetried(t *testing.T) {
	var posts int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		posts++
		mu.Unlock()
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	e := New(srv.URL, "ganglion", nil)
	e.Logf = func(string, ...any) {}
	for i := 0; i < 3; i++ {
		_, end := e.Span(context.Background(), "turn")
		end(nil)
	}
	mu.Lock()
	defer mu.Unlock()
	if posts != 3 {
		t.Fatalf("posts = %d, want 3: a 503 is transient and must be retried", posts)
	}
}
