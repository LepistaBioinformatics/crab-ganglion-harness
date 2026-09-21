# Compaction stops destroying what it compacts

Today `compact()` is drop-oldest by message count. Everything it drops from the
window is gone from the window, and for one class of content it is gone from the
machine: **a tool result was never written to the transcript.**

`loop.go` says so at the point the result is built:

> The tool result goes to the window only. The served transcript records what the
> member saw, and they never saw this.

And `window.go`'s `conversational` says it from the other side:

> The results answering them were never written there, because the member never
> saw one.

That is correct as a rule about the *served history* — a member did not see a
tool result and a transcript is what they saw. It is a problem as a rule about
*durability*, because the same content is:

- the **largest** thing in the window. `exec` caps one result at 64 KiB
  (`exec.go:49`), which is roughly 16k tokens, and `DefaultWindowBudget` counts
  it as **one of 40 messages**. A window can be twenty times its own budget's
  worth of tokens and the budget cannot see it.
- the **only** content with no second copy anywhere.

So the two facts compose into the thing this feature fixes: the budget is blind
to exactly the content whose loss is irreversible.

## The order, and why it is this order

Three changes, and the first is a precondition for the third rather than merely
cheaper than it.

**FR-1** gives a large tool result a durable home and leaves a pointer in the
window. **FR-2** lets compaction replace an old result with that pointer instead
of dropping it. **FR-3** writes a marker into the transcript saying compaction
happened, which is what makes it visible to the member and recoverable on a
window rebuild.

A marker written first would preserve what was already preserved. The transcript
holds what the member said and what the agent answered, and those were never at
risk — they are append-only by FR-9 of the transcript's own spec. The content
that compaction actually destroys is not in the transcript at all, and FR-1 is
what puts it somewhere a marker can point.

## FR-1 — A large tool result gets a file

**FR-1.1** When a tool returns more than `offloadThreshold` bytes, the content is
written to a file and the message carries a **head, a tail, the byte count and the
path** instead of the whole thing.

Head *and* tail, not head alone: the useful end of a command's output is as often
the last line (an error, a total, a summary) as the first, and a head-only clamp
reliably cuts off the answer while keeping the noise.

**FR-1.2** The file lands inside the **turn's own workspace**.

Not a choice of tidiness. `exec`'s Landlock root is the turn's workspace and
nothing above it (`exec.go:198-213`) — a project turn gets `workspace-<id>`, a
turn outside one gets `workspace/`. A path outside that root is a path the agent
cannot read, which would make the pointer a lie.

**FR-1.3** It does **not** land in `.tmp`. The composition root clears that
directory at boot (`exec.go:235-241`), so a pointer written there would resolve
until the next restart and dangle after it — the worst of both designs, because
nothing would report the loss.

**FR-1.4** The message's **shape** does not change: still `RoleTool`, still the
same `ToolCallID`. `dropOrphanTools` and `groupToolRuns` must not be able to tell
the difference, and no provider-facing code learns a new case.

**FR-1.5** A failed write degrades to today's behaviour — the full content stays
inline — and never fails the turn. The offload is an optimisation of where bytes
live. A turn that works today must not start failing because a disk is full.

## FR-2 — Compaction elides an old result rather than dropping it

**FR-2.1** When compaction **actually drops** messages, a tool result that
survives the cut but is older than the `elisionKeep` most recent ones has its
content replaced by its pointer.

"Actually drops", not "runs". Compaction runs once per iteration, so a window
sitting at 8 of 40 would otherwise trade an excerpt the agent is still working
with for a filename — costing it one of its twelve iterations to read back a
file it had a moment ago, to save room nothing is asking for. Dropping is the
signal that the window is under pressure; eliding is for a window that is.

This is the same trade the Anthropic API makes server-side with
`clear_tool_uses_20250919` and OpenCode makes with its `prune`: recent tool output
is context, old tool output is a filename. It costs no provider call.

**FR-2.2** Only a result that was **offloaded** is elided. A result below the
threshold has no file to point at, and its content is the same order of magnitude
as the pointer would be — eliding it would trade bytes for nothing and lose them.

**FR-2.3** Elision is idempotent. Compaction runs three times in a turn
(`loop.go:290`, `:423`, `:439`); eliding an already-elided message must be a
no-op, not a pointer wrapped in a pointer.

## FR-3 — Compaction leaves a durable marker

**FR-3.1** When compaction drops messages, a **marker record** is appended to the
transcript carrying how many were dropped and the summary text.

**FR-3.2** The marker never reaches a provider, and it is an **events-only
assistant message** — `Role: assistant`, no content, one `TurnEvent` of a new
kind — because that is the only shape in this format for which that is true.

`conversational` drops exactly two things when it rebuilds a window: a tool
result, and an entry with no content that carries events. The second is the
`Events` record the loop already writes, whose comment says *"It never reaches a
provider"*. So the marker rides a rule that exists rather than needing one.

Both alternatives leak, and neither leaks loudly. A marker carrying **content**
is seeded into a rebuilt window and sent verbatim, as something the agent said.
A marker with a **role of its own** leaks too: nothing in this harness validates
a role, `conversational` drops only `RoleTool`, and the wire adapter passes the
role through as a string.

The count rides in a new `TurnEvent.Count` rather than in prose. The harness has
no locale — it writes the number and the webapp writes *"N earlier messages"* or
*"N mensagens anteriores"*.

**FR-3.3** The marker is appended **at most once per turn**, at the point the
context is saved, carrying that turn's total.

Compaction runs up to three times in one turn. Three markers for one turn's
compaction would be three dividers in the member's transcript for one event.

**FR-3.4** A rebuilt window states how many messages it left behind — counted
from the rebuild, **not** read off a marker.

This requirement was specified the other way round and was wrong. A marker
records what the LIVE window dropped, against a history the rebuild is not
reconstructing: `rebuild` seeds a fresh tail of the transcript and leaves a
different amount behind. Carrying the marker's number over would state a count
that was true of the window this one replaces.

What the rebuild owed the agent was not the marker's number but *a* number. It
used to begin mid-conversation with nothing marking the cut, so a seed read as
though it were the whole exchange — the same silence compaction is being taught
to break, arriving from the other direction.

**FR-3.5** The marker lands on a **turn boundary**: it is appended after a
complete turn's records, never between an assistant message and the records
belonging to the same exchange.

This is the one that ships broken if it is skipped. A marker is durable — a
badly-placed one is re-read on every later turn of that conversation, which is
precisely the argument `compact.go` already makes for running `repair()` on load
and not only on save. The placement is the first defence; `repair()` staying in
place on load is the second, and neither replaces the other.

## What this deliberately does NOT do

**The window is still saved, not derived.** In OpenHands the condensation event is
the *sole* definition of the view: the log is the truth and `View.from_events()`
rebuilds the context every time. Here the window file remains a real artifact and
the marker is a record of what happened to it, plus an input to `rebuild`.

Making the window purely derived is a coherent design and a larger, riskier
change — it would put every turn's prompt behind a full transcript replay, and
the window file is load-bearing today. It is not needed for anything this feature
promises. Left as DQ-2.

**No summarization.** `compact()` remains drop-oldest and the marker records what
was dropped. DQ-1 is untouched and still open: summarizing properly needs a
provider call and a policy nobody has chosen.

**No summarization, still.** DQ-1 is untouched: `compact()` remains drop-oldest,
and what FR-6 adds is a way to go and read what was dropped rather than a
compressed stand-in for it.

## FR-6 — The agent can read what left the window

**FR-6.1** `search_history` searches this conversation's transcript.

Case-insensitive substring, optionally narrowed to one speaker. It searches only
what was **said** — a transcript also holds the loop's own events records, which
carry no content, and matching one would answer a search with an empty
quotation.

**FR-6.2** The conversation reaches the tool through the **context**.

`domain.WithConversation`, set by the loop beside `WithProject` and for the same
reason: the reader is a tool, whose `Invoke` takes `(ctx, args)`, and the tool is
constructed once for the whole process — there is no per-turn instance to hand
an id to. Outside a turn it refuses rather than guessing, because a guess would
search somebody else's conversation.

**FR-6.3** The answer is bounded, says where each match sits, and says when it
was cut.

- `maxHits` matches, keeping the **newest**. A conversation that says "the
  report" forty times is asking about the most recent one.
- The snippet is cut **around the match**, not from the head: a match 3000
  characters into an answer is invisible in a head clamp, which would report a
  hit and show nothing resembling it.
- A capped answer that does not say it was capped is one the agent reads as
  "this is everything".

**FR-6.4** A **sub-agent does not get the tool**, at any depth.

A child runs under a fixed `SessionID`, not the member's conversation — so the
tool would read a transcript every child in the workspace shares, and answer a
question about this conversation with another one's text. The existing
`HideAtDepth` does not cover it: that bounds recursion and fires only at the
cap. This is a tool whose meaning does not survive the crossing at all, so it is
withheld from every child, through a second list beside it.

A child is given a task, not a conversation.

**FR-6.5** Every refusal is a `Result`, never an error. The agent carries on
without it rather than losing the turn to it.

**FR-6.6** The window's summary names the affordance.

This is the half that is easy to skip and worthless to skip. MemGPT keeps a
statistics block in the context for exactly this reason: an affordance the agent
does not know about is not an affordance. Here the two halves already had homes
— the summary says HOW MUCH is outside the window, the tool's description says
what to do about it — so the summary gained one clause and nothing else.

It names the affordance **generically**, not by tool name. `compact()` is pure
over a window and cannot know which tools a deployment registered; a note naming
a tool nobody wired would be a second kind of lie about the same conversation.

## FR-7 — Parked output does not grow without bound

**FR-7.1** A conversation keeps its `Retain` most recently modified parked
results; older ones are deleted.

**FR-7.2** `Retain` is bounded **below** by the window budget, not chosen by
taste. A pointer is only ever read out of the window, and the window holds at
most `WindowBudget` messages — so at most that many pointers can be live, and
deleting anything older cannot orphan one. 128 is 40 with room to raise the
budget without anyone remembering this file.

**FR-7.3** Pruning never deletes the file the call just wrote, **named
explicitly** rather than inferred from its timestamp.

Modification time is the only ordering a directory offers and its granularity is
the filesystem's. On one that stamps whole seconds a burst of parks shares a
timestamp, and `sort.Slice` is unstable — so the file whose pointer is certainly
live can land anywhere in the ordering.

**FR-7.4** Pruning is per conversation, and its failure is never a turn's
failure. It runs after the write, and its error is dropped: a turn that produced
output must not fail because the sweep of what it replaced did.

**FR-7.5** Deleting at all is a departure worth stating. Nothing else in this
harness removes what it writes, and the transcript specifically may not be
shortened (FR-9 there). The difference is whose bytes they are: a transcript
entry is the member's, and a parked result is the agent's scratch — output the
member never saw, kept so the agent can re-read what it ran.

The alternative was checked and is not "the proxy handles it".
`admin-managed-storage-limits` does not: it is **specified and not implemented**
(no quota code exists in the proxy), and its own spec says it is *"not a
retention or clean-up policy"* and *"not a quota on anything but member
uploads"*. Unbounded is what leaving this alone would have meant.

## Checked and found to need nothing

**Index skew across the webapp's two history fetches.** Widening the BFF filter
means `route.ts` now passes events-only entries while `history-cache.ts` still
drops them, so the two arrays no longer agree on length. That would matter if
anything carried an index from one to the other — it does not: the tree's scroll
anchor is a `created_at` string, re-resolved with `findIndex` against the view's
own array (`chat-view.tsx`). Recorded because the question is worth asking again
if either filter moves.

## Gate

`gofmt -l . | (! read)`, `go vet ./...`, `go test -race ./...`, `go build ./...` —
the four the repo's own `ci.yml` runs.
