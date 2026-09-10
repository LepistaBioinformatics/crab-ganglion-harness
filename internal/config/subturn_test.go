package config

import (
	"testing"
	"time"
)

// The defaults, and the one that is deliberately NOT picoclaw's.
func TestSubturnDefaults(t *testing.T) {
	st := load(t, write(t, `{"model_list":[{"model_name":"a","api_base":"https://a","api_keys":["k"]}]}`), nil).Subturn

	if !st.Enabled {
		t.Error("sub-agents must be on by default, as tools.subagent.enabled is in picoclaw")
	}
	// picoclaw's default is 3. Ours is 1, so a child cannot itself dispatch and
	// the worst case stays a number somebody wrote down.
	if st.MaxDepth != 1 {
		t.Errorf("max_depth = %d, want 1", st.MaxDepth)
	}
	// picoclaw's default, kept -- the difference is that ours is enforced at
	// depth 0, where picoclaw's semaphore is nil.
	if st.MaxConcurrent != 5 {
		t.Errorf("max_concurrent = %d, want picoclaw's 5", st.MaxConcurrent)
	}
	if st.Timeout != 5*time.Minute {
		t.Errorf("timeout = %v, want 5m", st.Timeout)
	}
	if st.MaxChildIterations != 6 || st.MaxChildrenPerTurn != 16 {
		t.Errorf("child bounds = %d/%d, want 6/16", st.MaxChildIterations, st.MaxChildrenPerTurn)
	}
}

// A container that has never been recreated has no config file at all, and it
// must not get sub-agents switched off by a zero struct.
func TestAMissingFileStillCarriesTheSubturnDefaults(t *testing.T) {
	reg := load(t, write(t, `{}`), nil)
	if reg.Subturn.MaxConcurrent == 0 || reg.Subturn.MaxChildrenPerTurn == 0 {
		t.Fatalf("an empty file produced a zero Subturn: %+v", reg.Subturn)
	}
	if DefaultSubturn().MaxDepth != 1 {
		t.Error("DefaultSubturn drifted from the documented default")
	}
}

// picoclaw's key paths, spelled exactly, so one config.json serves both.
func TestSubturnIsReadFromPicoclawsKeyPaths(t *testing.T) {
	st := load(t, write(t, `{
	  "model_list":[{"model_name":"a","api_base":"https://a","api_keys":["k"]}],
	  "tools":{"subagent":{"enabled":false}},
	  "agents":{"defaults":{"subturn":{
	    "max_depth":3,"max_concurrent":2,"default_timeout_minutes":9,
	    "max_child_iterations":4,"max_children_per_turn":7}}}
	}`), nil).Subturn

	if st.Enabled {
		t.Error("tools.subagent.enabled false was not honoured")
	}
	if st.MaxDepth != 3 || st.MaxConcurrent != 2 || st.Timeout != 9*time.Minute {
		t.Errorf("subturn = %+v", st)
	}
	if st.MaxChildIterations != 4 || st.MaxChildrenPerTurn != 7 {
		t.Errorf("child bounds = %d/%d, want 4/7", st.MaxChildIterations, st.MaxChildrenPerTurn)
	}
}

// Every key is a pointer in the file struct, so a configured ZERO survives.
// "max_concurrent: 0" is an operator saying "run nothing"; reading it as "use
// the default of 5" would be the opposite instruction.
func TestAConfiguredZeroIsNotReadAsAbsent(t *testing.T) {
	st := load(t, write(t, `{
	  "model_list":[{"model_name":"a","api_base":"https://a","api_keys":["k"]}],
	  "agents":{"defaults":{"subturn":{"max_children_per_turn":0,"max_depth":0}}}
	}`), nil).Subturn

	if st.MaxChildrenPerTurn != 0 || st.MaxDepth != 0 {
		t.Fatalf("a configured zero was replaced by a default: %+v", st)
	}
	// The other keys keep their defaults; only what was written changed.
	if st.MaxConcurrent != 5 {
		t.Errorf("max_concurrent = %d, want the untouched default 5", st.MaxConcurrent)
	}
}
