# crab-ganglion-harness

The ganglion: zombie-crab's own agent harness. Go, native HTTP+SSE, streaming.

A turn streams its work as it happens: narration and reasoning arrive on the
progress channel as the agent uses its tools, and the answer arrives as content.
The answer is delivered once the model has finished writing it rather than token
by token -- what an iteration's text IS depends on how its frame ends, so it is
classified before it is sent. See `internal/runtime/loop.go`.

Specs live in the product repo: `.specs/features/crab-ganglion-harness/`.

## Configuration an operator sets by hand

Everything else comes from the mounted `config.json` or from the variables
`crab-shell-proxy` injects when it creates the container. This one does not, and
it is the only bound on how long a single turn may work:

| Variable | Default | What it does |
|---|---|---|
| `GANGLION_MAX_ITERATIONS` | 12 | How many model calls one turn may make. A turn that reaches it stops and says so (`iteration cap reached before the agent finished`) rather than failing silently. |

Set it on the container, not in `config.json`. The harness prints the number in
force at boot (`turns: up to N iterations each`), so a value that did not arrive
is visible in the container log rather than only in a turn that stopped early. A
value that is set but is not a positive whole number fails the boot naming the
variable.

Raising it raises the worst case with it: the whole-turn budget is
`MaxIterations + max_children_per_turn * max_child_iterations` model calls.
