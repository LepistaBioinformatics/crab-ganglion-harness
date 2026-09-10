package runtime

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

// The child runner: one sub-agent turn, run by the same loop that runs a
// member's turn.
//
// Same process, same loop, fresh context -- which is what picoclaw does too
// (spawnSubTurn shallow-copies the agent instance and calls runTurn). What is
// NOT copied from picoclaw is everything around it; see the package comment on
// the dispatcher tool and the spec's D-2..D-6.

// SubAgentName is the conversation id a child's in-memory stores use. It never
// reaches disk (see memoryTranscript), so it only has to be stable.
const SubAgentName = domain.ConversationID("subagent")

// Child implements domain.SubAgent over a parent Loop.
type Child struct {
	// Parent is the loop a child is cloned from. Everything about how a turn
	// runs -- the provider, the prompt, the tools, the approver -- comes from
	// here, so a child cannot drift from its parent by construction.
	Parent *Loop
	// MaxIterations bounds one child. Smaller than the parent's on purpose:
	// NFR-1's arithmetic is the reason the whole feature has a stated worst
	// case, and this is the term that multiplies.
	MaxIterations int
	// Timeout bounds one child in wall-clock.
	Timeout time.Duration
	// MaxDepth is the depth at which a child may no longer dispatch. A child AT
	// that depth is given a tool set with the dispatcher removed, rather than a
	// dispatcher that refuses -- a tool the model can see and cannot use costs
	// a turn to discover.
	MaxDepth int
	// HideAtDepth names the tools withheld once MaxDepth is reached.
	HideAtDepth []string
}

// Run executes one child turn.
//
// It never returns an error, because a child failing is DATA: the dispatcher
// reports it in that child's slot and the batch carries on. The only thing that
// ends a batch early is sequential mode's own rule, which is the dispatcher's.
func (c *Child) Run(ctx context.Context, task domain.SubTask) domain.SubReport {
	rep := domain.SubReport{Label: task.Label}
	if c.Parent == nil {
		rep.Err = fmt.Errorf("no parent loop is configured")
		return rep
	}

	fan := domain.FanoutFrom(ctx).Descend()

	// The child's own loop. A shallow copy, then the four fields that make it a
	// child rather than a second member turn.
	child := *c.Parent
	child.MaxIterations = c.MaxIterations
	// In memory, and never on disk. A child writing <id>.jsonl into
	// workspace/sessions/ would make the proxy's history endpoint report
	// conversations no member ever had -- and the child's finding is already
	// durable, inside the parent's tool result, which is in the parent's
	// transcript.
	mem := &memoryStore{}
	child.Transcript = mem
	child.Context = mem
	// A checkpoint is a sidecar for an answer a member is watching arrive.
	// Nobody is watching this one.
	child.Checkpoints = nil
	// Evolution observes MEMBER turns. Counting a fan-out's six children as six
	// turns would make every pattern that used the dispatcher look six times as
	// common as it is.
	child.Learner = nil
	if fan.Depth() >= c.MaxDepth {
		child.Tools = withoutTools(child.Tools, c.HideAtDepth)
	}

	// The context a child runs under. Derived from the parent's, so cancelling
	// the turn cancels the children -- picoclaw deliberately does the opposite
	// (context.Background()), which is how it ends up with results that arrive
	// after the turn that asked for them.
	cctx := domain.WithFanout(ctx, fan)
	// Its own depth cell: a child may decide ITS problem is hard without
	// changing what the parent decided about the parent's.
	cctx = domain.WithDepth(cctx, &domain.Depth{})
	if c.Timeout > 0 {
		var cancel context.CancelFunc
		cctx, cancel = context.WithTimeout(cctx, c.Timeout)
		defer cancel()
	}

	answer, err := child.Run(cctx, domain.Turn{
		SessionID:  SubAgentName,
		SessionKey: "",
		Input:      domain.Message{Role: domain.RoleUser, Content: prompt(task)},
	}, domain.Sink{}) // SILENT. See the dispatcher: one goroutine narrates.

	rep.Answer, rep.Err = answer, err
	rep.Iterations = mem.turns()
	return rep
}

// prompt is what the child actually reads.
//
// The task first and the inherited findings after it, because the task is the
// instruction and the findings are material -- a child that read four
// paragraphs of context before learning what to do with them tends to summarise
// them instead.
func prompt(t domain.SubTask) string {
	if t.Context == "" {
		return t.Task
	}
	return t.Task + "\n\n--- What the earlier steps found ---\n" + t.Context
}

// withoutTools hides names from a tool set without the underlying executor
// knowing.
func withoutTools(inner domain.ToolExecutor, hide []string) domain.ToolExecutor {
	if inner == nil || len(hide) == 0 {
		return inner
	}
	h := make(map[string]bool, len(hide))
	for _, n := range hide {
		h[n] = true
	}
	return filtered{inner: inner, hide: h}
}

type filtered struct {
	inner domain.ToolExecutor
	hide  map[string]bool
}

func (f filtered) Available(ctx context.Context) []domain.ToolSchema {
	var out []domain.ToolSchema
	for _, s := range f.inner.Available(ctx) {
		if !f.hide[s.Name] {
			out = append(out, s)
		}
	}
	return out
}

// Invoke refuses a hidden name rather than passing it through. The schema list
// is what the model was told, but a model can name a tool it was not offered.
func (f filtered) Invoke(ctx context.Context, c domain.ToolCall) (domain.Result, error) {
	if f.hide[c.Name] {
		return domain.Result{Content: fmt.Sprintf(
			"%s is not available at this depth.", c.Name)}, nil
	}
	return f.inner.Invoke(ctx, c)
}

// memoryStore is a child's transcript and window, both in RAM and both gone
// when the child is.
type memoryStore struct {
	mu  sync.Mutex
	log []domain.Message
	w   domain.Window
}

func (m *memoryStore) Append(_ context.Context, _ domain.ConversationID, msg domain.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.log = append(m.log, msg)
	return nil
}

func (m *memoryStore) Read(context.Context, domain.ConversationID) ([]domain.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]domain.Message(nil), m.log...), nil
}

func (m *memoryStore) Load(context.Context, domain.ConversationID) (domain.Window, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.w, nil
}

func (m *memoryStore) Save(_ context.Context, _ domain.ConversationID, w domain.Window) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.w = w
	return nil
}

// turns counts the assistant messages the window accumulated, which is what
// "how many steps did this child take" means to a reader of the report.
func (m *memoryStore) turns() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, msg := range m.w.Messages {
		if msg.Role == domain.RoleAssistant {
			n++
		}
	}
	return n
}
