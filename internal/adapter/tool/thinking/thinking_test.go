package thinking

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/config"
	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

func reg(levels ...string) config.Registry {
	r := config.Registry{Default: "m0"}
	for i, l := range levels {
		name := "m" + string(rune('0'+i))
		r.Models = append(r.Models, config.ModelSpec{
			Name: name, APIBase: "https://e", Enabled: true, ThinkingLevel: l,
		})
		if i > 0 {
			r.DefaultFallbacks = append(r.DefaultFallbacks, name)
		}
	}
	return r
}

func invoke(t *testing.T, tool *Tool, d *domain.Depth, in string) domain.Result {
	t.Helper()
	ctx := context.Background()
	if d != nil {
		ctx = domain.WithDepth(ctx, d)
	}
	res, err := tool.Invoke(ctx, json.RawMessage(in))
	if err != nil {
		t.Fatalf("Invoke returned an error, which it must never do: %v", err)
	}
	return res
}

// AC-7. With no model declaring a level there is NO TOOL.
//
// The imagegen precedent: a model told it can choose a depth, whose every
// choice then changes nothing on the wire, has spent a turn learning what boot
// already knew. And unlike search there is no keyless fallback that could have
// worked -- the operator has to declare the key.
func TestWithNoModelDeclaringALevelThereIsNoTool(t *testing.T) {
	if New(config.Registry{}) != nil {
		t.Error("an empty registry yielded a tool")
	}
	if New(reg("", "")) != nil {
		t.Error("a registry whose models declare no thinking_level yielded a tool")
	}
	if New(reg("", "high")) == nil {
		t.Error("one model in the chain declaring a level should be enough")
	}
}

func TestAChosenLevelLandsOnTheTurnsCell(t *testing.T) {
	d := &domain.Depth{}
	res := invoke(t, New(reg("low")), d, `{"level":"XHigh","reason":"two schemas disagree"}`)

	if d.Level() != "xhigh" {
		t.Errorf("level = %q, want xhigh (normalized)", d.Level())
	}
	if d.Reason() != "two schemas disagree" {
		t.Errorf("reason = %q", d.Reason())
	}
	if !strings.Contains(res.Content, "xhigh") {
		t.Errorf("the model was not told what it set: %q", res.Content)
	}
}

// A typo must not also cost the depth the agent had already chosen. Refusing
// AND resetting would punish a mistake twice.
func TestAnUnknownLevelIsRefusedWithoutDisturbingTheCurrentOne(t *testing.T) {
	d := &domain.Depth{}
	d.Set("high", "set earlier")
	res := invoke(t, New(reg("low")), d, `{"level":"ultra"}`)

	if d.Level() != "high" {
		t.Errorf("level = %q; the refusal changed it", d.Level())
	}
	if !strings.Contains(res.Content, "ultra") || !strings.Contains(res.Content, "xhigh") {
		t.Errorf("the refusal did not say what was wrong or what is allowed: %q", res.Content)
	}
}

func TestEveryFailureIsAResultAndNeverAnError(t *testing.T) {
	tool := New(reg("high"))
	for _, in := range []string{`{"level":"high"}`, `{"level":""}`, `{}`, `not json`} {
		res, err := tool.Invoke(domain.WithDepth(context.Background(), &domain.Depth{}), json.RawMessage(in))
		if err != nil {
			t.Errorf("Invoke(%s) returned an error: %v", in, err)
		}
		if res.Content == "" {
			t.Errorf("Invoke(%s) said nothing", in)
		}
	}
}

// Outside a turn there is no cell, and the tool says so rather than reporting
// success for something that changed nothing.
func TestWithNoTurnInProgressTheToolSaysSo(t *testing.T) {
	res := invoke(t, New(reg("high")), nil, `{"level":"high"}`)
	if !strings.Contains(res.Content, "no turn") {
		t.Errorf("content = %q", res.Content)
	}
}

// The schema's enum IS the vocabulary. If the two drift, the model is offered a
// value the parser refuses.
func TestTheSchemaOffersExactlyTheSixLevels(t *testing.T) {
	var schema struct {
		Properties struct {
			Level struct {
				Enum []string `json:"enum"`
			} `json:"level"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(New(reg("high")).Schema().Parameters, &schema); err != nil {
		t.Fatalf("the schema is not valid JSON: %v", err)
	}
	got := schema.Properties.Level.Enum
	if len(got) != len(config.ThinkingLevels) {
		t.Fatalf("the schema offers %v, the parser accepts %v", got, config.ThinkingLevels)
	}
	for i, l := range config.ThinkingLevels {
		if got[i] != l {
			t.Errorf("schema[%d] = %q, want %q", i, got[i], l)
		}
	}
}
