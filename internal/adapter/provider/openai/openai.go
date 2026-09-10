// Package openai is the Provider adapter for any OpenAI-compatible endpoint.
//
// It assembles partial tool-call arguments so the loop never sees a half-parsed
// object -- that reassembly is the one genuinely fiddly part of this wire
// format, and doing it here is why domain.Stream can promise a finished
// Message after io.EOF.
package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

// maxLine matches the transcript store's, and for the same reason: a long
// assistant answer exceeds bufio.Scanner's 64KiB default, which the Hermes
// runner discovered the hard way.
const maxLine = 8 << 20

// Client talks to one OpenAI-compatible endpoint.
//
// BaseURL is mandatory. There is no provider registry mapping a name to an
// endpoint, and an unset base URL fails as a 404 or an auth error from the
// provider rather than as a configuration error -- recorded by the Hermes work
// as the reason config.ModelConfig grew a BaseURL field.
type Client struct {
	BaseURL string
	APIKey  string
	HTTP    *http.Client

	// ExtraBody is merged into the request object at the TOP LEVEL, which is
	// where every provider quirk this format needs lives: minimax's
	// reasoning_split, a vendor's enable_thinking, a routing preference. It is
	// picoclaw's `extra_body` and carries the same values.
	//
	// Merged rather than templated, and merged so that it can never displace a
	// field this adapter sets: model, stream, messages and tools are the
	// contract the loop depends on, and a config key that could overwrite
	// `stream` would turn streaming off from a text box in an admin screen.
	ExtraBody map[string]json.RawMessage

	// Headers are sent verbatim. Authorization is set after them, so a header
	// declared here cannot replace the bearer -- a config key that could would
	// let one model's configuration send another model's key.
	Headers map[string]string

	// ThinkingLevel is this model's configured depth, from picoclaw's
	// `thinking_level`. EMPTY MEANS NO DEPTH FIELD IS EVER SENT to this
	// endpoint, whatever a turn asks for.
	//
	// That is the capability declaration, and it is the operator's to make.
	// picoclaw needs a provider table here because it speaks four dialects and
	// can therefore know that DeepSeek takes reasoning_effort and Zhipu does
	// not; this adapter speaks one wire to every endpoint in the registry and
	// cannot tell them apart. Sending a field an endpoint rejects would turn
	// depth selection into a turn failure, which is the production shape
	// `vision-unsupported-glm` already cost us once.
	ThinkingLevel string

	// ThinkingBody overrides thinkingFor's default per level. A level present
	// here replaces that row entirely, including with {} to emit nothing.
	ThinkingBody map[string]map[string]json.RawMessage
}

func New(baseURL, apiKey string, hc *http.Client) *Client {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), APIKey: apiKey, HTTP: hc}
}

// reserved names the request fields ExtraBody may not touch. See Client.ExtraBody.
var reserved = map[string]bool{
	"model": true, "stream": true, "messages": true, "tools": true, "stream_options": true,
}

// effort maps a level onto the OpenAI-compatible vocabulary.
//
// `xhigh` collapses onto `high` because reasoning_effort has no fourth step and
// inventing one would send a value no endpoint recognises. An operator whose
// provider does have a higher step reaches it through ThinkingBody, and the
// collapse is logged once per model at boot so the ceiling is known rather than
// discovered.
//
// `adaptive` emits nothing: it means "the provider decides", and the way to say
// that on this wire is to say nothing.
var effort = map[string]string{
	"off": "none", "low": "low", "medium": "medium", "high": "high", "xhigh": "high",
}

// thinkingFor returns the fields this request should carry for the level, or
// nil for none.
func (c *Client) thinkingFor(req domain.Completion) map[string]json.RawMessage {
	// The model never declared a level, so it is never sent one. This beats
	// anything the turn asked for -- an agent that chooses `xhigh` on a model
	// the operator did not vouch for gets nothing, not a 400.
	if req.NoThinking || c.ThinkingLevel == "" {
		return nil
	}
	level := req.ThinkingLevel
	if level == "" {
		level = c.ThinkingLevel
	}
	if over, ok := c.ThinkingBody[level]; ok {
		return over
	}
	e, ok := effort[level]
	if !ok {
		return nil
	}
	return map[string]json.RawMessage{"reasoning_effort": json.RawMessage(`"` + e + `"`)}
}

// encode marshals the request and merges the depth fields, then ExtraBody, over
// it.
//
// Two marshal passes rather than a struct with an inline map, because the
// merge has to happen at the top level of an object whose other fields are
// typed -- and encoding/json offers no way to say that.
//
// ORDER MATTERS AND IS ASSERTED. ExtraBody goes LAST, so an operator who pinned
// `extra_body.reasoning_effort` keeps it: they said "always this", and a
// feature that quietly won that argument would be a config key that stopped
// meaning what it says.
func (c *Client) encode(req domain.Completion) ([]byte, error) {
	body, err := json.Marshal(buildRequest(req))
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	think := c.thinkingFor(req)
	if len(c.ExtraBody) == 0 && len(think) == 0 {
		return body, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, fmt.Errorf("merge request body: %w", err)
	}
	for _, m := range []map[string]json.RawMessage{think, c.ExtraBody} {
		for k, v := range m {
			if reserved[k] {
				continue
			}
			obj[k] = v
		}
	}
	return json.Marshal(obj)
}

func (c *Client) Complete(ctx context.Context, req domain.Completion) (domain.Stream, error) {
	body, err := c.encode(req)
	if err != nil {
		return nil, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept", "text/event-stream")
	for k, v := range c.Headers {
		hreq.Header.Set(k, v)
	}
	// LAST, so no configured header can replace it.
	if c.APIKey != "" {
		hreq.Header.Set("Authorization", "Bearer "+c.APIKey)
	}

	resp, err := c.HTTP.Do(hreq)
	if err != nil {
		return nil, fmt.Errorf("provider request: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("provider %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 0, 64<<10), maxLine)
	return &stream{body: resp.Body, sc: sc, msg: domain.Message{Role: domain.RoleAssistant}, tools: map[int]*domain.ToolCall{}}, nil
}

type stream struct {
	body  io.ReadCloser
	sc    *bufio.Scanner
	msg   domain.Message
	usage domain.Usage
	tools map[int]*domain.ToolCall
	order []int
	args  map[int]*strings.Builder
	done  bool
}

func (s *stream) Next(_ context.Context) (domain.Delta, error) {
	for s.sc.Scan() {
		line := strings.TrimSpace(s.sc.Text())
		// Only data: lines carry payload. event:, id: and comment lines are
		// skipped -- a comment is also how a keep-alive arrives.
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			s.finish()
			return domain.Delta{}, io.EOF
		}
		var ch chunk
		if err := json.Unmarshal([]byte(payload), &ch); err != nil {
			// A line that is not a chunk is skipped, not fatal: providers
			// interleave their own event types and treating one as content
			// injects raw JSON into the member's answer.
			continue
		}
		if ch.Usage != nil {
			s.usage = domain.Usage{
				PromptTokens:     ch.Usage.PromptTokens,
				CompletionTokens: ch.Usage.CompletionTokens,
				TotalTokens:      ch.Usage.TotalTokens,
			}
		}
		if len(ch.Choices) == 0 {
			continue
		}
		d := ch.Choices[0].Delta
		s.absorbToolCalls(d.ToolCalls)

		// A role-only first frame carries no content; emitting it would push an
		// empty delta the client renders as nothing but counts as activity.
		if d.Content == "" && d.Reasoning == "" {
			continue
		}
		s.msg.Content += d.Content
		s.msg.Reasoning += d.Reasoning
		return domain.Delta{Content: d.Content, Reasoning: d.Reasoning}, nil
	}
	if err := s.sc.Err(); err != nil {
		return domain.Delta{}, fmt.Errorf("read stream: %w", err)
	}
	s.finish()
	return domain.Delta{}, io.EOF
}

// absorbToolCalls accumulates the partial argument strings a provider streams
// one fragment at a time, keyed by index.
func (s *stream) absorbToolCalls(tcs []toolCallDelta) {
	if s.args == nil {
		s.args = map[int]*strings.Builder{}
	}
	for _, tc := range tcs {
		cur, ok := s.tools[tc.Index]
		if !ok {
			cur = &domain.ToolCall{}
			s.tools[tc.Index] = cur
			s.order = append(s.order, tc.Index)
			s.args[tc.Index] = &strings.Builder{}
		}
		if tc.ID != "" {
			cur.ID = tc.ID
		}
		if tc.Function.Name != "" {
			cur.Name = tc.Function.Name
		}
		if tc.Function.Arguments != "" {
			s.args[tc.Index].WriteString(tc.Function.Arguments)
		}
	}
}

func (s *stream) finish() {
	if s.done {
		return
	}
	s.done = true
	for _, i := range s.order {
		tc := s.tools[i]
		raw := s.args[i].String()
		if raw == "" {
			raw = "{}"
		}
		tc.Args = json.RawMessage(raw)
		s.msg.ToolCalls = append(s.msg.ToolCalls, *tc)
	}
}

func (s *stream) Message() domain.Message { s.finish(); return s.msg }
func (s *stream) Usage() domain.Usage     { return s.usage }
func (s *stream) Close() error            { return s.body.Close() }

// --- wire types -------------------------------------------------------------

type chunk struct {
	Choices []struct {
		Delta struct {
			Content   string          `json:"content"`
			Reasoning string          `json:"reasoning_content"`
			ToolCalls []toolCallDelta `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

type toolCallDelta struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// wireMessage's Content is `any` for one reason, and it is the whole of this
// format's multimodal support: OpenAI accepts EITHER a plain string OR an array
// of typed parts, and the two are not interchangeable.
//
// A text-only turn must keep sending the string. Some providers reject the
// array form outright, others bill it differently, and every one of them has
// been tested against the string form for years -- so "always send parts" would
// change the bytes of every request this harness has ever made in order to
// serve the small fraction that carry an image.
type wireMessage struct {
	Role       string     `json:"role"`
	Content    any        `json:"content"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolCalls  []wireCall `json:"tool_calls,omitempty"`
}

// contentPart is one element of the array form.
type contentPart struct {
	Type     string    `json:"type"`
	Text     string    `json:"text,omitempty"`
	ImageURL *imageURL `json:"image_url,omitempty"`
}

type imageURL struct {
	URL string `json:"url"`
}

// contentFor returns the string form for an ordinary message and the array form
// for one carrying attachments.
//
// Images travel as data: URLs rather than as links. A link would have to be
// reachable BY THE PROVIDER, which means publishing the member's image on the
// open internet to show it to a model -- the exact thing a per-tenant isolated
// workspace exists to prevent.
func contentFor(m domain.Message) any {
	if len(m.Attachments) == 0 {
		return m.Content
	}
	parts := make([]contentPart, 0, len(m.Attachments)+1)
	if m.Content != "" {
		parts = append(parts, contentPart{Type: "text", Text: m.Content})
	}
	for _, a := range m.Attachments {
		if a.Kind != domain.AttachmentImage || len(a.Data) == 0 {
			continue
		}
		mime := a.MIME
		if mime == "" {
			mime = "image/png"
		}
		parts = append(parts, contentPart{
			Type:     "image_url",
			ImageURL: &imageURL{URL: "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(a.Data)},
		})
	}
	if len(parts) == 0 {
		return m.Content
	}
	return parts
}

type wireCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type wireRequest struct {
	Model    string        `json:"model"`
	Stream   bool          `json:"stream"`
	Messages []wireMessage `json:"messages"`
	Tools    []wireTool    `json:"tools,omitempty"`
	// StreamOptions asks for usage on the final chunk. Without it most
	// OpenAI-compatible providers omit token counts entirely from a stream,
	// which would make FR-8 unimplementable against the very endpoint it needs.
	StreamOptions *streamOptions `json:"stream_options,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type wireTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Parameters  json.RawMessage `json:"parameters,omitempty"`
	} `json:"function"`
}

func buildRequest(req domain.Completion) wireRequest {
	out := wireRequest{
		Model:         req.Model,
		Stream:        true,
		StreamOptions: &streamOptions{IncludeUsage: true},
	}
	if req.System != "" {
		out.Messages = append(out.Messages, wireMessage{Role: string(domain.RoleSystem), Content: req.System})
	}
	if req.Window.Summary != "" {
		out.Messages = append(out.Messages, wireMessage{Role: string(domain.RoleSystem), Content: req.Window.Summary})
	}
	for _, m := range req.Messages {
		wm := wireMessage{Role: string(m.Role), Content: contentFor(m), ToolCallID: m.ToolCallID}
		for _, tc := range m.ToolCalls {
			var c wireCall
			c.ID = tc.ID
			c.Type = "function"
			c.Function.Name = tc.Name
			c.Function.Arguments = string(tc.Args)
			wm.ToolCalls = append(wm.ToolCalls, c)
		}
		out.Messages = append(out.Messages, wm)
	}
	for _, t := range req.Tools {
		var wt wireTool
		wt.Type = "function"
		wt.Function.Name = t.Name
		wt.Function.Description = t.Description
		wt.Function.Parameters = t.Parameters
		out.Tools = append(out.Tools, wt)
	}
	return out
}
