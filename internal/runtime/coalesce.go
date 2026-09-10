package runtime

import "time"

// coalescer batches text and hands it on at most once per interval.
//
// It exists because a delta is not a unit of anything a person perceives. A
// provider emits sub-word tokens; a client renders whole messages. Forwarding
// one to the other at the producer's rate makes the consumer do a full render
// per syllable.
//
// Time-based rather than count-based on purpose: a slow stream must still
// deliver promptly, and a fast one must not be able to outrun the budget by
// emitting shorter tokens.
type coalescer struct {
	every time.Duration
	now   func() time.Time
	emit  func(string)

	buf  []byte
	last time.Time
}

func newCoalescer(every time.Duration, now func() time.Time, emit func(string)) *coalescer {
	return &coalescer{every: every, now: now, emit: emit, last: now()}
}

// Add buffers text, emitting the batch when the interval has elapsed.
func (c *coalescer) Add(text string) {
	if text == "" {
		return
	}
	c.buf = append(c.buf, text...)
	if c.now().Sub(c.last) >= c.every {
		c.Flush()
	}
}

// Flush emits whatever is buffered. Safe to call with nothing pending, which
// is what makes it safe on every exit path.
func (c *coalescer) Flush() {
	if len(c.buf) == 0 {
		return
	}
	c.emit(string(c.buf))
	c.buf = c.buf[:0]
	c.last = c.now()
}
