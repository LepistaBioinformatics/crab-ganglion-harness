package runtime

import (
	"testing"
	"time"
)

func TestCoalescer_BatchesWithinTheInterval(t *testing.T) {
	var out []string
	now := time.Unix(0, 0)
	c := newCoalescer(50*time.Millisecond, func() time.Time { return now }, func(s string) { out = append(out, s) })

	// Sub-word tokens, all inside one interval: one emission, not four.
	for _, tok := range []string{"Be", "le", "za", "!"} {
		c.Add(tok)
	}
	if len(out) != 0 {
		t.Fatalf("emitted %v before the interval elapsed", out)
	}
	now = now.Add(60 * time.Millisecond)
	c.Add("") // empty adds nothing and must not flush
	c.Flush()
	if len(out) != 1 || out[0] != "Beleza!" {
		t.Errorf("out = %v, want one batch %q", out, "Beleza!")
	}
}

func TestCoalescer_EmitsOncePerElapsedInterval(t *testing.T) {
	var out []string
	now := time.Unix(0, 0)
	c := newCoalescer(50*time.Millisecond, func() time.Time { return now }, func(s string) { out = append(out, s) })

	c.Add("um ")
	now = now.Add(60 * time.Millisecond)
	c.Add("dois") // interval elapsed -> flushes both
	now = now.Add(60 * time.Millisecond)
	c.Add("tres")
	c.Flush()

	if len(out) != 2 {
		t.Fatalf("out = %v, want two batches", out)
	}
	if out[0] != "um dois" || out[1] != "tres" {
		t.Errorf("out = %v", out)
	}
}

// Nothing may be dropped: every exit path flushes, and a flush with an empty
// buffer must be a no-op rather than an empty emission the client renders.
func TestCoalescer_FlushIsSafeAndLosesNothing(t *testing.T) {
	var out []string
	now := time.Unix(0, 0)
	c := newCoalescer(time.Hour, func() time.Time { return now }, func(s string) { out = append(out, s) })

	c.Flush() // nothing buffered
	if len(out) != 0 {
		t.Errorf("an empty flush emitted %v", out)
	}
	c.Add("resto")
	c.Flush()
	c.Flush() // idempotent
	if len(out) != 1 || out[0] != "resto" {
		t.Errorf("out = %v, want exactly one %q", out, "resto")
	}
}
