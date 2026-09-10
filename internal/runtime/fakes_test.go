package runtime

import (
	"context"
	"encoding/json"
	"io"
	"sync"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

// Fakes for all five ports. The loop is fully exercisable with these -- no
// container, no provider key, no network. That property is the return on the
// port split, so it is asserted here by construction.

type fakeProvider struct {
	turns      []fakeTurn // one per Complete call, in order
	calls      int
	err        error
	onComplete func(domain.Completion)
	// gate decides per REQUEST whether Complete fails, which err cannot: the
	// depth degradation is "this request shape fails and that one does not",
	// and a single err would fail both halves of it.
	gate func(domain.Completion) error
}

type fakeTurn struct {
	deltas []string
	msg    domain.Message
	usage  domain.Usage
	err    error
}

func (p *fakeProvider) Complete(_ context.Context, c domain.Completion) (domain.Stream, error) {
	if p.onComplete != nil {
		p.onComplete(c)
	}
	if p.err != nil {
		return nil, p.err
	}
	if p.gate != nil {
		if err := p.gate(c); err != nil {
			return nil, err
		}
	}
	t := p.turns[min(p.calls, len(p.turns)-1)]
	p.calls++
	return &fakeStream{t: t}, nil
}

type fakeStream struct {
	t fakeTurn
	i int
}

func (s *fakeStream) Next(context.Context) (domain.Delta, error) {
	if s.t.err != nil {
		return domain.Delta{}, s.t.err
	}
	if s.i >= len(s.t.deltas) {
		return domain.Delta{}, io.EOF
	}
	d := domain.Delta{Content: s.t.deltas[s.i]}
	s.i++
	return d, nil
}
func (s *fakeStream) Message() domain.Message { return s.t.msg }
func (s *fakeStream) Usage() domain.Usage     { return s.t.usage }
func (s *fakeStream) Close() error            { return nil }

type fakeTranscript struct {
	mu  sync.Mutex
	log []domain.Message
	err error
}

func (f *fakeTranscript) Append(_ context.Context, _ domain.ConversationID, m domain.Message) error {
	if f.err != nil {
		return f.err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log = append(f.log, m)
	return nil
}

func (f *fakeTranscript) Read(context.Context, domain.ConversationID) ([]domain.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.Message(nil), f.log...), nil
}

type fakeContext struct {
	w      domain.Window
	saved  []domain.Window
	loaded int
}

func (f *fakeContext) Load(context.Context, domain.ConversationID) (domain.Window, error) {
	f.loaded++
	return f.w, nil
}

func (f *fakeContext) Save(_ context.Context, _ domain.ConversationID, w domain.Window) error {
	f.saved = append(f.saved, w)
	f.w = w
	return nil
}

type fakeTools struct {
	schemas []domain.ToolSchema
	invoked []domain.ToolCall
	result  domain.Result
	err     error
}

func (f *fakeTools) Available(context.Context) []domain.ToolSchema { return f.schemas }

func (f *fakeTools) Invoke(_ context.Context, c domain.ToolCall) (domain.Result, error) {
	f.invoked = append(f.invoked, c)
	if f.err != nil {
		return domain.Result{}, f.err
	}
	return f.result, nil
}

type fakeApprover struct {
	dec   domain.Decision
	err   error
	block chan struct{} // when non-nil, Request waits on it (or on ctx)
	asked []domain.ActionRequest
}

func (f *fakeApprover) Request(ctx context.Context, a domain.ActionRequest) (domain.Decision, error) {
	f.asked = append(f.asked, a)
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return domain.Decision{}, ctx.Err()
		}
	}
	return f.dec, f.err
}

// collect records everything a sink received, so a test can assert on the
// stream the member would actually have seen.
type collect struct {
	mu       sync.Mutex
	content  []string
	progress []domain.Progress
	errs     []string
}

func (c *collect) sink() domain.Sink {
	return domain.Sink{
		Content: func(s string) {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.content = append(c.content, s)
		},
		Progress: func(p domain.Progress) {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.progress = append(c.progress, p)
		},
		Error: func(s string) {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.errs = append(c.errs, s)
		},
	}
}

func (c *collect) joined() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := ""
	for _, s := range c.content {
		out += s
	}
	return out
}

func (c *collect) kinds(kind string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, p := range c.progress {
		if p.Kind == kind {
			n++
		}
	}
	return n
}

func args(s string) json.RawMessage { return json.RawMessage(s) }
