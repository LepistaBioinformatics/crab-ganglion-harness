# Stop has to reach the process, not just the socket

Pressing Stop in the webapp clears the bands and the agent keeps working. The
member's next message on that conversation then waits behind a turn they already
cancelled.

## What was broken first, and is not fixed here

The stop answered **400 "Request path does not match any service"**. Mycelium's
gateway matches a request against an explicit per-role path list and
`/v1/chat/cancel` was never in it, so no stop had ever reached crab-shell-proxy,
under either harness. That is fixed in `zombie-crab-project` — the deployment
configs, plus a check that fails a PR when a proxy route is not reachable.

Everything below is the SECOND bug, found while the first one hid it: what
happens once the cancellation does arrive.

## What was already right

The whole chain exists and is correct up to this harness:

- `stopTurn` → `POST /api/chat/<r>/cancel` → proxy `POST /v1/chat/cancel` →
  `turnerFor(harness).Cancel()`, all on the same workspace and the same session id.
- `ganglion.Client.Cancel` cancels the context of the proxy's outbound request,
  which closes the connection.
- This harness threads the **request's own context** end to end —
  `httpsse.go:171` passes `r.Context()` into `Loop.Run`, sub-agents derive from
  the parent, the provider call carries it, `claim` selects on `ctx.Done()`.
  There is no detached context and no worker queue anywhere on the turn path.

So the cancellation arrives. Three places drop it on the floor.

## FR-1 — A killed command takes its children with it

**FR-1.1** The command runs in its own **process group** (`Setpgid`), and `cmd.Cancel`
kills the group rather than the leader.

`osexec.CommandContext`'s default kill signals the shell's pid only. `sh -c` spawns
children; they survive, and because `cmd.Stdout`/`cmd.Stderr` are a `bytes.Buffer`,
os/exec gives the command real pipes — so a surviving grandchild holds the write end
and `cmd.Run()` blocks in `Wait` **past the kill, indefinitely**. That is the hang:
the turn never unwinds, `claim`'s deferred release never runs, and the conversation
stays occupied by a turn nobody is watching.

**FR-1.2** `cmd.WaitDelay` bounds the wait regardless. Setpgid closes the common case;
the delay is what makes "the turn ends" independent of whether it did.

**FR-1.3** A cancelled command returns `ctx.Err()`, not a `Result`.

The timeout case was already special-cased and `context.Canceled` was not, so a
cancelled shell came back as a **successful tool result with a nil error** — the loop
read `signal: killed` as output and went on to the next iteration.

The two are not the same event and must not be reported the same way. A deadline is
something to tell the agent about, because it is about the command it wrote. A
cancellation is the member leaving; there is no turn left to inform.

## FR-2 — The loop notices between iterations

**FR-2.1** `Loop.Run` checks `ctx.Err()` at the head of each iteration and returns.

**FR-2.2** `tryChain` stops walking the fallback chain once the context is done.

Without it a cancellation is indistinguishable from "this model did not answer": the
chain walks every remaining model, each failing instantly, emitting a
`"X did not answer (context canceled); trying Y"` progress frame per model — telling
a member who just pressed Stop that their models are down.

**FR-2.3** Neither path emits an error to the sink. A stop is not a failure, and the
socket it would be written to is the one that closed.

## FR-3 — A cancelled provider stream is not a finished one

**FR-3.1** `stream.Next` honours the context it is handed. It took `_ context.Context`
and read a `bufio.Scanner` over the response body.

It worked by accident: the transport kills the body when the request context dies, so
`sc.Scan()` fails and `sc.Err()` surfaces. But a scanner that ends with a nil `Err()`
falls through to `s.finish()` and returns `io.EOF` — a **clean end of stream**. The
caller would then treat a truncated answer as the model's complete one.

## Out of scope

**picoclaw.** Its stop is a `/stop` command over the Pico protocol reaching picoclaw's
own `AgentLoop.HardAbort`; the code that has to honour it is not in this stack. The
proxy's side of it (`internal/pico/cancel.go`) is unchanged and still dials, sends and
waits for an acknowledgement.
