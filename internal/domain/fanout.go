package domain

// Sub-agent fan-out, the part of it the domain owns: how deep this turn already
// is, and how many child turns it has left to spend.
//
// Both are per TURN and shared across the whole tree beneath it, which is why
// they are a cell in the context rather than a field anywhere. A budget held per
// call would let a model that calls the dispatcher on each of its twelve
// iterations start twelve full batches; a depth held per call could not tell a
// child from its parent at all.

import (
	"context"
	"sync"
)

// Fanout is one node's view of a turn's fan-out: its own depth, and a budget it
// SHARES with every other node in the same turn.
type Fanout struct {
	depth int
	b     *budget
}

type budget struct {
	mu        sync.Mutex
	remaining int
}

// NewFanout starts a turn at depth 0 with a whole-turn child budget.
func NewFanout(children int) *Fanout {
	if children < 0 {
		children = 0
	}
	return &Fanout{b: &budget{remaining: children}}
}

// Depth is how many dispatchers deep this node is. 0 is the turn the member
// started.
func (f *Fanout) Depth() int {
	if f == nil {
		return 0
	}
	return f.depth
}

// Descend returns the view a child runs under: one deeper, the SAME budget.
func (f *Fanout) Descend() *Fanout {
	if f == nil {
		return nil
	}
	return &Fanout{depth: f.depth + 1, b: f.b}
}

// Take claims up to n children and returns how many it got, which may be zero.
//
// Partial rather than all-or-nothing: a batch of five with three left runs
// three and says so, which is more useful than refusing and more honest than
// running five.
func (f *Fanout) Take(n int) int {
	if f == nil || f.b == nil || n <= 0 {
		return 0
	}
	f.b.mu.Lock()
	defer f.b.mu.Unlock()
	if n > f.b.remaining {
		n = f.b.remaining
	}
	f.b.remaining -= n
	return n
}

// Remaining is what is left of the turn's budget.
func (f *Fanout) Remaining() int {
	if f == nil || f.b == nil {
		return 0
	}
	f.b.mu.Lock()
	defer f.b.mu.Unlock()
	return f.b.remaining
}

type fanoutKey struct{}

func WithFanout(ctx context.Context, f *Fanout) context.Context {
	return context.WithValue(ctx, fanoutKey{}, f)
}

func FanoutFrom(ctx context.Context) *Fanout {
	f, _ := ctx.Value(fanoutKey{}).(*Fanout)
	return f
}

// The turn's sink, reachable from inside a tool.
//
// A tool's Invoke takes no sink, deliberately: a tool returns a Result and the
// loop does the talking. The dispatcher is the one exception, because it is the
// only tool that can run for minutes, and a member watching a five-minute
// silence has no way to tell a fan-out from a hang.
//
// It is a value in the context rather than a widening of the Tool port so that
// every other tool stays unable to write to the stream -- and the dispatcher
// serialises its own emissions through one goroutine, because Sink is a struct
// of plain funcs with no mutex and CI runs -race.
type sinkKey struct{}

func WithSink(ctx context.Context, s Sink) context.Context {
	return context.WithValue(ctx, sinkKey{}, s)
}

// SinkFrom returns the turn's sink, or a zero Sink whose emit helpers are
// already nil-safe -- so a caller never has to check.
func SinkFrom(ctx context.Context) Sink {
	s, _ := ctx.Value(sinkKey{}).(Sink)
	return s
}
