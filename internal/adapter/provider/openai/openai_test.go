package openai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

// sseServer replays a recorded event stream. The provider path is testable
// without a key or a network because the adapter's only job is this parse.
func sseServer(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, body)
	}))
}

func drain(t *testing.T, s domain.Stream) []domain.Delta {
	t.Helper()
	var out []domain.Delta
	for {
		d, err := s.Next(context.Background())
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		out = append(out, d)
	}
}

func TestStream_EmitsContentDeltasAndSkipsRoleOnlyFrames(t *testing.T) {
	srv := sseServer(t, strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant"}}]}`,
		`data: {"choices":[{"delta":{"content":"Oi"}}]}`,
		`: keep-alive`,
		`data: {"choices":[{"delta":{"content":", tudo bem?"}}]}`,
		`data: {"usage":{"prompt_tokens":11,"completion_tokens":4,"total_tokens":15}}`,
		`data: [DONE]`,
		``,
	}, "\n\n"))
	defer srv.Close()

	st, err := New(srv.URL, "k", srv.Client()).Complete(context.Background(), domain.Completion{Model: "m"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	defer st.Close()

	got := drain(t, st)
	if len(got) != 2 {
		t.Fatalf("got %d deltas, want 2 (the role-only frame must not become an empty delta): %+v", len(got), got)
	}
	if st.Message().Content != "Oi, tudo bem?" {
		t.Errorf("assembled content = %q", st.Message().Content)
	}
	// FR-8: without stream_options.include_usage most providers omit this, which
	// is why the request asks for it.
	if st.Usage().TotalTokens != 15 {
		t.Errorf("usage = %+v, want 15 total", st.Usage())
	}
}

// The fiddly one: providers stream tool-call arguments one fragment at a time.
// The loop must never see a half-parsed object.
func TestStream_ReassemblesFragmentedToolCallArguments(t *testing.T) {
	srv := sseServer(t, strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"sh"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"cmd\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"ls\"}"}}]}}]}`,
		`data: [DONE]`,
		``,
	}, "\n\n"))
	defer srv.Close()

	st, err := New(srv.URL, "k", srv.Client()).Complete(context.Background(), domain.Completion{Model: "m"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	defer st.Close()
	drain(t, st)

	msg := st.Message()
	if len(msg.ToolCalls) != 1 {
		t.Fatalf("got %d tool calls, want 1", len(msg.ToolCalls))
	}
	tc := msg.ToolCalls[0]
	if tc.ID != "call_1" || tc.Name != "sh" {
		t.Errorf("tool call identity lost: %+v", tc)
	}
	if string(tc.Args) != `{"cmd":"ls"}` {
		t.Errorf("arguments = %s, want {\"cmd\":\"ls\"} -- fragments were not reassembled", tc.Args)
	}
}

// A provider that interleaves its own event types must not inject raw JSON into
// the member's answer. Hermes' hermes.tool.progress was exactly this.
func TestStream_SkipsUnparseableLinesInsteadOfFailing(t *testing.T) {
	srv := sseServer(t, strings.Join([]string{
		`event: vendor.progress`,
		`data: not json at all`,
		`data: {"choices":[{"delta":{"content":"ok"}}]}`,
		`data: [DONE]`,
		``,
	}, "\n\n"))
	defer srv.Close()

	st, err := New(srv.URL, "k", srv.Client()).Complete(context.Background(), domain.Completion{Model: "m"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	defer st.Close()

	if got := drain(t, st); len(got) != 1 || got[0].Content != "ok" {
		t.Errorf("deltas = %+v, want exactly the one real content frame", got)
	}
}

func TestComplete_NonOKStatusIsAnErrorNamingTheBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":"bad key"}`)
	}))
	defer srv.Close()

	_, err := New(srv.URL, "k", srv.Client()).Complete(context.Background(), domain.Completion{Model: "m"})
	if err == nil || !strings.Contains(err.Error(), "bad key") {
		t.Errorf("err = %v, want it to name the provider's own message", err)
	}
}

// The request must ask for usage, or FR-8 is unimplementable against the very
// endpoint it depends on.
func TestComplete_RequestsUsageInTheStream(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	st, err := New(srv.URL, "k", srv.Client()).Complete(context.Background(), domain.Completion{Model: "m"})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	st.Close()

	if !strings.Contains(body, `"include_usage":true`) {
		t.Errorf("request did not ask for usage: %s", body)
	}
	if !strings.Contains(body, `"stream":true`) {
		t.Errorf("request did not ask to stream: %s", body)
	}
}

// captured records the request one Complete call actually put on the wire.
type captured struct {
	body    map[string]any
	headers http.Header
}

func capture(t *testing.T, mutate func(*Client)) captured {
	t.Helper()
	var got captured
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.headers = r.Header.Clone()
		_ = json.NewDecoder(r.Body).Decode(&got.body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	c := New(srv.URL, "the-real-key", srv.Client())
	mutate(c)
	s, err := c.Complete(context.Background(), domain.Completion{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	return got
}

// extra_body is how picoclaw carries provider quirks -- minimax's
// reasoning_split, a vendor's enable_thinking. It has to land at the TOP level
// of the request object, not nested under a key of its own.
func TestExtraBodyIsMergedAtTheTopLevel(t *testing.T) {
	got := capture(t, func(c *Client) {
		c.ExtraBody = map[string]json.RawMessage{
			"reasoning_split": json.RawMessage(`true`),
			"top_p":           json.RawMessage(`0.4`),
		}
	})
	if got.body["reasoning_split"] != true {
		t.Errorf("reasoning_split = %v, want true at the top level", got.body["reasoning_split"])
	}
	if got.body["top_p"] != 0.4 {
		t.Errorf("top_p = %v, want 0.4", got.body["top_p"])
	}
}

// The reserved fields are the contract the loop depends on. A config key that
// could set `stream: false` would turn streaming off from a text box in an
// admin screen -- and streaming is FR-2, the capability picoclaw lacks.
func TestExtraBodyCannotDisplaceTheFieldsTheLoopDependsOn(t *testing.T) {
	got := capture(t, func(c *Client) {
		c.ExtraBody = map[string]json.RawMessage{
			"stream":   json.RawMessage(`false`),
			"model":    json.RawMessage(`"a-different-model"`),
			"messages": json.RawMessage(`[]`),
		}
	})
	if got.body["stream"] != true {
		t.Error("extra_body turned streaming off")
	}
	if got.body["model"] != "m" {
		t.Errorf("model = %v, want m -- extra_body replaced it", got.body["model"])
	}
}

// A custom header that could replace Authorization would let one model's
// configuration send another model's key.
func TestACustomHeaderCannotReplaceTheBearer(t *testing.T) {
	got := capture(t, func(c *Client) {
		c.Headers = map[string]string{
			"Authorization": "Bearer someone-elses-key",
			"X-Vendor-Tag":  "kept",
		}
	})
	if h := got.headers.Get("Authorization"); h != "Bearer the-real-key" {
		t.Errorf("Authorization = %q, want the configured key", h)
	}
	if h := got.headers.Get("X-Vendor-Tag"); h != "kept" {
		t.Errorf("X-Vendor-Tag = %q, want kept -- ordinary headers must still be sent", h)
	}
}

// No extra_body must mean no second marshal pass changed anything.
func TestWithoutExtraBodyTheRequestIsUnchanged(t *testing.T) {
	got := capture(t, func(*Client) {})
	if got.body["stream"] != true || got.body["model"] != "m" {
		t.Fatalf("plain request came out wrong: %v", got.body)
	}
	if _, ok := got.body["extra_body"]; ok {
		t.Error("extra_body leaked into the request as a key of its own")
	}
}

// AC-3 of multimodal-with-fallback, and the reason wireMessage.Content is
// `any`: a text-only turn must keep sending the STRING form. Some providers
// reject the array form outright, others bill it differently, and every one of
// them has been tested against the string form for years.
func TestATextOnlyTurnStillSendsAPlainStringContent(t *testing.T) {
	got := captureCompletion(t, domain.Completion{
		Model:    "m",
		Messages: []domain.Message{{Role: domain.RoleUser, Content: "hello"}},
	})
	msgs := got.body["messages"].([]any)
	last := msgs[len(msgs)-1].(map[string]any)
	if _, isString := last["content"].(string); !isString {
		t.Fatalf("content was not a plain string: %#v", last["content"])
	}
}

// And a turn WITH an image sends the array form, with the bytes inline as a
// data: URL -- never a link, which would have to be reachable BY THE PROVIDER
// and would mean publishing a member's image to show it to a model.
func TestAnImageTurnSendsTypedPartsWithInlineBytes(t *testing.T) {
	got := captureCompletion(t, domain.Completion{
		Model: "m",
		Messages: []domain.Message{{
			Role:    domain.RoleUser,
			Content: "what is this?",
			Attachments: []domain.Attachment{
				{Kind: domain.AttachmentImage, MIME: "image/png", Data: []byte{1, 2, 3}},
			},
		}},
	})
	msgs := got.body["messages"].([]any)
	last := msgs[len(msgs)-1].(map[string]any)
	parts, isArray := last["content"].([]any)
	if !isArray {
		t.Fatalf("content was not an array: %#v", last["content"])
	}
	if len(parts) != 2 {
		t.Fatalf("expected a text part and an image part, got %d", len(parts))
	}
	if parts[0].(map[string]any)["type"] != "text" {
		t.Errorf("the first part is not the text: %#v", parts[0])
	}
	img := parts[1].(map[string]any)
	if img["type"] != "image_url" {
		t.Fatalf("the second part is not an image: %#v", img)
	}
	url := img["image_url"].(map[string]any)["url"].(string)
	if !strings.HasPrefix(url, "data:image/png;base64,") {
		t.Errorf("the image did not travel inline as a data URL: %s", url)
	}
	if strings.Contains(url, "http") {
		t.Errorf("the image travelled as a link: %s", url)
	}
}

// An attachment with no MIME must still be sent as something a provider
// accepts, rather than as `data:;base64,` which every one of them rejects.
func TestAnAttachmentWithNoMIMEGetsADefault(t *testing.T) {
	got := captureCompletion(t, domain.Completion{
		Model: "m",
		Messages: []domain.Message{{
			Role:        domain.RoleUser,
			Attachments: []domain.Attachment{{Kind: domain.AttachmentImage, Data: []byte{1}}},
		}},
	})
	msgs := got.body["messages"].([]any)
	parts := msgs[len(msgs)-1].(map[string]any)["content"].([]any)
	url := parts[0].(map[string]any)["image_url"].(map[string]any)["url"].(string)
	if !strings.HasPrefix(url, "data:image/") {
		t.Fatalf("no MIME default was applied: %s", url)
	}
}

func captureCompletion(t *testing.T, req domain.Completion) captured {
	t.Helper()
	var got captured
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.headers = r.Header.Clone()
		_ = json.NewDecoder(r.Body).Decode(&got.body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()
	s, err := New(srv.URL, "k", srv.Client()).Complete(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	return got
}
