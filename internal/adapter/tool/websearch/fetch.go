package websearch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

// web_fetch retrieves one page as text.
//
// It is a separate tool from web_search on purpose, and picoclaw splits them
// the same way: search chooses what to read, fetch reads it, and a model that
// can only search ends up quoting snippets it cannot verify.
//
// THE URL COMES FROM THE MODEL, WHICH MEANS IT COMES FROM THE MEMBER.
//
// That is the whole security argument for this file. Everything else the
// harness calls is an endpoint an administrator configured; this is an address
// chosen by whatever the conversation talked the model into. Inside the agent
// network that is a server-side request forgery primitive: the proxy's Docker
// socket, the metadata endpoint, another tenant's container. So the fetch
// refuses private and link-local destinations, and it does so at DIAL time
// rather than by inspecting the URL -- a hostname that resolves to 169.254.169.254
// passes every string check ever written.

const defaultFetchLimit = 50000 // characters, picoclaw's defaultMaxChars

// FetchTool implements web_fetch.
type FetchTool struct {
	hc    *http.Client
	limit int
	logf  func(string, ...any)
}

// NewFetch builds the tool. limitBytes is tools.web.fetch_limit_bytes; zero
// takes picoclaw's default.
func NewFetch(limitBytes int64, logf func(string, ...any)) *FetchTool {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	limit := defaultFetchLimit
	if limitBytes > 0 {
		limit = int(limitBytes)
	}
	return &FetchTool{hc: guardedClient(), limit: limit, logf: logf}
}

func (t *FetchTool) Name() string { return "web_fetch" }

func (t *FetchTool) Schema() domain.ToolSchema {
	return domain.ToolSchema{
		Name:        "web_fetch",
		Description: "Fetch the contents of one URL as plain text. Use it to read a page found with web_search.",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {"url": {"type": "string", "description": "Absolute http(s) URL to fetch"}},
  "required": ["url"]
}`),
	}
}

func (t *FetchTool) Invoke(ctx context.Context, raw json.RawMessage) (domain.Result, error) {
	var a struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		return domain.Result{Content: "web_fetch: could not read the arguments: " + err.Error()}, nil
	}
	u, err := url.Parse(strings.TrimSpace(a.URL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return domain.Result{Content: "web_fetch: `url` must be an absolute http or https URL."}, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return domain.Result{Content: "web_fetch: " + err.Error()}, nil
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; crab-ganglion/1.0)")
	req.Header.Set("Accept", "text/html,text/plain,application/json;q=0.9,*/*;q=0.5")

	resp, err := t.hc.Do(req)
	if err != nil {
		// The dial guard's refusal arrives here. Reported as a Result rather
		// than an error, like every other tool-level problem: the model should
		// be able to say "I could not reach that" and carry on.
		return domain.Result{Content: fmt.Sprintf("web_fetch %s failed: %v", u, err)}, nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return domain.Result{Content: fmt.Sprintf("web_fetch %s: %s", u, resp.Status)}, nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return domain.Result{Content: fmt.Sprintf("web_fetch %s: %v", u, err)}, nil
	}

	text := toText(resp.Header.Get("Content-Type"), string(body))
	truncated := ""
	if len(text) > t.limit {
		text = text[:t.limit]
		// Said in the result rather than left implicit: a model reasoning over
		// a silently cut page draws conclusions from an ending that is not one.
		truncated = fmt.Sprintf("\n\n[truncated at %d characters]", t.limit)
	}
	return domain.Result{Content: u.String() + "\n\n" + text + truncated}, nil
}

var (
	// RE2 has no backreferences, so each element is named on both sides
	// rather than captured and referred back to.
	scriptRe = regexp.MustCompile(`(?is)<script[^>]*>.*?</script>|<style[^>]*>.*?</style>|<noscript[^>]*>.*?</noscript>`)
	blockRe  = regexp.MustCompile(`(?i)</(p|div|li|tr|h[1-6]|section|article|br)\s*>`)
)

// toText reduces HTML to something worth spending tokens on. JSON and plain
// text pass through untouched -- reflowing JSON would break it for a model that
// asked for it precisely because it is structured.
func toText(contentType, body string) string {
	ct := strings.ToLower(contentType)
	if !strings.Contains(ct, "html") {
		return body
	}
	s := scriptRe.ReplaceAllString(body, " ")
	s = blockRe.ReplaceAllString(s, "\n")
	s = tagRe.ReplaceAllString(s, " ")
	s = html(s)
	// Collapse runs of blank lines, keeping paragraph structure: it is what
	// makes the difference between a readable page and one wall of words.
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if l := strings.Join(strings.Fields(line), " "); l != "" {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

// guardedClient refuses to connect to anything that is not a public address.
//
// The check is on the RESOLVED IP at dial time, which is the only place it can
// be correct: a URL check cannot see where a hostname points, and a
// resolve-then-check-then-connect sequence is a DNS rebinding window.
func guardedClient() *http.Client {
	d := &net.Dialer{Timeout: 10 * time.Second}
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				host, _, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}
				ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
				if err != nil {
					return nil, err
				}
				for _, ip := range ips {
					if !public(ip.IP) {
						return nil, fmt.Errorf("refusing to fetch a non-public address (%s)", ip.IP)
					}
				}
				return d.DialContext(ctx, network, addr)
			},
			// Redirects are followed by the client, and each hop dials again
			// through this same guard -- so a public URL that redirects to
			// 169.254.169.254 is refused on the second dial, not the first.
			MaxIdleConns:    10,
			IdleConnTimeout: 30 * time.Second,
		},
	}
}

// public reports whether an address is one the open internet routes.
//
// Everything else is refused: loopback and private ranges reach the rest of
// this stack, and link-local reaches cloud metadata -- 169.254.169.254 is the
// address that turns a fetch tool into a credential leak.
func public(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
		return false
	}
	// Carrier-grade NAT (100.64.0.0/10) is not covered by IsPrivate and is
	// routable inside a provider's network.
	if v4 := ip.To4(); v4 != nil && v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
		return false
	}
	// An IPv4-mapped or 6to4-embedded v6 address carries a v4 address that the
	// checks above would otherwise never see.
	if v4 := ip.To4(); v4 == nil && len(ip) == net.IPv6len {
		if ip[0] == 0x20 && ip[1] == 0x02 { // 2002::/16, 6to4
			return public(net.IPv4(ip[2], ip[3], ip[4], ip[5]))
		}
		if ip[0] == 0x00 && ip[1] == 0x64 && ip[2] == 0xff && ip[3] == 0x9b { // 64:ff9b::/96, NAT64
			return public(net.IPv4(ip[12], ip[13], ip[14], ip[15]))
		}
	}
	return true
}
