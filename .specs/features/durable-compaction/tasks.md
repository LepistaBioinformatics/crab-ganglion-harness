# Tasks — durable compaction

Five repositories. The code lives in three of them; the other two carry pointers.
Merged bottom-up, one level at a time, as `.claude/rules/submodule-pointers.md`
requires.

```
crab-ganglion-harness   T1 T2 T3 T9 T10   parks, elides, marks, recalls, prunes
crab-shell-proxy        T4                reads the marker, serves it
crab-exoskeleton-webapp T5                renders it
zombie-crab-project     T6 T7             docs + submodule pointers
zombie-crab-project-mkt T8                pointer
```

## T1 — A large tool result gets a file (FR-1)

**Where** `internal/runtime/loop.go` at the single point the `RoleTool` message is
built; a new file under `internal/runtime/` for the offload itself.

**Done when** a result over the threshold is written under the turn's workspace and
the message carries head + tail + size + path; the role and `ToolCallID` are
unchanged; a write failure leaves the content inline.

**Tests** a result over the threshold is offloaded and the file holds the original
bytes; a result under it is untouched; a failed write keeps the whole content and
does not fail the turn; the offloaded message still pairs with its call through
`groupToolRuns`.

**Gate** `gofmt -l`, `go vet ./...`, `go test -race ./...`, `go build ./...`

## T2 — Compaction elides an old result (FR-2)

**Depends on** T1 — there is nothing to point at before it.

**Where** `internal/runtime/compact.go`.

**Done when** a surviving tool result older than the keep count carries its pointer
instead of its content; a result that was never offloaded is untouched; running it
twice changes nothing the second time.

**Tests** elision replaces content with the pointer past the keep count; the most
recent results keep their content; a non-offloaded result is left alone;
idempotence across a second `compact` call.

## T3 — The marker (FR-3)

**Depends on** T1 for the claim it makes about what survives; independent of T2.

**Where** `internal/domain/` for the record, `internal/runtime/loop.go` for the
append, `internal/adapter/store/window/window.go` for the rebuild.

**Done when** one marker per turn is appended when compaction dropped messages; it
carries the count and a note formatted from that same count; it never reaches a
provider; `rebuild` states what IT left behind (counted from the rebuild, never
read off a marker); a transcript with no marker behaves exactly as today.

**Tests** a turn that compacts writes exactly one marker; a turn that does not
compact writes none; the marker's note and its count agree; the marker is the one
shape a provider never sees; a rebuilt window says what it left behind and ignores
a marker's own count; a marker the transcript refuses does not fail the turn.

## T9 — The agent can read what left the window (FR-6)

**Depends on** nothing in T1-T3; it is the other half of the same promise.

**Where** `internal/domain/session.go` for the context carrier,
`internal/adapter/tool/recall/` for the tool, `cmd/crab-ganglion/main.go` to
register it, `internal/runtime/compact.go` and
`internal/adapter/store/window/window.go` for the note that names it.

**Done when** a message dropped from the window is findable by substring; the
answer is capped, says where each match sits and says when it was cut; a turn
outside a conversation refuses; the window's summary names the affordance.

**Tests** a match that left the window is found; a child is never offered the
tool; matching is case-insensitive;
only what was said is searched; the cap keeps the newest and discloses itself;
the snippet is cut around the match; the role filter narrows; nothing-found says
how much was searched; every refusal is a Result; no conversation is a refusal.

## T10 — Parked output does not grow without bound (FR-7)

**Depends on** T1.

**Where** `internal/adapter/store/tooloutput/`.

**Done when** a conversation keeps its `Retain` newest parked results; the file
just written is never pruned; pruning is per conversation and never fails a turn.

**Tests** a conversation over the retention keeps at most `Retain`; every
pointer resolves at the moment it is handed out; a busy conversation does not
evict a quiet one; `prune` never deletes the file it was told to keep, even when
that file is the oldest in the directory.

## T4 — The proxy serves the marker (FR-4)

**Depends on** T3 merged, so the record exists to read.

**Where** `internal/history/`, plus the history handler.

**Done when** a marker line is parsed and reaches the history response as its own
item; an unrecognised line still cannot break the reader; the served shape is
additive, so a webapp that does not know the item ignores it.

**Tests** a transcript containing a marker serves it; one without is byte-identical
to today; a malformed marker line is skipped rather than failing the read.

**Gate** `go build ./...`, `go vet ./...`, `go test ./...` — `internal/docker` has
10 pre-existing `lchown` failures on this host that pass as root in Docker.

## T5 — The webapp renders it (FR-5)

**Depends on** T4.

**Where** `app/chat/`, `lib/` for the type and its parse, `lib/i18n/` for the copy.

**Done when** the marker renders as a divider inside the transcript saying how many
messages were compacted, with the summary revealable; both locales; an item the
parser does not recognise is dropped rather than crashing the list.

**Tests** a history payload containing a marker renders the divider; the summary is
hidden until revealed and shown after; a payload without one renders unchanged.

**Gate** `./node_modules/.bin/vitest run` and `next build`. `yarn lint` does not run
in this repo and `tsc` has pre-existing errors under `app/chat/*.test.*`.

## T6 — Docs

**Where** `docs/book/src/51-crab-ganglion-harness.md`, which already describes the
transcript/window split at lines 70-73 and is now incomplete.

**Done when** the offload and the marker are described where that split is.

## T7 / T8 — Pointers

Bottom-up after each child PR merges: harness → proxy → webapp → product → mkt.
A pointer may only name a commit reachable from the child's default branch;
`.github/workflows/submodule-pointers.yml` enforces it.

## Language

Everything in these three repositories is written in **English** — code, comments,
commits, PR titles and bodies, test names, this file. Only the top-level
`zombie-crab-project-mkt` writes Portuguese.
