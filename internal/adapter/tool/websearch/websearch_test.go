package websearch

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/config"
)

func invoke(t *testing.T, tool *Tool, args string) string {
	t.Helper()
	res, err := tool.Invoke(context.Background(), json.RawMessage(args))
	if err != nil {
		t.Fatalf("Invoke returned an error, which it must never do: %v", err)
	}
	return res.Content
}

// A tool the model is told about and that can never answer costs a whole turn
// to discover. Absence is the honest signal.
func TestWithNoProviderThereIsNoTool(t *testing.T) {
	if tool := New(config.Web{}, nil, nil); tool != nil {
		t.Fatal("a Web block with no provider must yield no tool")
	}
	if tool := New(config.Web{Providers: []config.WebProvider{
		{Name: "brave", Enabled: true}, // enabled but keyless: not ready
	}}, nil, nil); tool != nil {
		t.Fatal("a provider that is enabled but not ready must not yield a tool")
	}
}

// Ready means "enabled AND carrying what it needs", and it differs per
// provider: brave needs a key, searxng needs an address, duckduckgo needs
// nothing -- which is exactly why it is the floor of the order.
func TestReadinessIsPerProvider(t *testing.T) {
	for _, tc := range []struct {
		p    config.WebProvider
		want bool
	}{
		{config.WebProvider{Name: "brave", Enabled: true}, false},
		{config.WebProvider{Name: "brave", Enabled: true, APIKey: "k"}, true},
		{config.WebProvider{Name: "searxng", Enabled: true}, false},
		{config.WebProvider{Name: "searxng", Enabled: true, BaseURL: "https://s/"}, true},
		{config.WebProvider{Name: "duckduckgo", Enabled: true}, true},
		{config.WebProvider{Name: "duckduckgo"}, false},
	} {
		got := New(config.Web{Providers: []config.WebProvider{tc.p}}, nil, nil) != nil
		if got != tc.want {
			t.Errorf("%+v: ready = %v, want %v", tc.p, got, tc.want)
		}
	}
}

// AC-3: the result text is picoclaw's, so a prompt written against one harness
// parses the other's output. Pinned literally, because a "shape-compatible"
// format that drifts is not compatible.
func TestTheResultTextIsPicoclawsFormat(t *testing.T) {
	got := Format("go generics", []Result{
		{Title: "Type parameters", URL: "https://go.dev/1", Snippet: "an introduction"},
		{Title: "FAQ", URL: "https://go.dev/2"},
	}, 10)
	want := "Results for: go generics\n" +
		"1. Type parameters\n   https://go.dev/1\n   an introduction\n" +
		"2. FAQ\n   https://go.dev/2"
	if got != want {
		t.Fatalf("format drifted:\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestTheCountIsClampedToTheSchemaMaximum(t *testing.T) {
	var asked string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.Query().Get("count")
		_, _ = w.Write([]byte(`{"web":{"results":[]}}`))
	}))
	defer srv.Close()

	tool := New(config.Web{Providers: []config.WebProvider{
		{Name: "brave", Enabled: true, APIKey: "k", BaseURL: srv.URL},
	}}, srv.Client(), nil)
	invoke(t, tool, `{"query":"x","count":500}`)
	if asked != "10" {
		t.Fatalf("count sent = %q, want 10", asked)
	}
}

// AC-5 and R6.1, the deliberate divergence from picoclaw: a provider that fails
// must not cost the turn when another one would have answered.
func TestAFailingProviderFallsThroughToTheNext(t *testing.T) {
	brave := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"rate limited"}`))
	}))
	defer brave.Close()
	searx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"results":[{"title":"T","url":"https://u","content":"S"}]}`))
	}))
	defer searx.Close()

	tool := New(config.Web{Providers: []config.WebProvider{
		{Name: "brave", Enabled: true, APIKey: "k", BaseURL: brave.URL},
		{Name: "searxng", Enabled: true, BaseURL: searx.URL},
	}}, brave.Client(), nil)

	out := invoke(t, tool, `{"query":"x"}`)
	if !strings.Contains(out, "https://u") {
		t.Fatalf("the second provider did not answer:\n%s", out)
	}
}

// A pinned provider is used ALONE. Quietly querying a different one would send
// the query somewhere the operator did not agree to -- which for a search
// provider means the query text itself, not just the traffic.
func TestAPinnedProviderIsNotSilentlySubstituted(t *testing.T) {
	var searxHit bool
	brave := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer brave.Close()
	searx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		searxHit = true
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer searx.Close()

	tool := New(config.Web{Provider: "brave", Providers: []config.WebProvider{
		{Name: "brave", Enabled: true, APIKey: "k", BaseURL: brave.URL},
		{Name: "searxng", Enabled: true, BaseURL: searx.URL},
	}}, brave.Client(), nil)

	invoke(t, tool, `{"query":"a private query"}`)
	if searxHit {
		t.Fatal("a pinned provider's failure leaked the query to another provider")
	}
}

// AC-1. Every failure is a Result, never an error: a tool-level problem is the
// agent's to react to, not the turn's to die of.
func TestEveryFailureIsAResultAndNeverAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	tool := New(config.Web{Providers: []config.WebProvider{
		{Name: "brave", Enabled: true, APIKey: "k", BaseURL: srv.URL},
	}}, srv.Client(), nil)

	for _, args := range []string{`{"query":"x"}`, `{"query":""}`, `not json`, `{"query":"  "}`} {
		res, err := tool.Invoke(context.Background(), json.RawMessage(args))
		if err != nil {
			t.Errorf("Invoke(%s) returned an error: %v", args, err)
		}
		if res.Content == "" {
			t.Errorf("Invoke(%s) said nothing at all", args)
		}
	}
}

// The key must travel in Brave's own header and nowhere else -- not in the
// query string, which lands in the provider's access log and in any proxy
// between here and it.
func TestTheBraveKeyTravelsInTheHeader(t *testing.T) {
	var header, rawQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header, rawQuery = r.Header.Get("X-Subscription-Token"), r.URL.RawQuery
		_, _ = w.Write([]byte(`{"web":{"results":[]}}`))
	}))
	defer srv.Close()
	tool := New(config.Web{Providers: []config.WebProvider{
		{Name: "brave", Enabled: true, APIKey: "BSA-secret", BaseURL: srv.URL},
	}}, srv.Client(), nil)

	invoke(t, tool, `{"query":"x"}`)
	if header != "BSA-secret" {
		t.Errorf("X-Subscription-Token = %q", header)
	}
	if strings.Contains(rawQuery, "BSA-secret") {
		t.Errorf("the key leaked into the query string: %s", rawQuery)
	}
}

// picoclaw's d/w/m/y has to become each provider's own vocabulary, or the
// filter is silently dropped on every one of them.
func TestTheTimeRangeIsTranslatedPerProvider(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query().Get("freshness")
		_, _ = w.Write([]byte(`{"web":{"results":[]}}`))
	}))
	defer srv.Close()
	tool := New(config.Web{Providers: []config.WebProvider{
		{Name: "brave", Enabled: true, APIKey: "k", BaseURL: srv.URL},
	}}, srv.Client(), nil)

	invoke(t, tool, `{"query":"x","range":"w"}`)
	if got != "pw" {
		t.Fatalf("freshness = %q, want pw for range=w", got)
	}
}

// Snippets arrive with <b> around the matched terms. Passing raw markup to a
// language model spends tokens on nothing.
func TestMarkupIsStrippedFromSnippets(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"web":{"results":[
          {"title":"T","url":"https://u","description":"a <b>match</b> &amp; more"}]}}`))
	}))
	defer srv.Close()
	tool := New(config.Web{Providers: []config.WebProvider{
		{Name: "brave", Enabled: true, APIKey: "k", BaseURL: srv.URL},
	}}, srv.Client(), nil)

	out := invoke(t, tool, `{"query":"x"}`)
	if !strings.Contains(out, "a match & more") {
		t.Fatalf("markup survived into the result:\n%s", out)
	}
}

// DuckDuckGo wraps every result in its own redirector. Handing the model the
// redirect makes every URL it quotes useless outside a browser session.
func TestTheDuckDuckGoRedirectorIsUnwrapped(t *testing.T) {
	page := `<div><a class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fgo.dev%2Fdoc&amp;rut=x">Go <b>docs</b></a>
	         <a class="result__snippet">The <b>Go</b> documentation</a></div>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(page))
	}))
	defer srv.Close()
	tool := New(config.Web{Providers: []config.WebProvider{
		{Name: "duckduckgo", Enabled: true, BaseURL: srv.URL},
	}}, srv.Client(), nil)

	out := invoke(t, tool, `{"query":"go"}`)
	if !strings.Contains(out, "https://go.dev/doc") {
		t.Errorf("the redirector was not unwrapped:\n%s", out)
	}
	if strings.Contains(out, "uddg=") {
		t.Errorf("the redirect URL reached the model:\n%s", out)
	}
	if !strings.Contains(out, "Go docs") {
		t.Errorf("the title was not cleaned:\n%s", out)
	}
}

// When the markup changes this must find nothing rather than guess. A parse
// that invented results would be worse than one that returns none, because the
// model cannot tell the difference.
func TestUnrecognisedMarkupYieldsNoResultsRatherThanGarbage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`<html><body><div class="totally-new-layout">stuff</div></body></html>`))
	}))
	defer srv.Close()
	tool := New(config.Web{Providers: []config.WebProvider{
		{Name: "duckduckgo", Enabled: true, BaseURL: srv.URL},
	}}, srv.Client(), nil)

	out := invoke(t, tool, `{"query":"go"}`)
	if !strings.Contains(out, "No results for: go") {
		t.Fatalf("expected an honest empty answer, got:\n%s", out)
	}
}

// ------------------------------------------------------------ web_fetch

// THE SECURITY PROPERTY OF web_fetch. The URL comes from the model, which means
// it comes from the conversation. Inside the agent network that is a
// server-side request forgery primitive, and 169.254.169.254 is the address
// that turns a fetch tool into a credential leak.
func TestFetchRefusesNonPublicAddresses(t *testing.T) {
	ft := NewFetch(0, nil)
	for _, u := range []string{
		"http://127.0.0.1:8080/",
		"http://localhost/",
		"http://169.254.169.254/latest/meta-data/",
		"http://10.0.0.5/",
		"http://192.168.1.1/",
		"http://[::1]/",
		"http://100.64.0.1/",
	} {
		res, err := ft.Invoke(context.Background(), json.RawMessage(`{"url":`+quote(u)+`}`))
		if err != nil {
			t.Fatalf("%s: Invoke returned an error: %v", u, err)
		}
		if !strings.Contains(res.Content, "failed") {
			t.Errorf("%s was NOT refused: %s", u, res.Content)
		}
	}
}

// The guard is on the resolved IP, so an address the checks above never see
// literally -- a hostname, a mapped form -- is still refused.
func TestThePublicCheckCoversTheEmbeddedForms(t *testing.T) {
	for _, tc := range []struct {
		ip   string
		want bool
	}{
		{"8.8.8.8", true},
		{"::ffff:127.0.0.1", false},  // IPv4-mapped loopback
		{"2002:a00:1::", false},      // 6to4 wrapping 10.0.0.1
		{"64:ff9b::c0a8:101", false}, // NAT64 wrapping 192.168.1.1
		{"2606:4700:4700::1111", true},
	} {
		if got := public(net.ParseIP(tc.ip)); got != tc.want {
			t.Errorf("public(%s) = %v, want %v", tc.ip, got, tc.want)
		}
	}
}

func TestFetchRejectsANonHTTPScheme(t *testing.T) {
	ft := NewFetch(0, nil)
	for _, u := range []string{"file:///etc/passwd", "gopher://x/", "not a url at all", ""} {
		res, _ := ft.Invoke(context.Background(), json.RawMessage(`{"url":`+quote(u)+`}`))
		if !strings.Contains(res.Content, "absolute http or https URL") {
			t.Errorf("%q was not rejected: %s", u, res.Content)
		}
	}
}

// A model reasoning over a silently cut page draws conclusions from an ending
// that is not one.
func TestATruncatedPageSaysSo(t *testing.T) {
	body := strings.Repeat("word ", 5000)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	ft := &FetchTool{hc: srv.Client(), limit: 100, logf: func(string, ...any) {}}
	res, _ := ft.Invoke(context.Background(), json.RawMessage(`{"url":`+quote(srv.URL)+`}`))
	if !strings.Contains(res.Content, "[truncated at 100 characters]") {
		t.Fatalf("truncation was silent:\n%s", res.Content)
	}
}

// JSON must pass through untouched: reflowing it would break it for a model
// that asked for the URL precisely because it is structured.
func TestJSONIsNotReflowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"a": [1, 2], "b": "<not markup>"}`))
	}))
	defer srv.Close()

	ft := &FetchTool{hc: srv.Client(), limit: defaultFetchLimit, logf: func(string, ...any) {}}
	res, _ := ft.Invoke(context.Background(), json.RawMessage(`{"url":`+quote(srv.URL)+`}`))
	if !strings.Contains(res.Content, `"b": "<not markup>"`) {
		t.Fatalf("JSON was mangled:\n%s", res.Content)
	}
}

func TestHTMLIsReducedToReadableText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><head><style>.a{color:red}</style>
		<script>var leak="do not show this"</script></head>
		<body><h1>Title</h1><p>First &amp; second.</p></body></html>`))
	}))
	defer srv.Close()

	ft := &FetchTool{hc: srv.Client(), limit: defaultFetchLimit, logf: func(string, ...any) {}}
	res, _ := ft.Invoke(context.Background(), json.RawMessage(`{"url":`+quote(srv.URL)+`}`))
	if strings.Contains(res.Content, "do not show this") || strings.Contains(res.Content, "color:red") {
		t.Errorf("script or style content survived:\n%s", res.Content)
	}
	if !strings.Contains(res.Content, "Title") || !strings.Contains(res.Content, "First & second.") {
		t.Errorf("readable text was lost:\n%s", res.Content)
	}
}

func quote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}

var _ = errors.New
