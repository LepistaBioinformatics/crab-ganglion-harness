// Package websearch is the web_search tool.
//
// # WHY THIS IS A GO TOOL AND NOT A SHELL COMMAND
//
// The cheap implementation is to let the agent curl a search API from the shell
// tool. It is refused, and the reason is structural rather than stylistic: the
// shell tool's environment is scrubbed to PATH, HOME, TERM, LANG and TZ, and an
// agreement test fails the build if any GANGLION_* variable joins that
// allowlist. Handing the shell a search key would mean widening it, and the key
// would then be readable by any command the model can be talked into running.
//
// So the call is made from the harness process, where the key never enters a
// child environment. Landlock does not restrain this and does not need to:
// Landlock governs the filesystem, and this tool touches none.
//
// # COMPATIBILITY
//
// The tool name, its JSON schema and its result text are picoclaw's, so a
// prompt or a skill written for one harness reads identically on the other.
// What is deliberately NOT picoclaw's is the failure behaviour -- see pick.
package websearch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/config"
	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

// Result is one search hit, normalized across providers before formatting.
type Result struct {
	Title   string
	URL     string
	Snippet string
}

// Provider is one search backend.
type Provider interface {
	// Name is the identifier used in configuration and in log lines.
	Name() string
	// Ready reports whether this provider can be called at all: enabled, and
	// carrying whatever credential or endpoint it needs.
	Ready() bool
	// Search returns at most count results. timeRange is picoclaw's d/w/m/y or
	// empty, and a provider that cannot express it ignores it rather than
	// failing -- a filter nobody can honour is not worth losing a turn over.
	Search(ctx context.Context, query string, count int, timeRange string) ([]Result, error)
}

// defaultCount matches picoclaw's schema default.
const defaultCount = 10

// maxCount matches picoclaw's schema maximum. Providers price per result and
// the model has no reason to ask for more than it can read.
const maxCount = 10

// Tool implements domain-facing search.
type Tool struct {
	providers []Provider
	// preferred is tools.web.provider. Empty or "auto" walks the list in
	// order.
	preferred string
	logf      func(string, ...any)
}

// New builds the tool from the resolved configuration.
//
// It returns nil when no provider is ready. A nil tool is NOT registered by the
// composition root, so the model is never told about a capability it does not
// have -- being told and then discovering the absence costs a whole turn.
func New(cfg config.Web, hc *http.Client, logf func(string, ...any)) *Tool {
	if hc == nil {
		hc = &http.Client{Timeout: 20 * time.Second}
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	var ps []Provider
	for _, name := range config.WebProviderNames {
		pc, ok := cfg.Find(name)
		if !ok || !pc.Enabled {
			continue
		}
		if p := build(name, pc, hc); p != nil && p.Ready() {
			ps = append(ps, p)
		}
	}
	if len(ps) == 0 {
		return nil
	}
	return &Tool{providers: ps, preferred: cfg.Provider, logf: logf}
}

func build(name string, pc config.WebProvider, hc *http.Client) Provider {
	switch name {
	case "brave":
		return &brave{cfg: pc, hc: hc}
	case "tavily":
		return &tavily{cfg: pc, hc: hc}
	case "searxng":
		return &searxng{cfg: pc, hc: hc}
	case "duckduckgo":
		return &duckduckgo{cfg: pc, hc: hc}
	}
	return nil
}

func (t *Tool) Name() string { return "web_search" }

// Schema is picoclaw's, field for field (pkg/tools/integration/web.go:1923).
// Matching it means a skill that tells the model to pass `range: "w"` works on
// either harness.
func (t *Tool) Schema() domain.ToolSchema {
	return domain.ToolSchema{
		Name: "web_search",
		Description: "Search the web for current information. Supports query, count, and an " +
			"optional temporal range filter. Returns titles, URLs, and snippets from search results.",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "query": {"type": "string", "description": "Search query"},
    "count": {"type": "integer", "description": "Number of results (default: 10, max: 10)", "minimum": 1, "maximum": 10},
    "range": {"type": "string", "description": "Optional time filter: d (day), w (week), m (month), y (year)", "enum": ["d","w","m","y"]}
  },
  "required": ["query"]
}`),
	}
}

type args struct {
	Query string `json:"query"`
	Count int    `json:"count"`
	Range string `json:"range"`
}

// Invoke never returns an error for a search that could not be done.
//
// DEC-2: a tool-level problem is the agent's to react to, not the turn's to die
// of. "No provider answered" is information the model can use -- it can say so,
// or answer from what it knows. An error would end the turn instead.
func (t *Tool) Invoke(ctx context.Context, raw json.RawMessage) (domain.Result, error) {
	var a args
	if err := json.Unmarshal(raw, &a); err != nil {
		return domain.Result{Content: "web_search: could not read the arguments: " + err.Error()}, nil
	}
	a.Query = strings.TrimSpace(a.Query)
	if a.Query == "" {
		return domain.Result{Content: "web_search: `query` is required."}, nil
	}
	if a.Count <= 0 {
		a.Count = defaultCount
	}
	if a.Count > maxCount {
		a.Count = maxCount
	}

	results, used, err := t.search(ctx, a)
	if err != nil {
		return domain.Result{Content: fmt.Sprintf("web_search failed: %v", err)}, nil
	}
	if len(results) == 0 {
		return domain.Result{Content: "No results for: " + a.Query}, nil
	}
	t.logf("web_search: %q answered by %s (%d results)", a.Query, used, len(results))
	return domain.Result{Content: Format(a.Query, results, a.Count)}, nil
}

// search walks the candidates until one answers.
//
// THE ONE DELIBERATE DIVERGENCE FROM PICOCLAW, and it is recorded in the spec
// rather than discovered here: picoclaw resolves ONE provider per query and
// does not retry against the next when the call fails -- only its key pool
// fails over, within a single provider. This falls through, because a search
// that fails on a rate limit is a turn wasted for a reason the member cannot
// act on, and there is usually a keyless provider underneath that would have
// answered.
func (t *Tool) search(ctx context.Context, a args) ([]Result, string, error) {
	var lastErr error
	for _, p := range t.candidates() {
		res, err := p.Search(ctx, a.Query, a.Count, a.Range)
		if err == nil {
			return res, p.Name(), nil
		}
		lastErr = fmt.Errorf("%s: %w", p.Name(), err)
		t.logf("web_search: %v", lastErr)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no search provider is configured")
	}
	return nil, "", lastErr
}

// candidates is the ordered list for one query.
//
// An explicitly named provider is used ALONE: an operator who pinned
// tools.web.provider chose that provider, and quietly querying a different one
// would send the query somewhere they did not agree to.
func (t *Tool) candidates() []Provider {
	name := strings.TrimSpace(strings.ToLower(t.preferred))
	if name != "" && name != "auto" {
		for _, p := range t.providers {
			if p.Name() == name {
				return []Provider{p}
			}
		}
		// A pinned provider that is not ready falls through to the order
		// rather than failing: the pin is a preference, and its absence is
		// already visible in the log line the loader writes.
	}
	return t.providers
}

// Format renders results the way picoclaw renders them
// (pkg/tools/integration/web.go:369-381), byte for byte in shape, so a model
// prompted against one harness parses the other's output identically.
func Format(query string, results []Result, count int) string {
	var b strings.Builder
	b.WriteString("Results for: " + query)
	for i, r := range results {
		if i >= count {
			break
		}
		fmt.Fprintf(&b, "\n%d. %s\n   %s", i+1, r.Title, r.URL)
		if r.Snippet != "" {
			b.WriteString("\n   " + r.Snippet)
		}
	}
	return b.String()
}
