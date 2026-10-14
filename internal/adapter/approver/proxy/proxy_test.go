package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

func ask(t *testing.T, c *Client, tool string) domain.Decision {
	t.Helper()
	dec, err := c.Request(context.Background(), domain.ActionRequest{
		SessionKey: "sk-1", SessionID: "conv-1",
		Call: domain.ToolCall{ID: "1", Name: tool, Args: json.RawMessage(`{}`)},
	})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	return dec
}

// THE GATE IS WHAT SAYS A PERSON WAS ASKED, and it is the only thing that can:
// an ungated call and a real allow are both `Allowed: true` at the call site.
// Without the distinction the transcript would carry an approval record under
// every tool call in a product whose gate list is empty, which is every product
// until somebody fills GANGLION_GATED_TOOLS.
func TestAnUngatedCallIsAllowedWithoutAskingAnybody(t *testing.T) {
	var reached bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		_ = json.NewEncoder(w).Encode(wireDecision{Allowed: true})
	}))
	defer srv.Close()

	dec := ask(t, New(srv.URL, "tok", []string{"schedule_create"}, srv.Client()), "sh")

	if reached {
		t.Error("an ungated call was put to the endpoint")
	}
	if !dec.Allowed {
		t.Error("an ungated call was not allowed")
	}
	if dec.Gate != "" {
		t.Errorf("gate = %q, want nothing: nobody was asked", dec.Gate)
	}
}

func TestAnAllowedGatedCallComesBackMarkedAllowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(wireDecision{Allowed: true, By: "acc-1"})
	}))
	defer srv.Close()

	dec := ask(t, New(srv.URL, "tok", []string{"schedule_create"}, srv.Client()), "schedule_create")

	if dec.Gate != domain.EventOK {
		t.Errorf("gate = %q, want %q", dec.Gate, domain.EventOK)
	}
	if dec.By != "acc-1" {
		t.Errorf("by = %q, want the account the proxy vouched for", dec.By)
	}
}

// A refusal with no reason is ordinary now: the proxy stopped substituting one,
// so that an English sentence it composed could not end up quoted as the
// member's own words in a transcript they read in another language.
func TestARefusedGatedCallCarriesWhateverReasonTheMemberGave(t *testing.T) {
	for _, tc := range []struct{ name, reason string }{
		{"with a reason", "nao quero isso agendado"},
		{"with none", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(wireDecision{Allowed: false, Reason: tc.reason})
			}))
			defer srv.Close()

			dec := ask(t, New(srv.URL, "tok", []string{"schedule_create"}, srv.Client()), "schedule_create")

			if dec.Allowed {
				t.Error("a refusal came back allowed")
			}
			if dec.Gate != domain.EventDenied {
				t.Errorf("gate = %q, want %q", dec.Gate, domain.EventDenied)
			}
			if dec.Reason != tc.reason {
				t.Errorf("reason = %q, want %q verbatim", dec.Reason, tc.reason)
			}
		})
	}
}
