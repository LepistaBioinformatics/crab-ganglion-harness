package main

import (
	"testing"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/adapter/store/tooloutput"
	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/runtime"
)

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

// THE ONE COUPLING BETWEEN THE WINDOW BUDGET AND WHAT DISK KEEPS, and the only
// place both constants are visible: internal/runtime may not import an adapter.
//
// A parked result's pointer is only ever read out of the window, so retaining
// more files than the window can hold messages means no live pointer can be
// pruned. Raise the budget past the retention and that stops being true --
// silently, and the failure arrives as an agent following a path into
// "no such file" several turns later, which reads as a bug in the shell.
//
// This is the guard. It is here rather than in a comment because the comment
// was already there and the scenario it describes is somebody raising a
// constant in a different package a year from now.
func TestTheRetentionOutlivesEveryPointerTheWindowCanHold(t *testing.T) {
	if tooloutput.Retain <= runtime.DefaultWindowBudget {
		t.Fatalf(
			"tooloutput.Retain is %d and the window holds up to %d messages: "+
				"a pointer still in the window can now be pruned from under the agent",
			tooloutput.Retain, runtime.DefaultWindowBudget)
	}
}
