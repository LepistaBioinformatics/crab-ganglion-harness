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
	"errors"
	"fmt"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

const (
	defaultTimeout = 2 * time.Minute
	maxOutput      = 64 << 10
	// waitDelay is how long Wait may go on copying output after the command was
	// killed, before os/exec closes the pipes under it and returns.
	//
	// Short, because by the time it matters the member has already pressed Stop
	// and every millisecond is one they spend watching a turn they cancelled.
	// Long enough that a command writing its last line as it dies is not cut off
	// in the ordinary case, which is the only thing the delay costs.
	waitDelay = 2 * time.Second
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

	// A PROCESS GROUP, because the thing being killed is `sh -c`, whose whole job
	// is to start other things.
	//
	// CommandContext's own kill signals the shell's pid and nothing else, so its
	// children outlive it. That is not merely untidy: Stdout and Stderr below are
	// a bytes.Buffer, which makes os/exec give the command real pipes and copy
	// them in goroutines -- and Wait does not return until those copies end,
	// which needs EVERY holder of the write end to be gone. One surviving
	// grandchild therefore blocks cmd.Run() past the kill, with nothing to break
	// it. The turn never unwinds, the ingress claim it holds is never released,
	// and the conversation stays occupied by a turn the member already stopped.
	//
	// The negative pid is the group. It is safe here because this process made
	// the group one line earlier; it is never a pid this harness did not start.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	// And a bound on the wait regardless of whether that worked. Setpgid closes
	// the case anyone can foresee; this is what makes "the turn ends" not depend
	// on having foreseen it.
	cmd.WaitDelay = waitDelay

	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()

	out := buf.String()
	if len(out) > maxOutput {
		out = out[:maxOutput] + fmt.Sprintf("\n[output truncated at %d bytes]", maxOutput)
	}
	// THE MEMBER LEFT, which is not a thing to tell the agent about.
	//
	// Reported as an error so the loop stops, and that is the difference from the
	// deadline below. A deadline is information: the agent wrote a command that
	// ran too long and should hear so, on the next iteration. A cancellation has
	// no next iteration -- there is no turn left to inform, and returning a
	// Result here is what let one continue. It did: the loop read `signal:
	// killed` as the command's output and asked the model what to do about it.
	if errors.Is(ctx.Err(), context.Canceled) {
		return domain.Result{}, ctx.Err()
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
	dir := t.projectDir(ctx)
	var cmd *osexec.Cmd
	if t.Self == "" {
		cmd = osexec.CommandContext(ctx, shell, "-c", script)
	} else {
		cmd = osexec.CommandContext(ctx, t.Self, SandboxArg, dir, shell, "-c", script)
	}
	// THE SANDBOX ROOT IS THE TURN'S OWN WORKSPACE, and the command starts
	// there. A project turn gets workspace-<id>; a turn outside one gets
	// workspace/, exactly as before.
	//
	// It could not stay the main workspace once projects became siblings of it,
	// and it must not become their PARENT: that directory also holds config.json
	// (the model registry, with its api_keys) and credential.key, both bound
	// there precisely because the Landlock root ends below them. Widening the
	// root by one level would hand a command both.
	//
	// So the root narrowed instead, and isolation between a member's own
	// projects stopped being a convention and became a kernel boundary. The one
	// thing that cost -- the admin's shared skills, which used to live only at
	// the main workspace's root -- the proxy now mounts into every workspace,
	// which is what picoclaw already does for its own project agents.
	cmd.Dir = dir
	cmd.Env = scrubEnv(os.Environ())

	// Scratch space inside the workspace rather than a second writable
	// hierarchy in the ruleset. mktemp, sort and anything else that reaches
	// for a temporary file lands here, where the agent may already write.
	// In the TURN'S workspace, not always the main one: a project turn writing
	// its scratch into the main workspace would leave files in a tree it is not
	// working in, and TMPDIR has to be somewhere the command can already write.
	tmp := filepath.Join(dir, TmpDirName)
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

// SecretPrefix marks a variable the MEMBER put there, and it is the one thing a
// command sees that the allowlist above does not name.
//
// It exists because there was no way for a member's own credential to reach their
// agent at all. Under picoclaw the secrets a member saved were files in a mounted
// `.secrets/`; the ganglion has no such bind, and the proxy's comment saying
// "credentials arrive as environment" was true of the harness PROCESS and false of
// the shell it hands the agent. A member migrating from picoclaw wrote a secret,
// got a 200, and their agent could not see it -- with nothing anywhere saying so.
//
// IT DOES NOT WEAKEN THE RULE ABOVE, which is the reason it is a prefix rather
// than a handful of new names. The principle is that the PROXY decides what a
// command may see, and a denylist here would have to learn every variable the
// proxy ever adds. A prefix keeps that exactly: the proxy decides what to put
// under it, and everything outside it -- GANGLION_API_KEY, GANGLION_TOKEN, the
// approval endpoint, the passphrase -- stays scrubbed with no edit here.
//
// TWO UNDERSCORES, so the boundary between the marker and the member's own name
// is unmistakable: `CRAB_SECRET__DB_URL` is `DB_URL`, and a member whose secret is
// itself called `SECRET_FOO` cannot be confused for one.
//
// WHAT THIS ACCEPTS. A command can now read the member's credential, so an agent
// steered by untrusted text can exfiltrate it. That is inherent -- a secret the
// agent cannot use is not a feature -- and it is the member's own credential
// rather than the deployment's, which is exactly the distinction the allowlist
// above draws.
const SecretPrefix = "CRAB_SECRET__"

// scrubEnv keeps only passThrough, and supplies a PATH if the container has
// none -- a command with no PATH cannot find /bin/ls, which would look like a
// broken tool rather than a missing variable.
func scrubEnv(environ []string) []string {
	out := make([]string, 0, len(passThrough))
	seen := map[string]bool{}
	for _, kv := range environ {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if passThrough[k] {
			out = append(out, kv)
			seen[k] = true
			continue
		}
		// The member's own, under the marker the proxy put there. The marker is
		// STRIPPED: what the member saved as DB_URL is what a command reads, or
		// every tool expecting a conventional name would have to be told about
		// this harness.
		if name, marked := strings.CutPrefix(k, SecretPrefix); marked && name != "" {
			out = append(out, name+"="+v)
		}
	}
	if !seen["PATH"] {
		out = append(out, "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	}
	return out
}

// projectDir is where a command starts.
//
// Created here rather than assumed: the proxy seeds a project's tree on every
// ensure, but a scale-to-zero container can take a turn for a project created
// while it was stopped, and a shell that starts in a directory that does not
// exist fails with an error about neither.
func (t *Tool) projectDir(ctx context.Context) string {
	root := domain.ProjectRoot(ctx, t.Workdir)
	if root == t.Workdir {
		return t.Workdir
	}
	// The PROJECT ROOT, not a files/ child of it. The main workspace's shell
	// starts where uploads/, media/ and sessions/ are children; a project's
	// must too, or the same instruction ("read uploads/report.csv") means two
	// different paths depending on where the member happens to be. The proxy
	// writes a project's uploads under this directory for the same reason.
	if err := os.MkdirAll(root, 0o755); err != nil {
		return t.Workdir
	}
	return root
}
