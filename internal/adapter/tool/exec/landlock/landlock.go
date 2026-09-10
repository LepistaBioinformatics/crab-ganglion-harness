// Package landlock confines a process to a set of filesystem paths using the
// Landlock LSM.
//
// # WHY A KERNEL LSM AND NOT A PATH CHECK
//
// picoclaw's restrict_to_workspace is tool-layer path validation -- its own
// documentation says the sandbox "relies entirely on application-level
// validation" with no kernel-level isolation, and its exec tool is guarded by
// a 41-pattern regex denylist on top. That works for tools that take a path
// ARGUMENT to resolve and refuse. This harness's only tool is
// `/bin/sh -c <arbitrary string>`: there is no path to check, and any denylist
// is defeated by `$(echo L2V0Yw== | base64 -d)`.
//
// So the confinement has to be enforced by something that sees the syscall
// rather than the string. Landlock is the one such mechanism an UNPRIVILEGED
// process can apply to itself -- no CAP_SYS_ADMIN, no privileged container, no
// seccomp profile change. The harness runs as uid 1000 and could not chroot or
// unshare if it wanted to.
//
// # WHAT THIS CLOSES THAT SCRUBBING THE ENVIRONMENT DID NOT
//
// exec's scrubEnv keeps the provider key out of a command's own environment.
// That is necessary and it is not sufficient: PID 1 is the harness, it holds
// the key, and a command running as the same uid reads it straight back with
//
//	cat /proc/1/environ
//
// Verified, not reasoned about. Denying /proc is why this package exists; the
// scrub alone left the credential one file away.
//
// # ZERO DEPENDENCIES
//
// Raw syscalls rather than golang.org/x/sys or landlock-lsm/go-landlock. The
// three syscall numbers are in the architecture-independent range (444-446),
// and the struct layouts are ABI-versioned by the kernel itself.
package landlock

import (
	"fmt"
	"syscall"
	"unsafe"
)

// Syscall numbers. Allocated in the generic range, so they are the same on
// amd64 and arm64 -- the two platforms this image is ever built for.
const (
	sysCreateRuleset = 444
	sysAddRule       = 445
	sysRestrictSelf  = 446

	createRulesetVersion = 1 << 0
	ruleTypePathBeneath  = 1

	prSetNoNewPrivs = 38
	oPath           = 0x200000
)

// Filesystem access rights.
const (
	AccessExecute    uint64 = 1 << 0
	AccessWriteFile  uint64 = 1 << 1
	AccessReadFile   uint64 = 1 << 2
	AccessReadDir    uint64 = 1 << 3
	AccessRemoveDir  uint64 = 1 << 4
	AccessRemoveFile uint64 = 1 << 5
	AccessMakeChar   uint64 = 1 << 6
	AccessMakeDir    uint64 = 1 << 7
	AccessMakeReg    uint64 = 1 << 8
	AccessMakeSock   uint64 = 1 << 9
	AccessMakeFifo   uint64 = 1 << 10
	AccessMakeBlock  uint64 = 1 << 11
	AccessMakeSym    uint64 = 1 << 12
	AccessRefer      uint64 = 1 << 13 // ABI 2
	AccessTruncate   uint64 = 1 << 14 // ABI 3
	AccessIoctlDev   uint64 = 1 << 15 // ABI 5
)

// ReadExecute is what a command needs of the system directories: run the
// binaries, read the libraries and the config files they open. No write of any
// kind, so nothing a command does to /usr or /etc survives its own turn.
const ReadExecute = AccessExecute | AccessReadFile | AccessReadDir

type rulesetAttr struct {
	HandledAccessFS  uint64
	HandledAccessNet uint64 // ABI 4
	Scoped           uint64 // ABI 6
}

type pathBeneathAttr struct {
	AllowedAccess uint64
	ParentFd      int32
	_             [4]byte
}

// Rule grants AllowedAccess beneath Path.
type Rule struct {
	Path          string
	AllowedAccess uint64
}

// ABI reports the kernel's Landlock ABI version. A version below 1 means the
// LSM is absent or switched off, and is returned as an error rather than a
// zero the caller might treat as success.
func ABI() (int, error) {
	v, _, errno := syscall.Syscall(sysCreateRuleset, 0, 0, createRulesetVersion)
	if errno != 0 {
		return 0, fmt.Errorf("landlock unavailable: %w "+
			"(needs Linux 5.13+ with the LSM enabled; on a container host also check the seccomp profile)", errno)
	}
	if int(v) < 1 {
		return 0, fmt.Errorf("landlock reports ABI version %d", int(v))
	}
	return int(v), nil
}

// handledFor is the set of rights the ruleset governs, masked to what this
// kernel knows.
//
// Negotiated, never assumed: asking for a bit the kernel does not recognise
// fails the whole create with EINVAL, which would read as "landlock is
// missing" on a kernel that has it.
func handledFor(abi int) uint64 {
	h := AccessExecute | AccessWriteFile | AccessReadFile | AccessReadDir |
		AccessRemoveDir | AccessRemoveFile | AccessMakeChar | AccessMakeDir |
		AccessMakeReg | AccessMakeSock | AccessMakeFifo | AccessMakeBlock |
		AccessMakeSym
	if abi >= 2 {
		h |= AccessRefer
	}
	if abi >= 3 {
		h |= AccessTruncate
	}
	if abi >= 5 {
		h |= AccessIoctlDev
	}
	return h
}

// attrSizeFor is the struct size this ABI expects. The kernel validates it, so
// sending the wrong one is EINVAL rather than silent misreading.
func attrSizeFor(abi int) uintptr {
	switch {
	case abi >= 6:
		return 24
	case abi >= 4:
		return 16
	default:
		return 8
	}
}

// FullAccess is every right a hierarchy can be granted on this kernel: what
// the workspace gets, so the agent's own directory behaves like an ordinary
// filesystem.
func FullAccess(abi int) uint64 { return handledFor(abi) }

// Restrict applies rules to the CALLING THREAD and everything it later
// forks or execs. It cannot be undone.
//
// Landlock domains only ever NARROW. A process already inside a domain that
// calls this again is intersected with the existing one, never widened --
// which is what makes the sandbox helper safe to expose as an argv mode: an
// agent re-invoking it with the root directory still cannot reach anything the
// first domain denied. Verified by running exactly that.
func Restrict(rules []Rule) error {
	abi, err := ABI()
	if err != nil {
		return err
	}
	attr := rulesetAttr{HandledAccessFS: handledFor(abi)}
	fd, _, errno := syscall.Syscall(sysCreateRuleset,
		uintptr(unsafe.Pointer(&attr)), attrSizeFor(abi), 0)
	if errno != 0 {
		return fmt.Errorf("landlock create_ruleset: %w", errno)
	}
	defer syscall.Close(int(fd))

	for _, r := range rules {
		if err := addRule(int(fd), r, abi); err != nil {
			return err
		}
	}

	// Landlock refuses to restrict a thread that could still gain privileges.
	if _, _, errno := syscall.Syscall(syscall.SYS_PRCTL, prSetNoNewPrivs, 1, 0); errno != 0 {
		return fmt.Errorf("landlock set_no_new_privs: %w", errno)
	}
	if _, _, errno := syscall.Syscall(sysRestrictSelf, fd, 0, 0); errno != 0 {
		return fmt.Errorf("landlock restrict_self: %w", errno)
	}
	return nil
}

// addRule grants one hierarchy.
//
// A path that does not exist is SKIPPED rather than fatal: the rule set names
// directories that differ between a scratch image and the deployed one
// (/lib64 on one, not the other), and failing the whole sandbox because /tmp
// is absent would trade a working confinement for none at all. A path that
// exists but cannot be opened IS fatal -- that is a permissions problem, not
// an absent directory, and silently continuing would grant less than intended
// without saying so.
func addRule(fd int, r Rule, abi int) error {
	pf, err := syscall.Open(r.Path, oPath|syscall.O_CLOEXEC, 0)
	if err != nil {
		if err == syscall.ENOENT {
			return nil
		}
		return fmt.Errorf("landlock open %s: %w", r.Path, err)
	}
	defer syscall.Close(pf)

	pb := pathBeneathAttr{AllowedAccess: r.AllowedAccess & handledFor(abi), ParentFd: int32(pf)}
	if _, _, errno := syscall.Syscall6(sysAddRule, uintptr(fd), ruleTypePathBeneath,
		uintptr(unsafe.Pointer(&pb)), 0, 0, 0); errno != 0 {
		return fmt.Errorf("landlock add_rule %s: %w", r.Path, errno)
	}
	return nil
}
