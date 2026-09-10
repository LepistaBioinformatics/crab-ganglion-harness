package otlp

import (
	"context"
	"encoding/json"
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
