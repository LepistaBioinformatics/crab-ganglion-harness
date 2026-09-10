// Package proxy is the Approver adapter: it asks crab-shell-proxy.
//
// DEC-1 -- the harness never decides who may approve. The proxy is where
// mycelium's unforgeable account id already lands; a policy engine in here
// would have no information the proxy lacks and no way to obtain it.
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

// Client asks one endpoint for a decision.
type Client struct {
	Endpoint string
	Token    string
	HTTP     *http.Client
	// Gated names the tools that require approval. Empty means nothing is
	// gated -- the v1 default. The path still runs for every call, so it is
	// exercised from the first deploy rather than sitting untested.
	Gated map[string]bool
}

func New(endpoint, token string, gated []string, hc *http.Client) *Client {
	if hc == nil {
		hc = http.DefaultClient
	}
	g := map[string]bool{}
	for _, n := range gated {
		g[n] = true
	}
	return &Client{Endpoint: endpoint, Token: token, HTTP: hc, Gated: g}
}

func (c *Client) Request(ctx context.Context, a domain.ActionRequest) (domain.Decision, error) {
	if !c.Gated[a.Call.Name] {
		return domain.Decision{Allowed: true, Reason: "not gated"}, nil
	}
	body, err := json.Marshal(wireRequest{
		SessionKey: string(a.SessionKey),
		SessionID:  string(a.SessionID),
		ToolCallID: a.Call.ID,
		Tool:       a.Call.Name,
		Arguments:  a.Call.Args,
	})
	if err != nil {
		return domain.Decision{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Endpoint, bytes.NewReader(body))
	if err != nil {
		return domain.Decision{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return domain.Decision{}, fmt.Errorf("approval request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return domain.Decision{}, fmt.Errorf("approval endpoint: %s", resp.Status)
	}
	var out wireDecision
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return domain.Decision{}, fmt.Errorf("decode decision: %w", err)
	}
	return domain.Decision{Allowed: out.Allowed, Reason: out.Reason, By: out.By}, nil
}

type wireRequest struct {
	SessionKey string          `json:"session_key"`
	SessionID  string          `json:"session_id"`
	ToolCallID string          `json:"tool_call_id"`
	Tool       string          `json:"tool"`
	Arguments  json.RawMessage `json:"arguments"`
}

type wireDecision struct {
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason"`
	By      string `json:"by"`
}
