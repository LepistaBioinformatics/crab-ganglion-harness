package subagents

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

// fakeRunner answers each task by label, and records what it was asked.
type fakeRunner struct {
	mu      sync.Mutex
	seen    []domain.SubTask
	answer  func(domain.SubTask) domain.SubReport
	live    atomic.Int32
	maxLive atomic.Int32
	delay   time.Duration
}

func (f *fakeRunner) Run(ctx context.Context, task domain.SubTask) domain.SubReport {
	n := f.live.Add(1)
	for {
		m := f.maxLive.Load()
		if n <= m || f.maxLive.CompareAndSwap(m, n) {
			break
		}
	}
	defer f.live.Add(-1)

	f.mu.Lock()
	f.seen = append(f.seen, task)
	f.mu.Unlock()

	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return domain.SubReport{Label: task.Label, Err: ctx.Err()}
		}
	}
	if f.answer != nil {
		return f.answer(task)
	}
	return domain.SubReport{Label: task.Label, Answer: "answer for " + task.Label, Iterations: 1}
}

func (f *fakeRunner) tasks() []domain.SubTask {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.SubTask(nil), f.seen...)
}

func ctxWith(children int) context.Context {
	return domain.WithFanout(context.Background(), domain.NewFanout(children))
}

func call(t *testing.T, tool *Tool, ctx context.Context, mode string, labels ...string) string {
	t.Helper()
	var tasks []map[string]string
	for _, l := range labels {
		tasks = append(tasks, map[string]string{"task": "do " + l, "label": l})
	}
	raw, _ := json.Marshal(map[string]any{"mode": mode, "tasks": tasks})
	res, err := tool.Invoke(ctx, raw)
	if err != nil {
		t.Fatalf("Invoke returned an error, which it must never do: %v", err)
	}
	return res.Content
}

// AC-1. The report is in TASK order, never completion order, and the same batch
// renders identically twice. A tool whose output depends on which child
// happened to finish first is one a model cannot reason about.
func TestParallelResultsComeBackInTaskOrderNotCompletionOrder(t *testing.T) {
	// The first task is the slowest, so completion order is the reverse of
	// request order.
	r := &fakeRunner{answer: func(task domain.SubTask) domain.SubReport {
		if task.Label == "a" {
			time.Sleep(30 * time.Millisecond)
		}
		return domain.SubReport{Answer: "answer for " + task.Label, Iterations: 1}
	}}
	tool := New(r, 5, 0, 0)

	out := call(t, tool, ctxWith(16), modeParallel, "a", "b", "c")
	ia, ib, ic := strings.Index(out, "] a "), strings.Index(out, "] b "), strings.Index(out, "] c ")
	if !(ia < ib && ib < ic) {
		t.Fatalf("order is %d/%d/%d, want a<b<c:\n%s", ia, ib, ic, out)
	}
	if again := call(t, tool, ctxWith(16), modeParallel, "a", "b", "c"); again != out {
		t.Errorf("the same batch rendered differently twice:\n%s\n---\n%s", out, again)
	}
}

// AC-2. One child failing is data about that child, not a failure of the batch.
func TestAFailedChildDoesNotFailAParallelBatch(t *testing.T) {
	r := &fakeRunner{answer: func(task domain.SubTask) domain.SubReport {
		if task.Label == "b" {
			return domain.SubReport{Err: errors.New("the endpoint refused")}
		}
		return domain.SubReport{Answer: "answer for " + task.Label}
	}}
	out := call(t, New(r, 5, 0, 0), ctxWith(16), modeParallel, "a", "b", "c")

	for _, want := range []string{"answer for a", "answer for c", "FAILED: the endpoint refused"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q from:\n%s", want, out)
		}
	}
}

// AC-3. Sequential STOPS. The later steps were written assuming the earlier
// ones happened; running them anyway is how a chain produces a confident wrong
// answer.
func TestAFailedStepStopsASequentialBatchAndSaysWhatDidNotRun(t *testing.T) {
	r := &fakeRunner{answer: func(task domain.SubTask) domain.SubReport {
		if task.Label == "second" {
			return domain.SubReport{Err: errors.New("no")}
		}
		return domain.SubReport{Answer: "answer for " + task.Label}
	}}
	out := call(t, New(r, 5, 0, 0), ctxWith(16), modeSequential, "first", "second", "third")

	if len(r.tasks()) != 2 {
		t.Fatalf("the runner saw %d tasks, want 2 — the third must never be started", len(r.tasks()))
	}
	if !strings.Contains(out, "not run") {
		t.Errorf("the result does not say what did not run:\n%s", out)
	}
}

// AC-4. THE DISCRIMINATING TEST between the two modes.
//
// A test that only measured timing would pass for an implementation that ran
// both modes sequentially. What actually distinguishes them is that a
// sequential child READS the previous answers.
func TestOnlySequentialHandsEachChildTheEarlierFindings(t *testing.T) {
	seq := &fakeRunner{}
	call(t, New(seq, 5, 0, 0), ctxWith(16), modeSequential, "first", "second")
	got := seq.tasks()
	if len(got) != 2 {
		t.Fatalf("saw %d tasks", len(got))
	}
	if got[0].Context != "" {
		t.Errorf("the first child was given context: %q", got[0].Context)
	}
	if !strings.Contains(got[1].Context, "answer for first") {
		t.Errorf("the second child did not receive the first's answer: %q", got[1].Context)
	}

	par := &fakeRunner{}
	call(t, New(par, 5, 0, 0), ctxWith(16), modeParallel, "first", "second")
	for _, task := range par.tasks() {
		if task.Context != "" {
			t.Errorf("a parallel child was given context: %q", task.Context)
		}
	}
}

// AC-5. The cap is enforced HERE, at depth 0, where every real fan-out happens
// -- which is the one place picoclaw's is not (its semaphore is nil at depth 0,
// so first-level sub-agents are uncapped). And exhaustion QUEUES: all eight run.
func TestConcurrencyIsCappedAndExhaustionQueuesRatherThanFails(t *testing.T) {
	r := &fakeRunner{delay: 10 * time.Millisecond}
	out := call(t, New(r, 2, 0, 0), ctxWith(16), modeParallel,
		"a", "b", "c", "d", "e", "f", "g", "h")

	if got := r.maxLive.Load(); got > 2 {
		t.Errorf("%d children ran at once, cap is 2", got)
	}
	if len(r.tasks()) != 8 {
		t.Errorf("%d of 8 tasks ran; exhaustion must queue, not fail", len(r.tasks()))
	}
	if strings.Contains(out, "FAILED") {
		t.Errorf("queuing produced a failure:\n%s", out)
	}
}

// AC-7. The budget is for the WHOLE TURN, across every call. A model that calls
// the dispatcher on each of its twelve iterations cannot start ninety-six
// children.
func TestTheChildBudgetIsSpentAcrossTheWholeTurn(t *testing.T) {
	r := &fakeRunner{}
	tool := New(r, 5, 0, 0)
	ctx := ctxWith(3)

	out := call(t, tool, ctx, modeParallel, "a", "b", "c", "d", "e")
	if len(r.tasks()) != 3 {
		t.Fatalf("%d children ran, want 3", len(r.tasks()))
	}
	if !strings.Contains(out, "NOT RUN") || !strings.Contains(out, "budget") {
		t.Errorf("the result does not name the budget:\n%s", out)
	}

	// A second call in the same turn gets nothing, and is told why rather than
	// silently returning an empty batch.
	out = call(t, tool, ctx, modeParallel, "f")
	if len(r.tasks()) != 3 {
		t.Errorf("the second call started %d more children", len(r.tasks())-3)
	}
	if !strings.Contains(out, "budget") {
		t.Errorf("the second call did not explain itself:\n%s", out)
	}
}

// AC-8. Cancelling the parent cancels the children. picoclaw deliberately does
// the opposite -- its child context is context.Background() -- which is how it
// ends up with results that arrive after the turn that asked for them.
func TestCancellingTheTurnReachesTheChildren(t *testing.T) {
	r := &fakeRunner{delay: time.Second}
	ctx, cancel := context.WithCancel(ctxWith(16))
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()

	start := time.Now()
	out := call(t, New(r, 5, 0, 0), ctx, modeParallel, "a", "b")
	if time.Since(start) > 500*time.Millisecond {
		t.Errorf("the batch outlived the cancellation by %v", time.Since(start))
	}
	if out == "" {
		t.Error("cancellation produced no result at all")
	}
}

// AC-11. Truncation on a RUNE boundary. picoclaw's equivalent slices bytes on a
// UTF-8 string and can hand the model half a character.
func TestALongAnswerIsTruncatedOnARuneBoundary(t *testing.T) {
	long := strings.Repeat("ç", 50)
	r := &fakeRunner{answer: func(domain.SubTask) domain.SubReport {
		return domain.SubReport{Answer: long}
	}}
	tool := New(r, 5, 0, 10)

	out := call(t, tool, ctxWith(16), modeParallel, "a")
	if !strings.Contains(out, "truncated") {
		t.Fatalf("nothing was truncated:\n%s", out)
	}
	for _, ru := range out {
		if ru == '\uFFFD' {
			t.Fatal("truncation split a rune")
		}
	}
}

func TestEveryFailureIsAResultAndNeverAnError(t *testing.T) {
	tool := New(&fakeRunner{}, 5, 0, 0)
	for _, in := range []string{
		`{"mode":"parallel","tasks":[{"task":"x"}]}`,
		`{"mode":"nonsense","tasks":[{"task":"x"}]}`,
		`{"mode":"parallel","tasks":[]}`,
		`{"mode":"parallel","tasks":[{"task":"  "}]}`,
		`{}`,
		`not json`,
	} {
		res, err := tool.Invoke(ctxWith(16), json.RawMessage(in))
		if err != nil {
			t.Errorf("Invoke(%s) returned an error: %v", in, err)
		}
		if res.Content == "" {
			t.Errorf("Invoke(%s) said nothing", in)
		}
	}
}

func TestWithNoRunnerThereIsNoTool(t *testing.T) {
	if New(nil, 5, 0, 0) != nil {
		t.Error("a nil runner yielded a tool")
	}
}

// One call cannot exceed max_tasks whatever the model sends, and the schema
// says the same number so the two cannot drift.
func TestOneCallIsBoundedByMaxTasks(t *testing.T) {
	r := &fakeRunner{}
	tool := New(r, 5, 3, 0)
	call(t, tool, ctxWith(16), modeParallel, "a", "b", "c", "d", "e")
	if len(r.tasks()) != 3 {
		t.Errorf("%d children ran, want max_tasks of 3", len(r.tasks()))
	}
	var schema struct {
		Properties struct {
			Tasks struct {
				MaxItems int `json:"maxItems"`
			} `json:"tasks"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(tool.Schema().Parameters, &schema); err != nil {
		t.Fatalf("the schema is not valid JSON: %v", err)
	}
	if schema.Properties.Tasks.MaxItems != 3 {
		t.Errorf("schema maxItems = %d, the tool enforces 3", schema.Properties.Tasks.MaxItems)
	}
}

// D-3, and the reason it exists.
//
// domain.Sink is a struct of plain funcs with NO mutex -- the real one closes
// over a coalescer that has none either -- and CI runs -race. So the recorder
// below is deliberately UNSYNCHRONISED: it is the shape of the real sink, and
// it is what makes this test able to fail. A recorder with a lock of its own
// would make the tool's emissions safe by accident and prove nothing.
//
// Verified by mutation: moving the emission into the workers makes `go test
// -race` report a data race here.
func TestNarrationIsSerialisedThroughOneGoroutine(t *testing.T) {
	var lines []string
	sink := domain.Sink{Progress: func(p domain.Progress) {
		lines = append(lines, p.Text) // NO LOCK. See above.
	}}
	ctx := domain.WithSink(ctxWith(16), sink)
	call(t, New(&fakeRunner{delay: time.Millisecond}, 6, 0, 0), ctx, modeParallel,
		"a", "b", "c", "d", "e", "f")

	// One opening line plus one per child.
	if len(lines) != 7 {
		t.Fatalf("emitted %d lines, want 7:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	for i, l := range lines {
		if !strings.HasPrefix(l, Name+": ") {
			t.Errorf("line %d is unlabelled: %q", i, l)
		}
	}
}

func TestWithNoSinkTheDispatcherStillWorks(t *testing.T) {
	out := call(t, New(&fakeRunner{}, 5, 0, 0), ctxWith(16), modeParallel, "a")
	if !strings.Contains(out, "answer for a") {
		t.Fatalf("no answer came back:\n%s", out)
	}
}
