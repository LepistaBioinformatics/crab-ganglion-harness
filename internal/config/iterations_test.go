package config

import (
	"strings"
	"testing"
)

// The turn's iteration cap, end to end from the environment.
//
// It is read here, carried to Loop.MaxIterations by cmd/crab-ganglion, and
// enforced by the loop -- TestRun_IterationCapIsReportedAndPartialWorkSurvives
// covers that last leg. What was missing was anything at all asserting the first
// one, which is how a variable nothing injects and nothing tests stayed
// unraisable for as long as it did.
func envForLoad(t *testing.T) {
	t.Helper()
	// Enough to get past the "no registry is mounted" check, which is the only
	// thing between Load and its return in a container with no config file.
	t.Setenv("GANGLION_BASE_URL", "https://example.invalid")
	t.Setenv("GANGLION_MODEL", "deepseek-chat")
	t.Setenv("GANGLION_CONFIG_FILE", "/nonexistent/config.json")
}

func TestTheIterationCapIsReadFromTheEnvironment(t *testing.T) {
	envForLoad(t)
	t.Setenv("GANGLION_MAX_ITERATIONS", "40")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.MaxTurnIter != 40 {
		t.Errorf("MaxTurnIter = %d, want the 40 that was set", c.MaxTurnIter)
	}
}

// Unset is ZERO, not twelve. The number lives in exactly one place --
// runtime.DefaultMaxIterations, which the loop applies to a zero field -- and a
// second copy here would be a default that can drift from the one in force.
func TestAnUnsetIterationCapLeavesTheDefaultToTheLoop(t *testing.T) {
	envForLoad(t)
	t.Setenv("GANGLION_MAX_ITERATIONS", "")

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.MaxTurnIter != 0 {
		t.Errorf("MaxTurnIter = %d, want 0 so the loop's own default applies", c.MaxTurnIter)
	}
}

// A value that cannot be honoured fails the BOOT, naming the variable. Silently
// falling back would reinstate the very default the operator was raising, and
// they would find out from a turn that stopped early with nothing naming the
// cause.
func TestAnUnusableIterationCapFailsTheBoot(t *testing.T) {
	for _, bad := range []string{"twelve", "0", "-3", "12.5"} {
		t.Run(bad, func(t *testing.T) {
			envForLoad(t)
			t.Setenv("GANGLION_MAX_ITERATIONS", bad)

			if _, err := Load(); err == nil {
				t.Fatalf("%q was accepted", bad)
			} else if !strings.Contains(err.Error(), "GANGLION_MAX_ITERATIONS") {
				t.Errorf("the error does not name the variable: %v", err)
			}
		})
	}
}
