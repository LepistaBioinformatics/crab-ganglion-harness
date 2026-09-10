package exec

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/adapter/tool/exec/landlock"
	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

// AC-1. The two secrets that were readable on the live gamma container must
// not survive into a command's environment.
func TestSecretsAreNotVisibleToACommand(t *testing.T) {
	t.Setenv("GANGLION_API_KEY", "sk-must-not-appear")
	t.Setenv("GANGLION_TOKEN", "bearer-must-not-appear")
	t.Setenv("PATH", os.Getenv("PATH"))

	tool := New(t.TempDir())
	res, err := tool.Invoke(context.Background(), json.RawMessage(`{"command":"env"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	for _, secret := range []string{"sk-must-not-appear", "bearer-must-not-appear"} {
		if strings.Contains(res.Content, secret) {
			t.Errorf("a command could read %q out of the environment:\n%s", secret, res.Content)
		}
	}
	// The scrub must not leave the command unable to find a binary.
	if !strings.Contains(res.Content, "PATH=") {
		t.Errorf("PATH did not survive the scrub:\n%s", res.Content)
	}
}

// AC-2. The agreement test.
//
// The failure this guards is not "someone adds GANGLION_API_KEY to the
// allowlist" -- nobody does that. It is that a NEW configuration variable gets
// added to internal/config, is set by crab-shell-proxy's ganglionEnv, happens
// to be named something innocuous, and is then quietly readable by the agent
// because nothing connects the two files.
//
// So this reads the config package's source and asserts that no variable it
// consumes is in passThrough. It is the only check available inside this
// repository that fails when the other side changes.
func TestNoConfiguredVariableIsPassedToCommands(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "..", "config", "config.go"))
	if err != nil {
		t.Fatalf("read the config package: %v (has it moved? this test must follow it)", err)
	}
	names := regexp.MustCompile(`"(GANGLION_[A-Z0-9_]+)"`).FindAllStringSubmatch(string(src), -1)
	if len(names) == 0 {
		t.Fatal("found no GANGLION_* variable in internal/config: the pattern no longer matches, so this test proves nothing")
	}
	for _, m := range names {
		if passThrough[m[1]] {
			t.Errorf("%s is read by internal/config AND passed to commands; the agent can read it", m[1])
		}
	}
}

// The allowlist is stated by name here as well as in the code, so widening it
// is a two-file change someone has to mean.
func TestAllowlistIsExactlyWhatIsIntended(t *testing.T) {
	want := map[string]bool{"PATH": true, "HOME": true, "TERM": true, "LANG": true, "TZ": true}
	for k := range passThrough {
		if !want[k] {
			t.Errorf("%s was added to passThrough: a command can now read it", k)
		}
	}
	for k := range want {
		if !passThrough[k] {
			t.Errorf("%s was removed from passThrough", k)
		}
	}
}

// Workdir alone confines nothing -- it is where a command starts, not a
// boundary. Pinned because it is the reason Self exists: an unsandboxed Tool
// is a Tool with no confinement at all, and the composition root must never
// build one.
func TestWorkdirAloneIsNotABoundary(t *testing.T) {
	unsandboxed := New(t.TempDir()) // Self deliberately empty
	res, err := unsandboxed.Invoke(context.Background(), json.RawMessage(`{"command":"cat /etc/hostname"}`))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if strings.Contains(res.Content, "denied") {
		t.Errorf("this test is no longer testing what it says: an unsandboxed tool was confined:\n%s", res.Content)
	}
}

// TestMain lets the test binary act as the sandbox helper, so the sandbox
// tests below exercise the REAL re-exec path -- the same argv, the same
// RunSandboxed, the same Landlock call the harness makes -- rather than a
// reimplementation of it that could agree with a bug.
func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == SandboxArg {
		if err := RunSandboxed(os.Args[2:]); err != nil {
			os.Stderr.WriteString("sandbox: " + err.Error() + "\n")
			os.Exit(126)
		}
		return
	}
	os.Exit(m.Run())
}

// requireLandlock skips loudly. A silent skip on a security test is how a
// confinement stops being tested without anyone noticing.
func requireLandlock(t *testing.T) int {
	t.Helper()
	abi, err := landlock.ABI()
	if err != nil {
		t.Skipf("SANDBOX NOT TESTED HERE: %v", err)
	}
	return abi
}

func sandboxed(t *testing.T, workdir string) *Tool {
	t.Helper()
	tool := New(workdir)
	tool.Self = os.Args[0]
	return tool
}

func run(t *testing.T, tool *Tool, command string) string {
	t.Helper()
	res, err := tool.Invoke(context.Background(), json.RawMessage(
		`{"command":`+mustJSON(t, command)+`}`))
	if err != nil {
		t.Fatalf("Invoke(%q): %v", command, err)
	}
	return res.Content
}

func mustJSON(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// THE FINDING THIS WHOLE SANDBOX EXISTS FOR.
//
// scrubEnv removes the provider key from the command's own environment, and
// that is not enough: PID 1 is the harness, it holds the key, and it runs as
// the same uid. `cat /proc/1/environ` reads it straight back. Confirmed by
// doing it in the real image before this was written.
func TestACommandCannotReadTheHarnessEnvironmentThroughProc(t *testing.T) {
	requireLandlock(t)
	tool := sandboxed(t, t.TempDir())

	out := run(t, tool, "cat /proc/1/environ; cat /proc/self/environ; ls /proc")
	if !strings.Contains(out, "denied") && !strings.Contains(out, "Permission") {
		t.Errorf("/proc was readable from inside the sandbox:\n%s", out)
	}
}

// The workspace must behave like an ordinary filesystem, or the sandbox is
// secure and useless.
func TestTheWorkspaceIsFullyUsable(t *testing.T) {
	requireLandlock(t)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	tool := sandboxed(t, dir)

	out := run(t, tool, `echo hello > a.txt && cat a.txt && mkdir sub && mv a.txt sub/b.txt && cat sub/b.txt && rm sub/b.txt && echo ALLDONE`)
	if !strings.Contains(out, "ALLDONE") {
		t.Errorf("ordinary file work failed inside the workspace:\n%s", out)
	}
}

// Found by running a shell under the ruleset: without /dev, `> /dev/null`
// fails and half of what an agent writes breaks in a way that looks like a
// broken tool.
func TestTheShellHasWhatItNeedsToFunction(t *testing.T) {
	requireLandlock(t)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	tool := sandboxed(t, dir)

	// busybox, so nothing GNU-only: `grep --version` exits 2 here, which is
	// how this test first failed -- on the tool, not on the sandbox.
	out := run(t, tool, `echo quiet > /dev/null && echo x | grep -q x && head -c 8 /dev/urandom > /dev/null; echo "exit=$?"`)
	if !strings.Contains(out, "exit=0") {
		t.Errorf("a shell could not use /dev/null or run a system binary:\n%s", out)
	}
}

// Escaping upward must fail, and the point is that it fails for a command
// written to evade a string check -- the mechanism is the kernel, not a
// blocklist.
func TestPathsOutsideTheAllowedSetAreDenied(t *testing.T) {
	requireLandlock(t)
	outside := filepath.Join(t.TempDir(), "secrets")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	secretPath := filepath.Join(outside, "provider.key")
	if err := os.WriteFile(secretPath, []byte("sk-outside-the-workspace"), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := sandboxed(t, t.TempDir())

	// Plain, and then obfuscated past any denylist that could be written.
	for _, cmd := range []string{
		"cat " + secretPath,
		`cat $(echo ` + base64Of(secretPath) + ` | base64 -d)`,
	} {
		out := run(t, tool, cmd)
		if strings.Contains(out, "sk-outside-the-workspace") {
			t.Errorf("%q read a file outside the sandbox:\n%s", cmd, out)
		}
	}
}

func base64Of(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// Landlock domains only narrow. The sandbox mode is reachable by argv from
// inside the sandbox itself, and that is safe BECAUSE of this property -- a
// second ruleset asking for "/" is intersected with the first, never
// substituted for it. Asserted rather than trusted.
func TestReinvokingTheSandboxCannotWidenIt(t *testing.T) {
	requireLandlock(t)
	outside := filepath.Join(t.TempDir(), "secrets")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	secretPath := filepath.Join(outside, "provider.key")
	if err := os.WriteFile(secretPath, []byte("sk-outside-the-workspace"), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := sandboxed(t, t.TempDir())
	out := run(t, tool, os.Args[0]+" "+SandboxArg+" / /bin/sh -c 'cat "+secretPath+"'")
	if strings.Contains(out, "sk-outside-the-workspace") {
		t.Errorf("re-invoking the sandbox helper with root=/ widened the domain:\n%s", out)
	}
}

// The construction, not the behaviour: a Tool with Self set must run through
// the helper. Without this, a refactor that dropped Self would leave every
// sandbox test above still passing -- they would just be testing an
// unsandboxed command that happens to have nothing to find.
func TestEveryCommandGoesThroughTheSandboxHelper(t *testing.T) {
	tool := sandboxed(t, "/work")
	cmd := tool.command(context.Background(), "/bin/sh", "echo hi")

	if len(cmd.Args) < 3 || cmd.Args[1] != SandboxArg {
		t.Fatalf("the command does not re-exec through the sandbox: %v", cmd.Args)
	}
	if cmd.Args[2] != "/work" {
		t.Errorf("the sandbox root is %q, want the workdir", cmd.Args[2])
	}
	for _, e := range cmd.Env {
		if strings.HasPrefix(e, "GANGLION_") {
			t.Errorf("a GANGLION_ variable survived into the sandboxed command: %q", e)
		}
	}
}

// AC-4 of ganglion-model-registry, and it is a NEW class of leak the existing
// tests do not cover.
//
// Until the model registry existed, everything secret reached the harness
// through the environment, and TestNoConfiguredVariableIsPassedToCommands plus
// the agreement test on `passThrough` covered all of it. The registry puts a
// FILE beside the harness carrying endpoints and, potentially, enc:// keys —
// and no test asserted that a file outside the workspace is unreachable *by
// the path the proxy actually mounts it at*.
//
// It also guards the design decision, not merely the config: the file lives
// outside the workspace so a tool steered by untrusted natural language cannot
// rewrite its own provider endpoint. Put it inside the workspace and this test
// is what fails.
func TestACommandCannotReadTheModelRegistryFile(t *testing.T) {
	requireLandlock(t)

	// The layout the proxy creates: <userDir>/.ganglion holds config.json and
	// credential.key, and only <userDir>/.ganglion/workspace is mounted.
	dataDir := t.TempDir()
	workspace := filepath.Join(dataDir, "workspace")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dataDir, "config.json")
	if err := os.WriteFile(configPath, []byte(
		`{"model_list":[{"model_name":"primary","api_keys":["sk-in-the-registry-file"]}]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := sandboxed(t, workspace)
	for _, cmd := range []string{
		"cat " + configPath,
		`cat $(echo ` + base64Of(configPath) + ` | base64 -d)`,
		"cat ../config.json",
		"ls " + dataDir,
	} {
		out := run(t, tool, cmd)
		if strings.Contains(out, "sk-in-the-registry-file") {
			t.Errorf("%q read the model registry file:\n%s", cmd, out)
		}
	}

	// And it cannot be REPLACED either, which is the half that would let a
	// command choose the endpoint its own keys are sent to.
	out := run(t, tool, "echo tampered > "+configPath+" ; cat "+configPath)
	if strings.Contains(out, "tampered") {
		t.Errorf("a command rewrote the model registry file:\n%s", out)
	}
}

// A project shell starts at the PROJECT ROOT, where uploads/, media/ and the
// rest are children -- exactly as the main workspace's shell starts where its
// own are. If it started one level deeper, the same instruction ("read
// uploads/report.csv") would mean two different paths depending on which
// project the member was in.
func TestAProjectShellStartsAtTheProjectRoot(t *testing.T) {
	ws := t.TempDir()
	tool := &Tool{Workdir: ws}

	got := tool.projectDir(domain.WithProject(context.Background(), "seed-trial"))
	want := filepath.Join(ws, domain.ProjectsDirName, "seed-trial")
	if got != want {
		t.Errorf("cwd = %q, want %q", got, want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Errorf("the directory was not created: %v", err)
	}
	// And a turn with no project starts exactly where it always has.
	if got := tool.projectDir(context.Background()); got != ws {
		t.Errorf("unscoped cwd = %q, want the workspace %q", got, ws)
	}
}
