# crab-ganglion-harness

The ganglion: zombie-crab's own agent harness. Go, native HTTP+SSE, streaming.

A turn streams its work as it happens: narration and reasoning arrive on the
progress channel as the agent uses its tools, and the answer arrives as content.
The answer is delivered once the model has finished writing it rather than token
by token -- what an iteration's text IS depends on how its frame ends, so it is
classified before it is sent. See `internal/runtime/loop.go`.

Specs live in the product repo: `.specs/features/crab-ganglion-harness/`.

## How long one turn may work

The only bound on a single turn is how many times it may come back for another
tool. A turn that reaches it stops and says so — `iteration cap reached before
the agent finished` — rather than failing silently.

Two places set it, and the first one wins:

| Where | Default | Who reaches it |
|---|---|---|
| `agents.defaults.max_tool_iterations` in the mounted `config.json` | — | an admin, through the config screen, per agent and in bulk |
| `GANGLION_MAX_ITERATIONS` on the container | 12 | an operator on a deployment with no screen in front of it |

**File first**, which is the opposite of how this harness resolves model keys.
That rule exists because a key is a secret and the file sits on a volume that
gets backed up; a tuning number is neither, and the file is the surface an admin
actually has.

The key is picoclaw's own, spelled the same, so it means the same thing whichever
harness is behind the agent.

The harness prints the number in force AND where it came from at boot (`turns: up
to N iterations each (<source>)`) — the number alone is not enough to debug with,
since `12` looks identical whether the operator set it, the file set it, or
nothing did. A variable that is set but is not a positive whole number fails the
boot naming it; a file value that is zero or negative is ignored rather than
obeyed, because it arrives through a config screen and "the admin typed 0" must
not take the agent off the air.

The cap is read once, at boot, and that is enough: `crab-shell-proxy` recreates
the container when the rendered config file's bytes change, so an admin's edit
arrives as a new process.

Raising it raises the worst case with it: the whole-turn budget is
`MaxIterations + max_children_per_turn * max_child_iterations` model calls.
