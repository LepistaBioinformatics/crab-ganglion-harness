package subagents

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

// The research tool.
//
// picoclaw has nothing like this: a search of v0.3.1 for deep_research,
// deepsearch, ultrathink or a plan/search/read/synthesize loop returns nothing,
// and pkg/tools/toolloop.go is a flat `for iteration < MaxIterations`.
//
// It is HERE, in the dispatcher's package, and not behind a wire parameter,
// because that is what it actually is: research is a procedure -- decompose,
// search in parallel, bring the findings back -- and reasoning depth is one
// field on one request. Putting a research loop behind `reasoning_effort` would
// have been the wrong shape, and building it as a second dispatcher would have
// given the turn two independent child budgets.

const ResearchName = "research"

const (
	defaultBreadth = 4
	minBreadth     = 2
	maxBreadth     = 6
)

// Research implements `research` over the same dispatcher.
type Research struct {
	d *Tool
}

// NewResearch builds the tool, or returns nil when the dispatcher is absent or
// no web provider is configured.
//
// Both conditions matter and neither is a formality: without search this is a
// model asked to recall, which is the failure mode it exists to replace.
func NewResearch(d *Tool, webConfigured bool) *Research {
	if d == nil || !webConfigured {
		return nil
	}
	return &Research{d: d}
}

func (r *Research) Name() string { return ResearchName }

func (r *Research) Schema() domain.ToolSchema {
	return domain.ToolSchema{
		Name: ResearchName,
		Description: "Research a question against the web and return findings with their sources. " +
			"Use `deep` when the question has parts that can be investigated separately, or when " +
			"one search will not settle it. You write the answer -- this returns material, not prose.",
		Parameters: json.RawMessage(fmt.Sprintf(`{
  "type": "object",
  "properties": {
    "question": {"type": "string", "description": "What you need to find out, in one sentence"},
    "mode": {
      "type": "string",
      "enum": ["quick", "deep"],
      "description": "quick: one sub-agent searches and reads. deep: the question is broken into sub-questions and each is researched in parallel. Default quick."
    },
    "breadth": {"type": "integer", "minimum": %d, "maximum": %d, "description": "Sub-questions in deep mode. Default %d."}
  },
  "required": ["question"]
}`, minBreadth, maxBreadth, defaultBreadth)),
	}
}

type researchArgs struct {
	Question string `json:"question"`
	Mode     string `json:"mode"`
	Breadth  int    `json:"breadth"`
}

func (r *Research) Invoke(ctx context.Context, raw json.RawMessage) (domain.Result, error) {
	var a researchArgs
	if err := json.Unmarshal(raw, &a); err != nil {
		return domain.Result{Content: ResearchName + ": could not read the arguments: " + err.Error()}, nil
	}
	q := strings.TrimSpace(a.Question)
	if q == "" {
		return domain.Result{Content: ResearchName + ": `question` is required."}, nil
	}
	if strings.ToLower(strings.TrimSpace(a.Mode)) != "deep" {
		return domain.Result{Content: r.quick(ctx, q)}, nil
	}
	breadth := a.Breadth
	if breadth < minBreadth {
		breadth = defaultBreadth
	}
	if breadth > maxBreadth {
		breadth = maxBreadth
	}
	return domain.Result{Content: r.deep(ctx, q, breadth)}, nil
}

func (r *Research) quick(ctx context.Context, q string) string {
	reports, ran, skipped := r.d.dispatch(ctx, modeParallel, []domain.SubTask{{
		Label: "research", Task: searchPrompt(q),
	}})
	if len(ran) == 0 {
		return budgetSpent(len(skipped))
	}
	return findings(q, reports, r.d.AnswerRunes)
}

// deep is three phases, and the third one is NOT here.
//
// One child decomposes, a parallel batch researches, and the FINDINGS GO BACK
// TO THE PARENT. A synthesis written by a child would be written by something
// that has never seen the conversation, and it would answer a question nobody
// asked -- politely, at length, and in the wrong register.
func (r *Research) deep(ctx context.Context, q string, breadth int) string {
	plan, ran, skipped := r.d.dispatch(ctx, modeParallel, []domain.SubTask{{
		Label: "plan", Task: decomposePrompt(q, breadth),
	}})
	if len(ran) == 0 {
		return budgetSpent(len(skipped))
	}

	subs := subQuestions(plan[0], breadth)
	if len(subs) == 0 {
		// The decomposition produced nothing readable. Researching the original
		// question is a worse answer than a good plan and a much better one
		// than an error, and it costs the same as quick mode.
		return r.quick(ctx, q)
	}

	var tasks []domain.SubTask
	for i, sq := range subs {
		tasks = append(tasks, domain.SubTask{
			Label: fmt.Sprintf("q%d", i+1), Task: searchPrompt(sq),
		})
	}
	reports, ran, skipped := r.d.dispatch(ctx, modeParallel, tasks)
	if len(ran) == 0 {
		return budgetSpent(len(skipped))
	}

	out := findings(q, reports, r.d.AnswerRunes)
	if len(skipped) > 0 {
		out += fmt.Sprintf("\n%d sub-question(s) were not researched: this turn's sub-agent "+
			"budget ran out.\n", len(skipped))
	}
	return out
}

func budgetSpent(n int) string {
	return fmt.Sprintf("%s: this turn's budget of sub-agents is spent, so %d task(s) did not "+
		"run. Use web_search directly, or answer with what you have.", ResearchName, n)
}

func searchPrompt(q string) string {
	return "Research this question and report what you found:\n\n" + q +
		"\n\nUse web_search to find sources and web_fetch to read the most promising ones. " +
		"Report the findings themselves, not a description of your process. " +
		"END WITH THE URLS you actually read, one per line. A finding without a source is " +
		"worth less than no finding, because nobody can check it."
}

func decomposePrompt(q string, breadth int) string {
	return fmt.Sprintf(
		"Break this question into exactly %d sub-questions that can each be researched "+
			"independently:\n\n%s\n\nEach must be answerable on its own, without the answer to "+
			"another. Reply with ONLY the sub-questions, one per line, numbered 1. to %d. "+
			"No preamble and no commentary.", breadth, q, breadth)
}

// subQuestions reads the plan child's reply.
//
// Line-based and forgiving, because the input is a language model's prose and
// the cost of being strict is a research call that fails on formatting. A reply
// nothing can be read out of yields none, and deep() falls back to quick.
func subQuestions(plan domain.SubReport, breadth int) []string {
	if plan.Err != nil {
		return nil
	}
	var out []string
	for _, raw := range strings.Split(plan.Answer, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		line = strings.TrimLeft(line, "-*• \t")
		// Strip a leading "1." / "2)" ordinal without eating a question that
		// legitimately starts with a number.
		if i := strings.IndexFunc(line, func(r rune) bool { return !unicode.IsDigit(r) }); i > 0 && i <= 2 {
			if c := line[i]; c == '.' || c == ')' {
				line = strings.TrimSpace(line[i+1:])
			}
		}
		// Below this length it is punctuation or a heading, not a question.
		if len([]rune(line)) < 8 {
			continue
		}
		out = append(out, line)
		if len(out) == breadth {
			break
		}
	}
	return out
}

// findings renders what came back. It is deliberately NOT an answer: the parent
// has the conversation and writes that.
func findings(q string, reports []domain.SubReport, runes int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Findings for: %s\n", q)
	fmt.Fprintf(&b, "These are MATERIAL, not an answer — write the answer yourself, and cite the "+
		"URLs below for anything you assert from them.\n")
	for i, r := range reports {
		fmt.Fprintf(&b, "\n[%d] %s — ", i+1, r.Label)
		if r.Err != nil {
			fmt.Fprintf(&b, "FAILED: %v\n", r.Err)
			continue
		}
		fmt.Fprintf(&b, "ok\n%s\n", truncate(r.Answer, runes))
	}
	return b.String()
}
