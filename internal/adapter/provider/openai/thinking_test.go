package openai

import (
	"encoding/json"
	"testing"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

func encoded(t *testing.T, c *Client, req domain.Completion) map[string]json.RawMessage {
	t.Helper()
	b, err := c.encode(req)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(b, &obj); err != nil {
		t.Fatalf("the encoded body is not an object: %v", err)
	}
	return obj
}

func field(obj map[string]json.RawMessage, k string) string {
	v, ok := obj[k]
	if !ok {
		return "<absent>"
	}
	return string(v)
}

// NFR-1, and it is the regression bar for the whole feature. A deployment that
// sets no thinking_level anywhere must put byte-identical requests on the wire.
func TestWithoutAConfiguredLevelTheBodyIsUnchanged(t *testing.T) {
	plain := New("https://e", "k", nil)
	req := domain.Completion{Model: "m", Messages: []domain.Message{{Role: domain.RoleUser, Content: "oi"}}}

	before, err := plain.encode(req)
	if err != nil {
		t.Fatal(err)
	}
	// The same request with a level the TURN asked for. Nothing declared the
	// model capable, so nothing goes out.
	req.ThinkingLevel = "high"
	after, err := plain.encode(req)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("the body changed:\n before: %s\n  after: %s", before, after)
	}
}

// AC-1. A configured level rides every request.
func TestAConfiguredLevelIsSentOnEveryRequest(t *testing.T) {
	c := New("https://e", "k", nil)
	c.ThinkingLevel = "high"
	if got := field(encoded(t, c, domain.Completion{Model: "m"}), "reasoning_effort"); got != `"high"` {
		t.Errorf("reasoning_effort = %s, want \"high\"", got)
	}
}

// AC-2. The turn's choice never manufactures a capability the operator did not
// declare. This is the rule that keeps an agent's ambition from turning into a
// 400 on an endpoint that has never heard of the field.
func TestATurnCannotAskForDepthOnAModelThatDeclaredNone(t *testing.T) {
	c := New("https://e", "k", nil) // no ThinkingLevel
	obj := encoded(t, c, domain.Completion{Model: "m", ThinkingLevel: "xhigh"})
	if _, ok := obj["reasoning_effort"]; ok {
		t.Fatalf("a depth field was sent to a model that declared none: %s", field(obj, "reasoning_effort"))
	}
}

// The turn's choice overrides the configured floor, in both directions.
func TestTheTurnsChoiceOverridesTheConfiguredLevel(t *testing.T) {
	c := New("https://e", "k", nil)
	c.ThinkingLevel = "low"
	for _, tc := range []struct{ asked, want string }{
		{"high", `"high"`},
		{"off", `"none"`},
		{"", `"low"`}, // nothing asked: the configured floor
	} {
		got := field(encoded(t, c, domain.Completion{Model: "m", ThinkingLevel: tc.asked}), "reasoning_effort")
		if got != tc.want {
			t.Errorf("asked %q -> %s, want %s", tc.asked, got, tc.want)
		}
	}
}

// adaptive means "the provider decides", and the way to say that on this wire
// is to say nothing. Sending an unrecognised effort value would say something
// else entirely.
func TestAdaptiveEmitsNothing(t *testing.T) {
	c := New("https://e", "k", nil)
	c.ThinkingLevel = "adaptive"
	if _, ok := encoded(t, c, domain.Completion{Model: "m"})["reasoning_effort"]; ok {
		t.Fatal("adaptive put a value on the wire")
	}
}

// AC-4. thinking_body is how a provider dialect the adapter does not know is
// taught to it without a code change -- including teaching it to send nothing.
func TestThinkingBodyReplacesTheDefaultForThatLevelOnly(t *testing.T) {
	c := New("https://e", "k", nil)
	c.ThinkingLevel = "xhigh"
	c.ThinkingBody = map[string]map[string]json.RawMessage{
		"xhigh": {"reasoning_effort": json.RawMessage(`"max"`)},
		"low":   {},
	}
	if got := field(encoded(t, c, domain.Completion{Model: "m"}), "reasoning_effort"); got != `"max"` {
		t.Errorf("xhigh -> %s, want \"max\"", got)
	}
	obj := encoded(t, c, domain.Completion{Model: "m", ThinkingLevel: "low"})
	if _, ok := obj["reasoning_effort"]; ok {
		t.Error("an empty thinking_body entry still emitted a field")
	}
	// A level with no entry keeps the default row.
	if got := field(encoded(t, c, domain.Completion{Model: "m", ThinkingLevel: "medium"}), "reasoning_effort"); got != `"medium"` {
		t.Errorf("medium -> %s, want \"medium\"", got)
	}
}

// thinking_body carries whatever shape a provider wants, not only an effort
// string -- which is the point of it being raw JSON.
func TestThinkingBodyCanSendANestedObject(t *testing.T) {
	c := New("https://e", "k", nil)
	c.ThinkingLevel = "high"
	c.ThinkingBody = map[string]map[string]json.RawMessage{
		"high": {"thinking": json.RawMessage(`{"type":"enabled","budget_tokens":32000}`)},
	}
	obj := encoded(t, c, domain.Completion{Model: "m"})
	if got := field(obj, "thinking"); got != `{"type":"enabled","budget_tokens":32000}` {
		t.Errorf("thinking = %s", got)
	}
	if _, ok := obj["reasoning_effort"]; ok {
		t.Error("the default row was emitted alongside the override")
	}
}

// AC-5. The operator's pin wins. extra_body means "always this"; a feature that
// quietly overrode it would be a config key that stopped meaning what it says.
func TestExtraBodyIsMergedAfterTheDepthFieldAndWins(t *testing.T) {
	c := New("https://e", "k", nil)
	c.ThinkingLevel = "high"
	c.ExtraBody = map[string]json.RawMessage{"reasoning_effort": json.RawMessage(`"low"`)}
	if got := field(encoded(t, c, domain.Completion{Model: "m", ThinkingLevel: "xhigh"}), "reasoning_effort"); got != `"low"` {
		t.Errorf("reasoning_effort = %s, want the pinned \"low\"", got)
	}
}

// NoThinking beats everything. It is what the loop sets to retry a request
// whose depth field the endpoint rejected, so if anything could survive it the
// retry would be identical to the request that already failed.
func TestNoThinkingRemovesTheFieldWhateverElseIsConfigured(t *testing.T) {
	c := New("https://e", "k", nil)
	c.ThinkingLevel = "high"
	c.ThinkingBody = map[string]map[string]json.RawMessage{
		"high": {"thinking": json.RawMessage(`{"type":"enabled"}`)},
	}
	obj := encoded(t, c, domain.Completion{Model: "m", ThinkingLevel: "xhigh", NoThinking: true})
	for _, k := range []string{"reasoning_effort", "thinking"} {
		if _, ok := obj[k]; ok {
			t.Errorf("%s survived NoThinking", k)
		}
	}
}

// The reserved set is not relaxed for this path. A thinking_body that could
// overwrite `stream` would turn streaming off from a text box in an admin
// screen -- the same argument extra_body's guard already makes.
func TestThinkingBodyCannotTouchTheReservedFields(t *testing.T) {
	c := New("https://e", "k", nil)
	c.ThinkingLevel = "high"
	c.ThinkingBody = map[string]map[string]json.RawMessage{
		"high": {"stream": json.RawMessage(`false`), "model": json.RawMessage(`"other"`)},
	}
	obj := encoded(t, c, domain.Completion{Model: "mine"})
	if field(obj, "stream") != "true" {
		t.Errorf("stream = %s", field(obj, "stream"))
	}
	if field(obj, "model") != `"mine"` {
		t.Errorf("model = %s", field(obj, "model"))
	}
}
