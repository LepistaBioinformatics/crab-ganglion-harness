package exec

import "syscall"

func execve(prog string, argv, env []string) error { return syscall.Exec(prog, argv, env) }
