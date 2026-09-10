package exec

import (
	"fmt"
	"os"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/adapter/tool/exec/landlock"
)

// SandboxArg is the argv token that puts the harness binary into sandbox mode.
//
// The binary re-execs ITSELF rather than shipping a second helper: os/exec
// offers no hook to run code in the child between fork and exec, and Landlock
// has to be applied there -- applying it in the parent would confine the
// harness, which must keep writing transcripts outside the agent's reach.
//
// Exposing a mode by argv is safe here for a reason specific to Landlock:
// domains only ever narrow. An agent that runs
//
//	crab-ganglion __sandbox_exec / sh -c 'cat /proc/1/environ'
//
// gets a SECOND domain intersected with the one it is already inside, not a
// wider one. Tested by doing it.
const SandboxArg = "__sandbox_exec"

// SandboxRules is the confinement every command runs under.
//
// Read-only on the system directories, read-write on the workspace and on the
// two scratch areas a shell needs to function. Everything not named here is
// denied, which is the point -- most importantly /proc, whose /proc/1/environ
// holds the harness's own environment and therefore the provider key that
// scrubEnv removed from the command's.
func SandboxRules(workspace string, abi int) []landlock.Rule {
	full := landlock.FullAccess(abi)
	return []landlock.Rule{
		// The workspace: an ordinary read-write filesystem, so an agent can
		// create, edit, rename and delete its own files.
		{Path: workspace, AllowedAccess: full},

		// The system: readable and executable, never writable. A command can
		// run `grep`, and cannot leave anything behind in /usr for the next
		// turn to find.
		{Path: "/usr", AllowedAccess: landlock.ReadExecute},
		{Path: "/bin", AllowedAccess: landlock.ReadExecute},
		{Path: "/sbin", AllowedAccess: landlock.ReadExecute},
		{Path: "/lib", AllowedAccess: landlock.ReadExecute},
		{Path: "/etc", AllowedAccess: landlock.ReadExecute},

		// /dev/null, /dev/urandom, /dev/tty. Without this, `cmd > /dev/null`
		// fails -- found by running a shell under the ruleset, not by
		// reasoning about it.
		{Path: "/dev", AllowedAccess: full},

		// NOT /tmp.
		//
		// It was granted here first, and a test caught what that means: a
		// grant of /tmp is a grant of everything any other process left in
		// /tmp. The container's /tmp happens to be empty, so it looked safe --
		// which is exactly the reasoning that ages badly the first time
		// something writes a token there.
		//
		// Tools that want scratch space get it inside the workspace instead,
		// via TMPDIR (see Tool.command). Then "restrict to the workspace"
		// means what it says, with no second writable hierarchy to remember.
	}
}

// RunSandboxed is the child half of the re-exec. It applies the confinement
// and replaces itself with the command; it never returns on success.
//
// argv is [root, program, args...].
func RunSandboxed(argv []string) error {
	if len(argv) < 2 {
		return fmt.Errorf("%s needs a root and a program", SandboxArg)
	}
	root, prog, args := argv[0], argv[1], argv[2:]

	abi, err := landlock.ABI()
	if err != nil {
		// Fail CLOSED. There is no configuration that turns the sandbox off,
		// so a kernel that cannot provide one means no command runs -- rather
		// than a command running unconfined while the logs say "sandboxed".
		return err
	}
	if err := landlock.Restrict(SandboxRules(root, abi)); err != nil {
		return err
	}
	return os.NewSyscallError("execve", execve(prog, append([]string{prog}, args...), os.Environ()))
}
