package main

import "testing"

// "No option" means no code path either. If shellTool could produce a Tool
// with an empty Self, an unconfined command would run while every log line
// and every document said otherwise -- and the sandbox tests in the exec
// package would keep passing, because they build their own Tool.
func TestTheShellToolIsAlwaysSandboxed(t *testing.T) {
	tool := shellTool("/data/.ganglion/workspace", "/usr/local/bin/crab-ganglion")
	if tool.Self == "" {
		t.Fatal("shellTool produced a tool with no sandbox helper: commands would run unconfined")
	}
	if tool.Workdir != "/data/.ganglion/workspace" {
		t.Errorf("Workdir = %q", tool.Workdir)
	}
}
