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
}

func New(baseURL, apiKey string, hc *http.Client) *Client {
	if hc == nil {
		hc = http.DefaultClient
	}
	return &Client{BaseURL: strings.TrimRight(baseURL, "/"), APIKey: apiKey, HTTP: hc}
}

func (c *Client) Complete(ctx context.Context, req domain.Completion) (domain.Stream, error) {
	body, err := json.Marshal(buildRequest(req))
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept", "text/event-stream")
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

type wireMessage struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolCalls  []wireCall `json:"tool_calls,omitempty"`
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
		wm := wireMessage{Role: string(m.Role), Content: m.Content, ToolCallID: m.ToolCallID}
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
