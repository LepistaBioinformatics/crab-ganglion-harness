// Package subagents is the `subagents` dispatcher tool.
//
// picoclaw has four tools here -- subagent, spawn, spawn_status and delegate --
// and three of them are wired wrong in v0.3.1. That is not a swipe; it is the
// reason this package has one tool with a different shape, and each divergence
// below names the defect it avoids.
//
//   - spawn_status polls SubagentManager.tasks, which is written only by
//     SubagentManager.Spawn, which has NO non-test caller. In the shipped
//     wiring it always answers "No subagents have been spawned yet."
//   - spawn's result never reaches the turn that asked for it: asyncCallback
//     republishes it as a NEW INBOUND MESSAGE on channel "system", because the
//     pendingResults channel that would inject it is nil at depth 0.
//   - max_concurrent is enforced on parentTS.concurrencySem, also nil at depth
//     0, so FIRST-LEVEL SUB-AGENTS ARE UNCAPPED. The cap bites only from depth
//     1 down, which is the one place it hardly matters.
//
// So: one tool, taking a BATCH, entirely SYNCHRONOUS, with the bounds enforced
// at depth 0. That is what "previsível" asks for -- the model reads the result
// in the same turn it asked, and a batch's outcome is a function of the batch
// rather than of when the model happens to poll.
//
// A batch is one tool call and one tool result, which is also what keeps the
// turn loop unchanged: it dispatches tool calls strictly sequentially and
// compact/dropOrphanTools enforce that every call is followed by its result.
// Fanning out INSIDE one call leaves both properties intact. Fanning out across
// calls would have required rewriting the loop and breaking the pairing.
package subagents

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

// Name is the tool the model sees, and the name the child runner hides once a
// child reaches max_depth.
const Name = "subagents"

// Defaults mirror picoclaw's `agents.defaults.subturn` keys where one exists.
const (
	// DefaultMaxTasks bounds ONE call. The whole-turn budget is separate and
	// smaller; this only stops a single call from being absurd.
	DefaultMaxTasks = 8
	// DefaultAnswerRunes bounds one child's answer in the result text. Runes,
	// not bytes: picoclaw truncates ForUser with a byte slice on a UTF-8 string
	// (subagent.go:436-439) and can split a rune.
	DefaultAnswerRunes = 4000
)

// Tool implements `subagents`.
type Tool struct {
	// Runner is the port. The implementation over a Loop is built in the
	// composition root; this package imports no adapter and no runtime.
	Runner domain.SubAgent
	// MaxConcurrent bounds children running at once. Exhaustion QUEUES: a batch
	// of eight with a cap of five is an ordinary request, not an error --
	// picoclaw returns ErrConcurrencyTimeout after 30s for the same situation.
	MaxConcurrent int
	MaxTasks      int
	AnswerRunes   int
}

// New builds the dispatcher, or returns nil when there is nothing to dispatch
// with. Nil means absent from the tool list, the rule every optional tool here
// follows.
func New(runner domain.SubAgent, maxConcurrent, maxTasks, answerRunes int) *Tool {
	if runner == nil {
		return nil
	}
	if maxConcurrent <= 0 {
		maxConcurrent = 5
	}
	if maxTasks <= 0 {
		maxTasks = DefaultMaxTasks
	}
	if answerRunes <= 0 {
		answerRunes = DefaultAnswerRunes
	}
	return &Tool{Runner: runner, MaxConcurrent: maxConcurrent, MaxTasks: maxTasks, AnswerRunes: answerRunes}
}

func (t *Tool) Name() string { return Name }

func (t *Tool) Schema() domain.ToolSchema {
	return domain.ToolSchema{
		Name: Name,
		Description: "Run several sub-agents and wait for all of them. Use it to split work that " +
			"is genuinely separable -- reading four documents, checking three hypotheses, " +
			"drafting two alternatives -- or to run a plan whose steps depend on each other. " +
			"Each sub-agent starts fresh and sees NONE of this conversation.",
		Parameters: json.RawMessage(fmt.Sprintf(`{
  "type": "object",
  "properties": {
    "mode": {
      "type": "string",
      "enum": ["parallel", "sequential"],
      "description": "parallel: every task runs at once, independently. sequential: each task runs after the previous one and is given its findings."
    },
    "tasks": {
      "type": "array",
      "minItems": 1,
      "maxItems": %d,
      "items": {
        "type": "object",
        "properties": {
          "task":  {"type": "string", "description": "The complete instruction. The sub-agent sees NONE of this conversation, so say everything it needs to know."},
          "label": {"type": "string", "description": "Short name for this task, shown to the member and used to label its result"}
        },
        "required": ["task"]
      }
    }
  },
  "required": ["mode", "tasks"]
}`, t.MaxTasks)),
	}
}

type request struct {
	Mode  string `json:"mode"`
	Tasks []struct {
		Task  string `json:"task"`
		Label string `json:"label"`
	} `json:"tasks"`
}

const (
	modeParallel   = "parallel"
	modeSequential = "sequential"
)

func (t *Tool) Invoke(ctx context.Context, raw json.RawMessage) (domain.Result, error) {
	var req request
	if err := json.Unmarshal(raw, &req); err != nil {
		return domain.Result{Content: Name + ": could not read the arguments: " + err.Error()}, nil
	}
	mode := strings.ToLower(strings.TrimSpace(req.Mode))
	if mode != modeParallel && mode != modeSequential {
		return domain.Result{Content: fmt.Sprintf(
			"%s: `mode` must be %q or %q.", Name, modeParallel, modeSequential)}, nil
	}

	var tasks []domain.SubTask
	for i, in := range req.Tasks {
		task := strings.TrimSpace(in.Task)
		if task == "" {
			continue
		}
		label := strings.TrimSpace(in.Label)
		if label == "" {
			label = fmt.Sprintf("task %d", i+1)
		}
		tasks = append(tasks, domain.SubTask{Label: label, Task: task})
	}
	if len(tasks) == 0 {
		return domain.Result{Content: Name + ": `tasks` must contain at least one task."}, nil
	}
	if len(tasks) > t.MaxTasks {
		tasks = tasks[:t.MaxTasks]
	}

	return domain.Result{Content: t.run(ctx, mode, tasks)}, nil
}

// run claims budget, dispatches, and renders.
func (t *Tool) run(ctx context.Context, mode string, tasks []domain.SubTask) string {
	reports, ran, skipped := t.dispatch(ctx, mode, tasks)
	if len(ran) == 0 {
		return fmt.Sprintf("%s: this turn's budget of sub-agents is spent, so none of the %d "+
			"tasks were run. Answer with what you already have, or ask the member.", Name, len(skipped))
	}
	return render(mode, reports, ran, skipped, t.AnswerRunes)
}

// dispatch is the whole mechanism, without the rendering: claim what the turn's
// budget allows, run it, and say what did not fit.
//
// Separate from run so `research` -- which is a caller of the same dispatcher
// rather than a second one -- reuses the budget, the concurrency cap and the
// narration without going through this tool's JSON schema.
func (t *Tool) dispatch(
	ctx context.Context, mode string, tasks []domain.SubTask,
) (reports []domain.SubReport, ran, skipped []domain.SubTask) {
	granted := domain.FanoutFrom(ctx).Take(len(tasks))
	ran, skipped = tasks[:granted], tasks[granted:]
	if len(ran) == 0 {
		return nil, nil, skipped
	}

	say := narrator(ctx)
	say(fmt.Sprintf("running %d sub-agent(s), %s", len(ran), mode))
	if mode == modeParallel {
		return t.parallel(ctx, ran, say), ran, skipped
	}
	return t.sequential(ctx, ran, say), ran, skipped
}

// parallel runs every task at once, bounded by MaxConcurrent.
//
// Results are written by INDEX and read after the wait, so the order of the
// report is the order of the request and never the order of completion -- the
// same batch renders identically twice, which is the difference between a tool
// a model can reason about and one it cannot.
func (t *Tool) parallel(ctx context.Context, tasks []domain.SubTask, say func(string)) []domain.SubReport {
	out := make([]domain.SubReport, len(tasks))
	sem := make(chan struct{}, t.MaxConcurrent)
	// Buffered to len(tasks), so no worker ever blocks announcing itself even
	// if the narrator is slow.
	done := make(chan domain.SubReport, len(tasks))

	var wg sync.WaitGroup
	for i, task := range tasks {
		wg.Add(1)
		go func(i int, task domain.SubTask) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			r := t.Runner.Run(ctx, task)
			r.Label = task.Label
			out[i] = r
			done <- r
		}(i, task)
	}

	// THE NARRATION HAPPENS HERE, in this goroutine and no other. domain.Sink
	// is a struct of plain funcs with no mutex and CI runs -race; letting the
	// workers emit would be a data race that the gate would catch and that a
	// member would experience as interleaved nonsense anyway.
	for range tasks {
		r := <-done
		say(outcomeLine(r))
	}
	wg.Wait()
	return out
}

// sequential runs tasks in order, handing each the previous answers.
//
// A FAILED STEP STOPS THE SEQUENCE. That is what distinguishes this mode from a
// slow parallel one: the later steps were written on the assumption that the
// earlier ones happened, and running them anyway is how a chain produces a
// confident wrong answer.
func (t *Tool) sequential(ctx context.Context, tasks []domain.SubTask, say func(string)) []domain.SubReport {
	var out []domain.SubReport
	var carried strings.Builder
	for _, task := range tasks {
		task.Context = carried.String()
		r := t.Runner.Run(ctx, task)
		r.Label = task.Label
		out = append(out, r)
		say(outcomeLine(r))
		if r.Err != nil {
			break
		}
		if carried.Len() > 0 {
			carried.WriteString("\n\n")
		}
		fmt.Fprintf(&carried, "[%s]\n%s", task.Label, r.Answer)
	}
	return out
}

func outcomeLine(r domain.SubReport) string {
	if r.Err != nil {
		return fmt.Sprintf("%s failed: %v", r.Label, r.Err)
	}
	return fmt.Sprintf("%s finished", r.Label)
}

// narrator returns a function that says one line to the member, or does nothing
// when there is no turn under this call.
func narrator(ctx context.Context) func(string) {
	sink := domain.SinkFrom(ctx)
	return func(text string) {
		sink.EmitProgress(domain.Progress{Kind: domain.ProgressThought, Text: Name + ": " + text})
	}
}

// render writes the one result the model reads.
func render(mode string, reports []domain.SubReport, tasks, skipped []domain.SubTask, runes int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Ran %d sub-agent(s) (%s).\n", len(reports), mode)

	for i, r := range reports {
		fmt.Fprintf(&b, "\n[%d] %s — ", i+1, r.Label)
		if r.Err != nil {
			fmt.Fprintf(&b, "FAILED: %v\n", r.Err)
			continue
		}
		fmt.Fprintf(&b, "ok (%d step(s))\n%s\n", r.Iterations, truncate(r.Answer, runes))
	}

	// Everything that did NOT run, and why. A batch that silently ran four of
	// six leaves the model believing it has six answers.
	if n := len(tasks) - len(reports); n > 0 {
		fmt.Fprintf(&b, "\n%d task(s) were not run: the sequence stopped at the failure above.\n", n)
	}
	for _, s := range skipped {
		fmt.Fprintf(&b, "\nNOT RUN (this turn's sub-agent budget is spent): %s\n", s.Label)
	}
	return b.String()
}

// truncate cuts at a RUNE boundary. picoclaw's equivalent uses a byte slice on
// a UTF-8 string and can leave the model reading a broken character.
func truncate(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + fmt.Sprintf("\n… (truncated at %d characters)", max)
}
