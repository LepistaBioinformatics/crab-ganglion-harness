// Package thinking is the set_reasoning_depth tool.
//
// picoclaw has no equivalent. Its `thinking_level` is fixed per model entry at
// configuration time, and an exhaustive search of v0.3.1 for deep_research,
// deepsearch, ultrathink and a /think command returns nothing -- depth there is
// reachable only as a side effect of routing to a different agent, and the
// spawn/delegate/subagent schemas expose no effort parameter at all.
//
// So this is the part of the feature with no compatibility constraint: the KEY
// is picoclaw's and the six values are picoclaw's, but choosing among them at
// runtime is ours.
//
// A tool because a model can only act through one. That costs an iteration of
// the turn's budget, which is why the choice is STICKY for the remainder of the
// turn rather than per completion: paying one call to raise the depth for the
// hard part -- which is usually after the tool results come back, not before --
// is worth it; paying one call per completion is not.
package thinking

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/config"
	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

// Tool implements set_reasoning_depth.
type Tool struct{}

// New builds the tool. It is always available.
//
// THIS USED TO RETURN NIL when no model declared a thinking_level, and the
// reason it gave was: "a model told it can choose a depth, whose every choice
// then changes nothing on the wire, has spent a turn learning what boot already
// knew -- and unlike search there is no keyless fallback that could have
// worked."
//
// The last clause was the mistake. There are two fallbacks, and neither needs a
// key:
//
//   - The depth can select a MODEL. Providers express thinking as a separate
//     model far more often than as a field -- deepseek-chat against
//     deepseek-reasoner, gpt-5 against the o-series -- so a registry holding one
//     of each already has everything needed (Loop.preferDeep).
//   - Failing that, the depth is said in the PROMPT (Loop.deliberate). That
//     works on every model that exists, configured or not.
//
// What the old gate actually produced was an agent running deepseek-chat with no
// way to ask for more thought under any circumstances, and a boot line saying so
// to an operator who could do nothing about it from inside the container. The
// capability is the agent's; how far it travels is the deployment's.
//
// Which of the three happens is decided per completion, not here, because the
// depth is raised mid-turn and the answer depends on the model the turn is on
// at that moment.
func New(reg config.Registry) *Tool {
	return &Tool{}
}

func (t *Tool) Name() string { return "set_reasoning_depth" }

func (t *Tool) Schema() domain.ToolSchema {
	return domain.ToolSchema{
		Name: "set_reasoning_depth",
		Description: "Choose how hard to think for the rest of this turn. Raise it for a problem " +
			"that needs real reasoning -- an ambiguous requirement, a subtle bug, conflicting " +
			"sources, a plan with dependencies. Leave it alone for a question you can already " +
			"answer. The choice lasts until this turn ends and costs nothing but this call.",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "level": {
      "type": "string",
      "enum": ["off", "low", "medium", "high", "xhigh", "adaptive"],
      "description": "off spends the least, xhigh the most; adaptive lets the model provider decide"
    },
    "reason": {
      "type": "string",
      "description": "One line on what about this problem needs that depth"
    }
  },
  "required": ["level"]
}`),
	}
}

type args struct {
	Level  string `json:"level"`
	Reason string `json:"reason"`
}

// Invoke records the level on the turn's depth cell.
//
// It calls no provider and spends no tokens, so the whole cost of agent-chosen
// depth is the one iteration the call itself occupies.
func (t *Tool) Invoke(ctx context.Context, raw json.RawMessage) (domain.Result, error) {
	var a args
	if err := json.Unmarshal(raw, &a); err != nil {
		return domain.Result{Content: "set_reasoning_depth: could not read the arguments: " + err.Error()}, nil
	}
	level, ok := config.ParseThinkingLevel(a.Level)
	if !ok {
		// The current level is left untouched. A refusal that also reset the
		// depth would punish a typo with a slower or shallower answer.
		return domain.Result{Content: fmt.Sprintf(
			"set_reasoning_depth: %q is not a level. Use one of: %s.",
			a.Level, strings.Join(config.ThinkingLevels, ", "))}, nil
	}

	d := domain.DepthFrom(ctx)
	if d == nil {
		// Nothing is carrying a turn, so there is nothing to set. Reported
		// rather than silently accepted: an agent told "done" whose next
		// completion is unchanged would have no way to find out.
		return domain.Result{Content: "set_reasoning_depth: no turn is in progress, so the depth was not changed."}, nil
	}
	d.Set(level, strings.TrimSpace(a.Reason))
	return domain.Result{Content: fmt.Sprintf(
		"Reasoning depth is now %q for the rest of this turn.", level)}, nil
}
