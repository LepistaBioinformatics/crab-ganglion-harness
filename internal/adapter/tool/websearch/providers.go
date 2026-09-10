package websearch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/config"
)

// The four providers of v1. Each is one type, one Ready and one Search, so
// adding a fifth is a file and a case in build().
//
// Every one of them reads its endpoint from configuration with a default, so a
// test points it at an httptest server without the provider knowing.

// maxBody bounds what a search endpoint can make this process allocate. A
// provider is a third party; a 400 MB answer to a search is either a mistake or
// an attack, and neither is worth the memory.
const maxBody = 4 << 20

func decodeJSON(resp *http.Response, into any) error {
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<10))
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(into)
}

func base(cfg config.WebProvider, def string) string {
	if cfg.BaseURL != "" {
		return strings.TrimRight(cfg.BaseURL, "/")
	}
	return def
}

// cap applies the provider's configured ceiling on top of the caller's.
func capCount(cfg config.WebProvider, count int) int {
	if cfg.MaxResults > 0 && count > cfg.MaxResults {
		return cfg.MaxResults
	}
	return count
}

// ---------------------------------------------------------------- brave

type brave struct {
	cfg config.WebProvider
	hc  *http.Client
}

func (b *brave) Name() string { return "brave" }
func (b *brave) Ready() bool  { return b.cfg.Enabled && b.cfg.APIKey != "" }

func (b *brave) Search(ctx context.Context, query string, count int, timeRange string) ([]Result, error) {
	count = capCount(b.cfg, count)
	u := base(b.cfg, "https://api.search.brave.com/res/v1/web/search") +
		"?q=" + url.QueryEscape(query) + "&count=" + strconv.Itoa(count)
	if f := braveFreshness(timeRange); f != "" {
		u += "&freshness=" + f
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", b.cfg.APIKey)

	resp, err := b.hc.Do(req)
	if err != nil {
		return nil, err
	}
	var out struct {
		Web struct {
			Results []struct {
				Title       string `json:"title"`
				URL         string `json:"url"`
				Description string `json:"description"`
			} `json:"results"`
		} `json:"web"`
	}
	if err := decodeJSON(resp, &out); err != nil {
		return nil, err
	}
	var res []Result
	for _, r := range out.Web.Results {
		res = append(res, Result{Title: r.Title, URL: r.URL, Snippet: stripTags(r.Description)})
	}
	return res, nil
}

// braveFreshness maps picoclaw's d/w/m/y onto Brave's own vocabulary. An
// unrecognised range yields "", which drops the filter rather than sending a
// value the API rejects.
func braveFreshness(r string) string {
	switch r {
	case "d":
		return "pd"
	case "w":
		return "pw"
	case "m":
		return "pm"
	case "y":
		return "py"
	}
	return ""
}

// ---------------------------------------------------------------- tavily

type tavily struct {
	cfg config.WebProvider
	hc  *http.Client
}

func (t *tavily) Name() string { return "tavily" }
func (t *tavily) Ready() bool  { return t.cfg.Enabled && t.cfg.APIKey != "" }

func (t *tavily) Search(ctx context.Context, query string, count int, timeRange string) ([]Result, error) {
	count = capCount(t.cfg, count)
	body := map[string]any{"query": query, "max_results": count}
	if d := tavilyDays(timeRange); d > 0 {
		body["topic"] = "news"
		body["days"] = d
	}
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		base(t.cfg, "https://api.tavily.com")+"/search", strings.NewReader(string(b)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+t.cfg.APIKey)

	resp, err := t.hc.Do(req)
	if err != nil {
		return nil, err
	}
	var out struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := decodeJSON(resp, &out); err != nil {
		return nil, err
	}
	var res []Result
	for _, r := range out.Results {
		res = append(res, Result{Title: r.Title, URL: r.URL, Snippet: r.Content})
	}
	return res, nil
}

func tavilyDays(r string) int {
	switch r {
	case "d":
		return 1
	case "w":
		return 7
	case "m":
		return 30
	case "y":
		return 365
	}
	return 0
}

// ---------------------------------------------------------------- searxng

type searxng struct {
	cfg config.WebProvider
	hc  *http.Client
}

func (s *searxng) Name() string { return "searxng" }

// No key: a SearXNG instance is self-hosted, so its ADDRESS is the credential
// in the sense that matters -- without one there is nothing to call.
func (s *searxng) Ready() bool { return s.cfg.Enabled && s.cfg.BaseURL != "" }

func (s *searxng) Search(ctx context.Context, query string, count int, timeRange string) ([]Result, error) {
	count = capCount(s.cfg, count)
	u := strings.TrimRight(s.cfg.BaseURL, "/") + "/search?format=json&q=" + url.QueryEscape(query)
	if r := searxRange(timeRange); r != "" {
		u += "&time_range=" + r
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := s.hc.Do(req)
	if err != nil {
		return nil, err
	}
	var out struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Content string `json:"content"`
		} `json:"results"`
	}
	if err := decodeJSON(resp, &out); err != nil {
		return nil, err
	}
	var res []Result
	for i, r := range out.Results {
		if i >= count {
			break
		}
		res = append(res, Result{Title: r.Title, URL: r.URL, Snippet: r.Content})
	}
	return res, nil
}

func searxRange(r string) string {
	switch r {
	case "d":
		return "day"
	case "w":
		return "week"
	case "m":
		return "month"
	case "y":
		return "year"
	}
	return ""
}

// ---------------------------------------------------------------- duckduckgo

// duckduckgo scrapes the HTML endpoint, because DuckDuckGo has no search API.
//
// THIS IS THE FRAGILE ONE AND IT IS DELIBERATE. It exists so that a deployment
// that has bought nothing still has a working web_search rather than a tool
// that always answers "not configured" -- picoclaw makes exactly the same
// trade. When the markup changes this returns no results rather than wrong
// ones, and the provider above it in the order keeps working; a parse that
// guessed would be worse than one that finds nothing.
type duckduckgo struct {
	cfg config.WebProvider
	hc  *http.Client
}

func (d *duckduckgo) Name() string { return "duckduckgo" }

// Nothing to configure, so being enabled is the whole of being ready. This is
// what makes it the floor of the priority order.
func (d *duckduckgo) Ready() bool { return d.cfg.Enabled }

var (
	ddgResult  = regexp.MustCompile(`(?s)<a[^>]+class="result__a"[^>]+href="([^"]+)"[^>]*>(.*?)</a>`)
	ddgSnippet = regexp.MustCompile(`(?s)<a[^>]+class="result__snippet"[^>]*>(.*?)</a>`)
	tagRe      = regexp.MustCompile(`<[^>]*>`)
)

func (d *duckduckgo) Search(ctx context.Context, query string, count int, timeRange string) ([]Result, error) {
	count = capCount(d.cfg, count)
	u := base(d.cfg, "https://html.duckduckgo.com/html/") + "?q=" + url.QueryEscape(query)
	if r := ddgRange(timeRange); r != "" {
		u += "&df=" + r
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	// Without a browser-shaped agent the HTML endpoint answers with a consent
	// interstitial rather than results.
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; crab-ganglion/1.0)")

	resp, err := d.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, err
	}

	links := ddgResult.FindAllSubmatch(body, count)
	snippets := ddgSnippet.FindAllSubmatch(body, count)
	var res []Result
	for i, m := range links {
		r := Result{Title: stripTags(string(m[2])), URL: ddgURL(string(m[1]))}
		if i < len(snippets) {
			r.Snippet = stripTags(string(snippets[i][1]))
		}
		res = append(res, r)
	}
	return res, nil
}

// ddgURL unwraps DuckDuckGo's redirector, which wraps every result in
// /l/?uddg=<escaped>. Handing the model the redirect would make every URL it
// quotes useless outside a browser session.
func ddgURL(raw string) string {
	raw = html(raw)
	if i := strings.Index(raw, "uddg="); i >= 0 {
		q := raw[i+len("uddg="):]
		if j := strings.IndexByte(q, '&'); j >= 0 {
			q = q[:j]
		}
		if dec, err := url.QueryUnescape(q); err == nil {
			return dec
		}
	}
	if strings.HasPrefix(raw, "//") {
		return "https:" + raw
	}
	return raw
}

func ddgRange(r string) string {
	switch r {
	case "d":
		return "d"
	case "w":
		return "w"
	case "m":
		return "m"
	case "y":
		return "y"
	}
	return ""
}

// stripTags removes markup and collapses whitespace. Search snippets arrive
// with <b> around the matched terms, and passing raw HTML to a language model
// spends tokens on nothing.
func stripTags(s string) string {
	return strings.Join(strings.Fields(html(tagRe.ReplaceAllString(s, ""))), " ")
}

// html unescapes the handful of entities that actually appear in search
// results. A full parser would be a dependency for five replacements, and this
// package's zero-dependency constraint is not negotiable for a convenience.
var htmlEntities = strings.NewReplacer(
	"&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`, "&#39;", "'", "&#x27;", "'", "&nbsp;", " ",
)

func html(s string) string { return htmlEntities.Replace(s) }
