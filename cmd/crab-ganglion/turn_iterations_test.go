package main

import (
	"testing"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/config"
	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/runtime"
)

// THE ORDER IS THE WHOLE OF IT: the config file wins, then the environment,
// then the loop's own default.
//
// File first is the opposite of how model keys resolve here, and deliberately:
// that rule is about SECRETS and a volume that gets backed up. A tuning number
// is neither, and the file is what the admin config screen edits — per agent
// and in bulk — while the variable is the blunt instrument for a deployment
// with no screen in front of it.
func TestTurnIterationsPrefersTheFileOverTheEnvironment(t *testing.T) {
	cases := []struct {
		name string
		file int
		env  int
		want int
		from string
	}{
		{"file wins", 40, 25, 40, "agents.defaults.max_tool_iterations"},
		{"environment when the file is silent", 0, 25, 25, "GANGLION_MAX_ITERATIONS"},
		{"the built-in default when both are", 0, 0, runtime.DefaultMaxIterations, "built-in default"},
		{"the file alone", 40, 0, 40, "agents.defaults.max_tool_iterations"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			reg := config.Registry{MaxToolIterations: c.file}
			cfg := config.Config{MaxTurnIter: c.env}

			if got := turnIterations(reg, cfg); got != c.want {
				t.Errorf("turnIterations = %d, want %d", got, c.want)
			}
			// The boot line names the source as well as the number: "12" looks
			// the same whether the operator set it, the file set it, or nothing
			// did, and that ambiguity is how a cap nothing was injecting went
			// unnoticed until somebody's turn stopped in the middle.
			if got := iterationsSource(reg, cfg); got != c.from {
				t.Errorf("iterationsSource = %q, want %q", got, c.from)
			}
		})
	}
}
