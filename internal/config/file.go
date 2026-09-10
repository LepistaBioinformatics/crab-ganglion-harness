package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/secret"
)

// The structural half of the configuration, read from a JSON file rather than
// from the environment.
//
// WHY A FILE, WHEN THE PACKAGE DOC SAYS ENVIRONMENT
//
// The environment carries scalars well and lists badly. A model registry is a
// list of records, each with its own endpoint and its own key, and packing that
// into one variable would mean inventing an encoding and then debugging it in
// `docker inspect` output. picoclaw solved this years ago with config.json, and
// solving it the same way is what lets ONE admin screen manage both harnesses --
// which is the whole requirement, not a nicety.
//
// WHY IT IS SAFE TO PUT IT IN A FILE
//
// Two reasons, and both have to hold:
//
//  1. It lives OUTSIDE the workspace bind, beside credential.key, so the
//     Landlock domain the shell tool runs under cannot reach it. If it were
//     inside the workspace, a tool steered by untrusted natural language could
//     rewrite its own provider endpoint -- exfiltration with a config edit.
//  2. Keys in it are enc:// values, useless without the passphrase (environment)
//     and the key file (a different read-only bind).
//
// WHY IT IS OPTIONAL
//
// GANGLION_MODEL + GANGLION_BASE_URL + GANGLION_API_KEY is what every deployed
// container uses today. Absence of this file therefore cannot be an error: it
// synthesizes a one-entry list named `default` and the harness behaves exactly
// as it did before. That is not politeness, it is the only thing that makes this
// change safe to ship to a running fleet.

// DefaultConfigFile is where crab-shell-proxy binds the config file: beside
// credential.key, and for the same reason.
const DefaultConfigFile = "/data/.ganglion/config.json"

// DefaultModelName is the name given to the model synthesized from the three
// environment variables when no file declares one.
const DefaultModelName = "default"

// notHere is the sentinel picoclaw's SecureString.MarshalJSON writes in place of
// a key it has moved into .security.yml. A file written by picoclaw's own save
// path carries it, and reading it as a key would send the literal string as a
// bearer token -- an auth error that names nothing useful.
const notHere = "[NOT_HERE]"

// ModelSpec is one entry of model_list. Field names and JSON keys are
// picoclaw's (pkg/config/config.go:760-798), so a config.json written for one
// harness loads in the other.
type ModelSpec struct {
	// Name is `model_name`: the alias the rest of the configuration refers to.
	// It is NOT what goes on the wire.
	Name string
	// Provider routes to a protocol adapter. Recorded and reported; v1 has one
	// adapter (OpenAI-compatible HTTP) and every provider goes through it,
	// which is what picoclaw itself does for 30 of its 42.
	Provider string
	// Model is the identifier sent to the endpoint.
	Model string
	// APIBase is the endpoint. Required: there is no registry mapping a
	// provider name to a URL.
	APIBase string
	// APIKey is resolved plaintext by the time anyone outside this package
	// sees it.
	APIKey string
	// ExtraBody is merged into the request body at the top level, which is how
	// picoclaw carries provider quirks such as minimax's reasoning_split.
	ExtraBody map[string]json.RawMessage
	// Headers are sent verbatim. Authorization is never overridden from here.
	Headers map[string]string
	// TimeoutSec bounds one provider call. Zero means the client's default.
	TimeoutSec int
	// Enabled false removes the entry from every chain without deleting it.
	Enabled bool
	// Fallbacks names other entries to try after this one.
	Fallbacks []string
}

// file is the on-disk shape. Every field is a pointer or a slice so that
// "absent" and "present but empty" stay distinguishable, and every key picoclaw
// writes that ganglion has no use for -- channel_list, cron, heartbeat,
// dispatch, most of tools -- is simply not declared here and is therefore
// ignored. Lenient decoding is a requirement (FR-1.2), not an accident: this
// file is expected to be a picoclaw config.json verbatim.
type file struct {
	ModelList []struct {
		ModelName string `json:"model_name"`
		Provider  string `json:"provider"`
		Model     string `json:"model"`
		APIBase   string `json:"api_base"`
		// Enabled is a pointer because picoclaw omits it on entries it
		// considers enabled by inference. Absent must read as true, and a
		// plain bool would read it as false and silently empty the registry.
		Enabled        *bool                      `json:"enabled"`
		APIKeys        []string                   `json:"api_keys"`
		Fallbacks      []string                   `json:"fallbacks"`
		ExtraBody      map[string]json.RawMessage `json:"extra_body"`
		CustomHeaders  map[string]string          `json:"custom_headers"`
		RequestTimeout int                        `json:"request_timeout"`
	} `json:"model_list"`

	Agents struct {
		Defaults struct {
			ModelName      string   `json:"model_name"`
			ModelFallbacks []string `json:"model_fallbacks"`
		} `json:"defaults"`
	} `json:"agents"`
}

// Registry is the resolved model configuration: an ordered list plus the
// default chain. It is what the composition root turns into providers.
type Registry struct {
	Models []ModelSpec
	// Default names the entry a turn runs on when it asks for nothing.
	Default string
	// DefaultFallbacks follow Default, before that entry's own Fallbacks.
	DefaultFallbacks []string
}

// Find returns the spec for a model_name.
func (r Registry) Find(name string) (ModelSpec, bool) {
	for _, m := range r.Models {
		if m.Name == name {
			return m, true
		}
	}
	return ModelSpec{}, false
}

// Chain resolves the ordered candidates for one turn.
//
// A turn naming a model IN the registry runs on it (and on its own fallbacks
// after it). A turn naming anything else -- including nothing -- runs the
// default chain.
//
// The unknown case is deliberately SILENT rather than reported to the member.
// crab-shell-proxy fills Turn.Model with the harness name as a placeholder
// ("picoclaw", historically), so "the turn named a model I do not have" is the
// ordinary case on every single turn, not an anomaly worth a frame.
func (r Registry) Chain(turnModel string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(names ...string) {
		for _, n := range names {
			if n == "" || seen[n] {
				continue
			}
			m, ok := r.Find(n)
			if !ok || !m.Enabled {
				continue
			}
			seen[n] = true
			out = append(out, n)
		}
	}

	if _, ok := r.Find(turnModel); ok {
		add(turnModel)
	} else {
		add(r.Default)
		add(r.DefaultFallbacks...)
	}
	// Each entry's own fallbacks extend the chain, one level, the way
	// picoclaw's resolveModelCandidates does. Bounded by the loop over a
	// snapshot rather than by recursion, so a cycle cannot hang a turn.
	for _, n := range append([]string(nil), out...) {
		if m, ok := r.Find(n); ok {
			add(m.Fallbacks...)
		}
	}
	return out
}

// LoadRegistry reads path and resolves every key it finds.
//
// A missing file yields an empty Registry and no error: the caller synthesizes
// the environment-configured entry instead (FR-1.1). A malformed one is an
// error, because a file that exists and does not parse is a mistake somebody
// wants to hear about.
func LoadRegistry(path string, res secret.Resolver, keyEnv func(string) string) (Registry, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Registry{}, nil
	}
	if err != nil {
		return Registry{}, fmt.Errorf("read %s: %w", path, err)
	}
	var f file
	if err := json.Unmarshal(b, &f); err != nil {
		return Registry{}, fmt.Errorf("parse %s: %w", path, err)
	}

	reg := Registry{
		Default:          f.Agents.Defaults.ModelName,
		DefaultFallbacks: f.Agents.Defaults.ModelFallbacks,
	}
	for _, e := range f.ModelList {
		if e.ModelName == "" {
			continue
		}
		spec := ModelSpec{
			Name:       e.ModelName,
			Provider:   e.Provider,
			Model:      e.Model,
			APIBase:    e.APIBase,
			ExtraBody:  e.ExtraBody,
			Headers:    e.CustomHeaders,
			TimeoutSec: e.RequestTimeout,
			Enabled:    e.Enabled == nil || *e.Enabled,
			Fallbacks:  e.Fallbacks,
		}
		// The model actually sent on the wire defaults to the alias, which is
		// how picoclaw's own examples read when the two are the same string.
		if spec.Model == "" {
			spec.Model = e.ModelName
		}
		key, err := resolveKey(e.APIKeys, spec.Name, res, keyEnv)
		if err != nil {
			return Registry{}, fmt.Errorf("model %q: %w", spec.Name, err)
		}
		spec.APIKey = key
		reg.Models = append(reg.Models, spec)
	}
	// A file with a list but no declared default names the first entry, so a
	// hand-written minimal config works without repeating itself.
	if reg.Default == "" && len(reg.Models) > 0 {
		reg.Default = reg.Models[0].Name
	}
	return reg, nil
}

// resolveKey applies FR-4's order: the environment first, the file second.
//
// The environment wins because that is the path the proxy uses, and because a
// key that never enters a file cannot be read by anything that gets pointed at
// the file by mistake.
func resolveKey(inline []string, name string, res secret.Resolver, keyEnv func(string) string) (string, error) {
	if keyEnv != nil {
		if v := keyEnv(KeyEnvVar(name)); v != "" {
			return res.Resolve(v)
		}
	}
	for _, k := range inline {
		if k == "" || k == notHere {
			continue
		}
		return res.Resolve(k)
	}
	return "", nil
}

// KeyEnvVar is the environment variable carrying one model's key.
//
// The mapping has to be stable and total: crab-shell-proxy computes the same
// name from the same model_name when it sets the variable, and the two
// derivations agreeing is the entire contract. Anything outside [A-Z0-9]
// becomes an underscore, so `gpt-5.4` and `gpt_5_4` collide -- accepted,
// because the alternative is an encoding nobody can read in `docker inspect`.
func KeyEnvVar(modelName string) string {
	var b strings.Builder
	b.WriteString("GANGLION_MODEL_KEY_")
	for _, r := range strings.ToUpper(modelName) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}
