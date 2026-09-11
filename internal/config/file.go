package config

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

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
	// ThinkingLevel is picoclaw's `thinking_level`: off|low|medium|high|xhigh|
	// adaptive. Empty means this model is NEVER sent a depth field, by any
	// path -- declaring the key IS the capability declaration, because the
	// harness cannot discover whether an endpoint accepts one and the operator
	// can.
	ThinkingLevel string
	// ThinkingBody overrides the default wire mapping per level, keyed by
	// level name. It is the one mechanism by which a provider dialect the
	// adapter does not know is taught to it without a code change:
	//
	//	"thinking_body": {"xhigh": {"reasoning_effort": "max"}}
	//
	// A level present here fully replaces the default row for that level,
	// including replacing it with {} to emit nothing.
	ThinkingBody map[string]map[string]json.RawMessage
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
		// ThinkingLevel is picoclaw's own key, same name and same six values.
		ThinkingLevel string `json:"thinking_level"`
		// ThinkingBody has no picoclaw equivalent. It is named to sit beside
		// thinking_level so a picoclaw that grows per-level bodies has an
		// obvious place to land, and picoclaw ignores it today.
		ThinkingBody map[string]map[string]json.RawMessage `json:"thinking_body"`
	} `json:"model_list"`

	Agents struct {
		Defaults struct {
			// Subturn is picoclaw's own agents.defaults.subturn block
			// (pkg/config/config.go:408-414). The first three keys are
			// picoclaw's, spelled the same; the last two have no picoclaw
			// equivalent and are named to sit beside them.
			Subturn *struct {
				MaxDepth              *int `json:"max_depth"`
				MaxConcurrent         *int `json:"max_concurrent"`
				DefaultTimeoutMinutes *int `json:"default_timeout_minutes"`
				MaxChildIterations    *int `json:"max_child_iterations"`
				MaxChildrenPerTurn    *int `json:"max_children_per_turn"`
			} `json:"subturn"`
			ModelName      string   `json:"model_name"`
			ModelFallbacks []string `json:"model_fallbacks"`
			// image_model and image_model_fallbacks are picoclaw's own keys
			// (pkg/config/config.go:430-431) for the model that READS images.
			ImageModel          string   `json:"image_model"`
			ImageModelFallbacks []string `json:"image_model_fallbacks"`
			// image_gen_model has no picoclaw equivalent -- picoclaw cannot
			// generate images at all -- and is named to sit beside the pair
			// above so a future picoclaw that grows the feature has an obvious
			// place to land.
			ImageGenModel          string   `json:"image_gen_model"`
			ImageGenModelFallbacks []string `json:"image_gen_model_fallbacks"`
		} `json:"defaults"`
	} `json:"agents"`

	// Evolution is picoclaw's own block, keys unchanged
	// (pkg/config/config.go:58-70), so a picoclaw config.json dropped in
	// behaves the same way here.
	Evolution *struct {
		Enabled         *bool    `json:"enabled"`
		Mode            string   `json:"mode"`
		StateDir        string   `json:"state_dir"`
		MinTaskCount    *int     `json:"min_task_count"`
		MinSuccessRatio *float64 `json:"min_success_ratio"`
		ColdPathTrigger string   `json:"cold_path_trigger"`
		ColdPathTimes   []string `json:"cold_path_times"`
	} `json:"evolution"`

	Tools struct {
		// Web is decoded twice: once for the settings that are the same for
		// every provider, and once as a raw map so a provider block can be
		// read by name without this struct having to enumerate them. Adding a
		// provider is then a file in the websearch adapter, not an edit here.
		Web json.RawMessage `json:"web"`
		// Subagent is picoclaw's own tools.subagent block. Only `enabled` is
		// read: picoclaw's tools.spawn and tools.spawn_status govern two tools
		// this harness deliberately does not have.
		Subagent *struct {
			Enabled *bool `json:"enabled"`
		} `json:"subagent"`
		// MCP is picoclaw's tools.mcp block, read verbatim so the proxy's
		// existing per-workspace writer serves both harnesses with one record.
		// `command` is present in that shape and is always empty for an HTTP
		// server; see loadMCP for why a non-empty one is refused rather than
		// ignored.
		MCP *struct {
			Enabled *bool                `json:"enabled"`
			Servers map[string]mcpServer `json:"servers"`
		} `json:"mcp"`
	} `json:"tools"`
}

// mcpServer is one entry of tools.mcp.servers, in picoclaw's own shape.
type mcpServer struct {
	Enabled *bool             `json:"enabled"`
	Command string            `json:"command"`
	Type    string            `json:"type"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
}

// webBlock is the shape shared by every provider under tools.web, plus the
// settings that sit beside them.
type webBlock struct {
	Provider        string `json:"provider"`
	Proxy           string `json:"proxy"`
	FetchLimitBytes int64  `json:"fetch_limit_bytes"`
}

// WebProvider is one search provider's configuration. Keys are picoclaw's
// (pkg/config/config.go:868-1001) so one record materializes for both
// harnesses.
type WebProvider struct {
	Name       string
	Enabled    bool
	APIKey     string
	BaseURL    string
	MaxResults int
}

// Web is the whole tools.web block.
type Web struct {
	// Provider names the one to use. Empty or "auto" walks the priority order.
	Provider        string
	Proxy           string
	FetchLimitBytes int64
	Providers       []WebProvider
}

// Enabled reports whether the search tool should exist at all. A tool the model
// is told about and that can never answer is worse than no tool: it spends a
// turn discovering the absence.
func (w Web) Enabled() bool {
	for _, p := range w.Providers {
		if p.Enabled {
			return true
		}
	}
	return false
}

// Find returns one provider's configuration.
func (w Web) Find(name string) (WebProvider, bool) {
	for _, p := range w.Providers {
		if p.Name == name {
			return p, true
		}
	}
	return WebProvider{}, false
}

// WebKeyEnvVar is the environment variable carrying one provider's key. Same
// contract as KeyEnvVar: crab-shell-proxy derives the same name.
func WebKeyEnvVar(provider string) string {
	return "GANGLION_WEB_KEY_" + strings.ToUpper(strings.ReplaceAll(provider, "-", "_"))
}

// Registry is the resolved configuration read from the file: the model list
// and default chain, plus the tool blocks that sit beside them.
type Registry struct {
	Models []ModelSpec
	// Default names the entry a turn runs on when it asks for nothing.
	Default string
	// DefaultFallbacks follow Default, before that entry's own Fallbacks.
	DefaultFallbacks []string
	// Vision is the chain for a turn carrying an image. Empty means there is
	// no dedicated one and the ordinary chain answers.
	Vision      string
	VisionFalls []string
	// ImageGen is the chain the generate_image tool calls. Empty means the
	// tool does not exist.
	ImageGen      string
	ImageGenFalls []string
	// Web is tools.web.
	Web Web
	// Evolution is the evolution block, with picoclaw's defaults applied.
	Evolution Evolution
	// Subturn is the sub-agent fan-out block, with defaults applied.
	Subturn Subturn
	// MCP is the enabled tools.mcp servers, in configuration order. Empty when
	// the block is absent or disabled, which is every deployment that has no
	// memory graph.
	MCP []MCPServer
	// Warnings are operator mistakes that are not errors: a key whose value
	// could not be read, where refusing to boot would be worse than carrying
	// on without it. Carried out rather than logged here because this package
	// has no logger, and because the router reloads this file on every turn --
	// the caller decides whether a repeated warning is worth repeating.
	Warnings []string
}

// EvolutionMode is the opt-in ladder. Each rung does strictly more than the one
// below it, and the default is the bottom.
type EvolutionMode string

const (
	// ModeObserve records what happened and writes nothing else.
	ModeObserve EvolutionMode = "observe"
	// ModeDraft additionally generates skill drafts, and writes no skill.
	ModeDraft EvolutionMode = "draft"
	// ModeApply may write a skill. Requires an approver -- see R10.1.
	ModeApply EvolutionMode = "apply"
)

// ColdTrigger is when the analysis pass runs.
type ColdTrigger string

const (
	ColdAfterTurn ColdTrigger = "after_turn"
	ColdScheduled ColdTrigger = "scheduled"
	ColdManual    ColdTrigger = "manual"
)

// Evolution is the resolved evolution configuration.
type Evolution struct {
	Enabled         bool
	Mode            EvolutionMode
	StateDir        string
	MinTaskCount    int
	MinSuccessRatio float64
	ColdTrigger     ColdTrigger
	ColdTimes       []string
}

// picoclaw's defaults (pkg/config/defaults.go:50-56), carried over so a config
// written for one harness behaves identically on the other.
const (
	DefaultMinTaskCount    = 2
	DefaultMinSuccessRatio = 0.7
)

// Records reports whether the hot path should write anything at all.
//
// AC-1: with evolution off, nothing is written and no turn is measurably
// slower. The hot path is OFF, not merely quiet.
func (e Evolution) Records() bool { return e.Enabled }

// Drafts reports whether the cold path may generate.
func (e Evolution) Drafts() bool {
	return e.Enabled && (e.Mode == ModeDraft || e.Mode == ModeApply)
}

// Writes reports whether an accepted draft may reach disk.
func (e Evolution) Writes() bool { return e.Enabled && e.Mode == ModeApply }

// Kind selects which chain a turn needs.
type Kind string

const (
	// KindText is the ordinary conversation chain.
	KindText Kind = "text"
	// KindVision reads images. Falls back to the text chain when unconfigured,
	// because a model that can see is a property of the model, not of a slot:
	// a deployment whose only model is multimodal needs no second entry.
	KindVision Kind = "vision"
	// KindImageGen produces images. Does NOT fall back: a text model asked to
	// generate an image returns prose describing one, which is worse than an
	// absent tool because it looks like success.
	KindImageGen Kind = "image"
)

// Subturn bounds sub-agent fan-out.
//
// The defaults are picoclaw's where picoclaw has one, with ONE deliberate
// difference: MaxDepth is 1 rather than 3.
//
// At depth 1 a child cannot itself dispatch, which is what keeps the worst case
// small enough to write down -- MaxIterations + MaxChildrenPerTurn *
// MaxChildIterations, or 108 model calls at these numbers. Raising it multiplies
// that per level, which is a decision for an operator who has read this comment.
//
// MaxConcurrent keeps picoclaw's 5, and unlike picoclaw's it is enforced at
// depth 0: there, concurrencySem is nil (newTurnState sets it only for child
// turns), so first-level fan-out is uncapped and the cap bites only one level
// down, where it hardly matters.
type Subturn struct {
	Enabled            bool
	MaxDepth           int
	MaxConcurrent      int
	Timeout            time.Duration
	MaxChildIterations int
	MaxChildrenPerTurn int
}

// The subturn defaults.
const (
	DefaultSubMaxDepth        = 1
	DefaultSubMaxConcurrent   = 5
	DefaultSubTimeoutMinutes  = 5
	DefaultSubChildIterations = 6
	DefaultSubChildrenPerTurn = 16
)

// DefaultSubturn is the block a deployment gets when it has no config file at
// all -- which is every container that has not been recreated since the harness
// grew one. Exported because the composition root builds a synthetic registry
// on exactly that path, and a zero Subturn there would mean max_depth 0 and
// max_concurrent 0: sub-agents silently switched off by a struct literal.
func DefaultSubturn() Subturn {
	return Subturn{
		Enabled:            true,
		MaxDepth:           DefaultSubMaxDepth,
		MaxConcurrent:      DefaultSubMaxConcurrent,
		Timeout:            DefaultSubTimeoutMinutes * time.Minute,
		MaxChildIterations: DefaultSubChildIterations,
		MaxChildrenPerTurn: DefaultSubChildrenPerTurn,
	}
}

// Reasoning depth.
//
// The vocabulary is picoclaw's, exactly: `model_list[].thinking_level` with
// these six values (pkg/config/config.go:780). A seventh would break the admin
// editor that already renders the field, so there is no seventh.
const (
	ThinkingOff      = "off"
	ThinkingLow      = "low"
	ThinkingMedium   = "medium"
	ThinkingHigh     = "high"
	ThinkingXHigh    = "xhigh"
	ThinkingAdaptive = "adaptive"
)

// ThinkingLevels is the vocabulary in ascending order of effort, with adaptive
// last because it is not a point on that scale -- it hands the decision to the
// provider rather than naming an amount.
var ThinkingLevels = []string{
	ThinkingOff, ThinkingLow, ThinkingMedium, ThinkingHigh, ThinkingXHigh, ThinkingAdaptive,
}

// ParseThinkingLevel normalizes a configured or model-chosen level.
//
// Case-insensitive and whitespace-tolerant, matching picoclaw's own parser
// (pkg/agent/thinking.go:16-54). An unrecognised value yields ok=false rather
// than a default: picoclaw reads unknown as `off`, which makes a typo look
// exactly like a deliberate choice to think less.
func ParseThinkingLevel(s string) (string, bool) {
	v := strings.ToLower(strings.TrimSpace(s))
	for _, l := range ThinkingLevels {
		if v == l {
			return l, true
		}
	}
	return "", false
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
func (r Registry) Chain(turnModel string, kind Kind) []string {
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

	switch kind {
	case KindVision:
		add(r.Vision)
		add(r.VisionFalls...)
		if len(out) == 0 {
			// No dedicated vision chain. The ordinary one answers, and if its
			// model cannot see, the loop's degradation path is what the member
			// meets -- not a refusal here.
			return r.Chain(turnModel, KindText)
		}
	case KindImageGen:
		add(r.ImageGen)
		add(r.ImageGenFalls...)
	default:
		if _, ok := r.Find(turnModel); ok {
			add(turnModel)
		} else {
			add(r.Default)
			add(r.DefaultFallbacks...)
		}
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
		return Registry{Subturn: DefaultSubturn()}, nil
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

			ThinkingBody: e.ThinkingBody,
		}
		// An unparseable level is ABSENT, not `off`. picoclaw reads unknown as
		// off, which makes a typo indistinguishable from a deliberate choice to
		// think less -- and a model that silently never thinks looks like a
		// working configuration right up to the bill.
		if e.ThinkingLevel != "" {
			if lvl, ok := ParseThinkingLevel(e.ThinkingLevel); ok {
				spec.ThinkingLevel = lvl
			} else {
				reg.Warnings = append(reg.Warnings, fmt.Sprintf(
					"model %q: thinking_level %q is not one of %s; the model will be sent no depth field",
					spec.Name, e.ThinkingLevel, strings.Join(ThinkingLevels, "|")))
			}
		}
		// The model actually sent on the wire defaults to the alias, which is
		// how picoclaw's own examples read when the two are the same string.
		if spec.Model == "" {
			spec.Model = e.ModelName
		}
		key, err := resolveKey(e.APIKeys, spec.Name, res, keyEnv, KeyEnvVar)
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

	reg.Evolution = loadEvolution(f.Evolution)
	reg.Subturn = loadSubturn(f)
	reg.Vision = f.Agents.Defaults.ImageModel
	reg.VisionFalls = f.Agents.Defaults.ImageModelFallbacks
	reg.ImageGen = f.Agents.Defaults.ImageGenModel
	reg.ImageGenFalls = f.Agents.Defaults.ImageGenModelFallbacks

	web, err := loadWeb(f.Tools.Web, res, keyEnv)
	if err != nil {
		return Registry{}, err
	}
	reg.Web = web
	servers, err := loadMCP(f)
	if err != nil {
		return Registry{}, err
	}
	reg.MCP = servers
	return reg, nil
}

// WebProviderNames is the set of providers this harness implements, in the
// order they are preferred when tools.web.provider is unset.
//
// picoclaw's own order, restricted to the implemented set: the keyed providers
// first, then the self-hosted one, then the one that needs no credential at all
// and therefore always works. Declared HERE rather than in the adapter because
// this is also the order the loader reads the config in, and two orders that
// have to agree are one order too many.
var WebProviderNames = []string{"brave", "tavily", "searxng", "duckduckgo"}

// loadWeb decodes tools.web. An absent block yields a Web with no enabled
// provider, which is what makes the tool absent rather than useless.
func loadWeb(raw json.RawMessage, res secret.Resolver, keyEnv func(string) string) (Web, error) {
	if len(raw) == 0 {
		return Web{}, nil
	}
	var head webBlock
	if err := json.Unmarshal(raw, &head); err != nil {
		return Web{}, fmt.Errorf("parse tools.web: %w", err)
	}
	// Decoded to raw messages FIRST, then one provider at a time.
	//
	// tools.web mixes scalars (provider, proxy, fetch_limit_bytes) with objects
	// (one per provider) in the same object. A single decode into
	// map[string]<providerStruct> therefore fails on the scalars and silently
	// yields no providers -- which is how the whole block reads as "search is
	// off" while looking perfectly correct in the file.
	var byName map[string]json.RawMessage
	if err := json.Unmarshal(raw, &byName); err != nil {
		return Web{}, fmt.Errorf("parse tools.web: %w", err)
	}

	w := Web{Provider: head.Provider, Proxy: head.Proxy, FetchLimitBytes: head.FetchLimitBytes}
	for _, name := range WebProviderNames {
		blob, ok := byName[name]
		if !ok {
			continue
		}
		var e struct {
			Enabled    *bool    `json:"enabled"`
			APIKey     string   `json:"api_key"`
			APIKeys    []string `json:"api_keys"`
			BaseURL    string   `json:"base_url"`
			MaxResults int      `json:"max_results"`
		}
		if err := json.Unmarshal(blob, &e); err != nil {
			return Web{}, fmt.Errorf("parse tools.web.%s: %w", name, err)
		}
		p := WebProvider{
			Name: name,
			// Unlike a model entry, a provider block that says nothing is NOT
			// enabled. Declaring `"brave": {}` is how picoclaw's own examples
			// leave a provider present and off, and turning every mentioned
			// provider on would enable one the operator only meant to key.
			Enabled:    e.Enabled != nil && *e.Enabled,
			BaseURL:    e.BaseURL,
			MaxResults: e.MaxResults,
		}
		key, err := resolveKey(append([]string{e.APIKey}, e.APIKeys...), name, res, keyEnv, WebKeyEnvVar)
		if err != nil {
			return Web{}, fmt.Errorf("tools.web.%s: %w", name, err)
		}
		p.APIKey = key
		w.Providers = append(w.Providers, p)
	}
	return w, nil
}

// resolveKey applies FR-4's order: the environment first, the file second.
//
// The environment wins because that is the path the proxy uses, and because a
// key that never enters a file cannot be read by anything that gets pointed at
// the file by mistake.
func resolveKey(inline []string, name string, res secret.Resolver, keyEnv func(string) string, namer func(string) string) (string, error) {
	if keyEnv != nil {
		if v := keyEnv(namer(name)); v != "" {
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

// loadEvolution applies picoclaw's defaults to whatever the file said.
//
// An unrecognised mode or trigger falls back to the SAFEST value rather than
// failing the load. A typo in `mode` must not stop an agent from answering, and
// the safe direction is unambiguous here: observe records and writes nothing.
func loadEvolution(raw *struct {
	Enabled         *bool    `json:"enabled"`
	Mode            string   `json:"mode"`
	StateDir        string   `json:"state_dir"`
	MinTaskCount    *int     `json:"min_task_count"`
	MinSuccessRatio *float64 `json:"min_success_ratio"`
	ColdPathTrigger string   `json:"cold_path_trigger"`
	ColdPathTimes   []string `json:"cold_path_times"`
}) Evolution {
	e := Evolution{
		Mode:            ModeObserve,
		MinTaskCount:    DefaultMinTaskCount,
		MinSuccessRatio: DefaultMinSuccessRatio,
		ColdTrigger:     ColdAfterTurn,
	}
	if raw == nil {
		return e
	}
	e.Enabled = raw.Enabled != nil && *raw.Enabled
	e.StateDir = raw.StateDir
	e.ColdTimes = raw.ColdPathTimes
	switch EvolutionMode(strings.TrimSpace(raw.Mode)) {
	case ModeDraft:
		e.Mode = ModeDraft
	case ModeApply:
		e.Mode = ModeApply
	}
	switch ColdTrigger(strings.TrimSpace(raw.ColdPathTrigger)) {
	case ColdScheduled:
		e.ColdTrigger = ColdScheduled
	case ColdManual:
		e.ColdTrigger = ColdManual
	}
	if raw.MinTaskCount != nil && *raw.MinTaskCount > 0 {
		e.MinTaskCount = *raw.MinTaskCount
	}
	if raw.MinSuccessRatio != nil && *raw.MinSuccessRatio > 0 {
		e.MinSuccessRatio = *raw.MinSuccessRatio
	}
	return e
}

// MCPServer is one configured MCP server, after validation.
//
// Only what a client needs: this harness speaks one transport, so there is no
// Type to carry forward -- a server that is not http never reaches here.
type MCPServer struct {
	// Name is the key under tools.mcp.servers. It names the server in a boot
	// refusal and in a failed call, and is the only handle an operator has.
	Name    string
	URL     string
	Headers map[string]string
}

// loadMCP reads tools.mcp.
//
// REFUSES rather than skips, and the distinction is the whole point. A silently
// absent memory is the failure the proxy's harness gate table exists to prevent:
// the agent is told nothing, remembers nothing, and looks exactly like an agent
// that was never given a graph. An operator who wrote a server block meant it.
//
// Three refusals, all naming the server:
//
//   - a `command`, which means stdio. The ganglion container has no package
//     manager and no way to run one, so this cannot be made to work later in the
//     turn; it has to stop the boot.
//   - a type that is not "http". Same reason, stated by the other field --
//     picoclaw's own record carries `command: ""` AND `type: "http"`, so an
//     entry missing the type was written by something else.
//   - no url, which is an http server that names no host.
//
// An `enabled: false` entry, or a disabled tools.mcp block, is not a refusal:
// that is an operator turning something off, which is an instruction rather than
// a mistake.
func loadMCP(f file) ([]MCPServer, error) {
	m := f.Tools.MCP
	if m == nil || (m.Enabled != nil && !*m.Enabled) {
		return nil, nil
	}
	// Sorted, because the servers arrive in a map and the tool order they
	// produce reaches the model's prompt. An order that changed between boots
	// would change the prompt for no reason anyone could see.
	names := make([]string, 0, len(m.Servers))
	for name := range m.Servers {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]MCPServer, 0, len(names))
	for _, name := range names {
		srv := m.Servers[name]
		if srv.Enabled != nil && !*srv.Enabled {
			continue
		}
		if strings.TrimSpace(srv.Command) != "" {
			return nil, fmt.Errorf("mcp server %q declares a command: this harness speaks only streamable HTTP, "+
				"and the container has no way to run a stdio server", name)
		}
		if t := strings.TrimSpace(srv.Type); t != "http" {
			return nil, fmt.Errorf("mcp server %q has type %q: only \"http\" is supported", name, t)
		}
		if strings.TrimSpace(srv.URL) == "" {
			return nil, fmt.Errorf("mcp server %q has no url", name)
		}
		out = append(out, MCPServer{Name: name, URL: srv.URL, Headers: srv.Headers})
	}
	return out, nil
}

// loadSubturn applies the defaults, then whatever the file overrides.
//
// Every key is a pointer in the file struct, so "absent" and "present but zero"
// stay distinguishable: max_concurrent: 0 is an operator saying "run nothing",
// and reading it as "use the default of 5" would be the opposite instruction.
func loadSubturn(f file) Subturn {
	st := DefaultSubturn()
	if sa := f.Tools.Subagent; sa != nil && sa.Enabled != nil {
		st.Enabled = *sa.Enabled
	}
	c := f.Agents.Defaults.Subturn
	if c == nil {
		return st
	}
	if c.MaxDepth != nil {
		st.MaxDepth = *c.MaxDepth
	}
	if c.MaxConcurrent != nil {
		st.MaxConcurrent = *c.MaxConcurrent
	}
	if c.DefaultTimeoutMinutes != nil {
		st.Timeout = time.Duration(*c.DefaultTimeoutMinutes) * time.Minute
	}
	if c.MaxChildIterations != nil {
		st.MaxChildIterations = *c.MaxChildIterations
	}
	if c.MaxChildrenPerTurn != nil {
		st.MaxChildrenPerTurn = *c.MaxChildrenPerTurn
	}
	return st
}
