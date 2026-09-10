package config

import (
	"strings"
	"testing"
)

// picoclaw's key, picoclaw's spelling, read verbatim -- including the leniency
// its own parser has (pkg/agent/thinking.go:16-54), because a config.json
// written for one harness has to load in the other.
func TestThinkingLevelIsReadFromPicoclawsOwnKey(t *testing.T) {
	reg := load(t, write(t, `{"model_list":[
	  {"model_name":"a","api_base":"https://a","api_keys":["k"],"thinking_level":"high"},
	  {"model_name":"b","api_base":"https://b","api_keys":["k"],"thinking_level":"  XHigh "},
	  {"model_name":"c","api_base":"https://c","api_keys":["k"]}
	]}`), nil)

	want := map[string]string{"a": "high", "b": "xhigh", "c": ""}
	for _, m := range reg.Models {
		if m.ThinkingLevel != want[m.Name] {
			t.Errorf("%s: thinking_level = %q, want %q", m.Name, m.ThinkingLevel, want[m.Name])
		}
	}
	if len(reg.Warnings) != 0 {
		t.Errorf("a clean file produced warnings: %v", reg.Warnings)
	}
}

// AC-9. An unparseable level is ABSENT and SAID OUT LOUD -- not silently read
// as "off".
//
// This is the discriminating case. picoclaw maps unknown to off, which makes a
// typo indistinguishable from a deliberate choice to think less: the model
// keeps answering, the operator keeps paying for a level they think they
// configured, and nothing anywhere says otherwise. Reading it as absent has the
// same wire effect and a completely different failure mode -- one that names
// itself in the log.
func TestAnUnparseableThinkingLevelIsAbsentAndWarnedAboutRatherThanReadAsOff(t *testing.T) {
	reg := load(t, write(t, `{"model_list":[
	  {"model_name":"a","api_base":"https://a","api_keys":["k"],"thinking_level":"HIGH!"}
	]}`), nil)

	if reg.Models[0].ThinkingLevel != "" {
		t.Fatalf("thinking_level = %q; an unreadable value must be absent, and must NOT become %q",
			reg.Models[0].ThinkingLevel, ThinkingOff)
	}
	if len(reg.Warnings) != 1 {
		t.Fatalf("warnings = %v, want exactly one", reg.Warnings)
	}
	w := reg.Warnings[0]
	for _, want := range []string{`"a"`, `"HIGH!"`, "off|low|medium|high|xhigh|adaptive"} {
		if !strings.Contains(w, want) {
			t.Errorf("the warning does not contain %s: %q", want, w)
		}
	}
}

func TestThinkingBodyIsReadPerLevel(t *testing.T) {
	reg := load(t, write(t, `{"model_list":[
	  {"model_name":"a","api_base":"https://a","api_keys":["k"],"thinking_level":"xhigh",
	   "thinking_body":{"xhigh":{"reasoning_effort":"max"}}}
	]}`), nil)

	got := reg.Models[0].ThinkingBody["xhigh"]["reasoning_effort"]
	if string(got) != `"max"` {
		t.Errorf("thinking_body.xhigh.reasoning_effort = %s", got)
	}
}

func TestParseThinkingLevelAcceptsExactlyTheSixValues(t *testing.T) {
	for _, l := range ThinkingLevels {
		if got, ok := ParseThinkingLevel(strings.ToUpper(l)); !ok || got != l {
			t.Errorf("ParseThinkingLevel(%q) = %q,%v", strings.ToUpper(l), got, ok)
		}
	}
	// A seventh value would break the admin editor that already renders this
	// field, which is why there is no seventh value.
	for _, bad := range []string{"", "none", "max", "minimal", "ultra"} {
		if _, ok := ParseThinkingLevel(bad); ok {
			t.Errorf("ParseThinkingLevel(%q) was accepted", bad)
		}
	}
}
