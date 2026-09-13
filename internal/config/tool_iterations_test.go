package config

import "testing"

// agents.defaults.max_tool_iterations is picoclaw's own key, and this harness
// reads it from the same place under the same name. An admin editing the config
// screen should not have to know which harness is behind the agent to know which
// key means "how long may this think".
func TestMaxToolIterationsIsReadFromTheFile(t *testing.T) {
	reg := load(t, write(t, `{
		"model_list": [{"model_name":"a","api_base":"https://a","api_keys":["k"]}],
		"agents": {"defaults": {"max_tool_iterations": 40}}
	}`), nil)

	if reg.MaxToolIterations != 40 {
		t.Errorf("max_tool_iterations = %d, want 40", reg.MaxToolIterations)
	}
}

// Absent is NOT zero. 0 is what the wiring reads as "the file said nothing",
// and it is the only thing that lets the environment and the built-in default
// still have a turn — a file that silently meant "no iterations at all" would
// stop every agent in the deployment.
func TestAnAbsentMaxToolIterationsLeavesTheChoiceToTheWiring(t *testing.T) {
	reg := load(t, write(t, `{"model_list":[{"model_name":"a","api_base":"https://a","api_keys":["k"]}]}`), nil)

	if reg.MaxToolIterations != 0 {
		t.Errorf("an absent key gave %d, want 0", reg.MaxToolIterations)
	}
}

// A number nobody can run a turn with is treated as absent rather than obeyed.
// Zero and negatives reach this file through a config screen, not through a
// compiler, and "the admin typed 0" must not take the agent off the air.
func TestAnUnusableMaxToolIterationsIsIgnored(t *testing.T) {
	for _, raw := range []string{"0", "-1"} {
		reg := load(t, write(t, `{
			"model_list": [{"model_name":"a","api_base":"https://a","api_keys":["k"]}],
			"agents": {"defaults": {"max_tool_iterations": `+raw+`}}
		}`), nil)

		if reg.MaxToolIterations != 0 {
			t.Errorf("max_tool_iterations %s gave %d, want it ignored", raw, reg.MaxToolIterations)
		}
	}
}
