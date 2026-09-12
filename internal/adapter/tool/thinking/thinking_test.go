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

// The tool exists whatever the registry holds, and this test replaced its exact
// inverse.
//
// The old rule gated the tool on some model declaring a thinking_level, on the
// imagegen precedent: a choice that changes nothing on the wire is a wasted
// turn. The premise it rested on -- that no keyless fallback could work -- was
// wrong twice over. Depth can select a MODEL (Loop.preferDeep), which is how
// providers express thinking more often than as a field, and failing that it is
// said in the PROMPT (Loop.deliberate), which works on every model there is.
//
// What the gate actually shipped was an agent running deepseek-chat that could
// not ask for more thought under any circumstances, and a boot line saying so to
// an operator who could do nothing about it from inside the container.
func TestTheToolExistsWhateverTheRegistryDeclares(t *testing.T) {
	for name, r := range map[string]config.Registry{
		"an empty registry":                  {},
		"models declaring no thinking_level": reg("", ""),
		"a model declaring one":              reg("", "high"),
	} {
		if New(r) == nil {
			t.Errorf("%s: no tool", name)
		}
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
