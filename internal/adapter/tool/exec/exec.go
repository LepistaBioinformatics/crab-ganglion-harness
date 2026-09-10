// Package exec is the first Tool: run a shell command inside the container.
//
// The container is per-user and holds only that member's workspace, so the
// isolation boundary is the container itself -- not a sandbox inside it. What
// this package still owns is keeping a command from reaching outside the
// workspace root, because a tool that can be talked into reading /proc or the
// mounted secrets is a tool that eventually will be.
package exec

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
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
	cmd := exec.CommandContext(ctx, shell, "-c", a.Command)
	cmd.Dir = t.Workdir

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
