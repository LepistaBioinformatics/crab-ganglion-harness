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

// --- the turn's event log ---------------------------------------------------

// Recorder collects what one turn DID, for the member to read afterwards.
//
// On the context for the same reason the sink is: the two places that make an
// event -- the fallback ladder and the depth change -- are several frames below
// the loop that owns the slice, and threading a pointer through four signatures
// would say nothing the context does not already say.
//
// A mutex although the loop is sequential: the sub-agent dispatcher fans out,
// and although it reports through Result.Events rather than reaching for this,
// a future tool that did reach for it must not be a data race nobody noticed.
type Recorder struct {
	mu     sync.Mutex
	events []TurnEvent
}

func (r *Recorder) Add(e TurnEvent) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
}

// Finish stamps the outcome on the event most recently added -- which is always
// the call this is reporting on, because the loop adds a tool's event
// immediately before invoking it and nothing else runs in between.
//
// A method rather than "add a second event when it ends": one call is one line
// on screen, and splitting it into a start and a finish would double the list
// and make the member reconstruct the pairing.
func (r *Recorder) Finish(status, detail string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.events) == 0 {
		return
	}
	last := &r.events[len(r.events)-1]
	last.Status = status
	if detail != "" {
		last.Detail = detail
	}
}

// Take returns what has been collected and empties the log, so the next
// iteration starts clean. The loop flushes once per iteration.
func (r *Recorder) Take() []TurnEvent {
	if r == nil {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.events
	r.events = nil
	return out
}

type recorderKey struct{}

func WithRecorder(ctx context.Context, r *Recorder) context.Context {
	return context.WithValue(ctx, recorderKey{}, r)
}

// RecorderFrom returns the turn's recorder, or nil -- whose Add and Take are
// already nil-safe, so a caller never has to check.
func RecorderFrom(ctx context.Context) *Recorder {
	r, _ := ctx.Value(recorderKey{}).(*Recorder)
	return r
}
