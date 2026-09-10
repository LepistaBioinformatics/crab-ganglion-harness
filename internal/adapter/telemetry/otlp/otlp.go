// Package otlp exports telemetry over OTLP/HTTP with JSON encoding.
//
// Hand-rolled against the standard library, deliberately. The OTel Go SDK
// would be this harness's FIRST third-party dependency and it is a large
// tree, pulled per user in a per-user image, to emit a token count. The OTLP
// specification defines a JSON encoding over HTTP precisely so a small
// producer need not take that on, and the collector in this stack accepts it
// once its http receiver is declared.
//
// What this is NOT: a general OTel implementation. It emits the two signals
// the harness has -- token usage as a metric, operations as spans -- and
// nothing else. Anything more belongs in the SDK, and taking the SDK then
// would be a reasonable trade; taking it now would not.
package otlp

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

// Exporter implements domain.Telemetry.
type Exporter struct {
	// Endpoint is the collector's OTLP/HTTP base, e.g. http://otel-collector:4318.
	// Empty disables export entirely.
	Endpoint string
	Service  string
	HTTP     *http.Client
	Logf     func(string, ...any)

	mu      sync.Mutex
	traceID string
}

func New(endpoint, service string, hc *http.Client) *Exporter {
	if hc == nil {
		// A short timeout on purpose: telemetry must never hold a turn open.
		hc = &http.Client{Timeout: 5 * time.Second}
	}
	return &Exporter{
		Endpoint: strings.TrimRight(endpoint, "/"),
		Service:  service,
		HTTP:     hc,
	}
}

// Span records one operation.
//
// The returned function ends it, and the export happens THERE rather than at
// start: one request then carries the finished span with its duration, where
// a start/end pair would cost two requests to say the same thing.
func (e *Exporter) Span(ctx context.Context, name string, attrs ...domain.Attr) (context.Context, func(error)) {
	start := time.Now()
	spanID := randHex(8)
	traceID := e.traceFor(ctx)

	return ctx, func(runErr error) {
		s := map[string]any{
			"traceId":           traceID,
			"spanId":            spanID,
			"name":              name,
			"kind":              1, // INTERNAL
			"startTimeUnixNano": fmt.Sprint(start.UnixNano()),
			"endTimeUnixNano":   fmt.Sprint(time.Now().UnixNano()),
			"attributes":        keyValues(attrs),
		}
		if runErr != nil {
			s["status"] = map[string]any{"code": 2, "message": runErr.Error()} // ERROR
		}
		e.post(ctx, "/v1/traces", map[string]any{
			"resourceSpans": []any{map[string]any{
				"resource":   e.resource(),
				"scopeSpans": []any{map[string]any{"spans": []any{s}}},
			}},
		})
	}
}

// Usage records token accounting.
//
// This is the capability the harness was built for. The stack's own dashboard
// records that picoclaw "writes no token counts to disk ... It is not coming
// later"; three sums here make a tenant's spend a query rather than an
// estimate.
func (e *Exporter) Usage(ctx context.Context, u domain.Usage, attrs ...domain.Attr) {
	if u.TotalTokens == 0 && u.PromptTokens == 0 && u.CompletionTokens == 0 {
		return // an all-zero sample is noise, not data
	}
	now := fmt.Sprint(time.Now().UnixNano())
	kv := keyValues(attrs)

	sum := func(name string, v int) map[string]any {
		return map[string]any{
			"name": name,
			"unit": "{token}",
			"sum": map[string]any{
				// DELTA: each turn reports its own consumption. The collector
				// and Prometheus accumulate. Reporting cumulative totals would
				// require the harness to remember them across a scale-to-zero
				// stop, which is exactly what it must not need to do.
				"aggregationTemporality": 1,
				"isMonotonic":            true,
				"dataPoints": []any{map[string]any{
					"asInt":        fmt.Sprint(v),
					"timeUnixNano": now,
					"attributes":   kv,
				}},
			},
		}
	}

	e.post(ctx, "/v1/metrics", map[string]any{
		"resourceMetrics": []any{map[string]any{
			"resource": e.resource(),
			"scopeMetrics": []any{map[string]any{"metrics": []any{
				sum("ganglion.tokens.prompt", u.PromptTokens),
				sum("ganglion.tokens.completion", u.CompletionTokens),
				sum("ganglion.tokens.total", u.TotalTokens),
			}}},
		}},
	})
}

// post sends one payload. Failures are logged and swallowed.
//
// Telemetry that can fail a turn is worse than no telemetry: the member would
// lose their answer so that a counter could be recorded. A collector that is
// down has to be invisible to them.
func (e *Exporter) post(ctx context.Context, path string, payload any) {
	if e.Endpoint == "" {
		return
	}
	b, err := json.Marshal(payload)
	if err != nil {
		e.logf("otlp marshal %s: %v", path, err)
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.Endpoint+path, bytes.NewReader(b))
	if err != nil {
		e.logf("otlp request %s: %v", path, err)
		return
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := e.HTTP.Do(req)
	if err != nil {
		e.logf("otlp post %s: %v", path, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		e.logf("otlp post %s: %s", path, resp.Status)
	}
}

// traceFor gives every span from this process one trace id.
//
// Per-turn correlation would be better and needs a turn id the harness does
// not carry yet. One id per process still separates two containers, which is
// the question this data is actually asked.
func (e *Exporter) traceFor(context.Context) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.traceID == "" {
		e.traceID = randHex(16)
	}
	return e.traceID
}

func (e *Exporter) resource() map[string]any {
	return map[string]any{
		"attributes": keyValues([]domain.Attr{{Key: "service.name", Value: e.Service}}),
	}
}

func keyValues(attrs []domain.Attr) []any {
	out := make([]any, 0, len(attrs))
	for _, a := range attrs {
		out = append(out, map[string]any{
			"key":   a.Key,
			"value": map[string]any{"stringValue": a.Value},
		})
	}
	return out
}

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return strings.Repeat("0", n*2)
	}
	return hex.EncodeToString(b)
}

func (e *Exporter) logf(f string, a ...any) {
	if e.Logf != nil {
		e.Logf(f, a...)
	}
}

var _ domain.Telemetry = (*Exporter)(nil)
