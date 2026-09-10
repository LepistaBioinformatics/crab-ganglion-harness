// Package exec is the first Tool: run a shell command inside the container.
//
// The container is per-user and holds only that member's workspace, so the
// isolation boundary is the container itself -- not a sandbox inside it.
//
// Commands are confined, and by the kernel rather than by inspecting the
// string the model wrote.
//
// An earlier version of this comment claimed a confinement that did not exist:
// Workdir is cmd.Dir, which is where a command starts, not a boundary. What
// replaced the claim is two controls that only work together:
//
//   - scrubEnv gives the command an allowlisted environment, so the provider
//     key and the ingress bearer are not in it;
//   - a re-exec through SandboxArg puts a Landlock domain around it, so the
//     command cannot read the same key back out of /proc/1/environ -- PID 1
//     being the harness, running as the same uid. The scrub alone left the
//     credential exactly one file away, which was found by reading it.
//
// Landlock rather than a path check, because the only tool here is
// `/bin/sh -c <arbitrary string>`: there is no path argument to resolve, and
// any denylist of `..` or `/etc` is defeated by
// `$(echo L2V0Yw== | base64 -d)`. picoclaw's restrict_to_workspace is such a
// check -- its own docs say the sandbox has no kernel-level isolation -- so
// what runs here is stronger than the mechanism it reached parity with, not a
// port of it.
//
// See .specs/features/ganglion-agent-confinement in the product repository.
package exec

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

const (
	defaultTimeout = 2 * time.Minute
	maxOutput      = 64 << 10
)

// Tool runs commands with Workdir as the working directory.
type Tool struct {
	Workdir string
	Timeout time.Duration
	Shell   string

	// Self is the harness binary, re-executed to apply the Landlock
	// confinement in the child (see sandbox.go). The composition root always
	// sets it; there is NO configuration that clears it, because a
	// confinement an operator can switch off is one that is off on the
	// deployment that needed it.
	//
	// Empty only in unit tests that are asserting something other than the
	// sandbox -- and TestEveryCommandIsSandboxed asserts that the real
	// construction never leaves it so.
	Self string
}

func New(workdir string) *Tool {
	return &Tool{Workdir: workdir, Timeout: defaultTimeout, Shell: "/bin/sh"}
}

func (t *Tool) Name() string { return "shell" }

func (t *Tool) Schema() domain.ToolSchema {
	return domain.ToolSchema{
		Name:        "shell",
		Description: "Run a shell command in the workspace and return its combined output.",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "command": {"type": "string", "description": "The command to run."}
  },
  "required": ["command"]
}`),
	}
}

func (t *Tool) Invoke(ctx context.Context, raw json.RawMessage) (domain.Result, error) {
	var a struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		// Malformed arguments are the model's mistake, so they come back as a
		// Result it can correct -- not as a turn failure.
		return domain.Result{Content: "The arguments were not valid JSON: " + err.Error()}, nil
	}
	if strings.TrimSpace(a.Command) == "" {
		return domain.Result{Content: "No command was given."}, nil
	}

	timeout := t.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	shell := t.Shell
	if shell == "" {
		shell = "/bin/sh"
	}
	cmd := t.command(ctx, shell, a.Command)

	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()

	out := buf.String()
	if len(out) > maxOutput {
		out = out[:maxOutput] + fmt.Sprintf("\n[output truncated at %d bytes]", maxOutput)
	}
	if ctx.Err() == context.DeadlineExceeded {
		return domain.Result{Content: out + fmt.Sprintf("\n[the command was still running after %s and was stopped]", timeout)}, nil
	}
	if err != nil {
		// A non-zero exit is information for the agent, not a harness failure.
		return domain.Result{Content: out + "\n[exit: " + err.Error() + "]"}, nil
	}
	if strings.TrimSpace(out) == "" {
		return domain.Result{Content: "[no output]"}, nil
	}
	return domain.Result{Content: out}, nil
}

// command builds the process that runs the agent's shell string.
//
// Two layers, and neither is sufficient alone:
//
//   - scrubEnv keeps the provider key and the ingress bearer out of the
//     command's own environment;
//   - the re-exec through SandboxArg applies Landlock, which is what stops the
//     command reading the SAME key back out of /proc/1/environ -- PID 1 being
//     the harness, running as the same uid.
//
// The first was written believing it closed the exposure. It did not, and the
// gap was found by reading /proc/1/environ from a scrubbed child.
func (t *Tool) command(ctx context.Context, shell, script string) *osexec.Cmd {
	var cmd *osexec.Cmd
	if t.Self == "" {
		cmd = osexec.CommandContext(ctx, shell, "-c", script)
	} else {
		cmd = osexec.CommandContext(ctx, t.Self, SandboxArg, t.Workdir, shell, "-c", script)
	}
	cmd.Dir = t.Workdir
	cmd.Env = scrubEnv(os.Environ())

	// Scratch space inside the workspace rather than a second writable
	// hierarchy in the ruleset. mktemp, sort and anything else that reaches
	// for a temporary file lands here, where the agent may already write.
	tmp := filepath.Join(t.Workdir, TmpDirName)
	if err := os.MkdirAll(tmp, 0o755); err == nil {
		cmd.Env = append(cmd.Env, "TMPDIR="+tmp)
	}
	return cmd
}

// TmpDirName is a dotfile, so it stays out of the agent's way when it lists
// its own workspace and out of the proxy's way when it reads sessions/ (which
// it does by name, one level below this).
//
// Exported because the composition root clears it at boot: nothing else
// removes what a command leaves here.
const TmpDirName = ".tmp"

// passThrough names the environment a command may see. An ALLOWLIST, not a
// denylist of the secret-looking names: the proxy decides what enters this
// container, so a denylist here would have to be edited in a second repository
// every time it adds a variable -- and the failure mode of forgetting is that
// a credential becomes readable with no test failing anywhere.
//
// The live containers carry GANGLION_API_KEY (the deploy-level provider key,
// shared by every member on that agent) and GANGLION_TOKEN (which is also the
// bearer the approver presents). Both were readable with a bare `env`.
var passThrough = map[string]bool{
	"PATH": true,
	"HOME": true,
	"TERM": true,
	"LANG": true,
	"TZ":   true,
}

// scrubEnv keeps only passThrough, and supplies a PATH if the container has
// none -- a command with no PATH cannot find /bin/ls, which would look like a
// broken tool rather than a missing variable.
func scrubEnv(environ []string) []string {
	out := make([]string, 0, len(passThrough))
	seen := map[string]bool{}
	for _, kv := range environ {
		k, _, ok := strings.Cut(kv, "=")
		if ok && passThrough[k] {
			out = append(out, kv)
			seen[k] = true
		}
	}
	if !seen["PATH"] {
		out = append(out, "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	}
	return out
}
