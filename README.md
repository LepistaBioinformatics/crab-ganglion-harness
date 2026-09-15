# crab-ganglion-harness

The ganglion is the agent runtime this project wrote for itself. A *harness* is
the program that runs inside a member's agent container: it holds the
conversation, calls the model, runs whatever tools the model asks for, and
writes the transcript to disk. `crab-shell-proxy` starts one such container per
member and speaks to it over HTTP; what happens inside is this program's job.

It is Go, and `go.mod` declares a module path and a Go version and nothing else:
no third-party dependencies at any depth — the MCP client, the OTLP exporter and
the Landlock bindings were written here, not taken from a library — so it
compiles to one static binary. It speaks HTTP with Server-Sent Events natively,
so the proxy needs no protocol translation, and it is now the stack's default
harness: an agent declaring no `harness:` key gets this one.

A turn streams its work as it happens. Narration and reasoning arrive on the
progress channel while the agent uses its tools; the answer arrives as content,
delivered once the model has finished writing it rather than token by token.
That is deliberate — what an iteration's text *is* depends on how its frame
ends, so it is classified before it is sent. See `internal/runtime/loop.go`.

## What it is responsible for, and what it is not

It serves one turn per request, bounds that turn, runs tools under a kernel
confinement, and persists the conversation. That is the whole of it. Container
lifecycle, scale-to-zero, volume provisioning and chown, secrets
materialization, mycelium identity and authorization, the admin API, memgraph,
projects and cron stay in `crab-shell-proxy`. The specification lists those as
out of scope *permanently*, not as work not yet done: they are done **to** a
container, and a harness inside one has no business provisioning anything,
itself included.

## Build and test

`.github/workflows/ci.yml` is the contract, and it is four commands — run them
before opening a pull request, because the workflow runs these and nothing else.

```sh
gofmt -l .          # must print nothing
go vet ./...
go test -race ./... # -race is not optional here
go build ./...
```

`-race` earns its place: the SSE writer has two writers by construction, the
turn and the heartbeat, and interleaving corrupts a frame mid-parse.
`release.yml` pushes the image to `ghcr.io/lepistabioinformatics/crab-ganglion`.
The tests run *inside* the `Dockerfile`, with no build cache, so the image
cannot be published without them passing for those exact bytes. **No moving tag
is ever published** — no `:latest`, no `:main`, no version tag that is rebuilt.
Every image is `:sha-<12 chars>`, plus the git tag's own name on a tagged build,
and a tag names the same bytes forever, so updating a deployment means pointing
`CRAB_GANGLION_IMAGE` at a new digest or sha tag; the incident behind that rule
is at the top of the workflow.

## Running it locally

Configuration is environment variables plus one optional JSON file. The minimum
is a model and an endpoint:

```sh
GANGLION_MODEL=deepseek-chat \
GANGLION_BASE_URL=https://api.deepseek.com/v1 \
GANGLION_API_KEY=sk-... \
GANGLION_DATA_DIR=/tmp/ganglion \
  go run ./cmd/crab-ganglion
```

**A Linux kernel with Landlock is required** — `main` resolves the ABI before
serving anything, and no setting disables the confinement.

The rest worth knowing: `GANGLION_ADDR` (default `:18800`), `GANGLION_TOKEN` (a
bearer then required on the turn route), `GANGLION_DATA_DIR` (default
`/data/.ganglion`, with everything under a `workspace` segment inside it, where
the proxy looks), `GANGLION_CONFIG_FILE` (default `/data/.ganglion/config.json`:
the model registry plus the tool, MCP and evolution blocks, and with one mounted
the model and base URL are no longer required), `GANGLION_SYSTEM_FILE` or
`GANGLION_SYSTEM` for the persona, `GANGLION_MAX_ITERATIONS`,
`GANGLION_APPROVAL_ENDPOINT` and `GANGLION_OTLP_ENDPOINT`. Any credential may be
an `enc://` value, resolved at boot from two factors that decrypt nothing alone
(`GANGLION_KEY_PASSPHRASE` and `GANGLION_KEY_FILE`); seal one from stdin with
`printf %s 'sk-...' | crab-ganglion encrypt`, keeping it out of shell history.

The HTTP surface is two routes and no more: `POST /v1/chat/completions` runs a
turn, and `GET /health` answers `{"status":"ok"}`.

## How long one turn may work

The only bound on a single turn is how many times it may come back for another
tool. A turn that reaches it stops and says so — `iteration cap reached before
the agent finished` — rather than failing silently, and still returns the work
already done.

Three sources, first one wins: `agents.defaults.max_tool_iterations` in the
mounted `config.json`, then `GANGLION_MAX_ITERATIONS`, then the built-in default
of 12. **File first**, the opposite of how this harness resolves model keys — a
key is a secret and the file sits on a volume that gets backed up, while a
tuning number is neither and the file is what an admin config screen edits. The
key is picoclaw's own, spelled the same, so it means the same under either.

Boot prints the number in force *and* where it came from (`turns: up to N
iterations each (<source>)`), since `12` looks identical whether the operator
set it, the file set it, or nothing did. A variable set to anything but a
positive whole number fails the boot naming it; a file value of zero or less is
ignored, because "the admin typed 0" must not take the agent off the air. The
cap is read once, at boot — the proxy recreates the container when the config
file changes. Raising it raises the worst case with it: the whole-turn budget is
`MaxIterations + max_children_per_turn * max_child_iterations` model calls.

## The tool set

A tool that cannot work is **absent** rather than present-and-explaining-itself,
and the set is fixed at boot, so a conversation never silently gains or loses a
capability (`tools()` in `cmd/crab-ganglion/main.go`). Always present: **`shell`**,
the tool that matters; **`load_image`**, which reads an image already in the
workspace, where the webapp's uploads land; and **`set_reasoning_depth`**, which
lets the model think harder for the rest of a turn — that one always exists, and
only its routing varies, depending on whether a model declares `thinking_level`.
Conditional:

- **`web_search` and `web_fetch`**, together, when a search provider in the
  registry is enabled and carries what it needs (brave, tavily, searxng,
  duckduckgo). Called from the harness process, not the shell, whose environment
  is cut to `PATH`, `HOME`, `TERM`, `LANG` and `TZ`.
- **`generate_image`**, when an image-generation model is registered with a key.
- **`subagents`**, when sub-turns are enabled with a positive child budget, and
  **`research`**, which additionally needs web search enabled.
- **MCP tools**, one set per server. A server that cannot be reached fails the
  boot, and so does a remote tool colliding with a built-in name — the registry
  overwrites, so a server offering `shell` would replace the sandboxed one.

## The sandbox

The only tool is `/bin/sh -c <arbitrary string>`: no path argument to validate,
and any denylist defeated by `$(echo L2V0Yw== | base64 -d)`, so the confinement
is the kernel's. Before running a command the binary re-execs *itself*, applies
a Landlock domain in the child, and only then `execve`s the shell. It fails
closed, at boot: a kernel with no Landlock stops the process.

The workspace is read-write; `/usr`, `/bin`, `/sbin`, `/lib` and `/etc` are
read-execute; `/dev` is writable because `cmd > /dev/null` must work. Everything
else is denied, above all `/proc`, whose `/proc/1/environ` holds the harness's
own environment and so the provider key that scrubbing had removed from the
command's. `/tmp` is *not* granted — granting it grants whatever any other
process left there — so commands get scratch inside the workspace via `TMPDIR`.
See `internal/adapter/tool/exec/`.

## Code layout

`cmd/crab-ganglion/main.go` is the composition root, the only file where
adapters meet. Everything else knows the domain and nothing else.

**`internal/domain`** holds the ports and the types crossing them, standard
library only, and **`internal/runtime`** the agent loop against those ports.
**`internal/config`** reads the environment and the JSON registry,
**`internal/secret`** resolves `enc://`, **`internal/skillfile`** is the
`SKILL.md` format, picoclaw's unchanged. Then one directory per adapter under
**`internal/adapter/`**: `httpsse` (ingress), `provider/openai`,
`provider/router`, `store/jsonl` (the append-only transcript, which implements
no way to shorten a file), `store/window` (the derived context, freely rewritten
and rebuilt from it), `tool/*`, `mcp`, `skills`, `evolution`, `telemetry/otlp`,
`approver/proxy`.

`internal/domain/arch_test.go` holds that shape as a test rather than a
convention: `TestDomainImportsOnlyStdlib` fails if the domain imports anything
outside the standard library, `TestAdaptersDoNotImportEachOther` if one adapter
imports another. `internal/skillfile` exists *because* of the second — the
skills loader and evolution both need the format, and the test caught the
coupling on the first build.

## Documentation

The book is canonical for the product story and how to deploy it:
<https://lepistabioinformatics.github.io/zombie-crab-project/>, with this
component's chapter at
[`51-crab-ganglion-harness.html`](https://lepistabioinformatics.github.io/zombie-crab-project/51-crab-ganglion-harness.html).
This repository keeps no `.specs/` of its own: its specification lives in the
product repository under `.specs/features/` — `crab-ganglion-harness/` for the
harness, plus one `ganglion-*` directory per feature.

## License

`MIT OR Apache-2.0`, at your option, as with the rest of
[zombie-crab-project](https://github.com/LepistaBioinformatics/zombie-crab-project),
whose `LICENSE-MIT` and `LICENSE-APACHE` carry the texts. Unless you state
otherwise, any contribution you submit for inclusion, as defined in the
Apache-2.0 license, is dual licensed as above with no added terms.
