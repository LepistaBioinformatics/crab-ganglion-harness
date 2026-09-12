// Package router selects which endpoint answers a turn.
//
// It is the Provider adapter the loop actually holds once more than one model
// exists. Its job is small and worth stating exactly, because everything it
// does NOT do belongs somewhere else:
//
//   - it maps a model_name to the client that speaks to that model's endpoint;
//   - it re-reads the registry file when the file changes;
//   - it answers "what is the candidate chain for this turn".
//
// It does NOT decide when to fall back. That is the loop's, because the rule --
// fall back only while nothing has reached the member yet -- is about the turn,
// and the router cannot see the turn.
package router

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/config"
	"github.com/LepistaBioinformatics/crab-ganglion-harness/internal/domain"
)

// Build makes a provider for one model. Injected so this package depends on no
// other adapter -- internal/domain/arch_test.go enforces that adapters do not
// import each other, and a router that constructed openai.Clients would break
// it for no benefit.
type Build func(config.ModelSpec) domain.Provider

// Reload re-reads the registry from wherever it came from.
type Reload func() (config.Registry, error)

// Router implements domain.Provider and domain.ModelChain over a registry.
type Router struct {
	build  Build
	reload Reload
	path   string
	logf   func(string, ...any)

	mu       sync.RWMutex
	reg      config.Registry
	clients  map[string]domain.Provider
	lastMod  time.Time
	lastSize int64
}

// New returns a router over an initial registry.
//
// path may be empty, which disables reloading -- used by tests and by a
// deployment configured entirely from the environment.
func New(reg config.Registry, path string, build Build, reload Reload, logf func(string, ...any)) *Router {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	r := &Router{build: build, reload: reload, path: path, logf: logf}
	r.adopt(reg)
	r.stamp()
	return r
}

// adopt replaces the registry and rebuilds the client map. Callers hold no
// lock; this takes it.
func (r *Router) adopt(reg config.Registry) {
	clients := make(map[string]domain.Provider, len(reg.Models))
	for _, m := range reg.Models {
		clients[m.Name] = r.build(m)
	}
	r.mu.Lock()
	r.reg, r.clients = reg, clients
	r.mu.Unlock()
}

// stamp records the file's identity so a later refresh can tell it apart.
func (r *Router) stamp() {
	if r.path == "" {
		return
	}
	if st, err := os.Stat(r.path); err == nil {
		r.mu.Lock()
		r.lastMod, r.lastSize = st.ModTime(), st.Size()
		r.mu.Unlock()
	}
}

// SendsThinking reports whether this model would carry a reasoning-depth field,
// which is what tells the loop that a failed request is worth retrying without
// one (domain.ThinkingChain).
//
// No refresh here: it is asked DURING a turn, and Chain already pinned the
// registry for that turn at its start. Re-reading mid-turn is the one thing
// this router is built not to do.
func (r *Router) SendsThinking(model string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	m, ok := r.reg.Find(model)
	return ok && m.ThinkingLevel != ""
}

// DeepModels reports the models that accept a depth field (domain.ThinkingChain).
//
// No refresh, for the reason SendsThinking gives: it is asked DURING a turn,
// against the registry Chain pinned at that turn's start.
func (r *Router) DeepModels() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.reg.Deep()
}

// Chain answers the loop's "which models, in what order" and is also the
// reload point.
//
// Reloading HERE rather than on a ticker means a change is picked up exactly
// once per turn, at its start, and never mid-turn -- so a turn runs against one
// coherent registry from beginning to end even if an admin saves twice while it
// is streaming.
func (r *Router) Chain(turnModel string, kind domain.ModelKind) []string {
	r.refresh()
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.reg.Chain(turnModel, config.Kind(kind))
}

// refresh re-reads the file if mtime or size moved.
//
// A failed reload KEEPS THE RUNNING CONFIGURATION and logs. The alternative --
// adopting an empty or half-written registry -- would take an agent off the air
// because somebody was midway through saving a file, which is the opposite of
// what a hot reload is for.
func (r *Router) refresh() {
	if r.path == "" || r.reload == nil {
		return
	}
	st, err := os.Stat(r.path)
	if err != nil {
		return
	}
	r.mu.RLock()
	same := st.ModTime().Equal(r.lastMod) && st.Size() == r.lastSize
	r.mu.RUnlock()
	if same {
		return
	}
	// Stamp before reading, so a file that fails to parse is not re-read on
	// every single turn for as long as it stays broken.
	r.mu.Lock()
	r.lastMod, r.lastSize = st.ModTime(), st.Size()
	r.mu.Unlock()

	reg, err := r.reload()
	if err != nil {
		r.logf("model registry %s changed but did not load, keeping the previous one: %v", r.path, err)
		return
	}
	if len(reg.Models) == 0 {
		r.logf("model registry %s changed and now declares no usable model, keeping the previous one", r.path)
		return
	}
	r.adopt(reg)
	r.logf("model registry %s reloaded: %d model(s), default %q", r.path, len(reg.Models), reg.Default)
	// Once per reload, and a reload only happens when the file actually
	// changed -- so an operator who just saved a typo hears about it now
	// rather than at the next restart.
	for _, w := range reg.Warnings {
		r.logf("model registry %s: %s", r.path, w)
	}
}

// Complete dispatches to the client for req.Model.
func (r *Router) Complete(ctx context.Context, req domain.Completion) (domain.Stream, error) {
	r.mu.RLock()
	c, ok := r.clients[req.Model]
	spec, found := r.reg.Find(req.Model)
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("no endpoint configured for model %q", req.Model)
	}
	// A candidate with no key is refused HERE rather than sent unauthenticated.
	// The provider's answer to a keyless request is a 401 whose text talks
	// about authorization, which reads like a wrong key rather than a missing
	// one -- and the loop would then try the next candidate for the wrong
	// reason and report the wrong thing when the chain ran out.
	if found && spec.APIKey == "" {
		return nil, fmt.Errorf("model %q has no API key: set %s", req.Model, config.KeyEnvVar(req.Model))
	}
	// The wire model, not the alias. `model_name` is this stack's handle for
	// the entry; `model` is what the endpoint understands, and sending the
	// alias is how a registry that looks correct returns "unknown model".
	if found && spec.Model != "" {
		req.Model = spec.Model
	}
	return c.Complete(ctx, req)
}

// Registry exposes the current registry, for the composition root's boot log
// and for tests.
func (r *Router) Registry() config.Registry {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.reg
}
