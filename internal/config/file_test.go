package config

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/secret"
)

// write puts a config file on disk and returns its path.
func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func load(t *testing.T, path string, env map[string]string) Registry {
	t.Helper()
	reg, err := LoadRegistry(path, secret.Resolver{}, func(k string) string { return env[k] })
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	return reg
}

// The floor the whole feature stands on: a container that has not been
// recreated has no file, and must behave exactly as it did before.
func TestAMissingFileIsNotAnError(t *testing.T) {
	reg, err := LoadRegistry(filepath.Join(t.TempDir(), "absent.json"), secret.Resolver{}, nil)
	if err != nil {
		t.Fatalf("a missing config file must not be an error, got %v", err)
	}
	if len(reg.Models) != 0 {
		t.Fatalf("expected an empty registry, got %d models", len(reg.Models))
	}
}

// A file that exists and does not parse is a mistake somebody wants to hear
// about -- the opposite of the case above, and the reason the two are separate.
func TestAMalformedFileIsAnError(t *testing.T) {
	if _, err := LoadRegistry(write(t, "{not json"), secret.Resolver{}, nil); err == nil {
		t.Fatal("expected an error from a malformed config file")
	}
}

// FR-1.2. This is a REAL picoclaw config.json shape: most of it is keys this
// harness has no types for, and every one of them must be ignored rather than
// rejected. If this test fails, a picoclaw agent's config can no longer be
// handed to a ganglion agent, which is the entire compatibility claim.
func TestAPicoclawConfigLoadsWithEveryUnknownKeyIgnored(t *testing.T) {
	reg := load(t, write(t, `{
      "version": 3,
      "gateway": {"log_level": "warn", "hot_reload": true},
      "channel_list": {"telegram": {"enabled": true, "type": "telegram"}},
      "heartbeat": {"enabled": true, "interval": 30},
      "evolution": {"enabled": false, "mode": "observe"},
      "tools": {"cron": {"enabled": true}, "web": {"brave": {"enabled": true}}},
      "model_list": [
        {"model_name": "primary", "provider": "deepseek", "model": "deepseek-chat",
         "api_base": "https://api.deepseek.com/v1", "api_keys": ["[NOT_HERE]"],
         "fallbacks": ["backup"], "rpm": 60, "thinking_level": "high"},
        {"model_name": "backup", "provider": "openai", "model": "gpt-5.4",
         "api_base": "https://api.openai.com/v1"}
      ],
      "agents": {
        "defaults": {"model_name": "primary", "model_fallbacks": ["backup"],
                     "workspace": "~/.picoclaw/workspace", "max_tokens": 8192},
        "list": [{"id": "research", "model": "primary"}],
        "dispatch": {"rules": []}
      }
    }`), nil)

	if len(reg.Models) != 2 {
		t.Fatalf("expected 2 models, got %d", len(reg.Models))
	}
	if reg.Default != "primary" {
		t.Errorf("default = %q, want primary", reg.Default)
	}
	m, _ := reg.Find("primary")
	if m.Provider != "deepseek" || m.Model != "deepseek-chat" || m.APIBase != "https://api.deepseek.com/v1" {
		t.Errorf("primary decoded wrong: %+v", m)
	}
}

// picoclaw's own save path writes this sentinel where a key used to be, having
// moved the real one into .security.yml. Sending it as a bearer token produces
// an auth error that names nothing useful, so it must read as "no key".
func TestThePicoclawNotHereSentinelIsNotAKey(t *testing.T) {
	reg := load(t, write(t, `{"model_list":[
      {"model_name":"m","model":"x","api_base":"https://e/v1","api_keys":["[NOT_HERE]"]}]}`), nil)
	if got, _ := reg.Find("m"); got.APIKey != "" {
		t.Fatalf("APIKey = %q, want empty -- the sentinel was read as a key", got.APIKey)
	}
}

// picoclaw omits `enabled` on entries it considers enabled by inference. A
// plain bool would decode that absence as false and silently empty the
// registry, which is why the field is a pointer.
func TestAnAbsentEnabledMeansEnabled(t *testing.T) {
	reg := load(t, write(t, `{"model_list":[
      {"model_name":"on","model":"x","api_base":"https://e/v1"},
      {"model_name":"off","model":"y","api_base":"https://e/v1","enabled":false}]}`), nil)
	if m, _ := reg.Find("on"); !m.Enabled {
		t.Error("an entry with no `enabled` key must be enabled")
	}
	if m, _ := reg.Find("off"); m.Enabled {
		t.Error("`enabled: false` must disable")
	}
	if got := reg.Chain(""); !reflect.DeepEqual(got, []string{"on"}) {
		t.Errorf("chain = %v, want [on] -- a disabled model must not be a candidate", got)
	}
}

// FR-4: the environment wins, because that is the path the proxy uses and
// because a key that never enters a file cannot leak through the file.
func TestTheEnvironmentKeyBeatsTheInlineOne(t *testing.T) {
	reg := load(t, write(t, `{"model_list":[
      {"model_name":"primary","model":"x","api_base":"https://e/v1","api_keys":["from-the-file"]}]}`),
		map[string]string{"GANGLION_MODEL_KEY_PRIMARY": "from-the-environment"})
	m, _ := reg.Find("primary")
	if m.APIKey != "from-the-environment" {
		t.Fatalf("APIKey = %q, want the environment's value", m.APIKey)
	}
}

func TestAnInlineKeyIsUsedWhenTheEnvironmentHasNone(t *testing.T) {
	reg := load(t, write(t, `{"model_list":[
      {"model_name":"primary","model":"x","api_base":"https://e/v1","api_keys":["from-the-file"]}]}`), nil)
	if m, _ := reg.Find("primary"); m.APIKey != "from-the-file" {
		t.Fatalf("APIKey = %q, want from-the-file", m.APIKey)
	}
}

// The proxy computes this name from the same model_name when it sets the
// variable. The two derivations agreeing IS the contract, so it is pinned.
func TestTheKeyVariableNameIsStable(t *testing.T) {
	for in, want := range map[string]string{
		"primary":         "GANGLION_MODEL_KEY_PRIMARY",
		"gpt-5.4":         "GANGLION_MODEL_KEY_GPT_5_4",
		"own-my model":    "GANGLION_MODEL_KEY_OWN_MY_MODEL",
		"deepseek-chat":   "GANGLION_MODEL_KEY_DEEPSEEK_CHAT",
		"Claude Sonnet 4": "GANGLION_MODEL_KEY_CLAUDE_SONNET_4",
	} {
		if got := KeyEnvVar(in); got != want {
			t.Errorf("KeyEnvVar(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTheChainIsDefaultThenItsFallbacks(t *testing.T) {
	reg := load(t, write(t, `{
      "model_list":[
        {"model_name":"a","model":"x","api_base":"https://e/v1","fallbacks":["c"]},
        {"model_name":"b","model":"x","api_base":"https://e/v1"},
        {"model_name":"c","model":"x","api_base":"https://e/v1"}],
      "agents":{"defaults":{"model_name":"a","model_fallbacks":["b"]}}}`), nil)
	if got := reg.Chain(""); !reflect.DeepEqual(got, []string{"a", "b", "c"}) {
		t.Fatalf("chain = %v, want [a b c]", got)
	}
}

// FR-3. The proxy fills Turn.Model with a placeholder on every turn, so an
// unrecognised name is the ORDINARY case and must resolve to the default chain
// rather than to an error or an empty chain.
func TestAnUnknownTurnModelFallsBackToTheDefaultChain(t *testing.T) {
	reg := load(t, write(t, `{
      "model_list":[{"model_name":"a","model":"x","api_base":"https://e/v1"}],
      "agents":{"defaults":{"model_name":"a"}}}`), nil)
	if got := reg.Chain("picoclaw"); !reflect.DeepEqual(got, []string{"a"}) {
		t.Fatalf("chain = %v, want [a] for the proxy's placeholder", got)
	}
}

func TestAKnownTurnModelIsHonoured(t *testing.T) {
	reg := load(t, write(t, `{
      "model_list":[
        {"model_name":"a","model":"x","api_base":"https://e/v1"},
        {"model_name":"b","model":"y","api_base":"https://e/v1"}],
      "agents":{"defaults":{"model_name":"a"}}}`), nil)
	if got := reg.Chain("b"); !reflect.DeepEqual(got, []string{"b"}) {
		t.Fatalf("chain = %v, want [b]", got)
	}
}

// A fallback cycle is a configuration mistake, not a reason to hang a turn.
func TestAFallbackCycleTerminates(t *testing.T) {
	reg := load(t, write(t, `{
      "model_list":[
        {"model_name":"a","model":"x","api_base":"https://e/v1","fallbacks":["b"]},
        {"model_name":"b","model":"x","api_base":"https://e/v1","fallbacks":["a"]}],
      "agents":{"defaults":{"model_name":"a"}}}`), nil)
	if got := reg.Chain(""); !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatalf("chain = %v, want [a b] with no repetition", got)
	}
}

// A minimal hand-written file should not have to say the same name twice.
func TestAListWithNoDeclaredDefaultUsesTheFirstEntry(t *testing.T) {
	reg := load(t, write(t, `{"model_list":[
      {"model_name":"only","model":"x","api_base":"https://e/v1"}]}`), nil)
	if reg.Default != "only" {
		t.Fatalf("default = %q, want only", reg.Default)
	}
}
